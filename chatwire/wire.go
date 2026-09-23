package chatwire

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
)

// The client-to-server protocol. One client daemon holds one plain TCP
// connection to the server, carrying length-prefixed JSON frames:
//
//	uint32 big-endian length | length bytes of JSON
//
// A length prefix rather than NDJSON because file chunks travel in the
// same stream and base64 in a line-oriented protocol makes the framing
// depend on the payload. JSON rather than a packed binary encoding
// because the whole protocol fits on one page of docs/protocol.md, and a
// chat application is never limited by its frame encoder.
//
// Every frame is a [Frame]. Unknown frame types are ignored rather than
// treated as errors: that is the entire forward-compatibility story, and
// it is why the type is a string and not an enum.
//
// See docs/protocol.md for the full specification, including what happens
// to a stale cursor, two clients on one identity, a server restart, a
// message composed while the server was down and an offer nobody answers.
const (
	// ProtocolVersion is the version announced in the beacon and in the
	// hello frame. A server or client announcing a different version is
	// ignored: with one version in the wild there is nothing to
	// negotiate, and pretending otherwise would be untested code.
	//
	// Version 1 was the peer-to-peer protocol this replaced. It is gone,
	// not deprecated: nothing in this repository speaks it.
	ProtocolVersion = 2

	// DefaultServerPort is where comms-chat-lan-server listens. It is
	// fixed rather than ephemeral because a client that cannot hear the
	// beacon is told "<host>" and should not also have to be told a port.
	DefaultServerPort = 47772

	// MaxFrame bounds one frame. A file chunk is the largest legitimate
	// frame; everything else is a few hundred bytes, except a roster,
	// which is bounded by how many people are enrolled. The limit exists
	// so that neither end can make the other allocate whatever it likes
	// by sending a large length prefix.
	MaxFrame = 1 << 20 // 1 MiB

	// ChunkSize is the payload of one file chunk before base64. It is a
	// compromise: large enough that a big file is not thousands of round
	// trips, small enough that progress moves visibly and a cancel is
	// acted on promptly.
	ChunkSize = 64 * 1024
)

// Frame types. Every one of them says who may send it, because a server
// that accepts a frame only a server should send is how a client starts
// speaking for other people.
const (
	// FrameHello is the client's first frame: who it is and where it got
	// to last time. The server answers with FrameWelcome. Client only.
	FrameHello = "hello"
	// FrameWelcome is the server's answer to a hello: the enrolment is
	// done. Server only.
	FrameWelcome = "welcome"
	// FrameSynced ends the backlog burst that follows a welcome or a
	// join: everything the client missed has been sent, and Cursor is
	// where it has now got to. Server only.
	FrameSynced = "synced"

	// FrameMsg carries one chat message: from a client, one it composed;
	// from the server, one it has sequenced, with Seq set.
	FrameMsg = "msg"
	// FrameSeq tells the sender the sequence number its message was
	// given. It is what turns "sending" into "delivered". Server only.
	FrameSeq = "seq"

	// FrameRoster is the whole roster (Peers) or one changed entry
	// (Peer). Server only.
	FrameRoster = "roster"
	// FramePresence is a client saying what it is doing. Client only;
	// the server relays it as a roster update.
	FramePresence = "presence"
	// FrameTyping is the typing indicator, advisory and never stored.
	// Both ways: the server stamps From and fans it out.
	FrameTyping = "typing"

	// FrameJoin and FrameLeave change the sender's room membership. The
	// server answers a join with the room's recent history. Client only.
	FrameJoin  = "join"
	FrameLeave = "leave"

	// FrameOffer offers a file. Nothing moves until it is accepted.
	FrameOffer = "offer"
	// FrameAccept accepts a file offer.
	FrameAccept = "accept"
	// FrameDecline declines an offer, cancels one, or reports that one
	// expired or cannot be relayed.
	FrameDecline = "decline"
	// FrameChunk is one slice of an accepted file, relayed through the
	// server.
	FrameChunk = "chunk"
	// FrameDone ends a file transfer.
	FrameDone = "done"

	// FramePing and FramePong keep an idle connection honest: a client
	// that has said nothing for a while is asked, and a connection that
	// does not answer is dropped rather than left looking alive.
	FramePing = "ping"
	FramePong = "pong"

	// FrameError reports a refusal. It never closes the connection by
	// itself; the frame says what went wrong and the client decides.
	FrameError = "error"
	// FrameBye is sent before a clean close by either end, so the other
	// does not have to wait out a timeout to know.
	FrameBye = "bye"
)

