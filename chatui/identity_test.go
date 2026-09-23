package chatui

import (
	"strings"
	"testing"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/uitoolkit/a11y"
	"github.com/codemodify/uitoolkit/style"
	"github.com/codemodify/uitoolkit/widget"
)

// findIdentityBar is deliberately a Walk rather than a field read: Walk
// skips components that are not visible, which is how the strip was
// missing from the window the first time — a Base that is never Init'd
// paints nothing and says nothing, without an error anywhere.
func findIdentityBar(t *testing.T, root widget.Component) *identityBar {
	t.Helper()
	var found *identityBar
	widget.Walk(root, func(c widget.Component) {
		if ib, ok := c.(*identityBar); ok {
			found = ib
		}
	})
	if found == nil {
		t.Fatal("the window does not show who you are")
	}
	return found
}

func TestTheWindowSaysWhoYouAre(t *testing.T) {
	_, w, cli := openWindow(t, style.DarkLook())
	id, err := cli.Identity()
	if err != nil {
		t.Fatal(err)
	}
	bar := findIdentityBar(t, w.Content())
	if bar.nick != id.Nick {
		t.Errorf("the strip says %q, the daemon says %q", bar.nick, id.Nick)
	}
	if bar.color != id.Color {
		t.Errorf("the avatar is %q, the identity is %q", bar.color, id.Color)
	}
	if bar.LocalBounds().Dy() <= 0 {
		t.Error("the strip has no height")
	}
	// And the window itself is named after them: two of these side by side
	// are one task-bar entry each.
	if title := w.Title(); !strings.Contains(title, id.Nick) {
		t.Errorf("window title %q does not name %q", title, id.Nick)
	}
}

func TestTheIdentityStripReadsAsOneThingToAReader(t *testing.T) {
	_, w, _ := openWindow(t, style.DarkLook())
	bar := findIdentityBar(t, w.Content())
	var n a11y.Node
	bar.Describe(&n)
	if !strings.HasPrefix(n.Name, "You: ") {
		t.Errorf("a reader hears %q, which does not say it is you", n.Name)
	}
	if n.Description == "" {
		t.Error("a reader is not told where the messages are going")
	}
	if !n.Actions.Has(a11y.ActionDefault) {
		t.Error("a reader cannot open preferences from it")
	}
}

func TestWhereToIsShortAndNeverEmpty(t *testing.T) {
	cases := []chat.DaemonStatus{
		{},
		{Server: "abox:47772"},
		{Server: "abox:47772", Connected: true},
		{Connected: true},
	}
	for _, st := range cases {
		for _, up := range []bool{true, false} {
			got := chat.WhereTo(st, up)
			if got == "" {
				t.Fatalf("%#v up=%v: no answer at all", st, up)
			}
			// It sits in a sidebar somebody can drag narrow.
			if len([]rune(got)) > 32 {
				t.Errorf("%q is too long for the strip it goes in", got)
			}
		}
	}
}
