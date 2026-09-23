package chatserver

import (
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatwire"
)

// The server's tests speak the wire protocol directly rather than through
// the client daemon. Two reasons: what is being tested here is the
// protocol as specified in docs/protocol.md, not one implementation of
// it; and a test that drives the frames can arrange the awkward cases — a
// stale cursor, two clients on one identity, a client that goes away
// mid-conversation — which a well-behaved client never produces.

func tempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(chat.EnvHome, dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "xdg"))
	return dir
}

// testServer is a whole server on a free port, with no discovery, so a
// test never announces itself on the network it runs on.
type testServer struct {
	*Server
	addr string
}

func startServer(t *testing.T) *testServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addr, srv, stop, err := StartInProcess(ctx, chatwire.NoDiscovery())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop()
		cancel()
	})
	return &testServer{Server: srv, addr: addr}
}

// testClient is one enrolled client: a connection, and every frame the
// server has sent it.
type testClient struct {
	t      *testing.T
	id     chat.PeerID
	conn   net.Conn
	mu     sync.Mutex
	frames []chatwire.Frame
	closed bool
}

func enroll(t *testing.T, srv *testServer, nick string, id chat.PeerID, cursor uint64, rooms ...string) *testClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", srv.addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := &testClient{t: t, id: id, conn: conn}
	self := chat.Identity{ID: id, Nick: nick, Rooms: rooms, Presence: chat.PresenceOnline}
	if err := chatwire.WriteFrame(conn, chatwire.HelloFrame(self, "testbox", cursor, srv.ID())); err != nil {
		t.Fatal(err)
	}
	go c.read()
	t.Cleanup(c.close)
	c.waitFor("the welcome", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameWelcome })
	return c
}

func (c *testClient) read() {
	for {
		f, err := chatwire.ReadFrame(c.conn)
		if err != nil {
			return
		}
		c.mu.Lock()
		c.frames = append(c.frames, f)
		c.mu.Unlock()
	}
}

func (c *testClient) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.conn.Close()
}

func (c *testClient) send(f chatwire.Frame) {
	c.t.Helper()
	if err := chatwire.WriteFrame(c.conn, f); err != nil {
		c.t.Fatalf("%s: %v", chat.ShortID(c.id), err)
	}
}

func (c *testClient) got() []chatwire.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]chatwire.Frame(nil), c.frames...)
}

// messages is every chat message this client has been sent, in the order
// it was sent them.
func (c *testClient) messages() []chatwire.Frame {
	var out []chatwire.Frame
	for _, f := range c.got() {
		if f.Type == chatwire.FrameMsg {
			out = append(out, f)
		}
	}
	return out
}

func (c *testClient) bodies() []string {
	var out []string
	for _, f := range c.messages() {
		out = append(out, f.Body)
	}
	return out
}

func (c *testClient) waitFor(what string, ok func(chatwire.Frame) bool) chatwire.Frame {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range c.got() {
			if ok(f) {
				return f
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("%s: timed out waiting for %s; got %d frames", chat.ShortID(c.id), what, len(c.got()))
	return chatwire.Frame{}
}

func (c *testClient) waitForBody(body string) chatwire.Frame {
	c.t.Helper()
	return c.waitFor("the message "+body, func(f chatwire.Frame) bool {
		return f.Type == chatwire.FrameMsg && f.Body == body
	})
}

func id(c byte) chat.PeerID { return chat.PeerID(strings.Repeat(string(c), 32)) }

// TestEnrolmentIsOpen is the decision, asserted: whoever asks is in. No
// code, no passphrase, no approval, and no list of who is allowed.
func TestEnrolmentIsOpen(t *testing.T) {
	tempHome(t)
	srv := startServer(t)

	a := enroll(t, srv, "alice", id('a'), 0)
	w := a.waitFor("the welcome", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameWelcome })
	if w.Server != srv.ID() {
		t.Fatalf("welcome from %q, want %q", w.Server, srv.ID())
	}
	// The server recorded who joined, which is the whole of what
	// enrolment does.
	if p, ok := srv.Store.Peer(id('a')); !ok || p.Nick != "alice" {
		t.Fatalf("the roster does not have alice: %#v", p)
	}
	// Somebody nobody has ever heard of, with a name already in use, is
	// also in.
	b := enroll(t, srv, "alice", id('b'), 0)
	b.waitFor("their welcome", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameWelcome })
	if srv.Clients() != 2 {
		t.Fatalf("%d clients, want 2", srv.Clients())
	}
}

