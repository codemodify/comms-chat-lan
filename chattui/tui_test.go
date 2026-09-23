package chattui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatclientd"
	"github.com/codemodify/comms-chat-lan/chatwire"
	"github.com/gdamore/tcell/v2"
)

// newTestUI starts a whole daemon in this process, with discovery off so
// the test never touches the real network, and points a terminal UI at it.
func newTestUI(t *testing.T) (*UI, *chat.Client) {
	t.Helper()
	t.Setenv("UITK_CHAT_HOME", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	socket, stop, err := chatclientd.StartInProcess(ctx, chatwire.NoDiscovery())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cli, err := chat.DialWait(socket, 3*time.Second)
	if err != nil {
		stop()
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cli.Close()
		stop()
		cancel()
	})
	return New(cli), cli
}

func TestTheTerminalUIShowsTheConversationItIsGiven(t *testing.T) {
	u, cli := newTestUI(t)

	if _, err := cli.JoinRoom("general"); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Send(chat.RoomConv("general"), "good morning"); err != nil {
		t.Fatal(err)
	}
	u.reloadAll()

	if n := u.list.GetItemCount(); n != 1 {
		t.Fatalf("%d conversations in the list, want 1", n)
	}
	main, _ := u.list.GetItemText(0)
	if !strings.Contains(main, "general") {
		t.Fatalf("the room is not in the list: %q", main)
	}
	u.selectIndex(0)
	if got := u.log.GetText(true); !strings.Contains(got, "good morning") {
		t.Fatalf("the transcript does not hold the message:\n%s", got)
	}
}

func TestSlashCommandsReachTheDaemon(t *testing.T) {
	u, cli := newTestUI(t)

	u.submit("/join general")
	rooms, err := cli.Rooms()
	if err != nil || len(rooms) != 1 || rooms[0].Room != "general" {
		t.Fatalf("/join did not join: %#v %v", rooms, err)
	}

	u.submit("/nick sam")
	id, err := cli.Identity()
	if err != nil || id.Nick != "sam" {
		t.Fatalf("/nick did not change the nickname: %#v %v", id, err)
	}

	u.selectConv(chat.RoomConv("general"))
	u.selectIndex(0)
	u.submit("hello everyone")
	msgs, err := cli.Messages(chat.RoomConv("general"), 0)
	if err != nil || len(msgs) != 1 || msgs[0].Body != "hello everyone" {
		t.Fatalf("the message did not reach the daemon: %#v %v", msgs, err)
	}

	u.submit("/leave")
	if rooms, err := cli.Rooms(); err != nil || len(rooms) != 0 {
		t.Fatalf("/leave did not leave: %#v %v", rooms, err)
	}
}

func TestAnUnknownCommandSaysSoRatherThanSendingIt(t *testing.T) {
	u, cli := newTestUI(t)
	u.submit("/join general")
	u.selectConv(chat.RoomConv("general"))
	u.selectIndex(0)

	u.submit("/nonsense")
	if got := u.log.GetText(true); !strings.Contains(got, "no such command") {
		t.Fatalf("no complaint about an unknown command:\n%s", got)
	}
	msgs, _ := cli.Messages(chat.RoomConv("general"), 0)
	if len(msgs) != 0 {
		t.Fatalf("a mistyped command was sent to the room as a message: %#v", msgs)
	}
}

// TestTheTerminalUIDrawsOnASimulatedTerminal runs the real event loop
// against tcell's simulation screen, so the layout is exercised without a
// terminal and without touching the one the tests are running in.
func TestTheTerminalUIDrawsOnASimulatedTerminal(t *testing.T) {
	u, cli := newTestUI(t)
	if _, err := cli.JoinRoom("standup"); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Send(chat.RoomConv("standup"), "morning"); err != nil {
		t.Fatal(err)
	}

	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 34)
	u.app.SetScreen(screen)

	done := make(chan error, 1)
	go func() { done <- u.Run() }()
	t.Cleanup(func() {
		u.app.Stop()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})

	if !waitForText(t, u, screen, "standup", 5*time.Second) {
		t.Fatalf("the room never appeared on screen:\n%s", u.snapshot(screen))
	}
	if !waitForText(t, u, screen, "Conversations", 3*time.Second) {
		t.Fatalf("the sidebar never drew:\n%s", u.snapshot(screen))
	}

	// Ctrl+G is the one key a new user is told about, so it had better
	// put the help up.
	screen.InjectKey(tcell.KeyCtrlG, 0, tcell.ModCtrl)
	if !waitForText(t, u, screen, "Keys and commands", 3*time.Second) {
		t.Fatalf("Ctrl+G did not open the help:\n%s", u.snapshot(screen))
	}
	// And Escape had better take it away again.
	screen.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !strings.Contains(u.snapshot(screen), "Keys and commands") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Escape did not close the help:\n%s", u.snapshot(screen))
}

func waitForText(t *testing.T, u *UI, s tcell.SimulationScreen, want string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(u.snapshot(s), want) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// snapshot reads the simulation screen from tview's own goroutine.
// SimulationScreen.GetContents hands back the live cell slice, which the
// draw goroutine is still writing to, so reading it from the test
// goroutine is a genuine data race in anybody's test. Queueing the read
// onto the loop serialises it with the drawing.
func (u *UI) snapshot(s tcell.SimulationScreen) string {
	ch := make(chan string, 1)
	go u.app.QueueUpdate(func() { ch <- screenText(s) })
	select {
	case text := <-ch:
		return text
	case <-time.After(3 * time.Second):
		return ""
	}
}

func screenText(s tcell.SimulationScreen) string {
	cells, w, h := s.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r := cells[y*w+x].Runes
			if len(r) == 0 || r[0] == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(r[0])
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func TestHumanSizeReadsLikeASize(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		2048:    "2.0 KiB",
		5 << 20: "5.0 MiB",
		3 << 30: "3.0 GiB",
	}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAMessageLineShowsWhetherItGotThere(t *testing.T) {
	self := chat.PeerID("aaaa")
	base := chat.Message{
		ID: "m", From: self, Mine: true, FromNick: "sam",
		Body: "hello", Sent: time.Now(),
	}
	for state, want := range map[chat.MessageState]string{
		chat.StateQueued:    "waiting",
		chat.StateSending:   "sending",
		chat.StateFailed:    "not delivered",
		chat.StateDelivered: "✓",
	} {
		m := base
		m.State = state
		if got := messageLine(m, self, nil); !strings.Contains(got, want) {
			t.Errorf("state %q renders as %q, want it to mention %q", state, got, want)
		}
	}
	// A message from somebody else carries no delivery mark: we cannot
	// know, and pretending would be a lie in the one place it matters.
	theirs := base
	theirs.From, theirs.Mine, theirs.State = "bbbb", false, chat.StateDelivered
	if got := messageLine(theirs, self, nil); strings.Contains(got, "✓") {
		t.Errorf("an incoming message claims delivery: %q", got)
	}
}
