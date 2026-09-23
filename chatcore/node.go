package chatcore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Node is the network half of comms-chatd: the TCP listener peers dial,
// the connections to the peers we have dialled, the discovery that finds
// them and the rules that tie all three to the [Store].
//
// Everything that arrives from the network arrives here, and everything
// that leaves does so from here. The RPC server above it never touches a
// socket other than its own, and the front ends never see one at all.
type Node struct {
	Store *Store
	// Disc finds peers. Nil means [NoDiscovery].
	Disc Discovery
	// Port is the TCP port to accept conversations on. Zero means any free
	// port, which is the normal case: the port is advertised, so it does
	// not have to be well known.
	Port int
	// Log receives one line for anything worth knowing about that is not
	// an event. Nil discards.
	Log func(string)

	mu      sync.Mutex
	conns   map[PeerID]*peerConn
	typing  map[PeerID]map[ConversationID]time.Time
	onEvent func(NodeEvent)
	listen  string
	host    string
	note    string
	started time.Time

	xfers *transfers
	// lastProgress throttles transfer progress events, one clock per
	// transfer.
	lastProgress map[TransferID]time.Time
	wg           sync.WaitGroup
	// dialing keeps two goroutines from dialling the same peer at once.
	dialing map[PeerID]bool
}

// NodeEvent is something the daemon's clients should hear about. It is
// deliberately a single struct: the RPC layer turns it into one JSON
// notification and the front ends switch on Kind.
type NodeEvent struct {
	Kind string `json:"kind"`
	// Conv is set for anything that belongs to a conversation.
	Conv ConversationID `json:"conv,omitempty"`
	Peer PeerID         `json:"peer,omitempty"`

	Msg      *Message  `json:"msg,omitempty"`
	Transfer *Transfer `json:"transfer,omitempty"`
	Typing   bool      `json:"typing,omitempty"`

	// Title and Body are set on EventNotify.
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	// Text is a plain line for the status bar and the log.
	Text string `json:"text,omitempty"`
}

// NewNode builds a node over a store.
func NewNode(store *Store, disc Discovery) *Node {
	if disc == nil {
		disc = NoDiscovery()
	}
	host, _ := os.Hostname()
	return &Node{
		Store: store, Disc: disc,
		conns: map[PeerID]*peerConn{}, typing: map[PeerID]map[ConversationID]time.Time{},
		dialing: map[PeerID]bool{}, lastProgress: map[TransferID]time.Time{},
		host:    strings.TrimSuffix(host, ".local"),
		xfers:   newTransfers(),
	}
}

// OnEvent registers the sink for [NodeEvent]s. It must be set before Run
// and is called from several goroutines, so the handler has to be safe for
// concurrent use — the RPC server's broadcast is.
func (n *Node) OnEvent(fn func(NodeEvent)) {
	n.mu.Lock()
	n.onEvent = fn
	n.mu.Unlock()
}

func (n *Node) emit(ev NodeEvent) {
	n.mu.Lock()
	fn := n.onEvent
	n.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

func (n *Node) logf(format string, args ...any) {
	n.mu.Lock()
	fn := n.Log
	n.mu.Unlock()
	if fn != nil {
		fn(fmt.Sprintf(format, args...))
	}
}

// Listen is the "host:port" other peers dial, once Run has started.
func (n *Node) Listen() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.listen
}

// Note is the last discovery problem, for status.get. Empty means none.
func (n *Node) Note() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.note
}

// Started is when Run began.
func (n *Node) Started() time.Time {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.started
}

