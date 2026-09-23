// Package chatui is comms-chat-lan's desktop front end, built on
// uitoolkit (https://github.com/codemodify/uitoolkit).
//
// It is a client of comms-chat-lan-clientd and nothing else: it opens no
// socket of its own, speaks no protocol to the LAN, never sees the
// server, and holds no state beyond what it is currently showing. Everything it shows came from a JSON-RPC call
// and everything it changes is a JSON-RPC call, which is what lets the
// desktop UI and the terminal UI be open at the same time and agree.
package chatui

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/uitoolkit/app"
	"github.com/codemodify/uitoolkit/platform"
	"github.com/codemodify/uitoolkit/widget"
	"github.com/codemodify/uitoolkit/widgets"
)

// session is one open window.
type session struct {
	app *app.Application
	win *app.Window
	cli *chat.Client

	// Widgets.
	root      widget.Component
	list      *widgets.ListView
	script    *transcript
	scroll    *widgets.ScrollView
	composer  *widgets.TextField
	sendBtn   *widgets.Button
	attachBtn *widgets.Button
	title     *widgets.Label
	subtitle  *widgets.Label
	offerBar  *widgets.FlexBox
	offerText *widgets.Label
	acceptBtn *widgets.Button
	declineBt *widgets.Button
	status    *widgets.StatusBar
	menu      *widgets.MenuBar

	mu        sync.Mutex
	self      chat.Identity
	convs     []chat.Conversation
	current   chat.ConversationID
	peers     map[chat.PeerID]chat.Peer
	typing    map[chat.ConversationID][]chat.PeerID
	transfers map[chat.TransferID]chat.Transfer
	daemonUp  bool
	note      string
	// pending is work that arrived from the daemon while no event loop
	// was pumping. See post.
	pending []func()

	notifier notifier
	tray     platform.StatusItem

	lastTyping time.Time
}

// Open builds the window's content over a connected client. The look is
// the application's, so the window is whatever theme pack the toolkit
// resolved (and whatever UITK_THEME asked for): nothing here paints a
// colour of its own except the peer avatars, which come from the palette.
func Open(a *app.Application, win *app.Window, cli *chat.Client) widget.Component {
	s := &session{
		app: a, win: win, cli: cli,
		peers:     map[chat.PeerID]chat.Peer{},
		typing:    map[chat.ConversationID][]chat.PeerID{},
		transfers: map[chat.TransferID]chat.Transfer{},
		daemonUp:  true,
	}
	s.build()
	s.notifier = newNotifier(a)
	s.setupTray()

	cli.OnEvent(s.onEvent)
	cli.AutoReconnect(func(up bool) {
		s.post(func() {
			s.mu.Lock()
			s.daemonUp = up
			s.mu.Unlock()
			if up {
				s.reloadAll()
			}
			s.drawStatus()
		})
	})
	s.reloadAll()
	return s.root
}

// post runs fn on the UI goroutine. Daemon events arrive on the client's
// reader goroutine, and uitoolkit widgets may only be touched from the
// goroutine that pumps the loop.
//
// When no loop is pumping — a headless test, the one-frame screenshot —
// the work is queued instead of run. It must not be run inline: this is
// called from the client's reader goroutine, and almost every handler
// makes a call of its own, which that goroutine would then have to read
// the reply to. The first version of this ran inline and deadlocked on
// the first event.
func (s *session) post(fn func()) {
	if s.app != nil && s.app.Looping() {
		s.app.Post(fn)
		return
	}
	s.mu.Lock()
	s.pending = append(s.pending, fn)
	s.mu.Unlock()
}

// DrainDaemonEvents runs the work queued by [session.post] while no loop
// was pumping, on the calling goroutine. A headless test calls it after
// doing something it expects the daemon to report back.
func (s *session) DrainDaemonEvents() {
	for {
		s.mu.Lock()
		queue := s.pending
		s.pending = nil
		s.mu.Unlock()
		if len(queue) == 0 {
			return
		}
		for _, fn := range queue {
			fn()
		}
	}
}

