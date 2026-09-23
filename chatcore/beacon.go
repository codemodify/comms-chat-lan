package chatcore

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

// The beacon: presence over UDP multicast.
//
// This is deliberately not mDNS. A correct DNS-SD responder is RFC 6762 and
// RFC 6763 — conflict resolution, known-answer suppression, cache-flush
// bits, negative responses — and a partial one is worse than none, because
// it collides with the real responder (avahi) already running on the
// machine. What this app needs is one sentence, repeated: "I am <id>, I am
// called <nick>, dial me on <port>". That is what the beacon sends.
//
// The cost is stated in the README and in docs/protocol.md: this app is
// discoverable only by other copies of itself. `avahi-browse` will not
// show it.
const (
	// BeaconGroup is the IPv4 multicast group. 239.192.0.0/14 is the
	// IPv4 organisation-local scope (RFC 2365): it is routed inside a
	// site and never off it, which is exactly the reach a LAN chat wants.
	BeaconGroup = "239.192.77.77"
	// BeaconPort is the UDP port the group is joined on.
	BeaconPort = 47771
	// BeaconMagic prefixes every packet, so anything else that happens to
	// use the group is rejected in one comparison instead of being handed
	// to the JSON parser.
	BeaconMagic = "CHATLAN/1 "
	// beaconMaxPacket bounds a packet. It is under the smallest MTU we
	// might meet, so an announcement is never fragmented.
	beaconMaxPacket = 1200

	// BeaconInterval is how often presence is re-announced.
	BeaconInterval = 12 * time.Second
	// BeaconExpiry is how long a peer stays in the roster without being
	// heard from. Three missed announcements: one lost packet, which on a
	// busy wireless network is routine, must not make a peer blink out.
	BeaconExpiry = 40 * time.Second
)

// Beacon is the UDP multicast [Discovery].
type Beacon struct {
	// Group and Port override the defaults. Tests use a private group so
	// a test run cannot be seen by, or see, a real one on the same LAN.
	Group string
	Port  int
	// Interval and Expiry override the defaults.
	Interval time.Duration
	Expiry   time.Duration
	// Iface pins the outgoing multicast interface by name. Empty means
	// every multicast-capable interface that is up.
	Iface string
	// Loopback lets a second instance on this machine hear us. It is on
	// by default: two copies on one box is how the app is tried out, and
	// it is how the tests work.
	Loopback bool

	mu  sync.Mutex
	ann Announcement
}

// NewBeacon is the beacon configured from the environment: UITK_CHAT_IFACE
// pins the interface.
func NewBeacon() *Beacon {
	return &Beacon{Iface: strings.TrimSpace(os.Getenv(EnvIface)), Loopback: true}
}

// Name implements [Discovery].
func (b *Beacon) Name() string { return "beacon" }

// Update implements [Discovery].
func (b *Beacon) Update(a Announcement) {
	a.Version = ProtocolVersion
	b.mu.Lock()
	b.ann = a
	b.mu.Unlock()
}

func (b *Beacon) current() Announcement {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ann
}

func (b *Beacon) group() string {
	if b.Group != "" {
		return b.Group
	}
	return BeaconGroup
}

func (b *Beacon) port() int {
	if b.Port > 0 {
		return b.Port
	}
	return BeaconPort
}

func (b *Beacon) interval() time.Duration {
	if b.Interval > 0 {
		return b.Interval
	}
	return BeaconInterval
}

func (b *Beacon) expiry() time.Duration {
	if b.Expiry > 0 {
		return b.Expiry
	}
	return BeaconExpiry
}

