// Package chattui is comms-chat-lan's terminal front end. It is a client
// of comms-chat-lan-clientd over the same Unix socket and the same
// JSON-RPC as the desktop UI, and it holds no business logic: everything
// it shows came from a method call, and everything it changes is a
// method call.
//
// It is meant to be used over ssh for a whole working day, so it is
// entirely keyboard-driven, it redraws only what changed, and it survives
// the daemon being restarted underneath it.
package chattui

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// UI is the whole terminal front end.
type UI struct {
	app    *tview.Application
	client *chat.Client

	pages    *tview.Pages
	list     *tview.List
	log      *tview.TextView
	input    *tview.InputField
	status   *tview.TextView
	headline *tview.TextView

	mu       sync.Mutex
	convs    []chat.Conversation
	current  chat.ConversationID
	self     chat.Identity
	typing   map[chat.ConversationID][]chat.PeerID
	peerName map[chat.PeerID]string
	up       bool
	// lastTyping throttles the typing.set calls: one keystroke is not one
	// round trip, and over ssh that matters.
	lastTyping time.Time
}

// New builds the terminal UI over a connected client.
func New(cli *chat.Client) *UI {
	u := &UI{
		app:      tview.NewApplication(),
		client:   cli,
		typing:   map[chat.ConversationID][]chat.PeerID{},
		peerName: map[chat.PeerID]string{},
		up:       true,
	}
	u.build()
	return u
}

func (u *UI) build() {
	u.headline = tview.NewTextView().SetDynamicColors(true)
	u.headline.SetTextAlign(tview.AlignLeft)

	u.list = tview.NewList().ShowSecondaryText(true)
	u.list.SetBorder(true).SetTitle(" Conversations ")
	u.list.SetChangedFunc(func(i int, _, _ string, _ rune) { u.selectIndex(i) })

	u.log = tview.NewTextView().
		SetDynamicColors(true).
		SetWordWrap(true).
		SetScrollable(true)
	u.log.SetBorder(true)
	u.log.SetChangedFunc(func() { u.app.Draw() })

	u.input = tview.NewInputField().SetLabel("> ")
	u.input.SetFieldWidth(0)
	u.input.SetDoneFunc(func(key tcell.Key) {
		if key != tcell.KeyEnter {
			return
		}
		text := strings.TrimSpace(u.input.GetText())
		u.input.SetText("")
		if text == "" {
			return
		}
		u.submit(text)
	})
	u.input.SetChangedFunc(func(text string) { u.noteTyping(text != "") })

	u.status = tview.NewTextView().SetDynamicColors(true)

	right := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(u.headline, 1, 0, false).
		AddItem(u.log, 0, 1, false).
		AddItem(u.input, 1, 0, true)

	body := tview.NewFlex().
		AddItem(u.list, 32, 0, false).
		AddItem(right, 0, 1, true)

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(body, 0, 1, true).
		AddItem(u.status, 1, 0, false)

	u.pages = tview.NewPages().AddPage("main", root, true, true)
	u.app.SetRoot(u.pages, true)
	u.app.SetInputCapture(u.keys)
}

// Run connects the event stream and blocks until the user quits.
func (u *UI) Run() error {
	u.client.OnEvent(u.onEvent)
	u.client.AutoReconnect(func(up bool) {
		u.mu.Lock()
		u.up = up
		u.mu.Unlock()
		u.post(func() {
			if up {
				u.reloadAll()
			}
			u.drawStatus()
		})
	})
	u.reloadAll()
	return u.app.Run()
}

// post runs fn on the tview goroutine. Everything that arrives from the
// daemon arrives on the client's reader goroutine, and tview is not safe
// to touch from there.
func (u *UI) post(fn func()) {
	go u.app.QueueUpdateDraw(fn)
}

// ------------------------------------------------------------------ events