// Run binds the peer listener, starts discovery and serves until ctx is
// cancelled. It returns once every goroutine it started has stopped.
func (n *Node) Run(ctx context.Context) error {
	port := n.Port
	if port == 0 {
		if p, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvPort))); err == nil {
			port = p
		}
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return fmt.Errorf("chat: peer listener: %w", err)
	}
	actual := ln.Addr().(*net.TCPAddr).Port

	n.mu.Lock()
	n.listen = net.JoinHostPort(localAddress(), strconv.Itoa(actual))
	n.started = time.Now()
	n.mu.Unlock()
	n.logf("listening for peers on %s", n.Listen())

	n.Disc.Update(n.announcement(actual))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Accept loop.
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.wg.Add(1)
			go func() {
				defer n.wg.Done()
				n.accept(ctx, c)
			}()
		}
	}()

	// Discovery.
	discDone := make(chan struct{})
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer close(discDone)
		err := n.Disc.Run(ctx, DiscoveryEvents{
			Appeared: func(a Announcement) { n.peerAppeared(ctx, a) },
			Left:     func(id PeerID) { n.peerLeft(id) },
			Note: func(s string) {
				n.mu.Lock()
				n.note = s
				n.mu.Unlock()
				n.logf("%s", s)
			},
		})
		if err != nil {
			n.mu.Lock()
			n.note = err.Error()
			n.mu.Unlock()
			n.logf("discovery stopped: %v", err)
			n.emit(NodeEvent{Kind: EventStatus, Text: "discovery: " + err.Error()})
		}
	}()

	// Housekeeping: expire stale typing indicators and retry queued
	// messages for peers that have come back.
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n.expireTyping()
			}
		}
	}()

	<-ctx.Done()
	_ = ln.Close()
	n.closeAllConns()
	n.xfers.closeAll()
	<-discDone
	n.wg.Wait()
	return nil
}

// announcement is what we tell the LAN about ourselves right now.
func (n *Node) announcement(port int) Announcement {
	self := n.Store.Self()
	return Announcement{
		Version: ProtocolVersion, ID: self.ID, Nick: self.Nick, Color: self.Color,
		Host: n.host, Port: port, Rooms: self.Rooms, Presence: self.Presence.Valid(),
	}
}

// Readvertise pushes the current identity into the beacon. The RPC layer
// calls it after the nickname, colour, presence or room list changes, so
// the LAN hears about it on the next announcement instead of whenever the
// daemon happens to restart.
func (n *Node) Readvertise() {
	_, portStr, err := net.SplitHostPort(n.Listen())
	if err != nil {
		return
	}
	port, _ := strconv.Atoi(portStr)
	n.Disc.Update(n.announcement(port))
	// Peers we are already talking to hear it at once rather than waiting
	// for a beacon they may or may not receive.
	self := n.Store.Self()
	n.broadcastFrame(Frame{
		Type: FramePresence, From: self.ID, Nick: self.Nick,
		Color: self.Color, Rooms: self.Rooms, Presence: self.Presence.Valid(),
	})
}

// ------------------------------------------------------------- discovery

func (n *Node) peerAppeared(ctx context.Context, a Announcement) {
	if a.ID == n.Store.Self().ID {
		return
	}
	if n.Store.Blocked(a.ID) {
		return
	}
	changed := n.Store.PutPeer(a.Peer())
	if changed {
		n.emit(NodeEvent{Kind: EventPeers, Peer: a.ID})
	}
	// Anything queued for this peer goes out now. A peer that has just
	// come back is the whole reason the queue exists.
	if n.Store.hasQueuedFor(a.ID) {
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.flushQueue(ctx, a.ID)
		}()
	}
}

func (n *Node) peerLeft(id PeerID) {
	if !n.Store.SetPresence(id, PresenceOffline) {
		return
	}
	n.dropConn(id, "peer went quiet")
	n.clearTyping(id)
	n.emit(NodeEvent{Kind: EventPeers, Peer: id})
}

// ------------------------------------------------------------ connections

// peerConn is one live TCP conversation with one peer.
type peerConn struct {
	peer     PeerID
	conn     net.Conn
	out      chan Frame
	outbound bool // true if we dialled them
	closed   chan struct{}
	once     sync.Once
}

func (c *peerConn) close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

// send queues a frame. It reports false when the connection is going away,
// so the caller can fall back to the queue rather than block.
func (c *peerConn) send(f Frame) bool {
	select {
	case c.out <- f:
		return true
	case <-c.closed:
		return false
	case <-time.After(5 * time.Second):
		// A peer that is not reading is worse than a peer that is gone:
		// it would otherwise wedge whatever goroutine is sending.
		c.close()
		return false
	}
}

// outQueue is how many frames may wait for one peer. File chunks make this
// matter: the sender should be slowed by the socket, not by a queue that
// grows until the machine notices.
const outQueue = 64