// TestTheServerOrdersAndRelaysAMessage is the other half of the same
// decision: one machine decides the order, and everybody is told the same
// one.
func TestTheServerOrdersAndRelaysAMessage(t *testing.T) {
	tempHome(t)
	srv := startServer(t)
	a := enroll(t, srv, "alice", id('a'), 0, "general")
	b := enroll(t, srv, "bob", id('b'), 0, "general")

	a.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m1", Conv: chat.RoomConv("general"), Body: "morning"})
	b.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m2", Conv: chat.RoomConv("general"), Body: "morning back"})

	for _, c := range []*testClient{a, b} {
		c.waitForBody("morning")
		c.waitForBody("morning back")
	}
	// Both were given a sequence number, in the order the server took
	// them, and both clients were told the same numbers.
	seqOf := func(c *testClient, body string) uint64 {
		return c.waitForBody(body).Seq
	}
	if seqOf(a, "morning") == 0 || seqOf(a, "morning back") == 0 {
		t.Fatal("a message was relayed without a sequence number")
	}
	// Not which of the two came first: they were sent down two connections
	// at once and either may reach the server first. What the server
	// promises is that it picks one order, that the two messages do not
	// share a number, and that everybody is told the same one.
	if seqOf(a, "morning") == seqOf(a, "morning back") {
		t.Fatal("two messages were given the same sequence number")
	}
	if seqOf(a, "morning") != seqOf(b, "morning") ||
		seqOf(a, "morning back") != seqOf(b, "morning back") {
		t.Fatal("two clients were given different sequence numbers for one message")
	}
	// One client's own messages do keep the order it sent them in: that is
	// one connection, and the server reads it in order.
	a.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m3", Conv: chat.RoomConv("general"), Body: "one"})
	a.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m4", Conv: chat.RoomConv("general"), Body: "two"})
	if seqOf(a, "one") >= seqOf(a, "two") {
		t.Fatal("one client's own messages came back out of order")
	}
	// And the sender was told where its own message landed.
	ack := a.waitFor("the sequence for m1", func(f chatwire.Frame) bool {
		return f.Type == chatwire.FrameSeq && f.ID == "m1"
	})
	if ack.Seq != seqOf(a, "morning") {
		t.Fatalf("the sender was told %d, everyone else %d", ack.Seq, seqOf(a, "morning"))
	}
}

// A one-to-one conversation has a different name at each end. The server
// stores it under one name and rewrites it for each recipient; getting
// this wrong files half a conversation in a conversation with yourself.
func TestADirectConversationIsNamedFromEachEnd(t *testing.T) {
	tempHome(t)
	srv := startServer(t)
	a := enroll(t, srv, "alice", id('a'), 0)
	b := enroll(t, srv, "bob", id('b'), 0)

	a.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m1", Conv: chat.DirectConv(id('b')), Body: "just you"})

	got := b.waitForBody("just you")
	if got.Conv != chat.DirectConv(id('a')) {
		t.Fatalf("bob was told the conversation is %q, want %q", got.Conv, chat.DirectConv(id('a')))
	}
	mine := a.waitForBody("just you")
	if mine.Conv != chat.DirectConv(id('b')) {
		t.Fatalf("alice was told %q, want %q", mine.Conv, chat.DirectConv(id('b')))
	}
	// And it is stored once, under a name neither end uses.
	convs := srv.Store.Convs()
	if len(convs) != 1 || !convs[0].IsDirectKey() {
		t.Fatalf("the server stored %v", convs)
	}
}

// TestAMessageWaitsForSomebodyWhoIsAway is the first thing the
// peer-to-peer version documented as missing. There is no queue and no
// retry here: the message is stored, and the person who was away asks for
// what they missed when they come back.
func TestAMessageWaitsForSomebodyWhoIsAway(t *testing.T) {
	tempHome(t)
	srv := startServer(t)
	a := enroll(t, srv, "alice", id('a'), 0)
	b := enroll(t, srv, "bob", id('b'), 0)
	// Bob has to have been seen once for alice to be able to write to him.
	b.waitFor("the roster", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameRoster })
	cursor := b.waitFor("the synced", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameSynced }).Cursor

	// Bob goes away.
	b.close()
	waitUntil(t, "bob to be gone from the server", func() bool { return srv.Clients() == 1 })

	a.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m1", Conv: chat.DirectConv(id('b')), Body: "back at four?"})
	a.waitForBody("back at four?")

	// Bob comes back and says where he had got to.
	b2 := enroll(t, srv, "bob", id('b'), cursor)
	got := b2.waitForBody("back at four?")
	if got.Conv != chat.DirectConv(id('a')) {
		t.Fatalf("it arrived in %q", got.Conv)
	}
	// And only what he missed: the resume is a delta, not the world.
	if n := len(b2.messages()); n != 1 {
		t.Fatalf("bob was replayed %d messages, want 1", n)
	}
}

