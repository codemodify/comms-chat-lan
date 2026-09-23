// Package chatwire is what comms-chat-lan-clientd and
// comms-chat-lan-server say to each other: how a client finds a server on
// the LAN, and the framed protocol it speaks once it has.
//
// It knows nothing about user interfaces, and nothing about the local
// socket a front end talks to. See docs/protocol.md, which specifies
// everything here well enough to write a second implementation from.
package chatwire

import (
	"context"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
)

// Discovery is how comms-chat-lan-clientd learns that a server exists on
// the local network. It is an interface with one implementation today,
// [Beacon] — a UDP multicast announcement the server sends and clients
// listen for — but clientd is written against the interface alone, so a
// second implementation (mDNS / DNS-SD, a list of hosts read from a file,
// a service record) can be dropped in without anything else moving.
//
// The direction is the one thing that changed when this application grew
// a server: before, every peer announced itself and every peer listened.
// Now the server announces and the clients only listen, which is also why
// [Announcement] no longer carries a nickname or a room list — a server
// has neither.
//
// The contract:
//
//   - Run blocks until ctx is cancelled. On the server it announces
//     periodically; on a client it listens.
//   - Every announcement heard, including repeats, is delivered to
//     [DiscoveryEvents.Appeared]. Repeats are how a client notices that
//     the server moved; the caller deduplicates.
//   - A server that stops announcing, or that says goodbye, is delivered
//     to [DiscoveryEvents.Left] exactly once, until it appears again.
//   - Update replaces what is announced from the next announcement on. It
//     may be called before Run, during Run, and concurrently.
//   - Run must send one final goodbye, best effort, before it returns.
type Discovery interface {
	// Run announces (a server) or listens (a client) until ctx is done.
	Run(ctx context.Context, ev DiscoveryEvents) error
	// Update changes what we announce. A client never calls it.
	Update(a Announcement)
	// Name is what status.get reports ("beacon", "off").
	Name() string
}

// DiscoveryEvents are the callbacks a Discovery implementation calls. They
// run on the discovery's own goroutine and must not block for long; the
// daemons' implementations only push onto a channel.
type DiscoveryEvents struct {
	// Appeared is called for every announcement heard, including repeats.
	Appeared func(Announcement)
	// Left is called once when a server goes quiet or says goodbye.
	Left func(ServerID)
	// Note is called with a human-readable transport problem, for
	// status.get and the logs. It is not fatal.
	Note func(string)
}

func (ev DiscoveryEvents) appeared(a Announcement) {
	if ev.Appeared != nil {
		ev.Appeared(a)
	}
}

func (ev DiscoveryEvents) left(id ServerID) {
	if ev.Left != nil {
		ev.Left(id)
	}
}

func (ev DiscoveryEvents) note(s string) {
	if ev.Note != nil {
		ev.Note(s)
	}
}

// ServerID identifies one comms-chat-lan-server: 16 random bytes, hex,
// minted on the server's first run and kept in its data directory.
//
// It is also the name of the ordering a client's cursor belongs to. A
// cursor is a position in one server's sequence and means nothing in
// another's, so a client that finds a different ID starts again rather
// than carrying a number across.
type ServerID string

// Announcement is what a server tells the network about itself: who it is
// and where to connect. It is the payload of a beacon packet and,
// deliberately, exactly the set of facts a client needs before it can
// dial — nothing in it is verified. See docs/protocol.md.
type Announcement struct {
	// Version is the protocol version the server speaks. A server
	// announcing a version this build does not understand is ignored
	// entirely rather than half-parsed.
	Version int `json:"v"`
	// ID is the server's [ServerID].
	ID ServerID `json:"id"`
	// Name is what the server calls itself, for display.
	Name string `json:"name,omitempty"`
	// Host is the server's host name, for display. Never resolved.
	Host string `json:"host,omitempty"`
	// Port is the TCP port the server accepts clients on. The address is
	// the packet's source IP with this port: a server cannot redirect a
	// client to a third machine.
	Port int `json:"port"`
	// Bye marks the farewell packet sent at shutdown.
	Bye bool `json:"bye,omitempty"`

	// Addr is filled in by the receiver from the packet source: "ip:port".
	// It is never read off the wire.
	Addr string `json:"-"`
	// Heard is when we received it. Never read off the wire.
	Heard time.Time `json:"-"`
}

// valid rejects an announcement that cannot be acted on. Everything here
// comes off an unauthenticated multicast socket, so each field is checked
// rather than assumed.
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
	if len([]rune(a.Name)) > 64 || len([]rune(a.Host)) > 64 {
		return false
	}
	return true
}

// clean folds an announcement to what the rest of the daemon may see.
func (a Announcement) clean() Announcement {
	a.Name = chat.ClipRunes(a.Name, 48)
	a.Host = chat.ClipRunes(a.Host, 48)
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

// noDiscovery is the Discovery used when UITK_CHAT_NO_DISCOVERY is set, or
// when a client was told where the server is: it announces nothing and
// hears nothing, so both daemons are usable — and testable — on a network
// where multicast is unwelcome or does not cross the segment.
type noDiscovery struct{}

// NoDiscovery is a Discovery that does nothing at all.
func NoDiscovery() Discovery { return noDiscovery{} }

func (noDiscovery) Run(ctx context.Context, _ DiscoveryEvents) error {
	<-ctx.Done()
	return nil
}

func (noDiscovery) Update(Announcement) {}
func (noDiscovery) Name() string        { return "off" }
