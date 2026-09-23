package chatcore

import (
	"context"
	"time"
)

// Discovery is how comms-chatd learns that other copies of the app exist on
// the local network. It is an interface with exactly one implementation
// today, [Beacon] — a UDP multicast announcement — but the daemon is
// written against the interface alone so a second implementation (mDNS /
// DNS-SD, or a static list of hosts read from a file) can be dropped in
// without touching the node, the store or the RPC.
//
// The contract:
//
//   - Run blocks until ctx is cancelled. It announces us periodically and
//     watches for other announcements.
//   - Every announcement it hears, including repeats, is delivered to the
//     [DiscoveryEvents.Appeared] callback. Repeats are how a peer's
//     presence and room list stay fresh; the caller deduplicates.
//   - A peer that stops announcing, or that says goodbye, is delivered to
//     [DiscoveryEvents.Left] exactly once, until it appears again.
//   - Update replaces what is announced from the next announcement on. It
//     may be called before Run, during Run, and concurrently.
//   - Run must send one final goodbye, best effort, before it returns, so
//     the other side does not have to wait out the expiry.
type Discovery interface {
	// Run announces and listens until ctx is cancelled.
	Run(ctx context.Context, ev DiscoveryEvents) error
	// Update changes what we announce.
	Update(a Announcement)
	// Name is what status.get reports ("beacon", "mdns", "off").
	Name() string
}

// DiscoveryEvents are the callbacks a Discovery implementation calls. They
// run on the discovery's own goroutine and must not block for long; the
// daemon's implementations only push onto a channel.
type DiscoveryEvents struct {
	// Appeared is called for every announcement heard, including repeats
	// from a peer that is already known.
	Appeared func(Announcement)
	// Left is called once when a peer goes quiet or says goodbye.
	Left func(PeerID)
	// Note is called with a human-readable transport problem, for
	// status.get and the logs. It is not fatal.
	Note func(string)
}

func (ev DiscoveryEvents) appeared(a Announcement) {
	if ev.Appeared != nil {
		ev.Appeared(a)
	}
}

func (ev DiscoveryEvents) left(id PeerID) {
	if ev.Left != nil {
		ev.Left(id)
	}
}

func (ev DiscoveryEvents) note(s string) {
	if ev.Note != nil {
		ev.Note(s)
	}
}

// Announcement is what one peer tells the network about itself: who it
// says it is and how to open a conversation with it. It is the payload of
// a beacon packet and, deliberately, exactly the set of facts a peer needs
// before it can dial — nothing in it is verified. See docs/protocol.md.
type Announcement struct {
	// Version is the protocol version the peer speaks. A peer announcing a
	// version this build does not understand is ignored entirely rather
	// than half-parsed.
	Version int `json:"v"`
	// ID is the announcing peer's self-asserted [PeerID].
	ID PeerID `json:"id"`
	// Nick and Color are display only.
	Nick  string `json:"nick,omitempty"`
	Color string `json:"color,omitempty"`
	// Host is the announcer's host name, for the UI. Never resolved.
	Host string `json:"host,omitempty"`
	// Port is the TCP port the peer accepts conversations on. The address
	// is the packet's source IP with this port: a peer cannot redirect us
	// to a third machine.
	Port int `json:"port"`
	// Rooms the peer has joined. Room traffic is only fanned out to peers
	// that say they are in the room.
	Rooms []string `json:"rooms,omitempty"`
	// Presence is what the peer says it is doing.
	Presence Presence `json:"pres,omitempty"`
	// Bye marks the farewell packet sent at shutdown.
	Bye bool `json:"bye,omitempty"`

	// Addr is filled in by the receiver from the packet source: "ip:port".
	// It is never read off the wire.
	Addr string `json:"-"`
	// Heard is when we received it. Never read off the wire.
	Heard time.Time `json:"-"`
}

// Peer is the roster entry an announcement implies.
func (a Announcement) Peer() Peer {
	pr := a.Presence.Valid()
	if a.Bye {
		pr = PresenceOffline
	}
	return Peer{
		ID: a.ID, Nick: a.Nick, Color: a.Color, Host: a.Host,
		Addr: a.Addr, Rooms: a.Rooms, Presence: pr,
		LastSeen: a.Heard, Known: true,
	}
}

// valid rejects an announcement that cannot be acted on, before it reaches
// the roster. Everything here comes off an unauthenticated multicast
// socket, so each field is checked rather than assumed.
func (a Announcement) valid() bool {
	if a.Version != ProtocolVersion {
		return false
	}
	if len(a.ID) != 32 || !isHex(string(a.ID)) {
		return false
	}
	if a.Port <= 0 || a.Port > 65535 {
		return false
	}
	if len([]rune(a.Nick)) > 64 || len([]rune(a.Host)) > 64 {
		return false
	}
	if len(a.Rooms) > 32 {
		return false
	}
	return true
}

// clean folds an announcement to what the rest of the daemon may see.
func (a Announcement) clean() Announcement {
	a.Nick = clipRunes(a.Nick, 32)
	a.Host = clipRunes(a.Host, 48)
	if !validColor(a.Color) {
		a.Color = ColorForID(a.ID)
	}
	a.Rooms = canonRooms(a.Rooms)
	a.Presence = a.Presence.Valid()
	return a
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return len(s) > 0
}

// noDiscovery is the Discovery used when UITK_CHAT_NO_DISCOVERY is set: it
// announces nothing and hears nothing, so the daemon is usable (and
// testable) on a network where multicast is unwelcome.
type noDiscovery struct{}

// NoDiscovery is a Discovery that does nothing at all.
func NoDiscovery() Discovery { return noDiscovery{} }

func (noDiscovery) Run(ctx context.Context, _ DiscoveryEvents) error {
	<-ctx.Done()
	return nil
}

func (noDiscovery) Update(Announcement) {}
func (noDiscovery) Name() string        { return "off" }