// TestALateJoinerReadsWhatWasSaidBeforeTheyArrived is the second thing
// the peer-to-peer version documented as missing.
func TestALateJoinerReadsWhatWasSaidBeforeTheyArrived(t *testing.T) {
	tempHome(t)
	srv := startServer(t)
	a := enroll(t, srv, "alice", id('a'), 0, "general")
	for _, body := range []string{"the switch in the cupboard", "not the machine"} {
		a.send(chatwire.Frame{
			Type: chatwire.FrameMsg, ID: chat.MessageID(body),
			Conv: chat.RoomConv("general"), Body: body,
		})
		a.waitForBody(body)
	}

	// Bob was not even running, let alone in the room.
	b := enroll(t, srv, "bob", id('b'), 0)
	b.send(chatwire.Frame{Type: chatwire.FrameJoin, Room: "General"})

	b.waitForBody("the switch in the cupboard")
	b.waitForBody("not the machine")
	b.waitFor("the room to be synced", func(f chatwire.Frame) bool {
		return f.Type == chatwire.FrameSynced && f.Room == "general"
	})
	// The order is the server's, which is the point of a late joiner
	// reading history at all.
	if got := b.bodies(); len(got) < 2 || got[0] != "the switch in the cupboard" {
		t.Fatalf("the room history arrived as %v", got)
	}
}

// A room is not a broadcast: somebody who has not joined it is not sent
// its messages, and cannot send to it either.
func TestARoomIsOnlyForThePeopleInIt(t *testing.T) {
	tempHome(t)
	srv := startServer(t)
	a := enroll(t, srv, "alice", id('a'), 0, "general")
	c := enroll(t, srv, "carol", id('c'), 0)

	a.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m1", Conv: chat.RoomConv("general"), Body: "in here"})
	a.waitForBody("in here")

	c.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m2", Conv: chat.RoomConv("general"), Body: "and me"})
	refusal := c.waitFor("a refusal", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameError })
	if refusal.Code != chatwire.ErrNotInRoom {
		t.Fatalf("refused with code %d, want %d", refusal.Code, chatwire.ErrNotInRoom)
	}
	if n := len(c.messages()); n != 0 {
		t.Fatalf("carol was sent %d messages from a room she is not in", n)
	}
}

// A cursor is a position in one server's sequence. A client holding a
// number from somewhere else — a different server, or this one restored
// from a backup — is told so and given a window of recent history rather
// than a delta computed from a number that means nothing.
func TestAStaleCursorIsRefusedRatherThanBelieved(t *testing.T) {
	tempHome(t)
	srv := startServer(t)
	a := enroll(t, srv, "alice", id('a'), 0, "general")
	a.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m1", Conv: chat.RoomConv("general"), Body: "one"})
	a.waitForBody("one")

	// A cursor from the future.
	b := enroll(t, srv, "bob", id('b'), 9_000_000, "general")
	w := b.waitFor("the welcome", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameWelcome })
	if !w.Reset {
		t.Fatal("a cursor ahead of the server was believed")
	}
	b.waitForBody("one")

	// And a cursor belonging to a different server, which the client says
	// so in its hello.
	conn, err := net.DialTimeout("tcp", srv.addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	self := chat.Identity{ID: id('d'), Nick: "dana", Rooms: []string{"general"}}
	if err := chatwire.WriteFrame(conn, chatwire.HelloFrame(self, "box", 1, chatwire.ServerID(strings.Repeat("f", 32)))); err != nil {
		t.Fatal(err)
	}
	f, err := chatwire.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Reset {
		t.Fatal("a cursor from another server's sequence was believed")
	}
}