// ------------------------------------------------------------ the window

func (s *session) build() {
	s.menu = widgets.NewMenuBar(s.menus()...)

	s.list = widgets.NewListView(0, func(i int) string { return s.convLabel(i) }, func(i int) { s.selectIndex(i) })
	s.list.Sidebar = true
	// The letters in this window are shortcuts and text, never a
	// type-ahead search through the conversation list.
	s.list.DisableTypeAhead = true
	s.list.SetAccessibleName("Conversations")

	s.title = widgets.NewTitle("comms-chat-lan")
	s.subtitle = widgets.NewLabel("Looking for people on this network…")

	s.script = newTranscript()
	s.script.nameOf = s.peerName
	s.script.colorOf = s.peerColor
	s.script.transferOf = s.transfer
	s.script.onActivate = s.activateMessage
	s.scroll = widgets.NewScrollView(s.script)

	s.offerText = widgets.NewLabel("")
	s.acceptBtn = widgets.NewButton("Accept", s.acceptOffer)
	s.acceptBtn.Primary = true
	s.declineBt = widgets.NewButton("Decline", s.declineOffer)
	s.offerBar = widgets.NewRow(s.offerText).WithGap(8).WithPadding(12, 6, 12, 6)
	s.offerBar.AddFlex(widgets.NewSpacer(), 1)
	s.offerBar.Add(s.acceptBtn)
	s.offerBar.Add(s.declineBt)
	s.offerBar.SetVisible(false)

	s.composer = widgets.NewTextField("", "Write a message…", nil)
	s.composer.OnSubmit = func(string) { s.send() }
	s.composer.OnInput = func(text string) { s.noteTyping(strings.TrimSpace(text) != "") }
	s.composer.SetAccessibleName("Message")
	s.sendBtn = widgets.NewButton("Send", s.send)
	s.sendBtn.Primary = true
	s.attachBtn = widgets.NewButton("Attach…", s.attach)

	composerRow := widgets.NewRow().WithGap(8).WithPadding(12, 8, 12, 10)
	composerRow.AddFlex(s.composer, 1)
	composerRow.Add(s.attachBtn)
	composerRow.Add(s.sendBtn)

	header := widgets.NewColumn(s.title, s.subtitle).WithGap(2).WithPadding(12, 10, 12, 8)

	right := widgets.NewColumn(header, widgets.NewSeparator()).WithGap(0)
	right.AddFlex(s.scroll, 1)
	right.Add(s.offerBar)
	right.Add(widgets.NewSeparator())
	right.Add(composerRow)

	split := widgets.NewSplitter(true, s.list, right)
	split.Ratio = 0.26

	s.status = widgets.NewStatusBar("", "", "")

	body := widgets.NewColumn(s.menu).WithGap(0)
	body.AddFlex(split, 1)
	body.Add(s.status)

	s.root = newShortcutRoot(s, body, s.handleKey, func(c widget.Component) {
		// The composer is where the cursor belongs the moment the window
		// opens: this is an application people type into.
		s.win.SetInitialFocus(s.composer)
	})
}

