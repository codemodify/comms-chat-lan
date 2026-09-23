// Package chatclientd is comms-chat-lan-clientd: the daemon on the
// person's own machine, and the only thing in this application that talks
// to comms-chat-lan-server.
//
// It does two jobs that pull in opposite directions and are kept apart
// here on purpose. Facing the server it is a client: it finds one, enrols
// with it, says what it last saw and is told what it missed. Facing the
// front ends it is a server: the same Unix-socket JSON-RPC as before,
// answered entirely from a local cache, so that a window opens instantly,
// a conversation scrolls while the network is down, and a message typed
// into a disconnected client is kept and sent when the server comes back.
//
// It links no user interface, and a test says so.
package chatclientd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatwire"
)

// Version is what status.get reports. It is bumped by hand at a release.
const Version = "0.2.0"

// Daemon is the client daemon: a cache, a link to the server and the set
// of front ends looking at it.
type Daemon struct {
	// Store is the local cache: the roster, the rooms and the history as
	// far as this machine has been told about them. It is not the
	// authority — the server is — but it is what every front end reads.
	Store *chat.Store
	// Disc finds the server. Nil means [chatwire.NoDiscovery], which is
	// what being told an address explicitly amounts to.
	Disc chatwire.Discovery
	// Addr is a server address given rather than discovered ("host:port"
	// or "host"). It wins over discovery, which is the point of it: a
	// client on another segment cannot hear the beacon.
	Addr string
	// Log receives one line for anything worth knowing about. Nil discards.
	Log func(string)

	mu        sync.Mutex
	onEvent   func(chat.Event)
	typing    map[chat.PeerID]map[chat.ConversationID]time.Time
	conn      *serverConn
	server    string
	serverID  chatwire.ServerID
	srvName   string
	heard     string // the address the beacon last announced
	discNote  string
	connected bool
	cursor    uint64
	started   time.Time
	host      string

	xfers        *transfers
	lastProgress map[chat.TransferID]time.Time
	wake         chan struct{}
	wg           sync.WaitGroup
}

// New builds a client daemon over a store.
func New(store *chat.Store, disc chatwire.Discovery) *Daemon {
	if disc == nil {
		disc = chatwire.NoDiscovery()
	}
	host, _ := os.Hostname()
	return &Daemon{
		Store: store, Disc: disc,
		typing:       map[chat.PeerID]map[chat.ConversationID]time.Time{},
		xfers:        newTransfers(),
		lastProgress: map[chat.TransferID]time.Time{},
		wake:         make(chan struct{}, 1),
		host:         strings.TrimSuffix(host, ".local"),
	}
}

// OnEvent registers the sink for [chat.Event]s. It must be set before Run
// and is called from several goroutines, so the handler has to be safe
// for concurrent use — the RPC server's broadcast is.
func (d *Daemon) OnEvent(fn func(chat.Event)) {
	d.mu.Lock()
	d.onEvent = fn
	d.mu.Unlock()
}

func (d *Daemon) emit(ev chat.Event) {
	d.mu.Lock()
	fn := d.onEvent
	d.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

func (d *Daemon) logf(format string, args ...any) {
	d.mu.Lock()
	fn := d.Log
	d.mu.Unlock()
	if fn != nil {
		fn(fmt.Sprintf(format, args...))
	}
}

// Started is when Run began.
func (d *Daemon) Started() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.started
}

// Connected reports whether the link to the server is up right now. When
// it is not, every front end is looking at the cache and says so.
func (d *Daemon) Connected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected
}

// ServerAddr is the server this daemon is enrolled with, or the last one
// it tried.
func (d *Daemon) ServerAddr() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.server != "" {
		return d.server
	}
	return d.heard
}

// ServerName is what the server calls itself, once it has said.
func (d *Daemon) ServerName() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.srvName
}

// Cursor is the last sequence number this daemon has seen. It is what a
// resume asks the server to carry on from.
func (d *Daemon) Cursor() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cursor
}

