// Package chat is what every part of comms-chat-lan agrees on: the model
// (peers, messages, conversations, transfers, identity), the store that
// holds a conversation on disk, and the local JSON-RPC a front end speaks
// to comms-chat-lan-clientd.
//
// Both daemons and both front ends import it, and it imports neither of
// them. It has no dependency on a UI toolkit, on a display or on a session
// bus; the one place a desktop is reachable at all is the
// [DesktopNotifier] function seam, and clientd leaves it nil.
package chat

import (
	"strings"
	"time"
)

// PeerID identifies one person's comms-chat-lan client. It is 16 random
// bytes, hex-encoded, generated on first run and kept in identity.json.
// The client keeps it; enrolling with a server does not change it.
//
// It is self-asserted: nothing proves that the client claiming an ID is
// the one that generated it, and the server does not ask. See
// docs/security.md.
type PeerID string

// ConversationID names a conversation. A one-to-one conversation is named
// after the other peer ("peer:<id>"); a room is named after the room
// ("room:<name>"). Use [DirectConv] and [RoomConv] rather than building the
// string by hand.
type ConversationID string

// DirectConv is the conversation with one peer, as a client names it:
// "the conversation with them". Both ends of a one-to-one conversation
// therefore call it by different names, which is why the server stores it
// under [DirectKey] and rewrites the name for each recipient.
func DirectConv(p PeerID) ConversationID { return ConversationID("peer:" + string(p)) }

// DirectKey is the server's name for the one-to-one conversation between
// two clients: "dm:<lower id>+<higher id>". It is the same string whoever
// asks, which a per-client name could never be.
func DirectKey(a, b PeerID) ConversationID {
	if b < a {
		a, b = b, a
	}
	return ConversationID("dm:" + string(a) + "+" + string(b))
}

// IsDirectKey reports whether c is a server-side one-to-one conversation.
func (c ConversationID) IsDirectKey() bool { return strings.HasPrefix(string(c), "dm:") }

// DirectPair is the two peers of a [DirectKey] conversation.
func (c ConversationID) DirectPair() (PeerID, PeerID, bool) {
	if !c.IsDirectKey() {
		return "", "", false
	}
	rest := strings.TrimPrefix(string(c), "dm:")
	i := strings.IndexByte(rest, '+')
	if i <= 0 {
		return "", "", false
	}
	return PeerID(rest[:i]), PeerID(rest[i+1:]), true
}

// ConvPeer is the peer a client-side direct conversation belongs to
// ("" for a room or a server-side key).
func ConvPeer(c ConversationID) PeerID {
	if !strings.HasPrefix(string(c), "peer:") {
		return ""
	}
	return PeerID(strings.TrimPrefix(string(c), "peer:"))
}

// RoomConv is the conversation in a room. Room names are lower-cased and
// trimmed so "General" and "general " are the same room on every machine.
func RoomConv(room string) ConversationID { return ConversationID("room:" + CanonRoom(room)) }