func (s *session) menus() []*widgets.Menu {
	return []*widgets.Menu{
		widgets.NewMenu("&Chat",
			widgets.ItemAccel("&Join Room…", "Ctrl+N", s.promptJoinRoom),
			widgets.ItemAccel("Send &File…", "Ctrl+O", s.attach),
			widgets.Sep(),
			widgets.ItemAccel("&Preferences…", "Ctrl+P", s.showPreferences),
			widgets.Sep(),
			widgets.ItemAccel("&Quit", "Ctrl+Q", s.quit),
		),
		widgets.NewMenu("&Conversation",
			widgets.ItemAccel("Mark as &Read", "Ctrl+R", func() { s.markRead(s.currentConv()) }),
			widgets.ItemAccel("&Next Conversation", "Alt+Down", func() { s.moveSelection(1) }),
			widgets.ItemAccel("&Previous Conversation", "Alt+Up", func() { s.moveSelection(-1) }),
			widgets.Sep(),
			widgets.Item("&Leave Room", s.leaveRoom),
			widgets.Item("&Block This Peer", func() { s.setBlocked(true) }),
			widgets.Item("&Unblock This Peer", func() { s.setBlocked(false) }),
		),
		widgets.NewMenu("&Status",
			widgets.RadioItem("&Online", "presence", true, func() { s.setPresence(chat.PresenceOnline) }),
			widgets.RadioItem("&Away", "presence", false, func() { s.setPresence(chat.PresenceAway) }),
			widgets.RadioItem("&Busy", "presence", false, func() { s.setPresence(chat.PresenceBusy) }),
		),
		widgets.NewMenu("&Help",
			widgets.Item("&Who Is Here", s.showPeers),
			widgets.Item("&Keyboard Shortcuts", s.showKeys),
			widgets.Item("&Security and Privacy", s.showSecurity),
			widgets.Sep(),
			widgets.Item("&About", s.showAbout),
		),
	}
}

// ------------------------------------------------------------- the events

func (s *session) onEvent(ev chat.Event) {
	switch ev.Kind {
	case chat.EventMessage, chat.EventState:
		s.post(func() {
			s.reloadConvs()
			if ev.Conv == s.currentConv() {
				s.reloadMessages()
				// The window is open on this conversation, so the user is
				// looking at it: do not let it accrue an unread badge.
				go s.markReadQuietly(ev.Conv)
			}
		})
	case chat.EventPeers:
		s.post(func() {
			s.reloadPeers()
			s.reloadConvs()
			s.drawHeader()
			s.drawStatus()
		})
	case chat.EventTyping:
		s.post(func() {
			s.mu.Lock()
			who := dropPeer(s.typing[ev.Conv], ev.Peer)
			if ev.Typing {
				who = append(who, ev.Peer)
			}
			s.typing[ev.Conv] = who
			s.mu.Unlock()
			s.drawHeader()
		})
	case chat.EventTransfer:
		s.post(func() {
			if ev.Transfer != nil {
				s.mu.Lock()
				s.transfers[ev.Transfer.ID] = *ev.Transfer
				s.mu.Unlock()
			}
			s.drawOfferBar()
			s.reloadMessages()
		})
	case chat.EventNotify:
		s.post(func() { s.notify(ev.Title, ev.Body) })
	case chat.EventStatus:
		s.post(func() {
			s.mu.Lock()
			s.note = ev.Text
			s.mu.Unlock()
			s.drawStatus()
		})
	}
}