// Discovery is how the server was found, for status.get.
func (d *Daemon) Discovery() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.Addr != "":
		return "told (" + d.Addr + ")"
	case d.discNote != "":
		return d.Disc.Name() + " (" + d.discNote + ")"
	default:
		return d.Disc.Name()
	}
}

// Run finds a server, enrols, and keeps the link up until ctx is
// cancelled. It returns once every goroutine it started has stopped.
func (d *Daemon) Run(ctx context.Context) error {
	d.mu.Lock()
	d.started = time.Now()
	d.cursor, d.serverID = loadCursor(d.Store.Dir())
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	discDone := make(chan struct{})
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer close(discDone)
		err := d.Disc.Run(ctx, chatwire.DiscoveryEvents{
			Appeared: func(a chatwire.Announcement) { d.serverAppeared(a) },
			Left:     func(chatwire.ServerID) {},
			Note: func(msg string) {
				d.mu.Lock()
				d.discNote = msg
				d.mu.Unlock()
				d.logf("%s", msg)
			},
		})
		if err != nil {
			d.mu.Lock()
			d.discNote = err.Error()
			d.mu.Unlock()
			d.logf("discovery stopped: %v", err)
			d.emit(chat.Event{Kind: chat.EventStatus, Text: "discovery: " + err.Error()})
		}
	}()

	// Typing indicators expire even when nothing arrives to expire them.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.expireTyping()
			}
		}
	}()

	d.linkLoop(ctx)
	d.xfers.closeAll()
	<-discDone
	d.wg.Wait()
	d.saveCursor()
	return nil
}

func (d *Daemon) serverAppeared(a chatwire.Announcement) {
	if a.Bye || a.Addr == "" {
		return
	}
	d.mu.Lock()
	changed := d.heard != a.Addr
	d.heard = a.Addr
	d.srvName = a.Name
	d.mu.Unlock()
	if changed {
		d.logf("a server announced itself at %s (%s)", a.Addr, a.Name)
		d.nudge()
	}
}

// nudge wakes the link loop: a server has appeared and there is no point
// waiting out a backoff before trying it.
func (d *Daemon) nudge() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// ------------------------------------------------------------- outbound

// Send composes a message. It returns as soon as the message is in the
// local cache, whether or not the server can be reached: a person typing
// into a disconnected client is not made to wait, and what they typed is
// not lost. The server's acceptance arrives afterwards as a state change.
func (d *Daemon) Send(conv chat.ConversationID, body string) (chat.Message, error) {
	body = strings.TrimRight(body, " \t\r\n")
	if strings.TrimSpace(body) == "" {
		return chat.Message{}, fmt.Errorf("chat: nothing to send")
	}
	if conv == "" {
		return chat.Message{}, fmt.Errorf("chat: no conversation to send to")
	}
	self := d.Store.Self()
	m := chat.Message{
		ID: newMessageID(self.ID), Conv: conv, From: self.ID, FromNick: self.Nick,
		Body: chat.ClipRunes(body, chat.MaxBodyRunes), Sent: time.Now(),
		Mine: true, Read: true, State: chat.StateQueued,
	}
	if _, err := d.Store.Append(m); err != nil {
		return chat.Message{}, err
	}
	d.emit(chat.Event{Kind: chat.EventMessage, Conv: conv, Msg: &m})
	d.sendMessage(m)
	return m, nil
}

// sendMessage puts one composed message on the wire if there is a wire.
// If there is not, it stays queued and goes out when the link is back;
// that is the same path a resend takes, so there is one of them.
func (d *Daemon) sendMessage(m chat.Message) {
	if !d.write(chatwire.MsgFrame(m)) {
		return
	}
	d.setState(m.ID, chat.StateSending)
}

// flushQueue sends everything still waiting, oldest first. It runs when
// the server has said it is up to date with us, so the messages go out
// behind whatever we had missed rather than in front of it.
func (d *Daemon) flushQueue() {
	for _, m := range d.Store.Queued("") {
		if !d.write(chatwire.MsgFrame(m)) {
			return
		}
		d.setState(m.ID, chat.StateSending)
	}
}