// accept handles an inbound connection.
func (n *Node) accept(ctx context.Context, raw net.Conn) {
	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	self := n.Store.Self()
	port := 0
	if _, ps, err := net.SplitHostPort(n.Listen()); err == nil {
		port, _ = strconv.Atoi(ps)
	}
	if err := WriteFrame(raw, helloFrame(self, port, n.host)); err != nil {
		_ = raw.Close()
		return
	}
	f, err := ReadFrame(raw)
	if err != nil {
		_ = raw.Close()
		return
	}
	if err := validHello(f); err != nil {
		n.logf("refused %s: %v", raw.RemoteAddr(), err)
		_ = raw.Close()
		return
	}
	if f.From == self.ID {
		// Our own announcement led us back to ourselves.
		_ = raw.Close()
		return
	}
	if n.Store.Blocked(f.From) {
		n.logf("refused blocked peer %s", ShortID(f.From))
		_ = raw.Close()
		return
	}
	_ = raw.SetDeadline(time.Time{})

	// A peer that connects without ever having been announced is a peer we
	// know nothing about. It is not refused — that is how a chat app on a
	// network with broken multicast still works — but it is marked, and
	// the UI says so.
	_, known := n.Store.Peer(f.From)
	p := peerFromHello(f, raw.RemoteAddr().String(), known)
	if n.Store.PutPeer(p) {
		n.emit(NodeEvent{Kind: EventPeers, Peer: f.From})
	}
	n.register(ctx, &peerConn{
		peer: f.From, conn: raw, out: make(chan Frame, outQueue),
		outbound: false, closed: make(chan struct{}),
	})
}

// dial opens a connection to a peer, unless one is already open or another
// goroutine is opening one.
func (n *Node) dial(ctx context.Context, id PeerID) (*peerConn, error) {
	n.mu.Lock()
	if c, ok := n.conns[id]; ok {
		n.mu.Unlock()
		return c, nil
	}
	if n.dialing[id] {
		n.mu.Unlock()
		return nil, errDialInProgress
	}
	n.dialing[id] = true
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.dialing, id)
		n.mu.Unlock()
	}()

	peer, ok := n.Store.Peer(id)
	if !ok || peer.Addr == "" {
		return nil, fmt.Errorf("chat: no address for %s", ShortID(id))
	}
	if peer.Blocked {
		return nil, fmt.Errorf("chat: %s is blocked", ShortID(id))
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", peer.Addr)
	if err != nil {
		return nil, err
	}
	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	self := n.Store.Self()
	port := 0
	if _, ps, err := net.SplitHostPort(n.Listen()); err == nil {
		port, _ = strconv.Atoi(ps)
	}
	if err := WriteFrame(raw, helloFrame(self, port, n.host)); err != nil {
		_ = raw.Close()
		return nil, err
	}
	f, err := ReadFrame(raw)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	if err := validHello(f); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if f.From != id {
		// The address we had belongs to somebody else now — a DHCP lease
		// that moved. Filing the conversation under the wrong peer would
		// be worse than failing.
		_ = raw.Close()
		return nil, fmt.Errorf("chat: %s answered as %s", peer.Addr, ShortID(f.From))
	}
	_ = raw.SetDeadline(time.Time{})
	n.Store.PutPeer(peerFromHello(f, raw.RemoteAddr().String(), true))

	c := &peerConn{
		peer: id, conn: raw, out: make(chan Frame, outQueue),
		outbound: true, closed: make(chan struct{}),
	}
	n.register(ctx, c)
	return c, nil
}

var errDialInProgress = errors.New("chat: already dialling that peer")

// register installs a connection, resolving the race in which both peers
// dialled each other at the same moment.
//
// The rule has to be one both sides compute the same way from facts both
// sides have, and the only such fact is the pair of peer IDs: keep the
// connection whose dialler is the numerically smaller ID, drop the other.
// Whichever side evaluates it first, both reach the same answer, so the
// pair ends up with exactly one connection rather than zero (both dropped)
// or two (both kept).
func (n *Node) register(ctx context.Context, c *peerConn) {
	self := n.Store.Self().ID
	n.mu.Lock()
	if old, ok := n.conns[c.peer]; ok {
		keepOld := dialerOf(old, self) == minID(self, c.peer)
		if keepOld {
			n.mu.Unlock()
			c.close()
			return
		}
		delete(n.conns, c.peer)
		old.close()
	}
	n.conns[c.peer] = c
	n.mu.Unlock()

	n.emit(NodeEvent{Kind: EventPeers, Peer: c.peer})

	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.writeLoop(c)
	}()
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.readLoop(ctx, c)
	}()
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.flushQueue(ctx, c.peer)
	}()
}

func dialerOf(c *peerConn, self PeerID) PeerID {
	if c.outbound {
		return self
	}
	return c.peer
}

func minID(a, b PeerID) PeerID {
	if a < b {
		return a
	}
	return b
}