func (u *UI) onEvent(ev chat.Event) {
	switch ev.Kind {
	case chat.EventMessage, chat.EventState:
		u.post(func() {
			u.reloadConvs()
			if ev.Conv == u.currentConv() {
				u.reloadMessages()
			}
		})
	case chat.EventPeers:
		u.post(func() {
			u.reloadConvs()
			u.drawStatus()
			u.drawHeadline()
		})
	case chat.EventTyping:
		u.mu.Lock()
		who := u.typing[ev.Conv]
		who = removePeer(who, ev.Peer)
		if ev.Typing {
			who = append(who, ev.Peer)
		}
		u.typing[ev.Conv] = who
		u.mu.Unlock()
		u.post(u.drawHeadline)
	case chat.EventTransfer:
		u.post(func() {
			if ev.Transfer != nil {
				u.note(transferLine(*ev.Transfer))
			}
			u.reloadMessages()
		})
	case chat.EventNotify:
		// The terminal's own notification: a bell, once, and a line in
		// the status bar. Anything more elaborate belongs to a terminal
		// emulator, not to us.
		u.post(func() {
			fmt.Print("\a")
			u.note("[yellow]" + ev.Title + ":[-] " + ev.Body)
		})
	}
}

func removePeer(list []chat.PeerID, id chat.PeerID) []chat.PeerID {
	out := list[:0]
	for _, p := range list {
		if p != id {
			out = append(out, p)
		}
	}
	return append([]chat.PeerID(nil), out...)
}

// ------------------------------------------------------------------ keys

func (u *UI) keys(ev *tcell.EventKey) *tcell.EventKey {
	switch ev.Key() {
	case tcell.KeyCtrlC:
		u.app.Stop()
		return nil
	case tcell.KeyTab:
		u.cycleFocus(1)
		return nil
	case tcell.KeyBacktab:
		u.cycleFocus(-1)
		return nil
	case tcell.KeyCtrlN:
		u.prompt("Join room", "", func(s string) { u.submit("/join " + s) })
		return nil
	case tcell.KeyCtrlG:
		u.showHelp()
		return nil
	case tcell.KeyCtrlP:
		u.cyclePresence()
		return nil
	case tcell.KeyCtrlU:
		// Page the transcript without leaving the composer, which is
		// where the cursor spends its life.
		row, _ := u.log.GetScrollOffset()
		u.log.ScrollTo(row-10, 0)
		return nil
	case tcell.KeyCtrlD:
		row, _ := u.log.GetScrollOffset()
		u.log.ScrollTo(row+10, 0)
		return nil
	}
	switch ev.Rune() {
	case 0:
	}
	// Alt+Up / Alt+Down move between conversations from anywhere, so the
	// hands never leave the composer.
	if ev.Modifiers()&tcell.ModAlt != 0 {
		switch ev.Key() {
		case tcell.KeyUp:
			u.moveSelection(-1)
			return nil
		case tcell.KeyDown:
			u.moveSelection(1)
			return nil
		}
	}
	return ev
}

func (u *UI) cycleFocus(dir int) {
	order := []tview.Primitive{u.input, u.list, u.log}
	cur := u.app.GetFocus()
	at := 0
	for i, p := range order {
		if p == cur {
			at = i
		}
	}
	at = (at + dir + len(order)) % len(order)
	u.app.SetFocus(order[at])
}

func (u *UI) moveSelection(delta int) {
	n := u.list.GetItemCount()
	if n == 0 {
		return
	}
	i := (u.list.GetCurrentItem() + delta + n) % n
	u.list.SetCurrentItem(i)
}

func (u *UI) cyclePresence() {
	order := []chat.Presence{chat.PresenceOnline, chat.PresenceAway, chat.PresenceBusy}
	u.mu.Lock()
	cur := u.self.Presence
	u.mu.Unlock()
	next := order[0]
	for i, p := range order {
		if p == cur {
			next = order[(i+1)%len(order)]
		}
	}
	if id, err := u.client.SetPresence(next); err == nil {
		u.mu.Lock()
		u.self = id
		u.mu.Unlock()
		u.drawStatus()
	} else {
		u.note("[red]" + err.Error())
	}
}

// ------------------------------------------------------------- the commands