func (d *Daemon) setState(id chat.MessageID, st chat.MessageState) {
	if m, ok := d.Store.SetState(id, st); ok {
		d.emit(chat.Event{Kind: chat.EventState, Conv: m.Conv, Msg: &m})
	}
}

// SetPresence records what we are doing and tells the server.
func (d *Daemon) SetPresence(p chat.Presence) (chat.Identity, error) {
	self := d.Store.Self()
	self.Presence = p.Valid()
	out, err := d.Store.SetSelf(self)
	if err != nil {
		return out, err
	}
	d.write(chatwire.Frame{Type: chatwire.FramePresence, Presence: out.Presence})
	return out, nil
}

// Readvertise tells the server about a changed nickname or colour. The
// server has no opinion about either; it relays them to everybody else.
func (d *Daemon) Readvertise() {
	self := d.Store.Self()
	d.write(chatwire.Frame{
		Type: chatwire.FramePresence, Nick: self.Nick,
		Color: self.Color, Presence: self.Presence.Valid(),
	})
}

// JoinRoom joins a room. The local cache records it at once so the front
// ends can open it; the server answers with what has been said in there,
// which is how somebody who joins on Thursday reads Monday.
func (d *Daemon) JoinRoom(name string) (string, bool, error) {
	room, changed, err := d.Store.JoinRoom(name)
	if err != nil {
		return room, false, err
	}
	d.write(chatwire.Frame{Type: chatwire.FrameJoin, Room: room})
	return room, changed, nil
}

// LeaveRoom leaves a room. Its history stays in the cache and on the
// server; rejoining shows it again.
func (d *Daemon) LeaveRoom(name string) (bool, error) {
	left, err := d.Store.LeaveRoom(name)
	if err != nil || !left {
		return left, err
	}
	d.write(chatwire.Frame{Type: chatwire.FrameLeave, Room: chat.CanonRoom(name)})
	return left, nil
}

// SetTyping tells the others in a conversation that we are composing. It
// is best effort and never queued: an indicator that arrives late is
// worse than one that never arrives.
func (d *Daemon) SetTyping(conv chat.ConversationID, on bool) {
	d.write(chatwire.Frame{Type: chatwire.FrameTyping, Conv: conv, Typing: on})
}

// ---------------------------------------------------------------- typing