// Error codes carried by [FrameError]. They exist so a client can tell
// "you asked for something impossible" from "try again later".
const (
	// ErrBadFrame is a frame that could not be acted on at all.
	ErrBadFrame = 1
	// ErrNotEnrolled is any frame other than hello before a hello.
	ErrNotEnrolled = 2
	// ErrNoSuchPeer is a message or offer to somebody the server has
	// never enrolled.
	ErrNoSuchPeer = 3
	// ErrNotInRoom is a message to a room the sender has not joined.
	ErrNotInRoom = 4
	// ErrRefused is a request the server will not do: too many transfers
	// open, a file over the limit, a body over the limit.
	ErrRefused = 5
)

// Frame is one unit of the protocol. It is a single flat struct rather
// than a tagged union: the fields a type does not use are omitted on the
// wire, and a reader that wants a field a sender did not send gets the
// zero value, which is the behaviour forward compatibility needs anyway.
type Frame struct {
	Type string `json:"t"`

	// hello / welcome
	Version int    `json:"v,omitempty"`
	Host    string `json:"host,omitempty"`
	// Cursor is the last sequence number the client has seen (hello), or
	// the server's current one (welcome, synced).
	Cursor uint64 `json:"cur,omitempty"`
	// Server and ServerName identify the server (welcome).
	Server     ServerID `json:"srv,omitempty"`
	ServerName string   `json:"srvname,omitempty"`
	// Reset says the cursor did not belong to this server's sequence, so
	// what follows is a fresh window rather than a delta.
	Reset bool      `json:"reset,omitempty"`
	Now   time.Time `json:"now,omitempty"`

	// From is who the frame is about: the sender's own id on a hello, and
	// the id the server has stamped on anything it relays. A client's
	// claim about anyone but itself is ignored.
	From  chat.PeerID `json:"from,omitempty"`
	Nick  string      `json:"nick,omitempty"`
	Color string      `json:"color,omitempty"`
	Rooms []string    `json:"rooms,omitempty"`

	// msg / seq
	ID     chat.MessageID      `json:"id,omitempty"`
	Conv   chat.ConversationID `json:"conv,omitempty"`
	Body   string              `json:"body,omitempty"`
	Sent   time.Time           `json:"sent,omitempty"`
	Seq    uint64              `json:"seq,omitempty"`
	System bool                `json:"sys,omitempty"`

	// roster
	Peers []chat.Peer `json:"peers,omitempty"`
	Peer  *chat.Peer  `json:"peer,omitempty"`

	// presence / typing / rooms
	Presence chat.Presence `json:"pres,omitempty"`
	Typing   bool          `json:"typing,omitempty"`
	Room     string        `json:"room,omitempty"`

	// offer / accept / decline / chunk / done
	Transfer chat.TransferID `json:"tid,omitempty"`
	To       chat.PeerID     `json:"to,omitempty"`
	Name     string          `json:"name,omitempty"`
	Size     int64           `json:"size,omitempty"`
	MIME     string          `json:"mime,omitempty"`
	SHA256   string          `json:"sha256,omitempty"`
	Offset   int64           `json:"off,omitempty"`
	Data     []byte          `json:"data,omitempty"` // base64 in JSON
	Reason   string          `json:"reason,omitempty"`
	Code     int             `json:"code,omitempty"`
}

// WriteFrame writes one length-prefixed frame.
func WriteFrame(w io.Writer, f Frame) error {
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(body) > MaxFrame {
		return fmt.Errorf("chat: frame %s is %d bytes, over the %d limit", f.Type, len(body), MaxFrame)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadFrame reads one length-prefixed frame. A length over [MaxFrame] is
// an error and the caller must drop the connection: the stream cannot be
// resynchronised, and trying is how a parser becomes an attack surface.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return Frame{}, fmt.Errorf("chat: empty frame")
	}
	if n > MaxFrame {
		return Frame{}, fmt.Errorf("chat: frame of %d bytes is over the %d limit", n, MaxFrame)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Frame{}, err
	}
	var f Frame
	if err := json.Unmarshal(body, &f); err != nil {
		return Frame{}, fmt.Errorf("chat: bad frame: %w", err)
	}
	return f, nil
}

