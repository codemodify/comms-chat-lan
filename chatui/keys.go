package chatui

import (
	"github.com/codemodify/paintengine2d"
	"github.com/codemodify/uitoolkit/layout"
	"github.com/codemodify/uitoolkit/platform"
	"github.com/codemodify/uitoolkit/widget"
	"github.com/codemodify/uitoolkit/widgets"
)

// shortcutRoot wraps the whole window so keys that belong to the
// application rather than to one widget are seen before they are lost.
//
// The toolkit has no "register a shortcut on this window" call: menu
// accelerators work because the menu bar is in the live tree and the
// window walks it looking for HandleAccelerator. Anything outside a menu —
// Escape, a plain key when the composer does not have the focus — needs a
// component in the tree that says so. This is it.
type shortcutRoot struct {
	widget.Base
	// session is the window's state. It hangs here so a headless test can
	// reach it from the component the package hands out, without this
	// package having to export its internals to do it.
	session *session
	onKey   func(widget.KeyEvent) bool
	onReady func(widget.Component)
	ready   bool
}

func newShortcutRoot(sess *session, child widget.Component, onKey func(widget.KeyEvent) bool, onReady func(widget.Component)) *shortcutRoot {
	s := &shortcutRoot{session: sess, onKey: onKey, onReady: onReady}
	s.Init(s)
	s.Add(child)
	return s
}

func (s *shortcutRoot) Measure(c layout.Constraints) paintengine2d.Point {
	return s.Children()[0].Measure(c)
}

func (s *shortcutRoot) Arrange(r paintengine2d.Rect) {
	s.SetBounds(r)
	s.Children()[0].Arrange(paintengine2d.XYWH(0, 0, r.Dx(), r.Dy()))
	if !s.ready && s.Host() != nil && s.onReady != nil {
		s.ready = true
		s.onReady(s)
	}
}

func (s *shortcutRoot) Paint(*paintengine2d.Context) {}

func (s *shortcutRoot) KeyPress(e widget.KeyEvent) bool {
	return s.onKey != nil && s.onKey(e)
}

// handleKey is the window's own keyboard. The menu bar already owns
// everything with a Ctrl in it; what is left is the handful of keys that
// have to work while the cursor is in the composer, which is where it
// lives almost all the time.
func (s *session) handleKey(e widget.KeyEvent) bool {
	// Alt+Up / Alt+Down move between conversations from anywhere. They
	// are in the Conversation menu too, so they are discoverable, but
	// they are handled here as well because a modal overlay or a focused
	// list would otherwise swallow them.
	if e.Mods.Alt() {
		switch e.Key {
		case platform.KeyUp:
			s.moveSelection(-1)
			return true
		case platform.KeyDown:
			s.moveSelection(1)
			return true
		}
	}
	switch e.Key {
	case platform.KeyEscape:
		// Escape gets you back to typing, from wherever you are. In an
		// app whose point is the composer, that is the right default.
		if isTextFocus(s.win.Focus()) {
			return false
		}
		s.composer.RequestFocus()
		return true
	case platform.KeyPageUp:
		s.scroll.ScrollBy(-s.pageStep())
		return true
	case platform.KeyPageDown:
		s.scroll.ScrollBy(s.pageStep())
		return true
	}
	return false
}

func (s *session) pageStep() float32 {
	if h := s.scroll.Bounds().Dy(); h > 0 {
		return h * 0.8
	}
	return 200
}

// isTextFocus reports whether the keyboard is in something that is
// entitled to every key it is given.
func isTextFocus(c widget.Component) bool {
	switch t := c.(type) {
	case *widgets.TextField, *widgets.NumberField:
		return true
	case *widgets.TextArea:
		return !t.ReadOnly
	default:
		return false
	}
}
