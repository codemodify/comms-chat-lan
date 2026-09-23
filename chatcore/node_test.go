package chatcore

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testNode is one whole daemon core — store, node, TCP listener — with
// discovery off, so a test never announces itself on the real network and
// never hears anything from it.
type testNode struct {
	*Node
	stop func()

	mu     sync.Mutex
	events []NodeEvent
}

func startNode(t *testing.T, nick string) *testNode {
	t.Helper()
	store := NewMemoryStore()
	self := store.Self()
	self.Nick = nick
	if _, err := store.SetSelf(self); err != nil {
		t.Fatal(err)
	}
	tn := &testNode{Node: NewNode(store, NoDiscovery())}
	tn.OnEvent(func(ev NodeEvent) {
		tn.mu.Lock()
		tn.events = append(tn.events, ev)
		tn.mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := tn.Run(ctx); err != nil {
			t.Errorf("%s: node stopped: %v", nick, err)
		}
	}()
	waitFor(t, 3*time.Second, "the listener to bind", func() bool { return tn.Listen() != "" })
	tn.stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("%s: node did not stop", nick)
		}
	}
	t.Cleanup(tn.stop)
	return tn
}

// loopbackAddr is the node's listener on 127.0.0.1. The address the node
// advertises is the machine's outbound one, which a test has no business
// depending on.
func (tn *testNode) loopbackAddr(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(tn.Listen())
	if err != nil {
		t.Fatal(err)
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// introduce tells a about b, which is what discovery would have done.
func introduce(t *testing.T, a, b *testNode) {
	t.Helper()
	self := b.Store.Self()
	a.Store.PutPeer(Peer{
		ID: self.ID, Nick: self.Nick, Color: self.Color, Addr: b.loopbackAddr(t),
		Rooms: self.Rooms, Presence: PresenceOnline, LastSeen: time.Now(), Known: true,
	})
}

func (tn *testNode) id() PeerID { return tn.Store.Self().ID }

func (tn *testNode) eventsOfKind(kind string) []NodeEvent {
	tn.mu.Lock()
	defer tn.mu.Unlock()
	var out []NodeEvent
	for _, ev := range tn.events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

func TestTwoPeersExchangeAMessageAndAcknowledgeIt(t *testing.T) {
	tempHome(t)
	alice := startNode(t, "alice")
	bob := startNode(t, "bob")
	introduce(t, alice, bob)

	conv := DirectConv(bob.id())
	sent, err := alice.Send(context.Background(), conv, "hello over the LAN")
	if err != nil {
		t.Fatal(err)
	}

	// Bob files it under his conversation with Alice, not under the one
	// the frame happens to name.
	bobConv := DirectConv(alice.id())
	waitFor(t, 3*time.Second, "bob to receive the message", func() bool {
		return len(bob.Store.Messages(bobConv, 0)) == 1
	})
	got := bob.Store.Messages(bobConv, 0)[0]
	if got.Body != "hello over the LAN" || got.From != alice.id() {
		t.Fatalf("bob received %#v", got)
	}
	if got.FromNick != "alice" {
		t.Fatalf("nickname %q, want alice", got.FromNick)
	}

	// Alice's copy goes to delivered when the ack comes back. That is the
	// only thing that sets it — nothing optimistically assumes delivery.
	waitFor(t, 3*time.Second, "the ack to arrive", func() bool {
		m, ok := alice.Store.Message(sent.ID)
		return ok && m.State == StateDelivered
	})
	if n := bob.Store.Unread(bobConv); n != 1 {
		t.Fatalf("bob's unread count is %d, want 1", n)
	}
}

func TestAMessageToAnUnreachablePeerIsQueuedNotLost(t *testing.T) {
	tempHome(t)
	alice := startNode(t, "alice")

	// A peer we have heard announce itself but that is not answering: it
	// is asleep, not gone, so the message waits rather than failing.
	dead := PeerID(strings.Repeat("b", 32))
	alice.Store.PutPeer(Peer{ID: dead, Nick: "bob", Addr: "127.0.0.1:1", Presence: PresenceOnline, Known: true})

	conv := DirectConv(dead)
	m, err := alice.Send(context.Background(), conv, "are you there?")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the send to fall back to the queue", func() bool {
		got, ok := alice.Store.Message(m.ID)
		return ok && got.State == StateQueued
	})
	if q := alice.Store.Queued(conv); len(q) != 1 || q[0].ID != m.ID {
		t.Fatalf("queue holds %#v", q)
	}
}

func TestAQueuedMessageGoesOutWhenThePeerComesBack(t *testing.T) {
	tempHome(t)
	alice := startNode(t, "alice")

	bob := startNode(t, "bob")
	bobID := bob.id()
	// Queue a message while bob is at an address that answers nothing.
	alice.Store.PutPeer(Peer{ID: bobID, Nick: "bob", Addr: "127.0.0.1:1", Presence: PresenceOnline, Known: true})
	conv := DirectConv(bobID)
	m, err := alice.Send(context.Background(), conv, "queued while you were out")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the message to queue", func() bool {
		got, ok := alice.Store.Message(m.ID)
		return ok && got.State == StateQueued
	})

	// Now bob announces himself at his real address, exactly as the
	// beacon would report it.
	alice.peerAppeared(context.Background(), Announcement{
		Version: ProtocolVersion, ID: bobID, Nick: "bob", Port: 1,
		Presence: PresenceOnline, Addr: bob.loopbackAddr(t), Heard: time.Now(),
	})
	waitFor(t, 5*time.Second, "the queue to flush", func() bool {
		got, ok := alice.Store.Message(m.ID)
		return ok && got.State == StateDelivered
	})
	if n := len(bob.Store.Messages(DirectConv(alice.id()), 0)); n != 1 {
		t.Fatalf("bob has %d messages, want 1", n)
	}
}

