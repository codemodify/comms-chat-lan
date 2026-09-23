package chatui

import (
	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/paintengine2d"
	"github.com/codemodify/uitoolkit/a11y"
	"github.com/codemodify/uitoolkit/layout"
	"github.com/codemodify/uitoolkit/platform"
	"github.com/codemodify/uitoolkit/style"
	"github.com/codemodify/uitoolkit/widget"
)

// identityBar sits above the conversation list and says whose window this
// is: the avatar, the nickname, and where the messages are going. Two of
// these windows side by side are otherwise identical — the status bar says
// the same thing, but at the bottom in small type, which is not where
// somebody looks to answer "which one am I typing into".
//
// It is one of ours rather than a Row of stock parts because the avatar is
// the transcript's avatar: a filled circle in the colour derived from the
// peer id, with the same initials. Two drawings of the same person should
// not disagree.
type identityBar struct {
	widget.Base

	nick  string
	color string
	// note is the second line: the server this is enrolled with, or why
	// there is none. Cached state is worth saying out loud.
	note string
	// onActivate opens Preferences, which is where a nickname is changed.
	onActivate func()

	hover bool
}

const (
	idAvatar = 30
	idGap    = 10
	idPadX   = 12
	idPadY   = 10
)

func newIdentityBar(onActivate func()) *identityBar {
	b := &identityBar{nick: "…", note: "starting up", onActivate: onActivate}
	b.Init(b) // without this the Base is invisible and never painted
	// It says it is a button and offers an action, so it has to be
	// reachable by keyboard like one. The window's accessibility test
	// caught this the first time round.
	b.SetWantsFocus(onActivate != nil)
	return b
}

// set updates what is drawn. It takes the identity whole so the avatar
// cannot drift from the name beside it.
func (b *identityBar) set(self chat.Identity, note string) {
	nick := self.Nick
	if nick == "" {
		nick = "(no name yet)"
	}
	if b.nick == nick && b.color == self.Color && b.note == note {
		return
	}
	b.nick, b.color, b.note = nick, self.Color, note
	b.Invalidate()
}

func (b *identityBar) Measure(c layout.Constraints) paintengine2d.Point {
	lk := b.Look()
	h := style.Dip(lk, idPadY)*2 + style.Dip(lk, idAvatar)
	w := c.MaxW
	if !c.HasMaxW() || w <= 0 {
		w = style.Dip(lk, 220)
	}
	return paintengine2d.Pt(w, h)
}

func (b *identityBar) Arrange(r paintengine2d.Rect) { b.SetBounds(r) }

func (b *identityBar) Paint(ctx *paintengine2d.Context) {
	lk := b.Look()
	pal := lk.Palette()
	r := b.LocalBounds()

	if b.hover && b.onActivate != nil {
		ctx.DrawRect(r, paintengine2d.Fill(pal.Highlight))
	}
	if b.Focused() {
		lk.DrawFocusRing(ctx, r)
	}

	av := style.Dip(lk, idAvatar)
	padX := style.Dip(lk, idPadX)
	padY := style.Dip(lk, idPadY)
	bold := lk.BoldFont()
	muted := lk.MutedFont()

	circle := paintengine2d.XYWH(r.Min.X+padX, r.Min.Y+padY, av, av)
	ctx.DrawOval(circle, paintengine2d.Fill(parseColor(b.color, pal.Accent)))
	mark := initials(b.nick)
	mw := bold.Advance(mark)
	bold.Draw(ctx, mark,
		paintengine2d.Pt(circle.Min.X+(av-mw)/2, circle.Min.Y+(av-bold.Height())/2),
		pal.TextOnAccent)

	x := circle.Max.X + style.Dip(lk, idGap)
	textW := r.Max.X - padX - x
	if textW < 0 {
		textW = 0
	}
	bold.Draw(ctx, bold.Fit(b.nick, textW), paintengine2d.Pt(x, r.Min.Y+padY), pal.Text)
	muted.Draw(ctx, muted.Fit(b.note, textW),
		paintengine2d.Pt(x, r.Min.Y+padY+bold.Height()+style.Dip(lk, 1)), pal.TextMuted)
}

func (b *identityBar) MouseEnter() { b.hover = true; b.Invalidate() }
func (b *identityBar) MouseLeave() { b.hover = false; b.Invalidate() }

func (b *identityBar) MousePress(e widget.MouseEvent) bool {
	if e.Button != platform.ButtonLeft || b.onActivate == nil {
		return false
	}
	b.RequestFocus()
	b.onActivate()
	return true
}

// KeyPress: Return and Space open Preferences, which is what a button does.
func (b *identityBar) KeyPress(e widget.KeyEvent) bool {
	if b.onActivate == nil {
		return false
	}
	switch e.Key {
	case platform.KeyReturn, platform.KeySpace:
		b.onActivate()
		return true
	}
	return false
}

func (b *identityBar) Tooltip() string {
	if b.onActivate == nil {
		return ""
	}
	return "You — click to change your name"
}

func (b *identityBar) Describe(n *a11y.Node) {
	// Read as one thing: a reader should hear who this window belongs to
	// and where it is connected, in that order, without hunting.
	n.Role = a11y.RoleButton
	n.Name = "You: " + b.nick
	n.Description = b.note
	if b.onActivate != nil {
		n.Actions = n.Actions.With(a11y.ActionDefault)
	}
}

func (b *identityBar) AccessibleAction(item int, a a11y.Action) bool {
	if a != a11y.ActionDefault || b.onActivate == nil {
		return false
	}
	b.onActivate()
	return true
}