// submit handles one line from the composer: a slash command, or a
// message. The commands are the ones a terminal user expects to exist,
// and each is a single RPC.
func (u *UI) submit(text string) {
	if !strings.HasPrefix(text, "/") {
		conv := u.currentConv()
		if conv == "" {
			u.note("[yellow]pick a conversation first (Tab, then the arrows)")
			return
		}
		if _, err := u.client.Send(conv, text); err != nil {
			u.note("[red]" + err.Error())
		}
		u.noteTyping(false)
		return
	}

	cmd, arg := text, ""
	if i := strings.IndexByte(text, ' '); i > 0 {
		cmd, arg = text[:i], strings.TrimSpace(text[i+1:])
	}
	switch cmd {
	case "/help", "/?":
		u.showHelp()
	case "/join":
		if arg == "" {
			u.note("[yellow]usage: /join <room>")
			return
		}
		room, err := u.client.JoinRoom(arg)
		if err != nil {
			u.note("[red]" + err.Error())
			return
		}
		u.reloadConvs()
		u.selectConv(chat.RoomConv(room))
	case "/leave":
		conv := u.currentConv()
		if arg == "" && conv.IsRoom() {
			arg = conv.Room()
		}
		if arg == "" {
			u.note("[yellow]usage: /leave <room>")
			return
		}
		if err := u.client.LeaveRoom(arg); err != nil {
			u.note("[red]" + err.Error())
			return
		}
		u.reloadConvs()
	case "/nick":
		if arg == "" {
			u.note("[yellow]usage: /nick <name>")
			return
		}
		u.mu.Lock()
		id := u.self
		u.mu.Unlock()
		id.Nick = arg
		out, err := u.client.SetIdentity(id)
		if err != nil {
			u.note("[red]" + err.Error())
			return
		}
		u.mu.Lock()
		u.self = out
		u.mu.Unlock()
		u.drawStatus()
	case "/who":
		u.showPeers()
	case "/send":
		if arg == "" {
			u.note("[yellow]usage: /send <path to a file>")
			return
		}
		conv := u.currentConv()
		if conv == "" || conv.IsRoom() {
			u.note("[yellow]files go to one peer, not to a room")
			return
		}
		go func() {
			if _, err := u.client.OfferFile(conv, arg); err != nil {
				u.post(func() { u.note("[red]" + err.Error()) })
			}
		}()
	case "/accept", "/decline":
		u.answerTransfer(cmd == "/accept", arg)
	case "/files":
		u.showTransfers()
	case "/status":
		u.showStatus()
	case "/quit":
		u.app.Stop()
	default:
		u.note("[yellow]no such command: " + cmd + "  (/help)")
	}
}

// answerTransfer accepts or declines an offer. With no argument it acts on
// the oldest offer still waiting, which is what a person means nine times
// out of ten.
func (u *UI) answerTransfer(accept bool, id string) {
	list, err := u.client.Transfers()
	if err != nil {
		u.note("[red]" + err.Error())
		return
	}
	var pick chat.Transfer
	found := false
	for i := len(list) - 1; i >= 0; i-- {
		tr := list[i]
		if tr.State != chat.TransferIncoming {
			continue
		}
		if id != "" && !strings.HasPrefix(string(tr.ID), id) && tr.Name != id {
			continue
		}
		pick, found = tr, true
		break
	}
	if !found {
		u.note("[yellow]no file offer is waiting")
		return
	}
	if accept {
		err = u.client.AcceptFile(pick.ID)
	} else {
		err = u.client.DeclineFile(pick.ID, "declined")
	}
	if err != nil {
		u.note("[red]" + err.Error())
	}
}

// ------------------------------------------------------------- the panels

func (u *UI) showHelp() {
	u.showText(" Keys and commands ", strings.TrimSpace(`
[::b]Keys[-::-]
  Tab / Shift+Tab   move between the composer, the list and the transcript
  Alt+Up / Alt+Down previous / next conversation, without leaving the composer
  Ctrl+U / Ctrl+D   scroll the transcript up / down
  Ctrl+N            join a room
  Ctrl+P            cycle presence: online, away, busy
  Ctrl+G            this help
  Ctrl+C            quit
  Escape            close a panel

[::b]Commands[-::-]
  /join <room>      join a room and open it
  /leave [room]     leave a room (the current one by default)
  /nick <name>      change the name the LAN sees
  /who              the roster: who is here, and how
  /send <path>      offer a file to the peer in this conversation
  /accept [name]    accept the waiting file offer
  /decline [name]   refuse it
  /files            every transfer this daemon knows about
  /status           what the daemon is doing
  /quit             quit

[::b]What this is[-::-]
  A LAN chat with one server on your own network and no accounts.
  Enrolment is open, nothing is encrypted and nobody is authenticated:
  anyone who can reach the server can join in and read what goes past.
  See docs/security.md.
`))
}