// Typing is every peer currently typing in a conversation.
func (d *Daemon) Typing(conv chat.ConversationID) []chat.PeerID {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []chat.PeerID
	cutoff := time.Now().Add(-typingTimeout)
	for id, convs := range d.typing {
		if t, ok := convs[conv]; ok && t.After(cutoff) {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// typingTimeout is how long a typing indicator survives without being
// refreshed. Someone who starts typing and then walks away must not be
// shown as typing for ever.
const typingTimeout = 6 * time.Second

func (d *Daemon) onTyping(id chat.PeerID, conv chat.ConversationID, on bool) {
	if conv == "" {
		conv = chat.DirectConv(id)
	}
	d.mu.Lock()
	if d.typing[id] == nil {
		d.typing[id] = map[chat.ConversationID]time.Time{}
	}
	if on {
		d.typing[id][conv] = time.Now()
	} else {
		delete(d.typing[id], conv)
	}
	d.mu.Unlock()
	d.emit(chat.Event{Kind: chat.EventTyping, Conv: conv, Peer: id, Typing: on})
}

func (d *Daemon) expireTyping() {
	cutoff := time.Now().Add(-typingTimeout)
	type gone struct {
		id   chat.PeerID
		conv chat.ConversationID
	}
	var expired []gone
	d.mu.Lock()
	for id, convs := range d.typing {
		for conv, t := range convs {
			if t.Before(cutoff) {
				delete(convs, conv)
				expired = append(expired, gone{id, conv})
			}
		}
	}
	d.mu.Unlock()
	for _, g := range expired {
		d.emit(chat.Event{Kind: chat.EventTyping, Conv: g.conv, Peer: g.id, Typing: false})
	}
}

func (d *Daemon) clearTyping(id chat.PeerID, conv chat.ConversationID) {
	d.mu.Lock()
	had := false
	if convs := d.typing[id]; convs != nil {
		_, had = convs[conv]
		delete(convs, conv)
	}
	d.mu.Unlock()
	if had {
		d.emit(chat.Event{Kind: chat.EventTyping, Conv: conv, Peer: id, Typing: false})
	}
}

func (d *Daemon) clearAllTyping() {
	d.mu.Lock()
	old := d.typing
	d.typing = map[chat.PeerID]map[chat.ConversationID]time.Time{}
	d.mu.Unlock()
	for id, convs := range old {
		for conv := range convs {
			d.emit(chat.Event{Kind: chat.EventTyping, Conv: conv, Peer: id, Typing: false})
		}
	}
}

// ----------------------------------------------------------- notification

func (d *Daemon) maybeNotify(m chat.Message, from chat.Peer) {
	prefs := d.Store.NotifyPrefs()
	if !prefs.Enabled || m.Mine || m.System {
		return
	}
	mention := prefs.Mentions && mentions(m.Body, d.Store.Self().Nick)
	if m.Conv.IsRoom() && prefs.DirectOnly && !mention {
		return
	}
	title := from.DisplayName()
	if m.Conv.IsRoom() {
		title = from.DisplayName() + " in #" + m.Conv.Room()
	}
	body := chat.ClipRunes(strings.TrimSpace(m.Body), 160)
	d.emit(chat.Event{Kind: chat.EventNotify, Conv: m.Conv, Peer: m.From, Title: title, Body: body})
	chat.NotifyDesktop(title, body)
}

// mentions reports whether a body names us. It is a whole-word, case
// insensitive match on the nickname, so "sam" does not fire on "same".
func mentions(body, nick string) bool {
	nick = strings.ToLower(strings.TrimSpace(nick))
	if nick == "" {
		return false
	}
	// A nick like "sam@box" is matched on its first component too: that
	// is what people actually type.
	names := []string{nick}
	if i := strings.IndexByte(nick, '@'); i > 0 {
		names = append(names, nick[:i])
	}
	low := strings.ToLower(body)
	for _, name := range names {
		for i := 0; ; {
			j := strings.Index(low[i:], name)
			if j < 0 {
				break
			}
			j += i
			before := j == 0 || !isWordByte(low[j-1])
			after := j+len(name) >= len(low) || !isWordByte(low[j+len(name)])
			if before && after {
				return true
			}
			i = j + 1
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}

// newMessageID is "<short peer id>-<nanoseconds>-<random>": unique without
// anybody coordinating, minted by the sender so that a message composed
// while the server was unreachable already has its final identity.
func newMessageID(self chat.PeerID) chat.MessageID {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("chat: crypto/rand: " + err.Error())
	}
	return chat.MessageID(fmt.Sprintf("%s-%d-%s", chat.ShortID(self), time.Now().UnixNano(), hex.EncodeToString(b[:])))
}

// systemMessage files a line the application wrote itself: a decline, a
// saved file. It is local to this machine — the server never sees it —
// because it is about what happened here.
func (d *Daemon) systemMessage(conv chat.ConversationID, text string) {
	if conv == "" {
		return
	}
	self := d.Store.Self()
	m := chat.Message{
		ID: newMessageID(self.ID), Conv: conv, From: self.ID,
		Body: text, Sent: time.Now(), Seq: d.Cursor(),
		System: true, Read: true, Mine: true, State: chat.StateDelivered,
	}
	if added, err := d.Store.Append(m); err == nil && added {
		d.emit(chat.Event{Kind: chat.EventMessage, Conv: conv, Msg: &m})
	}
}