// HelloFrame is a client's enrolment: who it is, what it believes it has
// joined, and the sequence number it last saw. Enrolment is open — the
// server refuses nobody — so there is nothing else in it.
// srv is the server the cursor belongs to, empty if the client has never
// enrolled anywhere. A server that is not that one ignores the cursor:
// a position in one server's sequence means nothing in another's.
func HelloFrame(self chat.Identity, host string, cursor uint64, srv ServerID) Frame {
	return Frame{
		Type: FrameHello, Version: ProtocolVersion,
		From: self.ID, Nick: self.Nick, Color: self.Color,
		Host: host, Rooms: self.Rooms, Cursor: cursor, Server: srv,
		Presence: self.Presence.Valid(),
	}
}

// ValidHello checks an enrolment before anything is done with it. It
// checks the shape of the identity and nothing about its right to exist:
// there is no such check, by design. See docs/security.md.
func ValidHello(f Frame) error {
	if f.Type != FrameHello {
		return fmt.Errorf("chat: expected a hello frame, got %q", f.Type)
	}
	if f.Version != ProtocolVersion {
		return fmt.Errorf("chat: client speaks protocol version %d, this build speaks %d", f.Version, ProtocolVersion)
	}
	if len(f.From) != 32 || !isHex(string(f.From)) {
		return fmt.Errorf("chat: client sent a malformed id")
	}
	return nil
}

// ValidWelcome checks the server's answer.
func ValidWelcome(f Frame) error {
	if f.Type == FrameError {
		return fmt.Errorf("chat: the server refused the enrolment: %s", f.Reason)
	}
	if f.Type != FrameWelcome {
		return fmt.Errorf("chat: expected a welcome frame, got %q", f.Type)
	}
	if f.Version != ProtocolVersion {
		return fmt.Errorf("chat: server speaks protocol version %d, this build speaks %d", f.Version, ProtocolVersion)
	}
	if len(f.Server) != 32 || !isHex(string(f.Server)) {
		return fmt.Errorf("chat: server sent a malformed id")
	}
	return nil
}

// PeerFromHello is the roster entry an enrolment implies. addr is the
// address the connection actually came from, which is trusted over
// anything the frame claims.
func PeerFromHello(f Frame, addr string) chat.Peer {
	p := chat.Peer{
		ID: f.From, Nick: chat.ClipRunes(f.Nick, 32), Color: f.Color,
		Host: chat.ClipRunes(f.Host, 48), Addr: addr,
		Rooms: chat.CanonRooms(f.Rooms), Presence: f.Presence.Valid(),
		LastSeen: time.Now(),
	}
	if p.Presence == chat.PresenceOffline {
		p.Presence = chat.PresenceOnline
	}
	if !chat.ValidColor(p.Color) {
		p.Color = chat.ColorForID(p.ID)
	}
	return p
}

// MsgFrame is the wire form of one message. The server sends it with Seq
// set and Conv rewritten to the name the recipient uses; a client sends it
// with neither.
func MsgFrame(m chat.Message) Frame {
	f := Frame{
		Type: FrameMsg, ID: m.ID, Conv: m.Conv, From: m.From,
		Nick: m.FromNick, Body: m.Body, Sent: m.Sent, Seq: m.Seq,
		System: m.System,
	}
	if m.Transfer != nil {
		f.Transfer = m.Transfer.ID
		f.Name = m.Transfer.Name
		f.Size = m.Transfer.Size
	}
	return f
}

// MessageFromFrame is the stored form of a message frame. from is the
// sender the frame was authenticated-by-connection as: on the server that
// is the connection's enrolled id, and on a client it is whoever the
// server stamped, because a client has no other source of truth.
//
// The conversation is checked rather than taken on trust: a client may
// only name a room, or the one-to-one conversation between itself and one
// other person.
func MessageFromFrame(f Frame, from chat.PeerID) (chat.Message, bool) {
	if f.ID == "" || len([]rune(f.Body)) > chat.MaxBodyRunes {
		return chat.Message{}, false
	}
	sent := f.Sent
	if sent.IsZero() {
		sent = time.Now()
	}
	m := chat.Message{
		ID: f.ID, Conv: f.Conv, From: from, FromNick: chat.ClipRunes(f.Nick, 32),
		Body: f.Body, Sent: sent, Seq: f.Seq, Received: time.Now(),
		System: f.System,
	}
	if f.Transfer != "" {
		m.Transfer = &chat.TransferRef{
			ID: f.Transfer, Name: chat.SafeBaseName(f.Name), Size: f.Size,
		}
	}
	return m, true
}

// ErrorFrame is a refusal with a reason a person could read.
func ErrorFrame(code int, reason string) Frame {
	return Frame{Type: FrameError, Code: code, Reason: chat.ClipRunes(reason, 200)}
}
