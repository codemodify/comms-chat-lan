package chatui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/uitoolkit/widgets"
)

// showPreferences is the one place a person changes what the LAN sees of
// them and what the app is allowed to interrupt them for. Everything in it
// is stored by the daemon, so the terminal front end agrees with it and
// the settings survive a restart.
func (s *session) showPreferences() {
	s.mu.Lock()
	self := s.self
	s.mu.Unlock()

	prefs, err := s.cli.NotifyPrefs()
	if err != nil {
		prefs = chat.DefaultNotifyPrefs()
	}

	nick := widgets.NewTextField(self.Nick, "your name on this network", nil)
	nick.SetAccessibleName("Nickname")

	colors := chat.AvatarColors()
	colorNames := make([]string, len(colors))
	selected := 0
	for i, c := range colors {
		colorNames[i] = colorName(c)
		if c == self.Color {
			selected = i
		}
	}
	colorBox := widgets.NewComboBox(colorNames, selected, nil)
	colorBox.SetAccessibleName("Avatar colour")

	presences := []chat.Presence{chat.PresenceOnline, chat.PresenceAway, chat.PresenceBusy}
	presenceNames := []string{"Online", "Away", "Busy"}
	presenceAt := 0
	for i, p := range presences {
		if p == self.Presence.Valid() {
			presenceAt = i
		}
	}
	presenceBox := widgets.NewComboBox(presenceNames, presenceAt, nil)
	presenceBox.SetAccessibleName("Presence")

	auto := widgets.NewTextField(strconv.FormatInt(self.AutoAcceptFiles/1024, 10), "0", nil)
	auto.SetAccessibleName("Accept files smaller than, in KiB")

	notify := widgets.NewCheckbox("Tell me about new messages", prefs.Enabled, nil)
	desktop := widgets.NewCheckbox("Put a notification on the desktop", prefs.Desktop, nil)
	direct := widgets.NewCheckbox("Only for one-to-one messages, not rooms", prefs.DirectOnly, nil)
	mention := widgets.NewCheckbox("…but still when a room message names me", prefs.Mentions, nil)

	form := widgets.NewForm()
	form.AddRow("Nickname", nick)
	form.AddRow("Avatar colour", colorBox)
	form.AddRow("Presence", presenceBox)
	form.AddRow("Accept files under (KiB)", auto)
	form.AddWide(widgets.NewSeparator())
	form.AddWide(notify)
	form.AddWide(desktop)
	form.AddWide(direct)
	form.AddWide(mention)

	var overlay *widgets.Overlay
	apply := func() {
		s.closeOverlay(overlay)

		next := self
		next.Nick = strings.TrimSpace(nick.Text)
		if i := colorBox.Selected; i >= 0 && i < len(colors) {
			next.Color = colors[i]
		}
		if i := presenceBox.Selected; i >= 0 && i < len(presences) {
			next.Presence = presences[i]
		}
		if kib, err := strconv.ParseInt(strings.TrimSpace(auto.Text), 10, 64); err == nil && kib >= 0 {
			next.AutoAcceptFiles = kib * 1024
		}
		nextPrefs := chat.NotifyPrefs{
			Enabled: notify.Checked, Desktop: desktop.Checked,
			DirectOnly: direct.Checked, Mentions: mention.Checked,
		}
		s.async(func() (any, error) {
			if _, err := s.cli.SetNotifyPrefs(nextPrefs); err != nil {
				return nil, err
			}
			return s.cli.SetIdentity(next)
		}, func(v any, err error) {
			if err != nil {
				s.warn("Preferences were not saved", err.Error())
				return
			}
			if id, ok := v.(chat.Identity); ok {
				s.mu.Lock()
				s.self = id
				s.mu.Unlock()
			}
			s.drawStatus()
			s.reloadMessages()
		})
	}

	card := widgets.DialogCard("Preferences",
		"There is no account to sign in to. A nickname and a colour are the whole of your identity here, and anyone on the network can claim any of both.",
		widgets.NewButton("Cancel", func() { s.closeOverlay(overlay) }),
		widgets.NewButton("Save", apply))
	card.Content().Add(form)
	overlay = widgets.NewOverlay(card)
	overlay.Modal = true
	overlay.InitialFocus = nick
	s.showOverlay(overlay)
}

// colorName gives an avatar colour a word, because a combo box of hex
// codes is unreadable and a swatch alone carries meaning by colour only.
func colorName(hex string) string {
	switch hex {
	case "#2f6fd0":
		return "Blue"
	case "#0f8f6f":
		return "Green"
	case "#b5651d":
		return "Amber"
	case "#8b4fbf":
		return "Violet"
	case "#c0392b":
		return "Red"
	case "#1f8fa8":
		return "Teal"
	case "#7a8b1f":
		return "Olive"
	case "#d0507f":
		return "Pink"
	case "#4f5f8f":
		return "Slate"
	case "#a07000":
		return "Bronze"
	}
	return hex
}