// Two clients enrolled under one identity — a desktop and a laptop
// sharing an identity.json — are both that person. Everything that goes
// to them goes to both, including the echo of what either one sends.
func TestTwoClientsOnOneIdentityBothGetEverything(t *testing.T) {
	tempHome(t)
	srv := startServer(t)
	desk := enroll(t, srv, "alice", id('a'), 0)
	lap := enroll(t, srv, "alice", id('a'), 0)
	bob := enroll(t, srv, "bob", id('b'), 0)

	bob.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m1", Conv: chat.DirectConv(id('a')), Body: "either of you"})
	desk.waitForBody("either of you")
	lap.waitForBody("either of you")

	desk.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m2", Conv: chat.DirectConv(id('b')), Body: "from the desk"})
	lap.waitForBody("from the desk")
	lap.waitFor("the sequence of the desk's message", func(f chatwire.Frame) bool {
		return f.Type == chatwire.FrameSeq && f.ID == "m2"
	})
}

// A resend is free. A client that composed while the server was down, or
// that never saw the sequence it was given, sends the same message again;
// it is filed once and sequenced once.
func TestAResentMessageIsFiledOnce(t *testing.T) {
	tempHome(t)
	srv := startServer(t)
	a := enroll(t, srv, "alice", id('a'), 0)
	b := enroll(t, srv, "bob", id('b'), 0)

	msg := chatwire.Frame{Type: chatwire.FrameMsg, ID: "m1", Conv: chat.DirectConv(id('b')), Body: "once"}
	a.send(msg)
	first := a.waitFor("the sequence", func(f chatwire.Frame) bool {
		return f.Type == chatwire.FrameSeq && f.ID == "m1"
	})
	a.send(msg)
	waitUntil(t, "the resend to be answered", func() bool {
		n := 0
		for _, f := range a.got() {
			if f.Type == chatwire.FrameSeq && f.ID == "m1" {
				n++
			}
		}
		return n == 2
	})
	for _, f := range a.got() {
		if f.Type == chatwire.FrameSeq && f.ID == "m1" && f.Seq != first.Seq {
			t.Fatalf("the resend was given sequence %d, the first %d", f.Seq, first.Seq)
		}
	}
	if n := len(b.messages()); n != 1 {
		t.Fatalf("bob received the message %d times", n)
	}
	if n := len(srv.Store.Messages(chat.DirectKey(id('a'), id('b')), 0)); n != 1 {
		t.Fatalf("the server stored it %d times", n)
	}
}

// A server restart with clients still connected: the clients reconnect,
// say where they had got to, and the conversation carries on, because the
// server is the same server and its history is on disk.
func TestAServerRestartKeepsTheConversation(t *testing.T) {
	home := tempHome(t)
	dir := filepath.Join(home, "server")

	run := func() (addr string, srv *Server, stop func()) {
		store, err := chat.NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		s := New(store, chatwire.NoDiscovery())
		s.Port = -1
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		waitUntil(t, "the server to listen", func() bool { return s.Listen() != "" })
		return loopback(s.Listen()), s, func() {
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("the server did not stop")
			}
		}
	}

	addr, srv, stop := run()
	first := &testServer{Server: srv, addr: addr}
	a := enroll(t, first, "alice", id('a'), 0, "general")
	a.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m1", Conv: chat.RoomConv("general"), Body: "before"})
	seq := a.waitForBody("before").Seq
	serverID := srv.ID()
	a.close()
	stop()

	addr2, srv2, stop2 := run()
	defer stop2()
	second := &testServer{Server: srv2, addr: addr2}
	if srv2.ID() != serverID {
		t.Fatal("the restarted server is a different server; every cursor on the LAN just became rubbish")
	}
	// A client that had got as far as "before" is told nothing it already
	// has, and one that had not is told everything it missed.
	caught := enroll(t, second, "alice", id('a'), seq, "general")
	behind := enroll(t, second, "bob", id('b'), 0, "general")
	behind.waitForBody("before")
	caught.waitFor("the synced", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameSynced })
	if n := len(caught.messages()); n != 0 {
		t.Fatalf("a client that was up to date was replayed %d messages", n)
	}
	// And the sequence carries on rather than starting again, which would
	// reorder everything already said.
	behind.send(chatwire.Frame{Type: chatwire.FrameMsg, ID: "m2", Conv: chat.RoomConv("general"), Body: "after"})
	if got := behind.waitForBody("after").Seq; got <= seq {
		t.Fatalf("the sequence went backwards over the restart: %d after %d", got, seq)
	}
}

