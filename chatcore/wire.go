package chatcore

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// The peer-to-peer wire protocol. Two peers talk over one plain TCP
// connection carrying length-prefixed JSON frames:
//
//	uint32 big-endian length | length bytes of JSON
//
// A length prefix rather than NDJSON because file chunks travel in the
// same stream and base64 in a line-oriented protocol makes the framing
// depend on the payload. JSON rather than a packed binary encoding
// because the whole protocol fits on one page of docs/protocol.md, and a
// chat app is never limited by its frame encoder.
//
// Every frame is a [Frame]. Unknown frame types are ignored rather than
// treated as errors: that is the entire forward-compatibility story, and
// it is why the type is a string and not an enum.
//
// See docs/protocol.md for the full specification.
const (
	// ProtocolVersion is the version announced in the beacon and in the
	// hello frame. A peer announcing a different version is ignored: with
	// one version in the wild there is nothing to negotiate, and pretending
	// otherwise would be untested code.
	ProtocolVersion = 1

	// MaxFrame bounds one frame. A file chunk is the largest legitimate
	// frame; everything else is a few hundred bytes. The limit exists so a
	// peer cannot make us allocate whatever it likes by sending a large
	// length prefix.
	MaxFrame = 1 << 20 // 1 MiB

	// ChunkSize is the payload of one file chunk before base64. It is a
	// compromise: large enough that a big file is not thousands of round
	// trips, small enough that progress moves visibly and a cancel is
	// acted on promptly.
	ChunkSize = 64 * 1024
)

// Frame types.
const (
	// FrameHello is the first frame in each direction. Both sides send it;
	// neither waits for the other's before sending its own.
	FrameHello = "hello"
	// FrameMsg carries one chat message.
	FrameMsg = "msg"
	// FrameAck confirms one message was stored by the other side. It is
	// what turns a message's state from "sending" into "delivered".
	FrameAck = "ack"
	// FrameTyping is the typing indicator. It is advisory and never stored.
	FrameTyping = "typing"
	// FramePresence is a presence change between beacons.
	FramePresence = "presence"
	// FrameOffer offers a file. Nothing is sent until it is accepted.
	FrameOffer = "offer"
	// FrameAccept accepts a file offer.
	FrameAccept = "accept"
	// FrameDecline declines or cancels a file offer.
	FrameDecline = "decline"
	// FrameChunk is one slice of an accepted file.
	FrameChunk = "chunk"
	// FrameDone ends a file transfer.
	FrameDone = "done"
	// FrameBye is sent before a clean close, so the other side can show
	// the peer as gone without waiting for the beacon to expire.
	FrameBye = "bye"
)

// Frame is one unit of the peer-to-peer protocol. It is a single flat
// struct rather than a tagged union: the fields a type does not use are
// omitted on the wire, and a reader that wants a field a sender did not
// send gets the zero value, which is the behaviour forward compatibility
// needs anyway.
type Frame struct {
	Type string `json:"t"`

	// hello
	Version int      `json:"v,omitempty"`
	From    PeerID   `json:"from,omitempty"`
	Nick    string   `json:"nick,omitempty"`
	Color   string   `json:"color,omitempty"`
	Host    string   `json:"host,omitempty"`
	Port    int      `json:"port,omitempty"`
	Rooms   []string `json:"rooms,omitempty"`

	// msg / ack
	ID   MessageID      `json:"id,omitempty"`
	Conv ConversationID `json:"conv,omitempty"`
	Body string         `json:"body,omitempty"`
	Sent time.Time      `json:"sent,omitempty"`
	Seq  uint64         `json:"seq,omitempty"`

	// typing / presence
	Typing   bool     `json:"typing,omitempty"`
	Presence Presence `json:"pres,omitempty"`

	// offer / accept / decline / chunk / done
	Transfer TransferID `json:"tid,omitempty"`
	Name     string     `json:"name,omitempty"`
	Size     int64      `json:"size,omitempty"`
	MIME     string     `json:"mime,omitempty"`
	SHA256   string     `json:"sha256,omitempty"`
	Offset   int64      `json:"off,omitempty"`
	Data     []byte     `json:"data,omitempty"` // base64 in JSON
	Last     bool       `json:"last,omitempty"`
	Reason   string     `json:"reason,omitempty"`
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

// helloFrame is our side of the handshake.
func helloFrame(self Identity, port int, host string) Frame {
	return Frame{
		Type: FrameHello, Version: ProtocolVersion,
		From: self.ID, Nick: self.Nick, Color: self.Color,
		Host: host, Port: port, Rooms: self.Rooms,
	}
}

// peerFromHello is the roster entry a hello implies. addr is the address we
// actually have the connection to, which is trusted over anything the
// frame claims.
func peerFromHello(f Frame, addr string, known bool) Peer {
	p := Peer{
		ID: f.From, Nick: clipRunes(f.Nick, 32), Color: f.Color,
		Host: clipRunes(f.Host, 48), Addr: addr, Rooms: canonRooms(f.Rooms),
		Presence: PresenceOnline, LastSeen: time.Now(), Known: known,
	}
	if !validColor(p.Color) {
		p.Color = ColorForID(p.ID)
	}
	return p
}

// validHello checks a handshake before anything is done with it.
func validHello(f Frame) error {
	if f.Type != FrameHello {
		return fmt.Errorf("chat: expected a hello frame, got %q", f.Type)
	}
	if f.Version != ProtocolVersion {
		return fmt.Errorf("chat: peer speaks protocol version %d, this build speaks %d", f.Version, ProtocolVersion)
	}
	if len(f.From) != 32 || !isHex(string(f.From)) {
		return fmt.Errorf("chat: peer sent a malformed id")
	}
	return nil
}

// msgFrame is the wire form of one outgoing message.
func msgFrame(m Message) Frame {
	return Frame{
		Type: FrameMsg, ID: m.ID, Conv: m.Conv, From: m.From,
		Nick: m.FromNick, Body: m.Body, Sent: m.Sent, Seq: m.Seq,
	}
}

// messageFromFrame is the stored form of an incoming message. from is the
// authenticated-by-connection sender: a peer may not file a message under
// somebody else's ID, whatever the frame says.
//
// The conversation is likewise recomputed rather than taken from the
// frame. A peer says "room:general" or nothing; if it names a direct
// conversation, the only one it may name is the one between us and it.
func messageFromFrame(f Frame, from PeerID, nick string) (Message, bool) {
	if f.ID == "" || len([]rune(f.Body)) > maxBodyRunes {
		return Message{}, false
	}
	conv := DirectConv(from)
	if f.Conv.IsRoom() {
		room := CanonRoom(f.Conv.Room())
		if room == "" || len(room) > 48 {
			return Message{}, false
		}
		conv = RoomConv(room)
	}
	sent := f.Sent
	if sent.IsZero() {
		sent = time.Now()
	}
	if nick == "" {
		nick = f.Nick
	}
	return Message{
		ID: f.ID, Conv: conv, From: from, FromNick: clipRunes(nick, 32),
		Body: f.Body, Sent: sent, Seq: f.Seq, Received: time.Now(),
	}, true
}