// CanonRoom folds a room name to its canonical form.
func CanonRoom(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// IsRoom reports whether c names a room rather than a single peer.
func (c ConversationID) IsRoom() bool { return strings.HasPrefix(string(c), "room:") }

// Room is the room name of a room conversation ("" for a direct one).
func (c ConversationID) Room() string { return strings.TrimPrefix(string(c), "room:") }

// Peer is somebody else enrolled with the same server: who they say they
// are, and whether they are connected right now. It is the server's
// roster entry, relayed to every client.
type Peer struct {
	ID    PeerID `json:"id"`
	Nick  string `json:"nick"`
	Color string `json:"color,omitempty"` // avatar colour, "#rrggbb"
	Host  string `json:"host,omitempty"`  // the peer's host name, for display
	// Addr is the address the server saw them enrol from, for display. It
	// is never dialled: clients do not talk to each other.
	Addr string `json:"addr,omitempty"`
	// Rooms the server has them down as having joined.
	Rooms []string `json:"rooms,omitempty"`

	Presence Presence  `json:"presence"`
	LastSeen time.Time `json:"lastSeen,omitempty"`
	// Blocked peers are filtered locally by this client's daemon: the
	// server relays their messages, and clientd drops them on arrival.
	Blocked bool `json:"blocked,omitempty"`
}

// DisplayName is the nickname if there is one, else a short form of the ID.
func (p Peer) DisplayName() string {
	if n := strings.TrimSpace(p.Nick); n != "" {
		return n
	}
	return ShortID(p.ID)
}

// ShortID is the first 8 hex digits of an ID, for display.
func ShortID(id PeerID) string {
	if len(id) <= 8 {
		return string(id)
	}
	return string(id[:8])
}

// Presence is what a peer says it is doing.
type Presence string

// The presence states. Anything else on the wire reads as PresenceOffline.
const (
	PresenceOnline  Presence = "online"
	PresenceAway    Presence = "away"
	PresenceBusy    Presence = "busy"
	PresenceOffline Presence = "offline"
)

// Valid folds an unknown presence string to offline.
func (p Presence) Valid() Presence {
	switch p {
	case PresenceOnline, PresenceAway, PresenceBusy:
		return p
	default:
		return PresenceOffline
	}
}

// MessageID is minted by the client that composes the message:
// "<short peer id>-<nanoseconds>-<random>". The sender mints it so that a
// message composed while the server was unreachable already has its final
// identity, and so that resending it after a reconnect is recognised as
// the same message rather than filed twice.
type MessageID string

// Message is one line of conversation.
type Message struct {
	ID   MessageID      `json:"id"`
	Conv ConversationID `json:"conv"`
	From PeerID         `json:"from"`
	// FromNick is the sender's nickname at the time they sent it. History
	// keeps what they were called then, not what they are called now.
	FromNick string `json:"fromNick,omitempty"`
	Body     string `json:"body"`
	// Sent is the sender's wall clock, shown beside the message. It does
	// not order anything: a client whose clock is wrong mislabels its own
	// messages and nothing else.
	Sent time.Time `json:"sent"`
	// Seq is the server's global sequence number, assigned when the server
	// accepted the message. It is what orders every conversation, and it
	// is the cursor a client resumes from. Zero means this copy has not
	// reached the server yet.
	Seq uint64 `json:"seq"`
	// Received is our clock, when we first saw it. Zero for our own.
	Received time.Time `json:"received,omitempty"`

	Mine  bool         `json:"mine,omitempty"`
	State MessageState `json:"state,omitempty"`
	Read  bool         `json:"read,omitempty"`
	// Transfer is set when the message announces a file offer.
	Transfer *TransferRef `json:"transfer,omitempty"`
	// System marks a message the app wrote itself (joined, left, declined).
	System bool `json:"system,omitempty"`
}

// TransferRef ties a message to a file transfer.
type TransferRef struct {
	ID   TransferID `json:"id"`
	Name string     `json:"name"`
	Size int64      `json:"size"`
}

// MessageState is the delivery state of a message we sent.
type MessageState string

// Delivery states. Only outgoing messages carry one.
const (
	StateQueued    MessageState = "queued"    // the server is not reachable yet
	StateSending   MessageState = "sending"   // on the wire
	StateDelivered MessageState = "delivered" // the server sequenced it
	StateFailed    MessageState = "failed"    // the server refused it
)

// Conversation is one row of the conversation list.
type Conversation struct {
	ID    ConversationID `json:"id"`
	Title string         `json:"title"`
	// Peer is the other end of a direct conversation ("" for a room).
	Peer PeerID `json:"peer,omitempty"`
	// Room is the room name ("" for a direct conversation).
	Room     string    `json:"room,omitempty"`
	Unread   int       `json:"unread"`
	Last     string    `json:"last,omitempty"`
	LastAt   time.Time `json:"lastAt,omitempty"`
	Presence Presence  `json:"presence,omitempty"`
	Color    string    `json:"color,omitempty"`
	// Members is how many people the server has in a room (0 for direct).
	Members int `json:"members,omitempty"`
}

// TransferID identifies one file transfer, minted by the sender.
type TransferID string

// TransferState is where a transfer has got to.
type TransferState string

// Transfer states.
const (
	TransferOffered   TransferState = "offered"   // we offered, awaiting a reply
	TransferIncoming  TransferState = "incoming"  // they offered, awaiting ours
	TransferRunning   TransferState = "running"   // bytes are moving
	TransferDone      TransferState = "done"      //
	TransferDeclined  TransferState = "declined"  // the other side said no
	TransferFailed    TransferState = "failed"    //
	TransferCancelled TransferState = "cancelled" // we said no
)

// Transfer is one offered or in-flight file.
type Transfer struct {
	ID   TransferID     `json:"id"`
	Conv ConversationID `json:"conv"`
	Peer PeerID         `json:"peer"`
	Name string         `json:"name"`
	Size int64          `json:"size"`
	MIME string         `json:"mime,omitempty"`
	// SHA256 is the sender's digest of the whole file, hex. The receiver
	// recomputes it: it catches a truncated or corrupted transfer, and a
	// relay that lost its place. It is not a security check — anyone who
	// can change the bytes can change the digest with them, and the bytes
	// pass through the server.
	SHA256 string `json:"sha256,omitempty"`

	Incoming bool          `json:"incoming"`
	State    TransferState `json:"state"`
	Done     int64         `json:"done"`
	Path     string        `json:"path,omitempty"`  // where the bytes landed
	Error    string        `json:"error,omitempty"` //
	At       time.Time     `json:"at"`
}

// Identity is who we tell the server we are. It lives in identity.json
// and is the only configuration a client needs: enrolment asks for
// nothing else, and there is no account.
type Identity struct {
	ID    PeerID `json:"id"`
	Nick  string `json:"nick"`
	Color string `json:"color"`
	// Rooms we have joined. The server keeps its own copy; this one is
	// what we re-assert when we enrol, so a client that joined a room
	// while the server was down is still in it afterwards.
	Rooms []string `json:"rooms,omitempty"`
	// AutoAcceptFiles under this many bytes are saved without asking.
	// Zero (the default) asks about every file.
	AutoAcceptFiles int64 `json:"autoAcceptFiles,omitempty"`
	// Presence is the state we tell the server we are in; clientd sends
	// offline on its way out.
	Presence Presence `json:"presence,omitempty"`
}

// Peer is the roster entry this identity implies.
func (id Identity) Peer() Peer {
	return Peer{
		ID: id.ID, Nick: id.Nick, Color: id.Color,
		Rooms: id.Rooms, Presence: id.Presence.Valid(),
	}
}

// NotifyPrefs is what the daemon should raise a notification for. The
// daemon only decides whether to broadcast the event; a front end decides
// whether to put it on a desktop.
type NotifyPrefs struct {
	Enabled bool `json:"enabled"`
	// DirectOnly suppresses room chatter and notifies only on a one-to-one
	// message.
	DirectOnly bool `json:"directOnly,omitempty"`
	// Desktop is the GUI's own toggle, kept here so the TUI and the GUI
	// agree and the setting survives a restart.
	Desktop bool `json:"desktop,omitempty"`
	// Mentions notifies on a room message containing our nickname even
	// when DirectOnly is set.
	Mentions bool `json:"mentions,omitempty"`
}

// DefaultNotifyPrefs is what a first run gets.
func DefaultNotifyPrefs() NotifyPrefs {
	return NotifyPrefs{Enabled: true, Desktop: true, Mentions: true}
}

// DaemonStatus is status.get: enough for a front end to say what is going
// on without a second round trip.
type DaemonStatus struct {
	Version string `json:"version"`
	Socket  string `json:"socket"`
	Self    Peer   `json:"self"`
	// Server is the address of the comms-chat-lan-server this daemon is
	// enrolled with, or the last one it tried.
	Server string `json:"server"`
	// Connected says whether that connection is up right now. When it is
	// false the front ends are looking at the cache, and say so.
	Connected bool `json:"connected"`
	// Discovery is how the server was found: "beacon", "flag", "env", or
	// the reason none of them worked.
	Discovery string `json:"discovery"`
	// Cursor is the last server sequence number this daemon has seen. It
	// is what a resume asks to carry on from.
	Cursor  uint64    `json:"cursor"`
	Peers   int       `json:"peers"`
	Online  int       `json:"online"`
	Started time.Time `json:"started"`
	DataDir string    `json:"dataDir"`
	Clients int       `json:"clients"`
}
