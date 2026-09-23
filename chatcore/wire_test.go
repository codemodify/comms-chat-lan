package chatcore

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	in := Frame{
		Type: FrameMsg, ID: "m1", Conv: RoomConv("general"), From: "abcd",
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
	// A peer that claims a 4 GiB frame must be refused on the header
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

func TestPeerCannotSendAMessageAsSomebodyElse(t *testing.T) {
	// The sender is the peer we authenticated by connection, never the
	// "from" the frame claims.
	f := Frame{Type: FrameMsg, ID: "m", From: "ffff", Body: "not me"}
	m, ok := messageFromFrame(f, "aaaa", "sam")
	if !ok {
		t.Fatal("frame rejected")
	}
	if m.From != "aaaa" {
		t.Fatalf("message filed as %q, want aaaa", m.From)
	}
}

func TestPeerCannotFileAMessageIntoSomebodyElsesConversation(t *testing.T) {
	// A peer may name a room, or nothing. If it names a direct
	// conversation, the only one it can possibly mean is ours with it.
	f := Frame{Type: FrameMsg, ID: "m", Conv: DirectConv("ffff"), Body: "hi"}
	m, ok := messageFromFrame(f, "aaaa", "")
	if !ok {
		t.Fatal("frame rejected")
	}
	if m.Conv != DirectConv("aaaa") {
		t.Fatalf("conversation %q, want %q", m.Conv, DirectConv("aaaa"))
	}

	room := Frame{Type: FrameMsg, ID: "m2", Conv: RoomConv("General"), Body: "hi"}
	m2, ok := messageFromFrame(room, "aaaa", "")
	if !ok || m2.Conv != RoomConv("general") {
		t.Fatalf("room conversation %q ok=%v", m2.Conv, ok)
	}
}

func TestOversizeBodyIsRejected(t *testing.T) {
	f := Frame{Type: FrameMsg, ID: "m", Body: strings.Repeat("x", maxBodyRunes+1)}
	if _, ok := messageFromFrame(f, "aaaa", ""); ok {
		t.Fatal("a body over the limit was accepted")
	}
}

func TestHelloIsCheckedBeforeItIsBelieved(t *testing.T) {
	good := Frame{Type: FrameHello, Version: ProtocolVersion, From: PeerID(strings.Repeat("a", 32))}
	if err := validHello(good); err != nil {
		t.Fatalf("a good hello was refused: %v", err)
	}
	for _, bad := range []Frame{
		{Type: FrameMsg, Version: ProtocolVersion, From: PeerID(strings.Repeat("a", 32))},
		{Type: FrameHello, Version: ProtocolVersion + 1, From: PeerID(strings.Repeat("a", 32))},
		{Type: FrameHello, Version: ProtocolVersion, From: "short"},
		{Type: FrameHello, Version: ProtocolVersion, From: PeerID(strings.Repeat("z", 32))},
	} {
		if err := validHello(bad); err == nil {
			t.Errorf("accepted %#v", bad)
		}
	}
}

func TestBeaconPacketRoundTripAndTheAddressComesFromThePacket(t *testing.T) {
	a := Announcement{
		Version: ProtocolVersion, ID: PeerID(strings.Repeat("a", 32)),
		Nick: "sam", Color: "#2f6fd0", Host: "box", Port: 47772,
		Rooms: []string{"General"}, Presence: PresenceOnline,
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
	if len(got.Rooms) != 1 || got.Rooms[0] != "general" {
		t.Fatalf("rooms %v, want [general]", got.Rooms)
	}
}

func TestBeaconRejectsRubbishOnTheGroup(t *testing.T) {
	src := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 47771}
	for _, pkt := range [][]byte{
		[]byte("some other protocol entirely"),
		[]byte(BeaconMagic + "not json"),
		[]byte(BeaconMagic + `{"v":99,"id":"aaaa","port":1}`),                                  // wrong version
		[]byte(BeaconMagic + `{"v":1,"id":"nothex--------------------------------","port":1}`), // bad id
		[]byte(BeaconMagic + `{"v":1,"id":"` + strings.Repeat("a", 32) + `","port":0}`),        // no port
		{},
	} {
		if _, ok := parseBeacon(pkt, src); ok {
			t.Errorf("accepted %q", pkt)
		}
	}
}

func TestMentionsMatchesWholeWordsOnly(t *testing.T) {
	cases := []struct {
		body, nick string
		want       bool
	}{
		{"sam, are you there?", "sam", true},
		{"the same thing", "sam", false},
		{"SAM!", "sam", true},
		{"ping sam@box please", "sam@box", true},
		{"hello sam", "sam@box", true}, // the part before the @ is what people type
		{"nothing here", "sam", false},
		{"anything", "", false},
	}
	for _, c := range cases {
		if got := mentions(c.body, c.nick); got != c.want {
			t.Errorf("mentions(%q, %q) = %v, want %v", c.body, c.nick, got, c.want)
		}
	}
}