func (s *session) showPeers() {
	peers, err := s.cli.Peers()
	if err != nil {
		s.warn("The roster could not be read", err.Error())
		return
	}
	if len(peers) == 0 {
		s.info("Nobody else is here yet",
			"Everyone enrolled with the same server appears a few seconds after they start.\n\n"+
				"If nobody ever appears, this machine may not have found the server. "+
				"Check the status bar, and if there is no server on this segment, start the daemon with -server <host>. See docs/protocol.md.")
		return
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].DisplayName() < peers[j].DisplayName() })

	var b strings.Builder
	for _, p := range peers {
		fmt.Fprintf(&b, "%s — %s", p.DisplayName(), p.Presence.Valid())
		if p.Addr != "" {
			b.WriteString("  at " + p.Addr)
		}
		if len(p.Rooms) > 0 {
			b.WriteString("  in #" + strings.Join(p.Rooms, " #"))
		}
		if p.Blocked {
			b.WriteString("\n    blocked")
		}
		b.WriteString("\n")
	}
	s.showText("Who is here", b.String())
}

func (s *session) showKeys() {
	s.showText("Keyboard", strings.TrimSpace(`
Ctrl+N        join a room
Ctrl+O        send a file to the peer in this conversation
Ctrl+R        mark this conversation read
Ctrl+P        preferences
Ctrl+Q        quit
Alt+Up/Down   previous / next conversation
Page Up/Down  scroll the conversation
Escape        put the cursor back in the composer
Tab           move between the composer, the list and the transcript
Return        send, or open the file on the selected line

Every one of these is in a menu as well: nothing in this window can only
be reached with the mouse, and nothing can only be reached with a key.
`))
}

func (s *session) showSecurity() {
	s.showText("Security and privacy", strings.TrimSpace(`
This is a LAN application, and it is honest about what that means.

What it does NOT do:

  • It does not encrypt anything. Messages and files cross the network in
    plain sight. Anyone who can watch this network — another machine on
    the same wifi, whoever runs the switch — can read every word.

  • It does not authenticate anybody. Enrolment with the server is open:
    whoever can reach it is in. A name, a colour and an identity are
    whatever the client claiming them says they are, and nothing stops
    somebody calling themselves by your colleague's name.

  • It does not protect you from the network. Anyone who can reach the
    server can join every room, be sent your one-to-one messages if they
    claim the right identity, and offer you files.

What it does do:

  • Nothing leaves this machine until you send it, and then it goes to
    one server on your own network. There is no account and no cloud.

  • Nothing is written to disk from a file offer until you accept it,
    and an accepted file always lands under a fresh name inside your
    download directory.

  • The daemon's socket is yours alone: it is mode 0600 and every
    connection is checked against your own user id, so another user on
    this machine cannot read your history or send messages as you.

  • Your history is also here, not only on the server: you can read a
    conversation with the server switched off.

  • A file's checksum is verified on arrival. That catches a truncated
    or corrupted transfer. It is not a security check: somebody who can
    change the bytes can change the checksum with them.

Treat it the way you would treat talking out loud in the same room.
`))
}

func (s *session) showAbout() {
	st, err := s.cli.Status()
	body := "comms-chat-lan\n\nLAN chat with one server and no accounts.\nBuilt on uitoolkit — https://github.com/codemodify/uitoolkit\n"
	if err == nil {
		body += fmt.Sprintf("\ndaemon      %s\nserver      %s (%s)\ndiscovery   %s\nstorage     %s\nyour id     %s\n",
			st.Version, st.Server, connectedWord(st.Connected), st.Discovery, storageName(st.DataDir), st.Self.ID)
	}
	body += "\nThe server announces itself with a UDP beacon of this application's own,\nnot mDNS: this application's clients can find it, and avahi-browse cannot."
	s.showText("About", body)
}

func connectedWord(up bool) string {
	if up {
		return "connected"
	}
	return "not connected — showing what this machine already has"
}

func storageName(dir string) string {
	if dir == "" {
		return "memory (nothing is written to disk)"
	}
	return dir
}

// showText is the read-only panel the Help menu puts up. It is a text area
// rather than a message box because these are paragraphs, and a person
// should be able to select a line out of them and scroll.
func (s *session) showText(title, body string) {
	view := widgets.NewTextView(body, "")
	view.SetAccessibleName(title)
	view.SetPreferred(560, 360)

	var overlay *widgets.Overlay
	card := widgets.DialogCard(title, "", widgets.NewButton("Close", func() { s.closeOverlay(overlay) }))
	card.Content().Add(view)
	overlay = widgets.NewOverlay(card)
	overlay.Modal = true
	overlay.InitialFocus = view
	s.showOverlay(overlay)
}
