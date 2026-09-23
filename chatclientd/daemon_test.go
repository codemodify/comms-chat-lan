package chatclientd

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatserver"
	"github.com/codemodify/comms-chat-lan/chatwire"
)

func tempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(chat.EnvHome, dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "xdg"))
	t.Setenv(chat.EnvServer, "")
	return dir
}

// startServer is a whole server on a free port with no discovery.
func startServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addr, _, stopSrv, err := chatserver.StartInProcess(ctx, chatwire.NoDiscovery())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return addr, func() {
		stopSrv()
		cancel()
	}
}

// client is one whole client daemon with a front end attached to it, the
// way the application actually runs: the daemon holds the cache and the
// link, and everything the test asks it is asked over the socket.
type client struct {
	*Daemon
	cli    *chat.Client
	dir    string
	socket string
	stop   func()
}

func startClient(t *testing.T, addr, dir, nick string) *client {
	t.Helper()
	store, err := chat.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	self := store.Self()
	if nick != "" {
		self.Nick = nick
		if _, err := store.SetSelf(self); err != nil {
			t.Fatal(err)
		}
	}
	d := New(store, chatwire.NoDiscovery())
	d.Addr = addr

	socket := filepath.Join(t.TempDir(), "clientd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, socket, d) }()

	cli, err := chat.DialWait(socket, 5*time.Second)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &client{Daemon: d, cli: cli, dir: dir, socket: socket}
	// Stopping twice is ordinary here: a test stops the client to show
	// what happens next, and the cleanup stops it again.
	var once sync.Once
	c.stop = func() {
		once.Do(func() {
			_ = cli.Close()
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("the client daemon did not stop")
			}
		})
	}
	t.Cleanup(c.stop)
	if addr != "" {
		waitUntil(t, "the client to enrol", c.Connected)
	}
	return c
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func bodiesIn(t *testing.T, c *client, conv chat.ConversationID) []string {
	t.Helper()
	msgs, err := c.cli.Messages(conv, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range msgs {
		out = append(out, m.Body)
	}
	return out
}

func hasBody(t *testing.T, c *client, conv chat.ConversationID, body string) bool {
	t.Helper()
	for _, b := range bodiesIn(t, c, conv) {
		if b == body {
			return true
		}
	}
	return false
}

// Two people, one server, one message: the whole application end to end,
// through both front-end sockets.
func TestTwoClientsExchangeAMessageThroughTheServer(t *testing.T) {
	home := tempHome(t)
	addr, stopServer := startServer(t)
	defer stopServer()

	alice := startClient(t, addr, filepath.Join(home, "alice"), "alice")
	bob := startClient(t, addr, filepath.Join(home, "bob"), "bob")
	waitUntil(t, "alice to see bob", func() bool {
		_, ok := alice.Store.Peer(bob.Store.Self().ID)
		return ok
	})

	sent, err := alice.cli.Send(chat.DirectConv(bob.Store.Self().ID), "hello over the LAN")
	if err != nil {
		t.Fatal(err)
	}
	convForBob := chat.DirectConv(alice.Store.Self().ID)
	waitUntil(t, "bob to receive it", func() bool {
		return hasBody(t, bob, convForBob, "hello over the LAN")
	})

	// Alice's own copy goes to delivered when the server says where it
	// landed. Nothing optimistically assumes delivery.
	waitUntil(t, "the sequence to come back", func() bool {
		m, ok := alice.Store.Message(sent.ID)
		return ok && m.State == chat.StateDelivered && m.Seq > 0
	})
	if n, _ := bob.cli.MarkRead(convForBob); n != 1 {
		t.Fatalf("bob's unread count was %d, want 1", n)
	}
}

// TestAMessageComposedWhileTheServerIsDownIsSentLater is the case the
// cache exists for. The person types, the front end accepts it, and the
// daemon holds it until there is somewhere to send it.
func TestAMessageComposedWhileTheServerIsDownIsSentLater(t *testing.T) {
	home := tempHome(t)
	addr, stopServer := startServer(t)
	defer stopServer()

	alice := startClient(t, addr, filepath.Join(home, "alice"), "alice")
	bob := startClient(t, addr, filepath.Join(home, "bob"), "bob")
	bobID := bob.Store.Self().ID
	waitUntil(t, "alice to see bob", func() bool {
		_, ok := alice.Store.Peer(bobID)
		return ok
	})
	// Bob has to have been heard of before he can be written to, which he
	// now has been.
	stopServer()
	waitUntil(t, "alice to notice the server has gone", func() bool { return !alice.Connected() })

	m, err := alice.cli.Send(chat.DirectConv(bobID), "composed with nobody listening")
	if err != nil {
		t.Fatalf("a message could not even be composed offline: %v", err)
	}
	if got, _ := alice.Store.Message(m.ID); got.State != chat.StateQueued {
		t.Fatalf("state %q, want %q", got.State, chat.StateQueued)
	}
	// And it is readable: the front end shows what the person typed.
	if !hasBody(t, alice, chat.DirectConv(bobID), "composed with nobody listening") {
		t.Fatal("the message the person typed is not in their own conversation")
	}
}

// TestTheCacheIsReadableWhileTheServerIsUnreachable is the other half of
// it: history, the roster and the conversation list all still answer.
func TestTheCacheIsReadableWhileTheServerIsUnreachable(t *testing.T) {
	home := tempHome(t)
	addr, stopServer := startServer(t)
	defer stopServer()

	alice := startClient(t, addr, filepath.Join(home, "alice"), "alice")
	bob := startClient(t, addr, filepath.Join(home, "bob"), "bob")
	if _, err := alice.cli.JoinRoom("general"); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.cli.JoinRoom("general"); err != nil {
		t.Fatal(err)
	}
	room := chat.RoomConv("general")
	if _, err := bob.cli.Send(room, "the build machine is back up"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "alice to receive it", func() bool { return hasBody(t, alice, room, "the build machine is back up") })

	stopServer()
	waitUntil(t, "alice to notice", func() bool { return !alice.Connected() })

	if !hasBody(t, alice, room, "the build machine is back up") {
		t.Fatal("the conversation vanished when the server did")
	}
	if convs, err := alice.cli.Conversations(); err != nil || len(convs) == 0 {
		t.Fatalf("the conversation list is empty while offline: %v %v", convs, err)
	}
	st, err := alice.cli.Status()
	if err != nil {
		t.Fatal(err)
	}
	// And it says so, rather than letting the window imply otherwise.
	if st.Connected {
		t.Fatal("status.get still claims the server is there")
	}
}

// TestAClientCatchesUpOnWhatItMissed is the resume: the daemon is
// restarted over the same data directory, says where it got to, and is
// told the rest.
func TestAClientCatchesUpOnWhatItMissed(t *testing.T) {
	home := tempHome(t)
	addr, stopServer := startServer(t)
	defer stopServer()

	aliceDir := filepath.Join(home, "alice")
	alice := startClient(t, addr, aliceDir, "alice")
	bob := startClient(t, addr, filepath.Join(home, "bob"), "bob")
	aliceID := alice.Store.Self().ID
	waitUntil(t, "bob to see alice", func() bool {
		_, ok := bob.Store.Peer(aliceID)
		return ok
	})
	convWithBob := chat.DirectConv(bob.Store.Self().ID)
	if _, err := bob.cli.Send(chat.DirectConv(aliceID), "before you left"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "alice to receive the first message", func() bool {
		return hasBody(t, alice, convWithBob, "before you left")
	})
	waitUntil(t, "alice's cursor to move", func() bool { return alice.Cursor() > 0 })

	// Alice's machine goes away entirely.
	alice.stop()

	if _, err := bob.cli.Send(chat.DirectConv(aliceID), "while you were out"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the server to have it", func() bool {
		return len(bob.Store.Messages(chat.DirectConv(aliceID), 0)) == 2
	})

	// And comes back, with the same identity and the same cursor on disk.
	again := startClient(t, addr, aliceDir, "alice")
	waitUntil(t, "the missed message to arrive", func() bool {
		return hasBody(t, again, convWithBob, "while you were out")
	})
	// What she already had is still there, and was not sent twice.
	got := bodiesIn(t, again, convWithBob)
	if len(got) != 2 || got[0] != "before you left" || got[1] != "while you were out" {
		t.Fatalf("the conversation after resuming is %v", got)
	}
}

// TestARoomsHistoryArrivesWhenYouJoinIt is the late joiner, through the
// whole stack: a person who joins a room today reads what was said in it
// yesterday, because the server kept it.
func TestARoomsHistoryArrivesWhenYouJoinIt(t *testing.T) {
	home := tempHome(t)
	addr, stopServer := startServer(t)
	defer stopServer()

	alice := startClient(t, addr, filepath.Join(home, "alice"), "alice")
	if _, err := alice.cli.JoinRoom("general"); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"it was the switch", "not the machine"} {
		if _, err := alice.cli.Send(chat.RoomConv("general"), line); err != nil {
			t.Fatal(err)
		}
	}

	// Bob starts up afterwards and joins.
	bob := startClient(t, addr, filepath.Join(home, "bob"), "bob")
	if _, err := bob.cli.JoinRoom("General"); err != nil {
		t.Fatal(err)
	}
	room := chat.RoomConv("general")
	waitUntil(t, "the room history to arrive", func() bool {
		return hasBody(t, bob, room, "it was the switch") && hasBody(t, bob, room, "not the machine")
	})
	// In the server's order, not in the order the frames happened to
	// arrive in.
	got := bodiesIn(t, bob, room)
	if got[0] != "it was the switch" {
		t.Fatalf("the history arrived as %v", got)
	}
}

