package chatui

import (
	"fmt"
	"strings"

	"github.com/codemodify/comms-chat-lan/chatcore"
	"github.com/codemodify/paintengine2d"
	"github.com/codemodify/uitoolkit/a11y"
	"github.com/codemodify/uitoolkit/layout"
	"github.com/codemodify/uitoolkit/platform"
	"github.com/codemodify/uitoolkit/style"
	"github.com/codemodify/uitoolkit/widget"
)

// transcript is the message view: the one widget this application draws
// itself rather than assembling from stock parts.
//
// None of the stock views fits a conversation. A ListView and a TableView
// give one line per row, and a CardList truncates its snippet to one line
// too — but a chat message is a paragraph of unpredictable length, and
// truncating it is the one thing a transcript may never do. So this
// measures its own wrapped height per message and paints the rows.
//
// It lives inside a [widgets.ScrollView], which is where the scrolling,
// the wheel and the scrollbar come from; this widget only has to report
// how tall it is.
type transcript struct {
	widget.Base

	msgs []chatcore.Message
	self chatcore.PeerID
	// nameOf and colorOf resolve a sender. They are supplied by the
	// session, which is the only thing that knows the roster.
	nameOf  func(chatcore.PeerID) string
	colorOf func(chatcore.PeerID) string
	// transferOf resolves the live state of a file a message announced.
	transferOf func(chatcore.TransferID) (chatcore.Transfer, bool)
	// onActivate is called when a row with a file on it is opened with
	// Return or a double click.
	onActivate func(chatcore.Message)

	// selected is the row the keyboard is on, -1 for none. A transcript
	// has to be keyboard-reachable like everything else in the window,
	// and a screen reader needs something to be "on".
	selected int

	// Laid out in relayout, keyed on the width it was laid out for.
	rows    []row
	laidFor float32
	total   float32
}

type row struct {
	msg   chatcore.Message
	lines []string
	top   float32
	h     float32
}

func newTranscript() *transcript {
	t := &transcript{selected: -1}
	t.Init(t)
	t.SetWantsFocus(true)
	t.SetAccessibleName("Messages")
	return t
}

// SetMessages replaces the conversation being shown.
func (t *transcript) SetMessages(msgs []chatcore.Message) {
	t.msgs = msgs
	if t.selected >= len(msgs) {
		t.selected = -1
	}
	t.laidFor = 0 // force a re-wrap
	t.RequestLayout()
	t.Invalidate()
}

// ---------------------------------------------------------------- layout

func (t *transcript) Measure(c layout.Constraints) paintengine2d.Point {
	w := c.MaxW
	if !c.HasMaxW() || w <= 0 {
		w = 640
	}
	t.relayout(w)
	h := t.total
	if h < c.MinH {
		h = c.MinH
	}
	return paintengine2d.Pt(w, h)
}

func (t *transcript) Arrange(r paintengine2d.Rect) {
	t.SetBounds(r)
	t.relayout(r.Dx())
}

// relayout wraps every message for a width, and remembers the width it
// did it for so a repaint at the same size is free.
func (t *transcript) relayout(w float32) {
	if w <= 0 || (w == t.laidFor && len(t.rows) == len(t.msgs)) {
		return
	}
	lk := t.Look()
	body := lk.Font()
	pad := style.Dip(lk, gutter)
	textW := w - pad*2 - style.Dip(lk, avatarSize+avatarGap)
	if textW < style.Dip(lk, 80) {
		textW = style.Dip(lk, 80)
	}

	t.rows = t.rows[:0]
	y := style.Dip(lk, 6)
	lineH := body.Height()
	for _, m := range t.msgs {
		lines := wrapText(body, displayBody(m, t.transferOf), textW)
		h := lineH*float32(len(lines)) + style.Dip(lk, headerLine+rowGap)
		t.rows = append(t.rows, row{msg: m, lines: lines, top: y, h: h})
		y += h
	}
	t.total = y + style.Dip(lk, 6)
	t.laidFor = w
}