func (n *Node) conn(id PeerID) (*peerConn, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	c, ok := n.conns[id]
	return c, ok
}

func (n *Node) dropConn(id PeerID, why string) {
	n.mu.Lock()
	c, ok := n.conns[id]
	if ok {
		delete(n.conns, id)
	}
	n.mu.Unlock()
	if ok {
		n.logf("dropped %s: %s", ShortID(id), why)
		c.close()
	}
}

func (n *Node) closeAllConns() {
	n.mu.Lock()
	conns := n.conns
	n.conns = map[PeerID]*peerConn{}
	n.mu.Unlock()
	for _, c := range conns {
		// A goodbye costs one frame and saves the other side forty
		// seconds of showing us as present.
		select {
		case c.out <- Frame{Type: FrameBye}:
			time.Sleep(20 * time.Millisecond)
		default:
		}
		c.close()
	}
}

// Online is every peer with a live connection.
func (n *Node) Online() []PeerID {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]PeerID, 0, len(n.conns))
	for id := range n.conns {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (n *Node) writeLoop(c *peerConn) {
	defer c.close()
	for {
		select {
		case <-c.closed:
			return
		case f := <-c.out:
			_ = c.conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if err := WriteFrame(c.conn, f); err != nil {
				return
			}
		}
	}
}

func (n *Node) readLoop(ctx context.Context, c *peerConn) {
	defer func() {
		c.close()
		n.mu.Lock()
		if n.conns[c.peer] == c {
			delete(n.conns, c.peer)
		}
		n.mu.Unlock()
		n.failRunningTransfers(c.peer)
		n.clearTyping(c.peer)
		n.emit(NodeEvent{Kind: EventPeers, Peer: c.peer})
	}()
	for {
		// A peer that says nothing at all for well over a beacon interval
		// is gone, whatever TCP still believes.
		_ = c.conn.SetReadDeadline(time.Now().Add(3 * BeaconExpiry))
		f, err := ReadFrame(c.conn)
		if err != nil {
			return
		}
		if f.Type == FrameBye {
			n.Store.SetPresence(c.peer, PresenceOffline)
			return
		}
		if err := n.handle(ctx, c, f); err != nil {
			n.logf("%s: %v", ShortID(c.peer), err)
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// ---------------------------------------------------------------- inbound

func (n *Node) handle(ctx context.Context, c *peerConn, f Frame) error {
	switch f.Type {
	case FrameMsg:
		return n.onMessage(c, f)
	case FrameAck:
		if m, ok := n.Store.SetState(f.ID, StateDelivered); ok {
			n.emit(NodeEvent{Kind: EventState, Conv: m.Conv, Msg: &m})
		}
	case FrameTyping:
		n.onTyping(c.peer, f)
	case FramePresence:
		p := peerFromHello(f, "", true)
		p.Addr = "" // presence never moves a peer's address
		p.Presence = f.Presence.Valid()
		if n.Store.PutPeer(p) {
			n.emit(NodeEvent{Kind: EventPeers, Peer: c.peer})
		}
	case FrameOffer:
		return n.onOffer(c, f)
	case FrameAccept:
		n.onAccept(ctx, c, f)
	case FrameDecline:
		n.onDecline(c, f)
	case FrameChunk:
		return n.onChunk(c, f)
	case FrameDone:
		return n.onDone(c, f)
	default:
		// Forward compatibility: a newer peer may send frames this build
		// has never heard of, and the conversation carries on regardless.
	}
	return nil
}

func (n *Node) onMessage(c *peerConn, f Frame) error {
	peer, _ := n.Store.Peer(c.peer)
	m, ok := messageFromFrame(f, c.peer, peer.Nick)
	if !ok {
		return fmt.Errorf("malformed message frame")
	}
	if m.Conv.IsRoom() && !n.inRoom(m.Conv.Room()) {
		// A room we are not in. Acknowledge it so the sender does not
		// retry for ever, but do not file it.
		c.send(Frame{Type: FrameAck, ID: f.ID})
		return nil
	}
	added, err := n.Store.Append(m)
	if err != nil {
		return err
	}
	// The ack goes out whether or not the message was new: a resend
	// happens precisely because the first ack was lost.
	c.send(Frame{Type: FrameAck, ID: f.ID})
	if !added {
		return nil
	}
	n.clearTypingConv(c.peer, m.Conv)
	n.emit(NodeEvent{Kind: EventMessage, Conv: m.Conv, Peer: c.peer, Msg: &m})
	n.maybeNotify(m, peer)
	return nil
}

func (n *Node) onTyping(id PeerID, f Frame) {
	conv := f.Conv
	if conv == "" || (!conv.IsRoom() && conv != DirectConv(id)) {
		conv = DirectConv(id)
	}
	n.mu.Lock()
	if n.typing[id] == nil {
		n.typing[id] = map[ConversationID]time.Time{}
	}
	if f.Typing {
		n.typing[id][conv] = time.Now()
	} else {
		delete(n.typing[id], conv)
	}
	n.mu.Unlock()
	n.emit(NodeEvent{Kind: EventTyping, Conv: conv, Peer: id, Typing: f.Typing})
}

// Typing is every peer currently typing in a conversation.
func (n *Node) Typing(conv ConversationID) []PeerID {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []PeerID
	cutoff := time.Now().Add(-typingTimeout)
	for id, convs := range n.typing {
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

func (n *Node) expireTyping() {
	cutoff := time.Now().Add(-typingTimeout)
	type gone struct {
		id   PeerID
		conv ConversationID
	}
	var expired []gone
	n.mu.Lock()
	for id, convs := range n.typing {
		for conv, t := range convs {
			if t.Before(cutoff) {
				delete(convs, conv)
				expired = append(expired, gone{id, conv})
			}
		}
	}
	n.mu.Unlock()
	for _, g := range expired {
		n.emit(NodeEvent{Kind: EventTyping, Conv: g.conv, Peer: g.id, Typing: false})
	}
}

func (n *Node) clearTyping(id PeerID) {
	n.mu.Lock()
	convs := n.typing[id]
	delete(n.typing, id)
	n.mu.Unlock()
	for conv := range convs {
		n.emit(NodeEvent{Kind: EventTyping, Conv: conv, Peer: id, Typing: false})
	}
}

func (n *Node) clearTypingConv(id PeerID, conv ConversationID) {
	n.mu.Lock()
	had := false
	if convs := n.typing[id]; convs != nil {
		_, had = convs[conv]
		delete(convs, conv)
	}
	n.mu.Unlock()
	if had {
		n.emit(NodeEvent{Kind: EventTyping, Conv: conv, Peer: id, Typing: false})
	}
}

func (n *Node) inRoom(room string) bool {
	room = CanonRoom(room)
	for _, r := range n.Store.Self().Rooms {
		if r == room {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- outbound

// Send composes and delivers a message. It returns as soon as the message
// is stored: delivery is reported afterwards as a state change, because a
// peer that is asleep must not make the composer hang.
func (n *Node) Send(ctx context.Context, conv ConversationID, body string) (Message, error) {
	body = strings.TrimRight(body, " \t\r\n")
	if strings.TrimSpace(body) == "" {
		return Message{}, fmt.Errorf("chat: nothing to send")
	}
	self := n.Store.Self()
	m := Message{
		ID: n.newMessageID(self.ID), Conv: conv, From: self.ID, FromNick: self.Nick,
		Body: clipRunes(body, maxBodyRunes), Sent: time.Now(), Seq: n.Store.NextSeq(),
		Mine: true, Read: true, State: StateQueued,
	}
	if _, err := n.Store.Append(m); err != nil {
		return Message{}, err
	}
	n.emit(NodeEvent{Kind: EventMessage, Conv: conv, Msg: &m})

	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.deliver(ctx, m)
	}()
	return m, nil
}

// newMessageID is "<peer id>-<seq>-<random>": unique without anybody
// coordinating, and traceable back to the sender when reading a log.
func (n *Node) newMessageID(self PeerID) MessageID {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("chat: crypto/rand: " + err.Error())
	}
	return MessageID(fmt.Sprintf("%s-%d-%s", ShortID(self), time.Now().UnixNano(), hex.EncodeToString(b[:])))
}

// deliver puts one message on the wire, to one peer or to a room.
func (n *Node) deliver(ctx context.Context, m Message) {
	if m.Conv.IsRoom() {
		n.deliverRoom(ctx, m)
		return
	}
	peer := PeerID(strings.TrimPrefix(string(m.Conv), "peer:"))
	n.setState(m.ID, StateSending)
	if err := n.sendTo(ctx, peer, msgFrame(m)); err != nil {
		// Back to queued, not failed: the peer is asleep, not gone, and
		// the message goes out by itself when it comes back.
		n.setState(m.ID, StateQueued)
		n.logf("queued %s for %s: %v", m.ID, ShortID(peer), err)
		return
	}
	// The delivered state is set when the ack arrives, not here.
}

// deliverRoom fans a room message out to every peer that advertises the
// room. A room has no history sync: a peer that is away when the message
// is sent does not get it later. That is stated in docs/protocol.md, and
// it is the price of having no server.
func (n *Node) deliverRoom(ctx context.Context, m Message) {
	room := m.Conv.Room()
	sent := 0
	for _, p := range n.Store.Peers() {
		if p.Blocked || !hasRoom(p.Rooms, room) {
			continue
		}
		if err := n.sendTo(ctx, p.ID, msgFrame(m)); err == nil {
			sent++
		}
	}
	if sent == 0 {
		n.setState(m.ID, StateFailed)
		return
	}
	// One ack is enough to call a room message delivered; the others
	// arrive and are ignored.
	n.setState(m.ID, StateSending)
}

func hasRoom(rooms []string, room string) bool {
	for _, r := range rooms {
		if r == room {
			return true
		}
	}
	return false
}

func (n *Node) setState(id MessageID, st MessageState) {
	if m, ok := n.Store.SetState(id, st); ok {
		n.emit(NodeEvent{Kind: EventState, Conv: m.Conv, Msg: &m})
	}
}

// sendTo delivers one frame to one peer, dialling if need be.
func (n *Node) sendTo(ctx context.Context, id PeerID, f Frame) error {
	c, ok := n.conn(id)
	if !ok {
		var err error
		c, err = n.dial(ctx, id)
		if err != nil {
			return err
		}
	}
	if !c.send(f) {
		return fmt.Errorf("chat: %s: connection closed", ShortID(id))
	}
	return nil
}

func (n *Node) broadcastFrame(f Frame) {
	n.mu.Lock()
	conns := make([]*peerConn, 0, len(n.conns))
	for _, c := range n.conns {
		conns = append(conns, c)
	}
	n.mu.Unlock()
	for _, c := range conns {
		c.send(f)
	}
}

// SetTyping tells the other side of a conversation that we are composing.
// It is best effort and never queued: a typing indicator that arrives late
// is worse than one that never arrives.
func (n *Node) SetTyping(conv ConversationID, on bool) {
	f := Frame{Type: FrameTyping, Conv: conv, Typing: on, From: n.Store.Self().ID}
	if conv.IsRoom() {
		room := conv.Room()
		for _, p := range n.Store.Peers() {
			if p.Presence == PresenceOffline || p.Blocked || !hasRoom(p.Rooms, room) {
				continue
			}
			if c, ok := n.conn(p.ID); ok {
				c.send(f)
			}
		}
		return
	}
	if c, ok := n.conn(PeerID(strings.TrimPrefix(string(conv), "peer:"))); ok {
		c.send(f)
	}
}

// flushQueue resends everything still waiting for one peer, oldest first.
func (n *Node) flushQueue(ctx context.Context, id PeerID) {
	conv := DirectConv(id)
	for _, m := range n.Store.Queued(conv) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n.setState(m.ID, StateSending)
		if err := n.sendTo(ctx, id, msgFrame(m)); err != nil {
			n.setState(m.ID, StateQueued)
			return
		}
	}
}

// ------------------------------------------------------------ notification

func (n *Node) maybeNotify(m Message, from Peer) {
	prefs := n.Store.NotifyPrefs()
	if !prefs.Enabled || m.Mine || m.System {
		return
	}
	mention := prefs.Mentions && mentions(m.Body, n.Store.Self().Nick)
	if m.Conv.IsRoom() && prefs.DirectOnly && !mention {
		return
	}
	title := from.DisplayName()
	if m.Conv.IsRoom() {
		title = from.DisplayName() + " in #" + m.Conv.Room()
	}
	body := clipRunes(strings.TrimSpace(m.Body), 160)
	n.emit(NodeEvent{Kind: EventNotify, Conv: m.Conv, Peer: m.From, Title: title, Body: body})
	notifyDesktop(title, body)
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

// localAddress is the address this machine is most likely reachable at, for
// display in status.get. Peers learn the real address from the packet they
// receive, so this is never used for routing.
func localAddress() string {
	c, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1: no packet is sent
	if err != nil {
		return "0.0.0.0"
	}
	defer func() { _ = c.Close() }()
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return ua.IP.String()
	}
	return "0.0.0.0"
}
