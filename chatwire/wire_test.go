package chatwire

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
)

func TestFrameRoundTrip(t *testing.T) {
	in := Frame{
		Type: FrameMsg, ID: "m1", Conv: chat.RoomConv("general"), From: "abcd",
		Body: "hello, LAN", Sent: time.Now().UTC().Truncate(time.Millisecond), Seq: 4,
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Type != in.Type || out.ID != in.ID || out.Body != in.Body || out.Seq != in.Seq {
		t.Fatalf("round trip changed the frame: %#v", out)
	}
	if !out.Sent.Equal(in.Sent) {
		t.Fatalf("sent %v, want %v", out.Sent, in.Sent)
	}
}

func TestReadFrameRefusesAnOversizeLengthWithoutAllocating(t *testing.T) {
	// A sender that claims a 4 GiB frame must be refused on the header
	// alone. Trying to resynchronise a stream after this is how a parser
	// becomes an attack surface, so the caller drops the connection.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 0xFFFFFFFF)
	_, err := ReadFrame(bytes.NewReader(hdr[:]))
	if err == nil {
		t.Fatal("an oversize length was accepted")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestAMessageIsFiledUnderTheSenderTheConnectionProves(t *testing.T) {
	// The sender is whoever enrolled on this connection, never the
	// "from" the frame claims.
	f := Frame{Type: FrameMsg, ID: "m", From: "ffff", Body: "not me"}
	m, ok := MessageFromFrame(f, "aaaa")
	if !ok {
		t.Fatal("frame rejected")
	}
	if m.From != "aaaa" {
		t.Fatalf("message filed as %q, want aaaa", m.From)
	}
}

func TestOversizeBodyIsRejected(t *testing.T) {
	f := Frame{Type: FrameMsg, ID: "m", Body: strings.Repeat("x", chat.MaxBodyRunes+1)}
	if _, ok := MessageFromFrame(f, "aaaa"); ok {
		t.Fatal("a body over the limit was accepted")
	}
}

func TestAnIncomingFileNameIsDefangedBeforeItIsEvenAMessage(t *testing.T) {
	f := Frame{Type: FrameMsg, ID: "m", Transfer: "t1", Name: "../../../.bashrc", Size: 3}
	m, ok := MessageFromFrame(f, "aaaa")
	if !ok || m.Transfer == nil {
		t.Fatalf("frame rejected: %#v", m)
	}
	if m.Transfer.Name != "bashrc" {
		t.Fatalf("transfer name %q, want bashrc", m.Transfer.Name)
	}
}

func TestHelloIsCheckedBeforeItIsBelieved(t *testing.T) {
	good := Frame{Type: FrameHello, Version: ProtocolVersion, From: chat.PeerID(strings.Repeat("a", 32))}
	if err := ValidHello(good); err != nil {
		t.Fatalf("a good hello was refused: %v", err)
	}
	for _, bad := range []Frame{
		{Type: FrameMsg, Version: ProtocolVersion, From: chat.PeerID(strings.Repeat("a", 32))},
		{Type: FrameHello, Version: ProtocolVersion + 1, From: chat.PeerID(strings.Repeat("a", 32))},
		{Type: FrameHello, Version: ProtocolVersion, From: "short"},
		{Type: FrameHello, Version: ProtocolVersion, From: chat.PeerID(strings.Repeat("z", 32))},
	} {
		if err := ValidHello(bad); err == nil {
			t.Errorf("accepted %#v", bad)
		}
	}
}

func TestWelcomeIsCheckedToo(t *testing.T) {
	good := Frame{Type: FrameWelcome, Version: ProtocolVersion, Server: ServerID(strings.Repeat("b", 32))}
	if err := ValidWelcome(good); err != nil {
		t.Fatalf("a good welcome was refused: %v", err)
	}
	// A refusal is reported as what it is rather than as a protocol error.
	err := ValidWelcome(Frame{Type: FrameError, Reason: "no"})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("an error frame came back as %v", err)
	}
}

func TestBeaconPacketRoundTripAndTheAddressComesFromThePacket(t *testing.T) {
	a := Announcement{
		Version: ProtocolVersion, ID: ServerID(strings.Repeat("a", 32)),
		Name: "the office", Host: "box", Port: 47772,
	}
	pkt, err := encodeBeacon(a)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pkt, []byte(BeaconMagic)) {
		t.Fatalf("packet does not start with the magic: %q", pkt[:12])
	}
	src := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 47771}
	got, ok := parseBeacon(pkt, src)
	if !ok {
		t.Fatal("our own packet did not parse")
	}
	// The address is built from where the packet came from, not from what
	// it says: an announcement may lie about its name but may not point
	// anyone at a third machine.
	if got.Addr != "10.0.0.7:47772" {
		t.Fatalf("addr %q, want 10.0.0.7:47772", got.Addr)
	}
}

func TestBeaconRejectsRubbishOnTheGroup(t *testing.T) {
	src := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 47771}
	for _, pkt := range [][]byte{
		[]byte("some other protocol entirely"),
		[]byte("CHATLAN/1 " + `{"v":1,"id":"` + strings.Repeat("a", 32) + `","port":47772}`), // the old protocol
		[]byte(BeaconMagic + "not json"),
		[]byte(BeaconMagic + `{"v":99,"id":"aaaa","port":1}`),                                  // wrong version
		[]byte(BeaconMagic + `{"v":2,"id":"nothex--------------------------------","port":1}`), // bad id
		[]byte(BeaconMagic + `{"v":2,"id":"` + strings.Repeat("a", 32) + `","port":0}`),        // no port
		{},
	} {
		if _, ok := parseBeacon(pkt, src); ok {
			t.Errorf("accepted %q", pkt)
		}
	}
}
