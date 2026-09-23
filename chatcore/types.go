// Package chatcore is comms-chat-lan's engine: identity, peer discovery,
// the peer-to-peer wire protocol, the message store and the JSON-RPC
// daemon that fronts them.
//
// It has no dependency on a UI toolkit, on a display or on a session bus.
// The one place a desktop is reachable at all is the [DesktopNotifier]
// function seam, and the daemon leaves it nil.
package chatcore

import (
	"strings"
	"time"
)

// PeerID identifies one running instance of comms-chat-lan. It is 16 random
// bytes, hex-encoded, generated on first run and kept in identity.json.
//
// It is self-asserted: nothing proves that the peer claiming an ID is the
// one that generated it. See docs/protocol.md, "What this does not protect
// against".
type PeerID string

// ConversationID names a conversation. A one-to-one conversation is named
// after the other peer ("peer:<id>"); a room is named after the room
// ("room:<name>"). Use [DirectConv] and [RoomConv] rather than building the
// string by hand.
type ConversationID string

// DirectConv is the conversation with one peer.
func DirectConv(p PeerID) ConversationID { return ConversationID("peer:" + string(p)) }

// RoomConv is the conversation in a room. Room names are lower-cased and
// trimmed so "General" and "general " are the same room on every machine.
func RoomConv(room string) ConversationID { return ConversationID("room:" + CanonRoom(room)) }

// CanonRoom folds a room name to its canonical form.
func CanonRoom(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// IsRoom reports whether c names a room rather than a single peer.
func (c ConversationID) IsRoom() bool { return strings.HasPrefix(string(c), "room:") }

// Room is the room name of a room conversation ("" for a direct one).
func (c ConversationID) Room() string { return strings.TrimPrefix(string(c), "room:") }

// Peer is the other end of a direct conversation: who they say they are,
// where to reach them, and whether they are reachable right now.
type Peer struct {
	ID    PeerID `json:"id"`
	Nick  string `json:"nick"`
	Color string `json:"color,omitempty"` // avatar colour, "#rrggbb"
	Host  string `json:"host,omitempty"`  // the peer's host name, for display
	Addr  string `json:"addr,omitempty"`  // "ip:port" last seen advertised
	// Rooms the peer advertises it has joined. Room messages are only sent
	// to peers that advertise the room.
	Rooms []string `json:"rooms,omitempty"`

	Presence Presence  `json:"presence"`
	LastSeen time.Time `json:"lastSeen,omitempty"`
	// Known is false for a peer that has only ever connected to us and was
	// never seen on the network — an unsolicited inbound peer. The UI marks
	// those, because anyone on the LAN can be one.
	Known bool `json:"known"`
	// Blocked peers are dropped on accept: no messages, no transfers.
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

// MessageID is globally unique without a coordinator: "<peer id>-<sender
// sequence>-<random>". Two peers that compose at the same instant cannot
// collide, and a duplicate delivery is recognised by ID alone.
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
	// Sent is the sender's wall clock. It orders the conversation; it is
	// not trustworthy, and peers whose clocks disagree will see the same
	// order as each other but not necessarily the true one.
	Sent time.Time `json:"sent"`
	// Seq is the sender's own monotonic counter, the tie-break when two
	// messages carry the same Sent.
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
	StateQueued    MessageState = "queued"    // the peer is not reachable yet
	StateSending   MessageState = "sending"   // on the wire
	StateDelivered MessageState = "delivered" // the peer acked it
	StateFailed    MessageState = "failed"    // gave up (see Message.State)
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
	// Members is how many peers currently advertise a room (0 for direct).
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
	// recomputes it: it catches a truncated or corrupted transfer. It is
	// not a security check — an attacker who can change the bytes can
	// change the digest with them.
	SHA256 string `json:"sha256,omitempty"`

	Incoming bool          `json:"incoming"`
	State    TransferState `json:"state"`
	Done     int64         `json:"done"`
	Path     string        `json:"path,omitempty"`  // where the bytes landed
	Error    string        `json:"error,omitempty"` //
	At       time.Time     `json:"at"`
}

// Identity is who we tell the LAN we are. It lives in identity.json and is
// the only configuration the app needs — there is no account.
type Identity struct {
	ID    PeerID `json:"id"`
	Nick  string `json:"nick"`
	Color string `json:"color"`
	// Rooms we have joined. Advertised in the beacon so other peers know to
	// fan room messages out to us.
	Rooms []string `json:"rooms,omitempty"`
	// AutoAcceptFiles under this many bytes are saved without asking.
	// Zero (the default) asks about every file.
	AutoAcceptFiles int64 `json:"autoAcceptFiles,omitempty"`
	// Presence is the state we advertise; the daemon overrides it with
	// offline while it is shutting down.
	Presence Presence `json:"presence,omitempty"`
}

// Peer is the Peer record other machines see for this identity.
func (id Identity) Peer() Peer {
	return Peer{
		ID: id.ID, Nick: id.Nick, Color: id.Color,
		Rooms: id.Rooms, Presence: id.Presence.Valid(), Known: true,
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
	Version   string    `json:"version"`
	Socket    string    `json:"socket"`
	Self      Peer      `json:"self"`
	Listen    string    `json:"listen"`    // the TCP address peers dial
	Discovery string    `json:"discovery"` // "beacon", "off", or the error
	Peers     int       `json:"peers"`
	Online    int       `json:"online"`
	Started   time.Time `json:"started"`
	DataDir   string    `json:"dataDir"`
	Clients   int       `json:"clients"`
}