// Design lengths, at 1x. style.Dip scales them for the window.
const (
	gutter     = 12
	avatarSize = 28
	avatarGap  = 10
	headerLine = 18
	rowGap     = 8
)

// displayBody is what a row's text is: the message, or a description of
// the file it announced with whatever the transfer is doing right now.
func displayBody(m chatcore.Message, lookup func(chatcore.TransferID) (chatcore.Transfer, bool)) string {
	if m.Transfer == nil {
		return m.Body
	}
	line := fmt.Sprintf("%s  (%s)", m.Transfer.Name, humanSize(m.Transfer.Size))
	if lookup == nil {
		return line
	}
	tr, ok := lookup(m.Transfer.ID)
	if !ok {
		return line
	}
	switch tr.State {
	case chatcore.TransferIncoming:
		return line + "  — offered, waiting for you"
	case chatcore.TransferOffered:
		return line + "  — offered, waiting for them"
	case chatcore.TransferRunning:
		pct := 0
		if tr.Size > 0 {
			pct = int(tr.Done * 100 / tr.Size)
		}
		return fmt.Sprintf("%s  — %d%%", line, pct)
	case chatcore.TransferDone:
		if tr.Incoming && tr.Path != "" {
			return line + "  — saved to " + tr.Path
		}
		return line + "  — sent"
	case chatcore.TransferDeclined:
		return line + "  — declined"
	case chatcore.TransferCancelled:
		return line + "  — cancelled"
	case chatcore.TransferFailed:
		return line + "  — failed: " + tr.Error
	}
	return line
}