// An offer nobody answers does not sit in two front ends for ever.
func TestAnOfferNobodyAnswersIsCancelled(t *testing.T) {
	tempHome(t)
	srv := startServer(t)
	a := enroll(t, srv, "alice", id('a'), 0)
	b := enroll(t, srv, "bob", id('b'), 0)

	a.send(chatwire.Frame{
		Type: chatwire.FrameOffer, Transfer: "t1", Conv: chat.DirectConv(id('b')),
		Name: "rack.jpg", Size: 1000,
	})
	b.waitFor("the offer", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameOffer })

	// Reach in and age it, rather than making the test wait two minutes
	// for a clock it cannot control.
	srv.mu.Lock()
	srv.xfers["t1"].At = time.Now().Add(-2 * OfferTimeout)
	srv.mu.Unlock()
	srv.expireOffers()

	for _, c := range []*testClient{a, b} {
		f := c.waitFor("the cancellation", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameDecline })
		if !strings.Contains(f.Reason, "not answered") {
			t.Fatalf("cancelled with %q", f.Reason)
		}
	}
}

// A file to somebody who is not connected is refused now rather than
// held: the bytes have nowhere to go, and a progress bar that never moves
// is worse than a sentence saying so.
func TestAFileToSomebodyNotConnectedIsRefusedAtOnce(t *testing.T) {
	tempHome(t)
	srv := startServer(t)
	a := enroll(t, srv, "alice", id('a'), 0)
	b := enroll(t, srv, "bob", id('b'), 0)
	b.waitFor("the roster", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameRoster })
	b.close()
	waitUntil(t, "bob to be gone", func() bool { return srv.Clients() == 1 })

	a.send(chatwire.Frame{
		Type: chatwire.FrameOffer, Transfer: "t1", Conv: chat.DirectConv(id('b')),
		Name: "rack.jpg", Size: 1000,
	})
	f := a.waitFor("the refusal", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameDecline })
	if !strings.Contains(f.Reason, "not connected") {
		t.Fatalf("refused with %q", f.Reason)
	}
}

// The relay checks the bytes it is passing on: only the offering side may
// send them, only in order, and never more than was offered.
func TestTheRelayRefusesBytesNobodyAgreedTo(t *testing.T) {
	tempHome(t)
	srv := startServer(t)
	a := enroll(t, srv, "alice", id('a'), 0)
	b := enroll(t, srv, "bob", id('b'), 0)

	a.send(chatwire.Frame{
		Type: chatwire.FrameOffer, Transfer: "t1", Conv: chat.DirectConv(id('b')),
		Name: "notes.txt", Size: 4,
	})
	b.waitFor("the offer", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameOffer })

	// A chunk before anyone accepted goes nowhere.
	a.send(chatwire.Frame{Type: chatwire.FrameChunk, Transfer: "t1", Offset: 0, Data: []byte("abcd")})
	time.Sleep(100 * time.Millisecond)
	for _, f := range b.got() {
		if f.Type == chatwire.FrameChunk {
			t.Fatal("a chunk was relayed before the offer was accepted")
		}
	}

	b.send(chatwire.Frame{Type: chatwire.FrameAccept, Transfer: "t1"})
	a.waitFor("the acceptance", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameAccept })

	// More than was offered ends it, for both sides.
	a.send(chatwire.Frame{Type: chatwire.FrameChunk, Transfer: "t1", Offset: 0, Data: []byte("far too many bytes")})
	for _, c := range []*testClient{a, b} {
		c.waitFor("the transfer to be ended", func(f chatwire.Frame) bool { return f.Type == chatwire.FrameDecline })
	}
}

// TestTheServerLinksNoUserInterface is the architectural rule of this
// family of applications, asserted rather than hoped for.
func TestTheServerLinksNoUserInterface(t *testing.T) {
	assertNoUI(t, "../cmd/comms-chat-lan-server")
}

func assertNoUI(t *testing.T, pkg string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Skipf("go list: %v: %s", err, out)
	}
	var offenders []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.Contains(line, "uitoolkit") || strings.Contains(line, "paintengine2d") {
			offenders = append(offenders, line)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("%s links %d user-interface packages:\n%s",
			pkg, len(offenders), strings.Join(offenders, "\n"))
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