func (u *UI) showPeers() {
	peers, err := u.client.Peers()
	if err != nil {
		u.note("[red]" + err.Error())
		return
	}
	var b strings.Builder
	if len(peers) == 0 {
		b.WriteString("Nobody else is here yet.\n\nEveryone enrolled with the same server appears a few seconds after\nthey start. If nobody ever appears, this machine may not have found\nthe server: see /status, and docs/protocol.md.")
	}
	for _, p := range peers {
		mark := " "
		if p.Blocked {
			mark = "x"
		}
		fmt.Fprintf(&b, "%s [%s]%-24s[-]  %-8s %-21s %s\n",
			mark, tcellHex(p.Color), p.DisplayName(), p.Presence.Valid(),
			p.Addr, strings.Join(p.Rooms, " "))
	}
	b.WriteString("\nx = blocked here: the server still relays them, this machine drops them.")
	u.showText(" Who is here ", b.String())
}

func (u *UI) showTransfers() {
	list, err := u.client.Transfers()
	if err != nil {
		u.note("[red]" + err.Error())
		return
	}
	var b strings.Builder
	if len(list) == 0 {
		b.WriteString("No files have been offered in either direction.")
	}
	for _, tr := range list {
		b.WriteString(transferLine(tr) + "\n")
		if tr.Path != "" {
			b.WriteString("    " + tr.Path + "\n")
		}
	}
	u.showText(" Files ", b.String())
}

func (u *UI) showStatus() {
	st, err := u.client.Status()
	if err != nil {
		u.note("[red]" + err.Error())
		return
	}
	link := "connected"
	if !st.Connected {
		link = "not connected — showing what this machine already has"
	}
	u.showText(" Daemon ", fmt.Sprintf(
		"version    %s\nsocket     %s\nserver     %s\nlink       %s\ndiscovery  %s\ncursor     %d\nstorage    %s\npeople     %d (%d here now)\nfront ends %d\nup since   %s",
		st.Version, st.Socket, st.Server, link, st.Discovery, st.Cursor, storage(st.DataDir),
		st.Peers, st.Online, st.Clients, st.Started.Format(time.RFC1123)))
}

func storage(dir string) string {
	if dir == "" {
		return "memory (nothing is written to disk)"
	}
	return dir
}

// showText puts a scrollable panel over the chat until Escape.
func (u *UI) showText(title, body string) {
	view := tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	view.SetText(body)
	view.SetBorder(true).SetTitle(title + " — Escape closes ")
	view.SetDoneFunc(func(tcell.Key) { u.closePanel() })
	view.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			u.closePanel()
			return nil
		}
		return ev
	})
	u.pages.AddPage("panel", view, true, true)
	u.app.SetFocus(view)
}

func (u *UI) closePanel() {
	u.pages.RemovePage("panel")
	u.app.SetFocus(u.input)
}

// prompt asks for one line.
func (u *UI) prompt(title, value string, done func(string)) {
	field := tview.NewInputField().SetLabel(title + ": ").SetText(value)
	field.SetFieldWidth(40)
	field.SetDoneFunc(func(key tcell.Key) {
		text := strings.TrimSpace(field.GetText())
		u.closePanel()
		if key == tcell.KeyEnter && text != "" {
			done(text)
		}
	})
	form := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).
		AddItem(field, 1, 0, true).
		AddItem(nil, 0, 1, false)
	form.SetBorder(true).SetTitle(" " + title + " — Escape cancels ")
	u.pages.AddPage("panel", centred(form, 60, 5), true, true)
	u.app.SetFocus(field)
}

// centred puts a fixed-size box in the middle of the terminal, whatever
// size the terminal is.
func centred(p tview.Primitive, width, height int) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(p, height, 0, true).
			AddItem(nil, 0, 1, false), width, 0, true).
		AddItem(nil, 0, 1, false)
}

// ------------------------------------------------------------- the drawing

func (u *UI) reloadAll() {
	if id, err := u.client.Identity(); err == nil {
		u.mu.Lock()
		u.self = id
		u.mu.Unlock()
	}
	u.reloadConvs()
	u.reloadMessages()
	u.drawStatus()
	u.drawHeadline()
}