func TestTwoPeersDiallingEachOtherEndUpWithOneConnection(t *testing.T) {
	tempHome(t)
	alice := startNode(t, "alice")
	bob := startNode(t, "bob")
	introduce(t, alice, bob)
	introduce(t, bob, alice)

	// Both sides send at the same instant, so both dial.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = alice.Send(context.Background(), DirectConv(bob.id()), "from alice")
	}()
	go func() {
		defer wg.Done()
		_, _ = bob.Send(context.Background(), DirectConv(alice.id()), "from bob")
	}()
	wg.Wait()

	// Both messages arrive, whichever connection survived the race...
	waitFor(t, 5*time.Second, "both messages to arrive", func() bool {
		return len(bob.Store.Messages(DirectConv(alice.id()), 0)) == 1 &&
			len(alice.Store.Messages(DirectConv(bob.id()), 0)) == 1
	})
	// ...and each side is left holding exactly one connection, not two.
	waitFor(t, 3*time.Second, "the losing connection to be dropped", func() bool {
		return len(alice.Online()) == 1 && len(bob.Online()) == 1
	})
}

func TestARoomMessageOnlyReachesPeersInThatRoom(t *testing.T) {
	tempHome(t)
	alice := startNode(t, "alice")
	bob := startNode(t, "bob")
	carol := startNode(t, "carol")

	for _, n := range []*testNode{alice, bob} {
		if _, _, err := n.Store.JoinRoom("general"); err != nil {
			t.Fatal(err)
		}
	}
	// Carol is on the LAN but has not joined the room.
	introduce(t, alice, bob)
	introduce(t, alice, carol)
	alice.Store.PutPeer(Peer{ID: bob.id(), Rooms: []string{"general"}})

	conv := RoomConv("general")
	if _, err := alice.Send(context.Background(), conv, "morning all"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "bob to receive the room message", func() bool {
		return len(bob.Store.Messages(conv, 0)) == 1
	})
	time.Sleep(200 * time.Millisecond)
	if n := len(carol.Store.Messages(conv, 0)); n != 0 {
		t.Fatalf("carol received %d room messages she was not in the room for", n)
	}
}

