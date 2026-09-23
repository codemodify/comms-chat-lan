package chatcore

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func startDaemon(t *testing.T) *Client {
	t.Helper()
	tempHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	socket, stop, err := StartInProcess(ctx, NoDiscovery())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cli, err := DialWait(socket, 3*time.Second)
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
	return cli
}

func TestAFrontEndCanDoEverythingOverTheSocket(t *testing.T) {
	cli := startDaemon(t)

	if v, err := cli.Ping(); err != nil || v == "" {
		t.Fatalf("ping %q: %v", v, err)
	}
	st, err := cli.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Listen == "" || st.Self.ID == "" {
		t.Fatalf("status is not filled in: %#v", st)
	}
	if st.Discovery != "off" {
		t.Fatalf("discovery %q, want off in a test", st.Discovery)
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
	// orphan every conversation on every other machine.
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
	rooms, err := cli.Rooms()
	if err != nil || len(rooms) != 1 {
		t.Fatalf("rooms %#v: %v", rooms, err)
	}

	conv := RoomConv(room)
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
	cli := startDaemon(t)
	err := cli.call("nonsense.method", nil, nil)
	if err == nil {
		t.Fatal("an unknown method succeeded")
	}
	var rpcErr *RPCError
	if e, ok := err.(*RPCError); ok {
		rpcErr = e
	}
	if rpcErr == nil || rpcErr.Code != ErrNoMethod {
		t.Fatalf("error %#v, want code %d", err, ErrNoMethod)
	}
	// The connection survives it: one bad call from one front end must
	// not take the socket down.
	if _, err := cli.Ping(); err != nil {
		t.Fatalf("the connection did not survive: %v", err)
	}
}

func TestEveryFrontEndHearsAboutEveryChange(t *testing.T) {
	tempHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket, stop, err := StartInProcess(ctx, NoDiscovery())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Two front ends at once, as when the GUI and the TUI are both open.
	a, err := DialWait(socket, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := DialWait(socket, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	var mu sync.Mutex
	seen := map[string]int{}
	watch := func(who string) func(NodeEvent) {
		return func(ev NodeEvent) {
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
	if _, err := a.Send(RoomConv("general"), "hello"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := seen["a:"+EventMessage] > 0 && seen["b:"+EventMessage] > 0
		mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("the second front end did not hear the message: %v", seen)
}

func TestTheClientReconnectsWhenTheDaemonRestarts(t *testing.T) {
	tempHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket, stop, err := StartInProcess(ctx, NoDiscovery())
	if err != nil {
		t.Fatal(err)
	}

	cli, err := DialWait(socket, 3*time.Second)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()

	states := make(chan bool, 8)
	cli.AutoReconnect(func(up bool) { states <- up })

	// A daemon restart under a running front end is an ordinary event.
	stop()
	select {
	case up := <-states:
		if up {
			t.Fatal("the first state change said the connection was up")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the client never noticed the daemon had gone")
	}

	// Bring a new daemon up on the same socket path.
	node, err := OpenNode("")
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx2, socket, node) }()
	t.Cleanup(func() {
		cancel2()
		<-done
	})

	select {
	case up := <-states:
		if !up {
			t.Fatal("the client reported down twice instead of coming back")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the client never reconnected")
	}
	if _, err := cli.Ping(); err != nil {
		t.Fatalf("the reconnected client cannot call: %v", err)
	}
}

// TestTheDaemonLinksNoUserInterface is the architectural rule of this
// family of applications, asserted rather than hoped for: the daemon owns
// the state and the network and knows nothing about a display. Breaking it
// is easy to do by accident — one import of a helper that happens to live
// in a UI package — and impossible to notice without this.
func TestTheDaemonLinksNoUserInterface(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}
	out, err := exec.Command("go", "list", "-deps", "../cmd/comms-chatd").CombinedOutput()
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
		t.Fatalf("comms-chatd links %d user-interface packages:\n%s",
			len(offenders), strings.Join(offenders, "\n"))
	}
}

func TestTheSocketIsOwnerOnly(t *testing.T) {
	tempHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket, stop, err := StartInProcess(ctx, NoDiscovery())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	st, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	// The socket is the only authorisation boundary the daemon has.
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("socket mode is %o; another local user can reach the history", perm)
	}
}