// Run implements [Discovery]: it joins the group, announces on a ticker,
// reads other announcements and expires peers that go quiet, until ctx is
// cancelled. It sends a goodbye on the way out.
func (b *Beacon) Run(ctx context.Context, ev DiscoveryEvents) error {
	addr := &net.UDPAddr{IP: net.ParseIP(b.group()).To4(), Port: b.port()}
	if addr.IP == nil {
		return fmt.Errorf("chat: %q is not an IPv4 multicast address", b.group())
	}

	// SO_REUSEADDR and SO_REUSEPORT: several copies of the app on one
	// machine must all be able to bind the group port. Without them the
	// second instance fails to start, which is the first thing anyone
	// trying this app out will do.
	lc := net.ListenConfig{Control: reusePort}
	pc, err := lc.ListenPacket(ctx, "udp4", ":"+strconv.Itoa(b.port()))
	if err != nil {
		return fmt.Errorf("chat: beacon listen: %w", err)
	}
	defer func() { _ = pc.Close() }()

	p := ipv4.NewPacketConn(pc)
	ifaces := b.multicastInterfaces(ev)
	joined := 0
	for i := range ifaces {
		if err := p.JoinGroup(&ifaces[i], addr); err != nil {
			ev.note(fmt.Sprintf("beacon: %s: join %s: %v", ifaces[i].Name, addr.IP, err))
			continue
		}
		joined++
	}
	if joined == 0 {
		// Still useful: on a single-instance machine with no multicast
		// route, the loopback join below keeps a local pair talking.
		if err := p.JoinGroup(nil, addr); err != nil {
			ev.note(fmt.Sprintf("beacon: no interface joined %s: %v", addr.IP, err))
		} else {
			joined = 1
		}
	}
	_ = p.SetMulticastLoopback(b.Loopback)
	_ = p.SetMulticastTTL(1) // one hop: this is a LAN app, by construction
	_ = p.SetControlMessage(ipv4.FlagInterface, true)

	var wg sync.WaitGroup
	heard := make(chan Announcement, 64)

	// The reader. It is stopped by closing the socket from the sender
	// goroutine, which is how a blocking ReadFrom is cancelled in Go.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(heard)
		buf := make([]byte, beaconMaxPacket+64)
		self := b.current().ID
		for {
			n, _, src, err := p.ReadFrom(buf)
			if err != nil {
				return
			}
			a, ok := parseBeacon(buf[:n], src)
			if !ok || a.ID == "" {
				continue
			}
			if self == "" {
				self = b.current().ID
			}
			if a.ID == self {
				continue // our own announcement, looped back
			}
			select {
			case heard <- a:
			default: // a flood must not grow a queue; drop and carry on
			}
		}
	}()

	b.announce(p, addr, ifaces, false)
	tick := time.NewTicker(b.interval())
	defer tick.Stop()
	sweep := time.NewTicker(b.expiry() / 4)
	defer sweep.Stop()

	last := map[PeerID]time.Time{}
	for {
		select {
		case <-ctx.Done():
			b.announce(p, addr, ifaces, true) // goodbye, best effort
			_ = pc.Close()
			wg.Wait()
			return nil

		case a, ok := <-heard:
			if !ok {
				return nil
			}
			if a.Bye {
				if _, known := last[a.ID]; known {
					delete(last, a.ID)
					ev.left(a.ID)
				}
				continue
			}
			last[a.ID] = a.Heard
			ev.appeared(a)

		case <-tick.C:
			b.announce(p, addr, ifaces, false)

		case now := <-sweep.C:
			for id, t := range last {
				if now.Sub(t) > b.expiry() {
					delete(last, id)
					ev.left(id)
				}
			}
		}
	}
}

// announce writes one packet to the group on every joined interface. A
// multi-homed machine (a laptop on wifi with a docker bridge up) otherwise
// announces on whichever interface the routing table prefers, which is
// rarely the one the other people are on.
func (b *Beacon) announce(p *ipv4.PacketConn, addr *net.UDPAddr, ifaces []net.Interface, bye bool) {
	a := b.current()
	if a.ID == "" {
		return
	}
	a.Version = ProtocolVersion
	a.Bye = bye
	if bye {
		a.Presence = PresenceOffline
	}
	pkt, err := encodeBeacon(a)
	if err != nil {
		return
	}
	if len(ifaces) == 0 {
		_, _ = p.WriteTo(pkt, nil, addr)
		return
	}
	for i := range ifaces {
		if err := p.SetMulticastInterface(&ifaces[i]); err != nil {
			continue
		}
		_, _ = p.WriteTo(pkt, nil, addr)
	}
}

// multicastInterfaces is every interface worth announcing on: up,
// multicast-capable, and with an IPv4 address. UITK_CHAT_IFACE pins one.
func (b *Beacon) multicastInterfaces(ev DiscoveryEvents) []net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		ev.note("beacon: cannot list interfaces: " + err.Error())
		return nil
	}
	var out []net.Interface
	for _, ifi := range all {
		if b.Iface != "" && ifi.Name != b.Iface {
			continue
		}
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if !hasIPv4(ifi) {
			continue
		}
		out = append(out, ifi)
	}
	if len(out) == 0 && b.Iface != "" {
		ev.note("beacon: " + EnvIface + "=" + b.Iface + " matches no up multicast interface")
	}
	return out
}

func hasIPv4(ifi net.Interface) bool {
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return true
		}
	}
	return false
}

// encodeBeacon is the wire form: the magic, then one compact JSON object.
func encodeBeacon(a Announcement) ([]byte, error) {
	body, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	pkt := append([]byte(BeaconMagic), body...)
	if len(pkt) > beaconMaxPacket {
		// Only the room list can grow without bound. Drop rooms rather
		// than send a packet that might be fragmented or dropped.
		a.Rooms = nil
		body, err = json.Marshal(a)
		if err != nil {
			return nil, err
		}
		pkt = append([]byte(BeaconMagic), body...)
		if len(pkt) > beaconMaxPacket {
			return nil, fmt.Errorf("chat: announcement too large (%d bytes)", len(pkt))
		}
	}
	return pkt, nil
}

// parseBeacon decodes a packet. The peer's address is taken from the
// packet source, never from the payload: an announcement can lie about its
// nickname, but it cannot point us at a machine it does not occupy.
func parseBeacon(pkt []byte, src net.Addr) (Announcement, bool) {
	if len(pkt) > beaconMaxPacket+64 || len(pkt) < len(BeaconMagic) {
		return Announcement{}, false
	}
	if string(pkt[:len(BeaconMagic)]) != BeaconMagic {
		return Announcement{}, false
	}
	var a Announcement
	if err := json.Unmarshal(pkt[len(BeaconMagic):], &a); err != nil {
		return Announcement{}, false
	}
	if !a.valid() {
		return Announcement{}, false
	}
	a = a.clean()
	a.Heard = time.Now()
	if ua, ok := src.(*net.UDPAddr); ok && ua.IP != nil {
		a.Addr = net.JoinHostPort(ua.IP.String(), strconv.Itoa(a.Port))
	}
	if a.Addr == "" && !a.Bye {
		return Announcement{}, false
	}
	return a, true
}