func (u *UI) reloadConvs() {
	convs, err := u.client.Conversations()
	if err != nil {
		u.note("[red]" + err.Error())
		return
	}
	peers, _ := u.client.Peers()
	names := map[chat.PeerID]string{}
	for _, p := range peers {
		names[p.ID] = p.DisplayName()
	}

	u.mu.Lock()
	u.convs = convs
	u.peerName = names
	current := u.current
	u.mu.Unlock()

	// Rebuilding the list loses the selection, so it is put back by
	// conversation id rather than by index: the order changes whenever
	// somebody speaks.
	u.list.Clear()
	selected := -1
	for i, c := range convs {
		if c.ID == current {
			selected = i
		}
		u.list.AddItem(convTitle(c), convSubtitle(c), 0, nil)
	}
	if selected >= 0 {
		u.list.SetCurrentItem(selected)
	} else if len(convs) > 0 && current == "" {
		u.list.SetCurrentItem(0)
		u.selectIndex(0)
	}
}

func convTitle(c chat.Conversation) string {
	name := c.Title
	if c.Room == "" {
		switch c.Presence.Valid() {
		case chat.PresenceOnline:
			name = "[green]•[-] " + name
		case chat.PresenceAway:
			name = "[yellow]•[-] " + name
		case chat.PresenceBusy:
			name = "[red]•[-] " + name
		default:
			name = "[gray]•[-] " + name
		}
	} else {
		name = "[blue]#[-] " + c.Room
	}
	if c.Unread > 0 {
		name += fmt.Sprintf("  [::b](%d)[-::-]", c.Unread)
	}
	return name
}

func convSubtitle(c chat.Conversation) string {
	line := strings.TrimSpace(strings.ReplaceAll(c.Last, "\n", " "))
	if line == "" && c.Members > 0 {
		return fmt.Sprintf("  %d here", c.Members)
	}
	if len([]rune(line)) > 28 {
		line = string([]rune(line)[:27]) + "…"
	}
	return "  " + line
}

func (u *UI) selectIndex(i int) {
	u.mu.Lock()
	if i < 0 || i >= len(u.convs) {
		u.mu.Unlock()
		return
	}
	conv := u.convs[i].ID
	if conv == u.current {
		u.mu.Unlock()
		return
	}
	u.current = conv
	u.mu.Unlock()

	u.reloadMessages()
	u.drawHeadline()
	go func() {
		if _, err := u.client.MarkRead(conv); err == nil {
			u.post(u.reloadConvs)
		}
	}()
}

func (u *UI) selectConv(id chat.ConversationID) {
	u.mu.Lock()
	for i, c := range u.convs {
		if c.ID == id {
			u.mu.Unlock()
			u.list.SetCurrentItem(i)
			return
		}
	}
	u.mu.Unlock()
}

func (u *UI) currentConv() chat.ConversationID {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.current
}

func (u *UI) reloadMessages() {
	conv := u.currentConv()
	if conv == "" {
		u.log.SetText("\n  Nothing selected yet.\n\n  Peers appear in the list on the left a few seconds after they\n  start. [::b]Ctrl+N[-::-] joins a room; [::b]Ctrl+G[-::-] lists every key and command.")
		return
	}
	msgs, err := u.client.Messages(conv, 500)
	if err != nil {
		u.note("[red]" + err.Error())
		return
	}
	u.mu.Lock()
	selfID := u.self.ID
	names := u.peerName
	u.mu.Unlock()

	var b strings.Builder
	lastDay := ""
	for _, m := range msgs {
		if day := m.Sent.Format("Monday 2 January 2006"); day != lastDay {
			lastDay = day
			fmt.Fprintf(&b, "\n[gray]──── %s ────[-]\n", day)
		}
		b.WriteString(messageLine(m, selfID, names))
	}
	u.log.SetText(b.String())
	u.log.ScrollToEnd()
}