// wrapText is greedy word wrap.
//
// The toolkit's Font offers Advance, which measures a string, and Fit,
// which truncates one to a width *and appends an ellipsis* — useful for a
// list row, useless for wrapping, because the ellipsis has to be taken
// back off before the remainder can go on the next line. There is no
// public word wrap and no "longest prefix that fits". So every
// application that paints a paragraph writes this, and here is ours.
func wrapText(f *style.Font, text string, maxW float32) []string {
	var out []string

	// emit puts one finished line out, breaking it by force if it is a
	// single word wider than the column. A word that ran off the edge of
	// the window would be a message the reader simply cannot see.
	emit := func(line string) {
		for f.Advance(line) > maxW {
			cut := longestPrefix(f, line, maxW)
			if cut == "" {
				break
			}
			out = append(out, cut)
			line = line[len(cut):]
		}
		out = append(out, line)
	}

	for _, para := range strings.Split(text, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range words {
			try := word
			if line != "" {
				try = line + " " + word
			}
			if f.Advance(try) <= maxW {
				line = try
				continue
			}
			if line != "" {
				emit(line)
			}
			line = word
		}
		emit(line)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// longestPrefix is the longest prefix of s that fits in maxW, at least one
// rune long. It never returns the whole of s.
func longestPrefix(f *style.Font, s string, maxW float32) string {
	runes := []rune(s)
	if len(runes) < 2 {
		return ""
	}
	n := 1
	for n < len(runes)-1 && f.Advance(string(runes[:n+1])) <= maxW {
		n++
	}
	return string(runes[:n])
}

// ----------------------------------------------------------------- paint

func (t *transcript) Paint(ctx *paintengine2d.Context) {
	lk := t.Look()
	pal := lk.Palette()
	b := t.LocalBounds()
	t.relayout(b.Dx())

	pad := style.Dip(lk, gutter)
	av := style.Dip(lk, avatarSize)
	gap := style.Dip(lk, avatarGap)
	body := lk.Font()
	bold := lk.BoldFont()
	muted := lk.MutedFont()

	for i, r := range t.rows {
		top := b.Min.Y + r.top
		if top > b.Max.Y || top+r.h < b.Min.Y {
			continue // outside the scroll viewport
		}
		rowRect := paintengine2d.XYWH(b.Min.X, top, b.Dx(), r.h)

		if i == t.selected {
			// The row the keyboard is on. Selection colour rather than a
			// colour of our own, so it is right in every theme pack.
			ctx.DrawRect(rowRect, paintengine2d.Fill(pal.Selection))
		}

		if r.msg.System {
			// The app talking about itself: centred, muted, no avatar.
			text := strings.Join(r.lines, " ")
			muted.Draw(ctx, muted.Fit(text, b.Dx()-pad*2),
				paintengine2d.Pt(b.Min.X+pad+av+gap, top+style.Dip(lk, 4)), pal.TextMuted)
			continue
		}

		// The avatar: a filled circle in the peer's colour with their
		// initials. The colour is a function of the peer id, so it is the
		// same on every machine without anyone agreeing on it.
		who := t.senderName(r.msg)
		circle := paintengine2d.XYWH(b.Min.X+pad, top+style.Dip(lk, 2), av, av)
		ctx.DrawOval(circle, paintengine2d.Fill(parseColor(t.senderColor(r.msg), pal.Accent)))
		mark := initials(who)
		mw := bold.Advance(mark)
		bold.Draw(ctx, mark,
			paintengine2d.Pt(circle.Min.X+(av-mw)/2, circle.Min.Y+(av-bold.Height())/2),
			pal.TextOnAccent)

		x := b.Min.X + pad + av + gap
		// Name, time and, for our own messages, whether it got there.
		bold.Draw(ctx, who, paintengine2d.Pt(x, top+style.Dip(lk, 2)), pal.Text)
		meta := r.msg.Sent.Format("15:04")
		if s := deliveryNote(r.msg); s != "" {
			meta += "  " + s
		}
		muted.Draw(ctx, meta,
			paintengine2d.Pt(x+bold.Advance(who)+style.Dip(lk, 8), top+style.Dip(lk, 3)),
			deliveryColor(r.msg, pal))

		// The message itself.
		ty := top + style.Dip(lk, headerLine) + style.Dip(lk, 2)
		col := pal.Text
		if r.msg.Transfer != nil {
			col = pal.Accent
		}
		for _, line := range r.lines {
			body.Draw(ctx, line, paintengine2d.Pt(x, ty), col)
			ty += body.Height()
		}
	}

	if len(t.rows) == 0 {
		muted.Draw(ctx, "No messages yet.",
			paintengine2d.Pt(b.Min.X+pad, b.Min.Y+pad), pal.TextMuted)
	}
	if t.Focused() {
		lk.DrawFocusRing(ctx, b)
	}
}

func (t *transcript) senderName(m chatcore.Message) string {
	if m.FromNick != "" {
		return m.FromNick
	}
	if t.nameOf != nil {
		if n := t.nameOf(m.From); n != "" {
			return n
		}
	}
	return chatcore.ShortID(m.From)
}

func (t *transcript) senderColor(m chatcore.Message) string {
	if t.colorOf != nil {
		if c := t.colorOf(m.From); c != "" {
			return c
		}
	}
	return chatcore.ColorForID(m.From)
}

// deliveryNote is what a message we sent says about itself. A message
// somebody else sent says nothing: we cannot know, and claiming to would
// be a lie in the one place it matters.
func deliveryNote(m chatcore.Message) string {
	if !m.Mine {
		return ""
	}
	switch m.State {
	case chatcore.StateQueued:
		return "waiting for them"
	case chatcore.StateSending:
		return "sending"
	case chatcore.StateDelivered:
		return "delivered"
	case chatcore.StateFailed:
		return "not delivered"
	}
	return ""
}

func deliveryColor(m chatcore.Message, pal style.Palette) paintengine2d.Color {
	if m.Mine && m.State == chatcore.StateFailed {
		return pal.Danger
	}
	return pal.TextMuted
}

// initials is the one or two letters drawn in the avatar.
func initials(name string) string {
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return "?"
	}
	first := []rune(fields[0])
	out := strings.ToUpper(string(first[0]))
	if len(fields) > 1 {
		rest := []rune(fields[1])
		out += strings.ToUpper(string(rest[0]))
	}
	return out
}

// parseColor turns "#rrggbb" into a paint colour, falling back when a
// peer sends something that is not one.
func parseColor(hex string, fallback paintengine2d.Color) paintengine2d.Color {
	if len(hex) != 7 || hex[0] != '#' {
		return fallback
	}
	var r, g, b int
	if _, err := fmt.Sscanf(hex[1:], "%02x%02x%02x", &r, &g, &b); err != nil {
		return fallback
	}
	return paintengine2d.RGB(float32(r)/255, float32(g)/255, float32(b)/255)
}

// ------------------------------------------------------------- keyboard

func (t *transcript) KeyPress(e widget.KeyEvent) bool {
	switch e.Key {
	case platform.KeyUp:
		return t.move(-1)
	case platform.KeyDown:
		return t.move(1)
	case platform.KeyHome:
		return t.moveTo(0)
	case platform.KeyEnd:
		return t.moveTo(len(t.rows) - 1)
	case platform.KeyReturn, platform.KeySpace:
		return t.activate()
	}
	return false
}

func (t *transcript) move(d int) bool {
	if len(t.rows) == 0 {
		return false
	}
	at := t.selected
	if at < 0 {
		if d > 0 {
			at = -1
		} else {
			at = len(t.rows)
		}
	}
	return t.moveTo(at + d)
}

func (t *transcript) moveTo(i int) bool {
	if len(t.rows) == 0 {
		return false
	}
	if i < 0 {
		i = 0
	}
	if i >= len(t.rows) {
		i = len(t.rows) - 1
	}
	t.selected = i
	t.revealSelected()
	t.Invalidate()
	return true
}

// revealSelected scrolls the enclosing view so the current row is visible.
func (t *transcript) revealSelected() {
	if t.selected < 0 || t.selected >= len(t.rows) {
		return
	}
	widget.RevealFocus(t)
	if sc, ok := t.Parent().(interface{ ScrollTo(float32) }); ok {
		r := t.rows[t.selected]
		sc.ScrollTo(r.top)
	}
}

func (t *transcript) activate() bool {
	if t.selected < 0 || t.selected >= len(t.rows) || t.onActivate == nil {
		return false
	}
	t.onActivate(t.rows[t.selected].msg)
	return true
}

func (t *transcript) MousePress(e widget.MouseEvent) bool {
	for i, r := range t.rows {
		if e.Pos.Y >= r.top && e.Pos.Y < r.top+r.h {
			t.selected = i
			t.RequestFocus()
			t.Invalidate()
			return true
		}
	}
	return false
}

// ---------------------------------------------------------- accessibility

func (t *transcript) Describe(n *a11y.Node) {
	n.Role = a11y.RoleList
	n.Count = len(t.rows)
}

func (t *transcript) AccessibleItems() []*a11y.Node {
	out := make([]*a11y.Node, 0, len(t.rows))
	origin := widget.DeviceBounds(t).Min
	for i, r := range t.rows {
		// What a screen reader reads out: who, when and what. That is the
		// whole message, spoken the way a person would say it.
		text := fmt.Sprintf("%s, %s: %s", t.senderName(r.msg),
			r.msg.Sent.Format("15:04"), strings.Join(r.lines, " "))
		if note := deliveryNote(r.msg); note != "" {
			text += ", " + note
		}
		n := &a11y.Node{
			ID: widget.ItemID(t, i), Role: a11y.RoleListItem, Name: text,
			Bounds: paintengine2d.XYWH(origin.X, origin.Y+r.top, t.LocalBounds().Dx(), r.h),
			State:  a11y.StateSelectable,
			Index:  i + 1, Count: len(t.rows),
		}
		if i == t.selected {
			n.State |= a11y.StateSelected
		}
		n.Actions = n.Actions.With(a11y.ActionDefault).With(a11y.ActionScrollIntoView)
		out = append(out, n)
	}
	return out
}

func (t *transcript) AccessibleFocusItem() int { return t.selected }

func (t *transcript) AccessibleAction(i int, a a11y.Action) bool {
	if i < 0 || i >= len(t.rows) {
		return false
	}
	switch a {
	case a11y.ActionDefault:
		t.selected = i
		return t.activate()
	case a11y.ActionScrollIntoView:
		t.selected = i
		t.revealSelected()
		return true
	}
	return false
}

// humanSize is a byte count a person can read.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