// A front end can do everything over the socket, with no server anywhere.
// This is also the sample and screenshot path, so it is the one that has
// to keep working when nothing else does.
func TestAFrontEndCanDoEverythingOverTheSocket(t *testing.T) {
	home := tempHome(t)
	c := startClient(t, "", filepath.Join(home, "alice"), "")
	cli := c.cli

	if v, err := cli.Ping(); err != nil || v == "" {
		t.Fatalf("ping %q: %v", v, err)
	}
	st, err := cli.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Self.ID == "" || st.Connected {
		t.Fatalf("status is wrong for a daemon with no server: %#v", st)
	}

	id, err := cli.Identity()
	if err != nil {
		t.Fatal(err)
	}
	id.Nick = "sam"
	id.Color = "#c0392b"
	out, err := cli.SetIdentity(id)
	if err != nil {
		t.Fatal(err)
	}
	if out.Nick != "sam" || out.Color != "#c0392b" {
		t.Fatalf("identity did not stick: %#v", out)
	}
	// The peer id is the one thing that cannot change: changing it would
	// orphan every conversation on the server and on every other machine.
	if out.ID != id.ID {
		t.Fatalf("the peer id changed from %s to %s", id.ID, out.ID)
	}

	room, err := cli.JoinRoom("  General ")
	if err != nil {
		t.Fatal(err)
	}
	if room != "general" {
		t.Fatalf("room %q, want general", room)
	}
	if rooms, err := cli.Rooms(); err != nil || len(rooms) != 1 {
		t.Fatalf("rooms %#v: %v", rooms, err)
	}

	conv := chat.RoomConv(room)
	m, err := cli.Send(conv, "anyone about?")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := cli.Messages(conv, 0)
	if err != nil || len(msgs) != 1 || msgs[0].ID != m.ID {
		t.Fatalf("messages %#v: %v", msgs, err)
	}
	view, err := cli.Conversation(conv)
	if err != nil {
		t.Fatal(err)
	}
	if view.Conv.Title != "#general" {
		t.Fatalf("conversation title %q", view.Conv.Title)
	}

	prefs, err := cli.NotifyPrefs()
	if err != nil {
		t.Fatal(err)
	}
	prefs.DirectOnly = true
	if prefs, err = cli.SetNotifyPrefs(prefs); err != nil || !prefs.DirectOnly {
		t.Fatalf("notify prefs %#v: %v", prefs, err)
	}
	if err := cli.LeaveRoom("general"); err != nil {
		t.Fatal(err)
	}
	if rooms, err := cli.Rooms(); err != nil || len(rooms) != 0 {
		t.Fatalf("rooms after leaving %#v: %v", rooms, err)
	}
}

