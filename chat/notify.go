package chat

import (
	"os"
	"strings"
)

// DesktopNotifier, when set, puts one toast on the desktop. It is the only
// seam in this package through which a desktop is reachable at all, and it
// is deliberately a plain function value rather than a call into a UI
// toolkit: that is what lets `go list -deps` on either daemon come back
// with no uitoolkit package in it.
//
// comms-chat-lan-clientd leaves it nil. It only broadcasts the
// chat.notify event, and each front end decides what to do with it — the
// GUI raises a toast, the TUI rings the terminal bell. The single-process
// launcher used for screenshots sets it, so a one-process run still
// notifies.
//
// Set it before the daemon starts serving and do not change it afterwards.
var DesktopNotifier func(title, body string)

// NotifyDesktop puts one toast on the desktop through [DesktopNotifier],
// if anything set it. The daemons leave it nil; a single-process run sets
// it.
func NotifyDesktop(title, body string) {
	notify := DesktopNotifier
	if notify == nil {
		return
	}
	if os.Getenv(EnvNoNotify) != "" {
		return
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return
	}
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title == "" {
		title = "Chat"
	}
	if len(body) > 180 {
		body = body[:180] + "…"
	}
	// Off the caller's goroutine: the daemon keeps answering its clients
	// while the desktop is asked, and a D-Bus call that takes its time
	// cannot stall a message arriving.
	go notify(title, body)
}