func dropPeer(list []chat.PeerID, id chat.PeerID) []chat.PeerID {
	out := make([]chat.PeerID, 0, len(list))
	for _, p := range list {
		if p != id {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------- loading

func (s *session) reloadAll() {
	if id, err := s.cli.Identity(); err == nil {
		s.mu.Lock()
		s.self = id
		s.mu.Unlock()
	}
	s.reloadPeers()
	s.reloadTransfers()
	s.reloadConvs()
	s.reloadMessages()
	s.drawHeader()
	s.drawStatus()
}

func (s *session) reloadPeers() {
	peers, err := s.cli.Peers()
	if err != nil {
		return
	}
	m := make(map[chat.PeerID]chat.Peer, len(peers))
	for _, p := range peers {
		m[p.ID] = p
	}
	s.mu.Lock()
	s.peers = m
	s.mu.Unlock()
}

func (s *session) reloadTransfers() {
	list, err := s.cli.Transfers()
	if err != nil {
		return
	}
	m := make(map[chat.TransferID]chat.Transfer, len(list))
	for _, tr := range list {
		m[tr.ID] = tr
	}
	s.mu.Lock()
	s.transfers = m
	s.mu.Unlock()
	s.drawOfferBar()
}

func (s *session) reloadConvs() {
	convs, err := s.cli.Conversations()
	if err != nil {
		return
	}
	s.mu.Lock()
	s.convs = convs
	current := s.current
	s.mu.Unlock()

	s.list.Count = len(convs)
	// The order changes whenever somebody speaks, so the selection is put
	// back by conversation, never by index.
	s.list.Selected = -1
	for i, c := range convs {
		if c.ID == current {
			s.list.Selected = i
		}
	}
	if s.list.Selected < 0 && current == "" && len(convs) > 0 {
		s.list.Selected = 0
		s.selectIndex(0)
	}
	s.list.Invalidate()
	s.win.RequestLayout()
}

func (s *session) convLabel(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.convs) {
		return ""
	}
	c := s.convs[i]
	label := c.Title
	if c.Room != "" {
		label = "# " + c.Room
		if c.Members > 0 {
			label += fmt.Sprintf("   %d here", c.Members)
		}
	} else {
		label = presenceMark(c.Presence) + " " + c.Title
	}
	if c.Unread > 0 {
		label += fmt.Sprintf("   (%d)", c.Unread)
	}
	return label
}

// presenceMark is a word, not a coloured dot. A dot in a list row would
// have to be painted, and in a high-contrast or monochrome theme pack it
// would be the only thing in the window carrying meaning by colour alone.
func presenceMark(p chat.Presence) string {
	switch p.Valid() {
	case chat.PresenceOnline:
		return "•"
	case chat.PresenceAway:
		return "◦"
	case chat.PresenceBusy:
		return "⊘"
	default:
		return " "
	}
}

func (s *session) reloadMessages() {
	conv := s.currentConv()
	if conv == "" {
		s.script.SetMessages(nil)
		return
	}
	msgs, err := s.cli.Messages(conv, 500)
	if err != nil {
		return
	}
	s.script.SetMessages(msgs)
	// A conversation is read bottom-up: land on the newest line. It has
	// to happen twice — once now, for the case where the transcript is
	// already the right size, and once after the next layout, because
	// until the transcript has measured the new messages the scroll view
	// does not yet know how far down "the bottom" is.
	pin := func() { s.scroll.ScrollTo(s.scroll.MaxOffset()) }
	pin()
	s.script.afterLayout = pin
	s.drawOfferBar()
}

func (s *session) drawHeader() {
	conv := s.currentConv()
	if conv == "" {
		s.title.SetText("comms-chat-lan")
		s.subtitle.SetText("Pick somebody on the left, or press Ctrl+N to join a room.")
		return
	}
	view, err := s.cli.Conversation(conv)
	if err != nil {
		return
	}
	s.title.SetText(view.Conv.Title)

	var parts []string
	if conv.IsRoom() {
		parts = append(parts, fmt.Sprintf("%d here", view.Conv.Members))
		var names []string
		for _, p := range view.Peers {
			names = append(names, p.DisplayName())
		}
		sort.Strings(names)
		if len(names) > 0 {
			parts = append(parts, strings.Join(names, ", "))
		}
	} else if len(view.Peers) == 1 {
		p := view.Peers[0]
		parts = append(parts, string(p.Presence.Valid()))
		if p.Addr != "" {
			parts = append(parts, p.Addr)
		}
		if p.Blocked {
			parts = append(parts, "blocked")
		}
	}
	if who := s.typingNames(conv); who != "" {
		parts = append(parts, who+" is typing…")
	}
	s.subtitle.SetText(strings.Join(parts, "  ·  "))
}

func (s *session) typingNames(conv chat.ConversationID) string {
	s.mu.Lock()
	ids := append([]chat.PeerID(nil), s.typing[conv]...)
	s.mu.Unlock()
	if len(ids) == 0 {
		return ""
	}
	var names []string
	for _, id := range ids {
		names = append(names, s.peerName(id))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (s *session) drawStatus() {
	s.mu.Lock()
	self, up, note := s.self, s.daemonUp, s.note
	online := 0
	for _, p := range s.peers {
		if p.Presence.Valid() != chat.PresenceOffline {
			online++
		}
	}
	total := len(s.peers)
	s.mu.Unlock()

	if !up {
		s.status.SetParts("comms-chat-lan-clientd is not answering — reconnecting…", "", "")
		return
	}
	who := fmt.Sprintf("%s — %s", self.Nick, self.Presence.Valid())
	people := fmt.Sprintf("%d here of %d known", online, total)
	if total == 0 {
		people = "nobody else here yet"
	}
	third := "not encrypted — LAN only"
	if note != "" {
		third = note
	}
	s.status.SetParts(who, people, third)
}

// drawOfferBar shows the accept/decline strip when a file is waiting for
// an answer in the conversation on screen. A file nobody has agreed to is
// never written to disk, so this strip is the only way one lands.
func (s *session) drawOfferBar() {
	conv := s.currentConv()
	tr, ok := s.pendingOffer(conv)
	if !ok {
		s.offerBar.SetVisible(false)
		return
	}
	s.offerText.SetText(fmt.Sprintf("%s offers %s (%s)",
		s.peerName(tr.Peer), tr.Name, humanSize(tr.Size)))
	s.offerBar.SetVisible(true)
	s.win.RequestLayout()
}

func (s *session) pendingOffer(conv chat.ConversationID) (chat.Transfer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best chat.Transfer
	found := false
	for _, tr := range s.transfers {
		if tr.State != chat.TransferIncoming || (conv != "" && tr.Conv != conv) {
			continue
		}
		if !found || tr.At.Before(best.At) {
			best, found = tr, true
		}
	}
	return best, found
}

// ---------------------------------------------------------------- actions

func (s *session) currentConv() chat.ConversationID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *session) selectIndex(i int) {
	s.mu.Lock()
	if i < 0 || i >= len(s.convs) {
		s.mu.Unlock()
		return
	}
	conv := s.convs[i].ID
	if conv == s.current {
		s.mu.Unlock()
		return
	}
	s.current = conv
	s.mu.Unlock()

	s.list.Selected = i
	s.reloadMessages()
	s.drawHeader()
	go s.markReadQuietly(conv)
}

func (s *session) selectConv(id chat.ConversationID) {
	s.mu.Lock()
	for i, c := range s.convs {
		if c.ID == id {
			s.mu.Unlock()
			s.selectIndex(i)
			return
		}
	}
	s.mu.Unlock()
}

func (s *session) moveSelection(d int) {
	s.mu.Lock()
	n := len(s.convs)
	s.mu.Unlock()
	if n == 0 {
		return
	}
	i := s.list.Selected
	if i < 0 {
		i = 0
	} else {
		i = (i + d + n) % n
	}
	s.selectIndex(i)
	s.list.EnsureVisible(i)
}

func (s *session) send() {
	text := strings.TrimSpace(s.composer.Text)
	if text == "" {
		return
	}
	conv := s.currentConv()
	if conv == "" {
		s.info("Nowhere to send it", "Pick a conversation on the left, or press Ctrl+N to join a room.")
		return
	}
	s.composer.SetText("")
	s.noteTyping(false)
	s.async(func() (any, error) { return s.cli.Send(conv, text) }, func(_ any, err error) {
		if err != nil {
			s.warn("The message was not sent", err.Error())
			return
		}
		s.reloadMessages()
		s.reloadConvs()
	})
}

func (s *session) attach() {
	conv := s.currentConv()
	if conv == "" || conv.IsRoom() {
		s.info("Files go to one person",
			"A file is offered to a single peer, who accepts or declines it. Pick a peer on the left rather than a room.")
		return
	}
	widgets.ShowFileDialog(s.root, widgets.FileDialogOptions{
		Title: "Send a file", Mode: widgets.FileOpen, Native: true,
		OnPick: func(path string) {
			s.async(func() (any, error) { return s.cli.OfferFile(conv, path) },
				func(_ any, err error) {
					if err != nil {
						s.warn("The file was not offered", err.Error())
						return
					}
					s.reloadTransfers()
					s.reloadMessages()
				})
		},
	})
}

func (s *session) acceptOffer() {
	tr, ok := s.pendingOffer(s.currentConv())
	if !ok {
		return
	}
	s.async(func() (any, error) { return nil, s.cli.AcceptFile(tr.ID) }, func(_ any, err error) {
		if err != nil {
			s.warn("The file was not accepted", err.Error())
		}
		s.reloadTransfers()
	})
}

func (s *session) declineOffer() {
	tr, ok := s.pendingOffer(s.currentConv())
	if !ok {
		return
	}
	s.async(func() (any, error) { return nil, s.cli.DeclineFile(tr.ID, "declined") }, func(_ any, err error) {
		if err != nil {
			s.warn("The file was not declined", err.Error())
		}
		s.reloadTransfers()
	})
}

// activateMessage is Return on a transcript row. On a row that announced a
// file it answers the offer; on anything else it does nothing, which is
// better than inventing a behaviour.
func (s *session) activateMessage(m chat.Message) {
	if m.Transfer == nil {
		return
	}
	tr, ok := s.transfer(m.Transfer.ID)
	if !ok {
		return
	}
	if tr.State == chat.TransferIncoming {
		widgets.Confirm(s.root, "Accept this file?",
			fmt.Sprintf("%s (%s) from %s.\n\nIt will be saved in %s.",
				tr.Name, humanSize(tr.Size), s.peerName(tr.Peer), chat.DownloadDir()),
			func(yes bool) {
				if yes {
					s.acceptOffer()
				} else {
					s.declineOffer()
				}
			})
		return
	}
	if tr.State == chat.TransferDone && tr.Path != "" {
		s.win.OpenURI("file://"+tr.Path, func(error) {})
	}
}

func (s *session) markRead(conv chat.ConversationID) {
	if conv == "" {
		return
	}
	s.async(func() (any, error) { return s.cli.MarkRead(conv) }, func(any, error) { s.reloadConvs() })
}

// markReadQuietly is the same thing without a UI hop, for the case where
// a message arrives in the conversation already on screen.
func (s *session) markReadQuietly(conv chat.ConversationID) {
	if conv == "" {
		return
	}
	if _, err := s.cli.MarkRead(conv); err == nil {
		s.post(s.reloadConvs)
	}
}

func (s *session) promptJoinRoom() {
	field := widgets.NewTextField("", "general", nil)
	field.SetAccessibleName("Room name")
	var overlay *widgets.Overlay
	join := func() {
		name := strings.TrimSpace(field.Text)
		if overlay != nil {
			overlay.OnClose = nil
		}
		s.closeOverlay(overlay)
		if name == "" {
			return
		}
		s.async(func() (any, error) { return s.cli.JoinRoom(name) }, func(v any, err error) {
			if err != nil {
				s.warn("The room was not joined", err.Error())
				return
			}
			s.reloadConvs()
			if room, ok := v.(string); ok {
				s.selectConv(chat.RoomConv(room))
			}
		})
	}
	field.OnSubmit = func(string) { join() }
	card := widgets.DialogCard("Join a room",
		"Anyone on this network who joins a room of the same name is in it with you.\nThere is nobody to ask: the name is the room.",
		widgets.NewButton("Cancel", func() { s.closeOverlay(overlay) }),
		widgets.NewButton("Join", join))
	card.Content().Add(field)
	overlay = widgets.NewOverlay(card)
	overlay.Modal = true
	overlay.InitialFocus = field
	s.showOverlay(overlay)
}

func (s *session) leaveRoom() {
	conv := s.currentConv()
	if !conv.IsRoom() {
		s.info("Not a room", "Leaving applies to a room. Pick one on the left first.")
		return
	}
	room := conv.Room()
	widgets.Confirm(s.root, "Leave #"+room+"?",
		"You will stop receiving messages sent to this room. What has already been said stays in your history.",
		func(yes bool) {
			if !yes {
				return
			}
			s.async(func() (any, error) { return nil, s.cli.LeaveRoom(room) }, func(any, error) {
				s.mu.Lock()
				s.current = ""
				s.mu.Unlock()
				s.reloadConvs()
				s.reloadMessages()
				s.drawHeader()
			})
		})
}

func (s *session) setBlocked(blocked bool) {
	conv := s.currentConv()
	peer := convPeer(conv)
	if peer == "" {
		s.info("Not a peer", "Blocking applies to one peer. Pick somebody on the left first.")
		return
	}
	s.async(func() (any, error) { return nil, s.cli.Block(peer, blocked) }, func(_ any, err error) {
		if err != nil {
			s.warn("The peer was not changed", err.Error())
			return
		}
		s.reloadPeers()
		s.reloadConvs()
		s.drawHeader()
	})
}

func convPeer(c chat.ConversationID) chat.PeerID {
	if c.IsRoom() || !strings.HasPrefix(string(c), "peer:") {
		return ""
	}
	return chat.PeerID(strings.TrimPrefix(string(c), "peer:"))
}

func (s *session) setPresence(p chat.Presence) {
	s.async(func() (any, error) { return s.cli.SetPresence(p) }, func(v any, err error) {
		if err != nil {
			s.warn("Presence was not changed", err.Error())
			return
		}
		if id, ok := v.(chat.Identity); ok {
			s.mu.Lock()
			s.self = id
			s.mu.Unlock()
		}
		s.drawStatus()
	})
}

// noteTyping tells the daemon we are composing, at most twice a second.
// One keystroke is not one round trip.
func (s *session) noteTyping(on bool) {
	conv := s.currentConv()
	if conv == "" {
		return
	}
	s.mu.Lock()
	if on && time.Since(s.lastTyping) < 500*time.Millisecond {
		s.mu.Unlock()
		return
	}
	s.lastTyping = time.Now()
	s.mu.Unlock()
	go func() { _ = s.cli.SetTyping(conv, on) }()
}

func (s *session) quit() {
	if s.tray != nil {
		// A live tray item keeps the loop running after the last window
		// closes, which is what makes close-to-tray work — and what would
		// make Quit look like a no-op if the item were left alive.
		_ = s.tray.Close()
		s.tray = nil
	}
	if s.notifier != nil {
		s.notifier.Close()
	}
	s.app.Quit()
}

// ---------------------------------------------------------------- helpers

func (s *session) peerName(id chat.PeerID) string {
	s.mu.Lock()
	p, ok := s.peers[id]
	self := s.self
	s.mu.Unlock()
	if id == self.ID {
		return self.Nick
	}
	if ok {
		return p.DisplayName()
	}
	return chat.ShortID(id)
}

func (s *session) peerColor(id chat.PeerID) string {
	s.mu.Lock()
	p, ok := s.peers[id]
	self := s.self
	s.mu.Unlock()
	if id == self.ID && self.Color != "" {
		return self.Color
	}
	if ok && p.Color != "" {
		return p.Color
	}
	return chat.ColorForID(id)
}

func (s *session) transfer(id chat.TransferID) (chat.Transfer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tr, ok := s.transfers[id]
	return tr, ok
}

// async runs work off the UI goroutine and delivers the result back on it.
// Every call in this package that reaches the daemon goes through it, so a
// daemon that is slow to answer slows nothing down but itself.
func (s *session) async(work func() (any, error), done func(any, error)) {
	if s.app == nil || !s.app.Looping() {
		// Headless and in tests: run inline, so behaviour stays
		// synchronous and a test does not have to pump the loop.
		v, err := work()
		if done != nil {
			done(v, err)
		}
		return
	}
	go func() {
		v, err := work()
		if done == nil {
			return
		}
		s.app.Post(func() { done(v, err) })
	}()
}

func (s *session) info(title, body string) { widgets.Info(s.root, title, body, nil) }
func (s *session) warn(title, body string) { widgets.Warn(s.root, title, body, nil) }

// showOverlay and closeOverlay are the window's one modal layer. The
// toolkit mounts an overlay on the window that hosts a component, so the
// root is what they are anchored to.
func (s *session) showOverlay(o *widgets.Overlay) {
	widget.ShowOverlay(s.root, o)
}

func (s *session) closeOverlay(o *widgets.Overlay) {
	widget.DismissOverlay(s.root)
}