func TestAnUnknownMethodIsAnErrorAndNotAHangUp(t *testing.T) {
	home := tempHome(t)
	c := startClient(t, "", filepath.Join(home, "alice"), "")
	socket := c.socket

	// The typed client has no way to call a method that does not exist,
	// which is the point of it, so this goes over the socket by hand.
	conn, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(chat.Request{JSONRPC: chat.RPCVersion, ID: 1, Method: "nonsense.method"}); err != nil {
		t.Fatal(err)
	}
	var resp chat.Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != chat.ErrNoMethod {
		t.Fatalf("response %#v, want code %d", resp, chat.ErrNoMethod)
	}
	// The connection survives it: one bad call from one front end must
	// not take the socket down.
	if err := enc.Encode(chat.Request{JSONRPC: chat.RPCVersion, ID: 2, Method: chat.MethodPing}); err != nil {
		t.Fatal(err)
	}
	resp = chat.Response{}
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("the connection did not survive: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("ping after a bad method: %v", resp.Error)
	}
}

func TestEveryFrontEndHearsAboutEveryChange(t *testing.T) {
	home := tempHome(t)
	c := startClient(t, "", filepath.Join(home, "alice"), "")

	// Two front ends at once, as when the GUI and the TUI are both open.
	a, err := chat.DialWait(c.socket, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := chat.DialWait(c.socket, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	var mu sync.Mutex
	seen := map[string]int{}
	watch := func(who string) func(chat.Event) {
		return func(ev chat.Event) {
			mu.Lock()
			seen[who+":"+ev.Kind]++
			mu.Unlock()
		}
	}
	a.OnEvent(watch("a"))
	b.OnEvent(watch("b"))

	if _, err := a.JoinRoom("general"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send(chat.RoomConv("general"), "hello"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the second front end to hear the message", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seen["a:"+chat.EventMessage] > 0 && seen["b:"+chat.EventMessage] > 0
	})
}

func TestTheSocketIsOwnerOnly(t *testing.T) {
	home := tempHome(t)
	c := startClient(t, "", filepath.Join(home, "alice"), "")
	st, err := os.Stat(c.socket)
	if err != nil {
		t.Fatal(err)
	}
	// The socket is the only authorisation boundary the daemon has.
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("socket mode is %o; another local user can reach the history", perm)
	}
}

// TestNeitherDaemonLinksAUserInterface is the architectural rule of this
// family of applications, asserted rather than hoped for: the daemons own
// the state and the network and know nothing about a display. Breaking it
// is easy to do by accident — one import of a helper that happens to live
// in a UI package — and impossible to notice without this.
func TestNeitherDaemonLinksAUserInterface(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	for _, pkg := range []string{
		"../cmd/comms-chat-lan-clientd",
		"../cmd/comms-chat-lan-server",
	} {
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
			t.Errorf("%s links %d user-interface packages:\n%s",
				pkg, len(offenders), strings.Join(offenders, "\n"))
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

// A file, offered by one person and accepted by another, arrives
// byte-identical — through the server, which is now the only path there
// is between two clients.
func TestAFileIsOfferedAcceptedAndArrivesIntact(t *testing.T) {
	home := tempHome(t)
	downloads := filepath.Join(home, "downloads")
	t.Setenv("UITK_CHAT_DOWNLOADS", downloads)

	addr, stopServer := startServer(t)
	defer stopServer()
	alice := startClient(t, addr, filepath.Join(home, "alice"), "alice")
	bob := startClient(t, addr, filepath.Join(home, "bob"), "bob")
	bobID := bob.Store.Self().ID
	waitUntil(t, "alice to see bob", func() bool {
		_, ok := alice.Store.Peer(bobID)
		return ok
	})

	// Big enough to be several chunks, so the relay's offset checks are
	// exercised rather than skipped.
	payload := strings.Repeat("the quick brown fox\n", 20000)
	src := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(src, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	tr, err := alice.cli.OfferFile(chat.DirectConv(bobID), src)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "bob to see the offer", func() bool {
		got, ok := bob.Transfer(tr.ID)
		return ok && got.State == chat.TransferIncoming
	})
	if err := bob.cli.AcceptFile(tr.ID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the transfer to finish", func() bool {
		got, ok := bob.Transfer(tr.ID)
		return ok && got.State == chat.TransferDone
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
	// The offer is in the conversation on both sides, in the server's
	// order, like anything else that was said.
	waitUntil(t, "the offer to be in bob's conversation", func() bool {
		return hasBody(t, bob, chat.DirectConv(alice.Store.Self().ID), "sent a file: notes.txt")
	})
}

func TestADeclinedFileLeavesNothingOnDisk(t *testing.T) {
	home := tempHome(t)
	downloads := filepath.Join(home, "downloads")
	t.Setenv("UITK_CHAT_DOWNLOADS", downloads)

	addr, stopServer := startServer(t)
	defer stopServer()
	alice := startClient(t, addr, filepath.Join(home, "alice"), "alice")
	bob := startClient(t, addr, filepath.Join(home, "bob"), "bob")
	bobID := bob.Store.Self().ID
	waitUntil(t, "alice to see bob", func() bool {
		_, ok := alice.Store.Peer(bobID)
		return ok
	})

	src := filepath.Join(home, "unwanted.bin")
	if err := os.WriteFile(src, []byte("no thank you"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := alice.cli.OfferFile(chat.DirectConv(bobID), src)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "bob to see the offer", func() bool {
		got, ok := bob.Transfer(tr.ID)
		return ok && got.State == chat.TransferIncoming
	})
	if err := bob.cli.DeclineFile(tr.ID, "not now"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "alice to hear the decline", func() bool {
		got, ok := alice.Transfer(tr.ID)
		return ok && got.State == chat.TransferDeclined
	})
	// Nothing was agreed to, so nothing is on disk.
	if entries, err := os.ReadDir(downloads); err == nil && len(entries) != 0 {
		t.Fatalf("declining left %d files in the download directory", len(entries))
	}
}

func TestAFileCannotBeSentToARoom(t *testing.T) {
	home := tempHome(t)
	addr, stopServer := startServer(t)
	defer stopServer()
	alice := startClient(t, addr, filepath.Join(home, "alice"), "alice")
	if _, err := alice.cli.OfferFile(chat.RoomConv("general"), os.Args[0]); err == nil {
		t.Fatal("a room file offer was accepted")
	}
}
