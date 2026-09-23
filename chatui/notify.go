package chatui

import (
	"os"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/uitoolkit/app"
	"github.com/codemodify/uitoolkit/platform"
	"github.com/codemodify/uitoolkit/style"
	"github.com/codemodify/uitoolkit/widgets"
)

// notifier is the desktop-notification seam. It is an interface, and the
// constructor is a package variable, so a test can watch what the window
// would have put on the desktop without a session bus being anywhere near
// it — which matters, because the tests run on the developer's own logged
// in desktop.
type notifier interface {
	Send(platform.DesktopNotification) (string, error)
	Close()
}

var newNotifier = func(a *app.Application) notifier {
	return a.NewNotifier(platform.NotifierOptions{
		AppName: "Chat", DesktopEntry: "comms-chat-lan",
	})
}

// notify raises one toast for a message that arrived. The daemon has
// already decided whether the event is worth broadcasting at all (the
// notification preferences live there, so both front ends agree); the
// window decides only whether to put it on this desktop.
func (s *session) notify(title, body string) {
	if s.notifier == nil || os.Getenv(chat.EnvNoNotify) != "" {
		return
	}
	prefs, err := s.cli.NotifyPrefs()
	if err == nil && !prefs.Desktop {
		return
	}
	if s.win != nil && s.win.Visible() && s.windowIsShowing(title) {
		// The conversation is already on screen and has the focus: a
		// toast about a message the person is looking at is noise.
		return
	}
	_, _ = s.notifier.Send(platform.DesktopNotification{
		// One id, so a burst of messages replaces the toast in place
		// rather than stacking ten of them down the screen.
		ID: "comms-chat-lan-message", Title: title, Body: body,
		IconName: "user-available",
		Actions:  []platform.NotificationAction{{ID: "open", Label: "Open Chat"}},
		OnActivate: func(string) {
			s.showMain()
		},
	})
}

// windowIsShowing reports whether the conversation the notice belongs to
// is the one on screen. It is a title comparison because that is all the
// event carries, and being wrong only costs one extra toast.
func (s *session) windowIsShowing(title string) bool {
	conv := s.currentConv()
	if conv == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.convs {
		if c.ID == conv {
			return c.Title == title
		}
	}
	return false
}

// setupTray puts an item in the system tray when the desktop has one, and
// turns the window's close button into a hide. Without a live tray item
// that would strand the user: the window would be gone with nothing left
// to bring it back, so the close-to-tray behaviour is switched on only
// when there is something to restore it from.
func (s *session) setupTray() {
	if s.app == nil {
		return
	}
	item, err := s.app.NewStatusItem(platform.StatusItemOptions{
		ID: "comms-chat-lan", Title: "Chat", Tooltip: "Chat on this network",
		MenuChrome: platform.HostMenu,
		Icon:       app.StatusIconFromTool(style.IconMail, s.app.Look(), 22),
		Menu: app.StatusMenuFromItems([]*widgets.MenuItem{
			widgets.Item("Show Chat", s.showMain),
			widgets.Sep(),
			widgets.Item("Quit", s.quit),
		}),
		OnClick: s.showMain, OnNotifyClick: s.showMain,
	})
	if err != nil || item == nil || !item.Alive() {
		return
	}
	s.tray = item
	s.win.SetCloseHides(true)
}

func (s *session) showMain() {
	if s.win == nil {
		return
	}
	s.win.Show()
	s.win.Raise()
	s.composer.RequestFocus()
}