func messageLine(m chat.Message, self chat.PeerID, names map[chat.PeerID]string) string {
	stamp := m.Sent.Format("15:04")
	if m.System {
		return fmt.Sprintf("[gray]%s  · %s[-]\n", stamp, m.Body)
	}
	who := m.FromNick
	if who == "" {
		if n, ok := names[m.From]; ok {
			who = n
		} else {
			who = chat.ShortID(m.From)
		}
	}
	colour := tcellHex(chat.ColorForID(m.From))
	mark := ""
	if m.From == self || m.Mine {
		switch m.State {
		case chat.StateQueued:
			mark = " [yellow](waiting)[-]"
		case chat.StateSending:
			mark = " [gray](sending)[-]"
		case chat.StateFailed:
			mark = " [red](not delivered)[-]"
		case chat.StateDelivered:
			mark = " [green]✓[-]"
		}
	}
	body := tview.Escape(m.Body)
	if m.Transfer != nil {
		body = fmt.Sprintf("[::b]%s[-::-]  %s", tview.Escape(m.Transfer.Name), humanSize(m.Transfer.Size))
	}
	return fmt.Sprintf("[gray]%s[-] [%s::b]%s[-::-]  %s%s\n", stamp, colour, who, body, mark)
}

func (u *UI) drawHeadline() {
	conv := u.currentConv()
	if conv == "" {
		u.headline.SetText("")
		return
	}
	view, err := u.client.Conversation(conv)
	if err != nil {
		u.headline.SetText("")
		return
	}
	line := " [::b]" + view.Conv.Title + "[-::-]"
	if conv.IsRoom() {
		line += fmt.Sprintf("   %d here", view.Conv.Members)
	} else if len(view.Peers) == 1 {
		p := view.Peers[0]
		line += "   " + string(p.Presence.Valid())
		if p.Addr != "" {
			line += "   [gray]" + p.Addr + "[-]"
		}
	}
	u.mu.Lock()
	who := u.typing[conv]
	names := u.peerName
	u.mu.Unlock()
	if len(who) > 0 {
		var list []string
		for _, id := range who {
			if n, ok := names[id]; ok {
				list = append(list, n)
			} else {
				list = append(list, chat.ShortID(id))
			}
		}
		sort.Strings(list)
		line += "   [gray]" + strings.Join(list, ", ") + " is typing…[-]"
	}
	u.headline.SetText(line)
	u.log.SetTitle(" " + view.Conv.Title + " ")
}

func (u *UI) drawStatus() {
	u.mu.Lock()
	self, up := u.self, u.up
	u.mu.Unlock()
	if !up {
		u.status.SetText(" [red]comms-chat-lan-clientd is not answering — reconnecting…[-]")
		return
	}
	colour := tcellHex(self.Color)
	u.status.SetText(fmt.Sprintf(
		" [%s::b]%s[-::-]  %s   [gray]Ctrl+G keys and commands   Ctrl+N join a room   Ctrl+C quit[-]",
		colour, self.Nick, self.Presence.Valid()))
}

// note puts one line in the transcript that came from us rather than from
// the LAN. It is not stored anywhere.
func (u *UI) note(text string) {
	fmt.Fprintf(u.log, "[gray]%s[-] %s\n", time.Now().Format("15:04"), text)
	u.log.ScrollToEnd()
}

// noteTyping tells the daemon we are composing, at most twice a second.
// One keystroke is not one round trip, which over ssh matters.
func (u *UI) noteTyping(on bool) {
	conv := u.currentConv()
	if conv == "" {
		return
	}
	u.mu.Lock()
	recent := time.Since(u.lastTyping) < 500*time.Millisecond
	if on && recent {
		u.mu.Unlock()
		return
	}
	u.lastTyping = time.Now()
	u.mu.Unlock()
	go func() { _ = u.client.SetTyping(conv, on) }()
}

// ------------------------------------------------------------------ helpers

// tcellHex turns "#rrggbb" into the form tview's dynamic colour tags want.
func tcellHex(s string) string {
	if len(s) == 7 && s[0] == '#' {
		return s
	}
	return "white"
}

func transferLine(tr chat.Transfer) string {
	dir := "→"
	if tr.Incoming {
		dir = "←"
	}
	state := string(tr.State)
	if tr.State == chat.TransferRunning && tr.Size > 0 {
		state = fmt.Sprintf("%d%%", tr.Done*100/tr.Size)
	}
	line := fmt.Sprintf("%s %-32s %9s  %s", dir, tr.Name, humanSize(tr.Size), state)
	if tr.Error != "" {
		line += "  [red]" + tr.Error + "[-]"
	}
	if tr.State == chat.TransferIncoming {
		line += "   [yellow]/accept or /decline[-]"
	}
	return line
}

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
