package chatui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codemodify/comms-chat-lan/chatcore"
	"github.com/codemodify/uitoolkit"
	"github.com/codemodify/uitoolkit/a11y"
	"github.com/codemodify/uitoolkit/app"
	"github.com/codemodify/uitoolkit/platform"
	"github.com/codemodify/uitoolkit/style"
	"github.com/codemodify/uitoolkit/widget"
	"github.com/codemodify/uitoolkit/widgets"
)

// These tests build the real window offscreen. They never open anything on
// the desktop they are running on: the application and the window are both
// headless, the daemon they talk to lives in this process on a private
// socket with discovery off, and tools/testenv.sh takes the display and
// the session bus away as well.

// openWindow is a whole application: a seeded daemon, a headless window
// and the real content.
func openWindow(t *testing.T, look style.LookAndFeel) (*app.Application, *app.Window, *chatcore.Client) {
	t.Helper()
	t.Setenv("UITK_CHAT_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// A tray item would keep the loop alive and reach for a session bus.
	t.Setenv("UITK_TRAY", "stub")
	t.Setenv("UITK_NOTIFY", "off")

	ctx, cancel := context.WithCancel(context.Background())
	socket, stop, err := chatcore.StartSample(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cli, err := chatcore.DialWait(socket, 3*time.Second)
	if err != nil {
		stop()
		cancel()
		t.Fatal(err)
	}

	a := uitoolkit.New(uitoolkit.Options{Look: look, Headless: true})
	w, err := a.NewWindow(platform.WindowOptions{
		Title: "Chat", Width: 1100, Height: 720, Headless: true,
	})
	if err != nil {
		_ = cli.Close()
		stop()
		cancel()
		t.Fatal(err)
	}
	w.SetContent(Open(a, w, cli))
	a.PumpOnce()
	sessionOfWindow(w).DrainDaemonEvents()
	a.PumpOnce()

	t.Cleanup(func() {
		w.Close()
		_ = cli.Close()
		stop()
		cancel()
	})
	return a, w, cli
}

func TestTheWindowIsAccessible(t *testing.T) {
	a, w, _ := openWindow(t, style.DarkLook())
	a.PumpOnce()

	tree := w.AccessibleTree()
	var problems []string
	for _, p := range a11y.Check(tree) {
		problems = append(problems, p.String())
	}
	if len(problems) > 0 {
		t.Fatalf("%d accessibility problems:\n%s", len(problems), strings.Join(problems, "\n"))
	}

	// The three things a screen reader has to find in a chat window: the
	// conversation list, the transcript and the composer.
	var lists, fields, menus int
	var named []string
	tree.Walk(func(n *a11y.Node) bool {
		switch n.Role {
		case a11y.RoleList:
			lists++
			named = append(named, n.Name)
		case a11y.RoleTextField:
			fields++
			named = append(named, n.Name)
		case a11y.RoleMenuBar:
			menus++
		}
		return true
	})
	if lists < 2 || fields < 1 || menus < 1 {
		t.Fatalf("lists %d, text fields %d, menu bars %d; named: %v", lists, fields, menus, named)
	}
	joined := strings.Join(named, "|")
	for _, want := range []string{"Conversations", "Messages", "Message"} {
		if !strings.Contains(joined, want) {
			t.Errorf("nothing in the tree is named %q; names are %v", want, named)
		}
	}
}

// Every message in the transcript is an accessible item, so a screen
// reader can walk the conversation rather than being told "list".
func TestEveryMessageIsAnAccessibleItem(t *testing.T) {
	a, w, cli := openWindow(t, style.DarkLook())
	msgs, err := cli.Messages(chatcore.RoomConv("general"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("the sample store has no messages")
	}
	sessionOf(t, w).selectConv(chatcore.RoomConv("general"))
	a.PumpOnce()

	var items []string
	w.AccessibleTree().Walk(func(n *a11y.Node) bool {
		if n.Role == a11y.RoleListItem && strings.Contains(n.Name, ":") {
			items = append(items, n.Name)
		}
		return true
	})
	found := false
	for _, it := range items {
		if strings.Contains(it, "build machine") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no accessible item carries a message body; got %d items:\n%s",
			len(items), strings.Join(items, "\n"))
	}
}

// The window has to be right in wildly different theme packs, not just in
// the one the author happened to run. These three are about as far apart
// as the toolkit goes: a 1995 bevelled pack, a modern KDE one and a
// modern GNOME one.
func TestTheWindowPaintsInVeryDifferentThemes(t *testing.T) {
	for _, theme := range []string{"win95", "breeze", "adwaita"} {
		theme := theme
		t.Run(theme, func(t *testing.T) {
			t.Setenv(style.ThemeEnv, theme)
			a, w, _ := openWindow(t, nil)
			a.PumpOnce()

			if got := a.Look().Name(); got == "" {
				t.Fatalf("no look resolved for %s", theme)
			}
			// The accessibility audit is a layout audit too: an
			// interactive control with an empty box is one a theme's
			// metrics collapsed.
			if problems := a11y.Check(w.AccessibleTree()); len(problems) > 0 {
				var lines []string
				for _, p := range problems {
					lines = append(lines, p.String())
				}
				t.Fatalf("%s: %s", theme, strings.Join(lines, "\n"))
			}

			path := filepath.Join(t.TempDir(), theme+".png")
			if err := w.WritePNG(path); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Size() < 2000 {
				t.Fatalf("%s: the frame is %d bytes; the window probably painted nothing", theme, st.Size())
			}
		})
	}
}

func TestTheWindowShowsTheConversationAndSends(t *testing.T) {
	a, w, cli := openWindow(t, style.DarkLook())
	s := sessionOf(t, w)

	// The sample store has a room and a direct conversation.
	if s.list.Count < 2 {
		t.Fatalf("%d conversations in the sidebar, want at least 2", s.list.Count)
	}
	s.selectConv(chatcore.RoomConv("general"))
	a.PumpOnce()
	if len(s.script.rows) == 0 {
		t.Fatal("the transcript is empty")
	}

	s.composer.SetText("on my way")
	s.send()
	s.DrainDaemonEvents()
	a.PumpOnce()

	msgs, err := cli.Messages(chatcore.RoomConv("general"), 0)
	if err != nil {
		t.Fatal(err)
	}
	last := msgs[len(msgs)-1]
	if last.Body != "on my way" {
		t.Fatalf("the daemon's last message is %q", last.Body)
	}
	if s.composer.Text != "" {
		t.Fatalf("the composer still holds %q after sending", s.composer.Text)
	}
}

// A file offer is never accepted silently: the strip with Accept and
// Decline on it is the only way one lands on disk.
func TestAFileOfferPutsUpAnAcceptStrip(t *testing.T) {
	a, w, _ := openWindow(t, style.DarkLook())
	s := sessionOf(t, w)

	conv := chatcore.DirectConv("11112222333344445555666677778888")
	s.selectConv(conv)
	a.PumpOnce()
	if s.offerBar.Visible() {
		t.Fatal("the accept strip is up with no offer waiting")
	}

	s.onEvent(chatcore.NodeEvent{
		Kind: chatcore.EventTransfer, Conv: conv,
		Transfer: &chatcore.Transfer{
			ID: "t1", Conv: conv, Peer: "11112222333344445555666677778888",
			Name: "rack-1.jpg", Size: 1 << 20, Incoming: true,
			State: chatcore.TransferIncoming, At: time.Now(),
		},
	})
	s.DrainDaemonEvents()
	a.PumpOnce()
	if !s.offerBar.Visible() {
		t.Fatal("an incoming offer did not put the accept strip up")
	}
	if !strings.Contains(s.offerText.Text, "rack-1.jpg") {
		t.Fatalf("the strip says %q", s.offerText.Text)
	}
}

// Nothing in this window may be reachable only with a mouse.
func TestTheWholeWindowIsReachableFromTheKeyboard(t *testing.T) {
	_, w, _ := openWindow(t, style.DarkLook())
	s := sessionOf(t, w)

	want := map[widget.Component]string{
		s.composer:  "the composer",
		s.list:      "the conversation list",
		s.script:    "the transcript",
		s.sendBtn:   "the send button",
		s.attachBtn: "the attach button",
	}
	for _, c := range widget.Focusables(w.Content()) {
		delete(want, c)
	}
	for _, name := range want {
		t.Errorf("%s cannot take the keyboard focus", name)
	}
}

func TestEveryMenuItemWithAShortcutAlsoHasALabel(t *testing.T) {
	_, w, _ := openWindow(t, style.DarkLook())
	s := sessionOf(t, w)
	for _, m := range s.menu.Menus() {
		for _, item := range m.Items {
			if item.Separator {
				continue
			}
			if strings.TrimSpace(item.Text) == "" {
				t.Errorf("menu %q has an item with no label", m.Title)
			}
			if item.Shortcut != "" {
				if _, _, ok := widgets.ParseAccel(item.Shortcut); !ok {
					t.Errorf("%q: %q is not an accelerator the toolkit understands", item.Text, item.Shortcut)
				}
			}
		}
	}
}

func TestWrapTextNeverRunsOffTheEdge(t *testing.T) {
	look := style.DarkLook()
	f := look.Font()
	const maxW = 180
	text := "A paragraph long enough to need several lines, with one enormous " +
		"unbreakableworditisreallyveryunreasonablylongindeed in the middle of it."
	for _, line := range wrapText(f, text, maxW) {
		if f.Advance(line) > maxW+1 {
			t.Errorf("line %q is %.1f wide, over %d", line, f.Advance(line), maxW)
		}
	}
	if got := wrapText(f, "", maxW); len(got) != 1 {
		t.Fatalf("an empty message wrapped to %d lines", len(got))
	}
}

func TestInitialsAreOneOrTwoLetters(t *testing.T) {
	cases := map[string]string{
		"sam":          "S",
		"nadia kovacs": "NK",
		"":             "?",
		"éa":           "É",
	}
	for in, want := range cases {
		if got := initials(in); got != want {
			t.Errorf("initials(%q) = %q, want %q", in, got, want)
		}
	}
}

// sessionOf digs the session out of the window. The package exports a
// component, not a struct, so the tests reach the state through the tree
// the way the toolkit does.
func sessionOf(t *testing.T, w *app.Window) *session {
	t.Helper()
	s := sessionOfWindow(w)
	if s == nil {
		t.Fatalf("the window's content is %T, not the shortcut root", w.Content())
	}
	return s
}

func sessionOfWindow(w *app.Window) *session {
	root, ok := w.Content().(*shortcutRoot)
	if !ok {
		return nil
	}
	return root.session
}