func TestATypingIndicatorReachesTheOtherSideAndExpires(t *testing.T) {
	tempHome(t)
	alice := startNode(t, "alice")
	bob := startNode(t, "bob")
	introduce(t, alice, bob)

	// A connection has to exist first; sending a message opens one.
	if _, err := alice.Send(context.Background(), DirectConv(bob.id()), "hi"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the connection", func() bool { return len(alice.Online()) == 1 })

	alice.SetTyping(DirectConv(bob.id()), true)
	waitFor(t, 3*time.Second, "bob to see alice typing", func() bool {
		return len(bob.Typing(DirectConv(alice.id()))) == 1
	})
	alice.SetTyping(DirectConv(bob.id()), false)
	waitFor(t, 3*time.Second, "the indicator to clear", func() bool {
		return len(bob.Typing(DirectConv(alice.id()))) == 0
	})
}

func TestAFileIsOfferedAcceptedAndArrivesIntact(t *testing.T) {
	home := tempHome(t)
	downloads := filepath.Join(home, "downloads")
	t.Setenv("UITK_CHAT_DOWNLOADS", downloads)

	alice := startNode(t, "alice")
	bob := startNode(t, "bob")
	introduce(t, alice, bob)

	// Big enough to be several chunks, so the offset checks are exercised.
	payload := strings.Repeat("the quick brown fox\n", 20000)
	src := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(src, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	tr, err := alice.Offer(context.Background(), DirectConv(bob.id()), src)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "bob to see the offer", func() bool {
		got, ok := bob.Transfer(tr.ID)
		return ok && got.State == TransferIncoming
	})
	if err := bob.Accept(tr.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the transfer to finish", func() bool {
		got, ok := bob.Transfer(tr.ID)
		return ok && got.State == TransferDone
	})

	got, _ := bob.Transfer(tr.ID)
	if filepath.Dir(got.Path) != downloads {
		t.Fatalf("the file landed at %s, outside the download directory", got.Path)
	}
	b, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != payload {
		t.Fatalf("the file arrived changed: %d bytes, want %d", len(b), len(payload))
	}
}

func TestADeclinedFileLeavesNothingOnDisk(t *testing.T) {
	home := tempHome(t)
	downloads := filepath.Join(home, "downloads")
	t.Setenv("UITK_CHAT_DOWNLOADS", downloads)

	alice := startNode(t, "alice")
	bob := startNode(t, "bob")
	introduce(t, alice, bob)

	src := filepath.Join(home, "unwanted.bin")
	if err := os.WriteFile(src, []byte("no thank you"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := alice.Offer(context.Background(), DirectConv(bob.id()), src)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "bob to see the offer", func() bool {
		got, ok := bob.Transfer(tr.ID)
		return ok && got.State == TransferIncoming
	})
	if err := bob.Decline(tr.ID, "not now"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "alice to hear the decline", func() bool {
		got, ok := alice.Transfer(tr.ID)
		return ok && got.State == TransferDeclined
	})
	// Nothing was agreed to, so nothing is on disk.
	if entries, err := os.ReadDir(downloads); err == nil && len(entries) != 0 {
		t.Fatalf("declining left %d files in the download directory", len(entries))
	}
}

func TestAFileCannotBeSentToARoom(t *testing.T) {
	tempHome(t)
	alice := startNode(t, "alice")
	if _, err := alice.Offer(context.Background(), RoomConv("general"), os.Args[0]); err == nil {
		t.Fatal("a room file offer was accepted")
	}
}

func TestABlockedPeerIsRefusedOnAccept(t *testing.T) {
	tempHome(t)
	alice := startNode(t, "alice")
	bob := startNode(t, "bob")

	// Alice knows bob and has blocked him.
	introduce(t, alice, bob)
	if !alice.Store.BlockPeer(bob.id(), true) {
		t.Fatal("block failed")
	}
	// Bob dials anyway.
	introduce(t, bob, alice)
	_, _ = bob.Send(context.Background(), DirectConv(alice.id()), "let me in")

	time.Sleep(500 * time.Millisecond)
	if n := len(alice.Store.Messages(DirectConv(bob.id()), 0)); n != 0 {
		t.Fatalf("a blocked peer delivered %d messages", n)
	}
}

func TestAPeerNobodyAnnouncedIsAcceptedButMarked(t *testing.T) {
	tempHome(t)
	alice := startNode(t, "alice")
	bob := startNode(t, "bob")

	// Only bob knows about alice: she never heard him announce himself,
	// which is what a network with broken multicast looks like.
	introduce(t, bob, alice)
	if _, err := bob.Send(context.Background(), DirectConv(alice.id()), "surprise"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "alice to receive it", func() bool {
		return len(alice.Store.Messages(DirectConv(bob.id()), 0)) == 1
	})
	p, ok := alice.Store.Peer(bob.id())
	if !ok {
		t.Fatal("the peer was not added to the roster")
	}
	if p.Known {
		t.Fatal("an unannounced peer was recorded as known; the UI would not warn about it")
	}
}
