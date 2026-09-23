// Package chatserver is comms-chat-lan-server: the one machine on the LAN
// that everybody's client daemon connects to.
//
// It is the authority. It enrols whoever asks, it decides the order of
// every message, it stores every conversation, and it relays what it
// stores to whoever is entitled to see it — now, if they are connected,
// and when they come back, if they are not. Nothing in this package knows
// what a window is; nothing in it links a user interface.
//
// The two things a serverless version of this application could not do
// fall out of that one decision: a message to somebody who is away is
// simply a message the server has and they have not yet asked for, and
// the history a late joiner missed is simply the part of a room's history
// their cursor is behind.
package chatserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatwire"
)

// Version is what the server prints and what it tells clients. It is
// bumped by hand at a release.
const Version = "0.2.0"

// Backlog limits. A resume sends what the client missed; these are what
// happens when "what it missed" is unreasonable.
const (
	// BacklogWindow is how much of a conversation a client gets when its
	// cursor cannot be used: the most recent messages of everything it
	// can see. It is also what a late joiner gets of a room.
	BacklogWindow = 200
	// MaxBacklog is the most messages one resume will replay from a
	// cursor. Past it, the cursor is abandoned and the client is sent the
	// window instead: a client that has been away for a month wants the
	// recent conversation, not a month of replay before it can type.
	MaxBacklog = 2000
)

// OfferTimeout is how long an offered file waits for an answer before the
// server cancels it and tells both ends. Without it, an offer to somebody
// who has walked away is a row in two front ends for ever, and a sender
// left believing something is about to happen.
const OfferTimeout = 2 * time.Minute

// idleTimeout drops a connection that has said nothing at all, not even a
// pong, for this long.
const idleTimeout = 3 * chatwire.BeaconExpiry

// Server is the whole of comms-chat-lan-server: a TCP listener, the
// connections of the clients on it, and the [chat.Store] that is the
// authoritative copy of everything.
type Server struct {
	// Store holds the roster, the rooms and the history. It is the
	// authority, not a cache.
	Store *chat.Store
	// Disc announces this server on the LAN. Nil means [chatwire.NoDiscovery].
	Disc chatwire.Discovery
	// Port is the TCP port clients connect to. Zero means
	// [chatwire.DefaultServerPort]; a test passes -1 for any free port.
	Port int
	// Name is what the server calls itself in its announcement.
	Name string
	// Log receives one line for anything worth knowing about. Nil discards.
	Log func(string)

	mu sync.Mutex
	// conns is every live connection, keyed by the identity that enrolled
	// on it. Two clients may enrol under one identity — a desktop and a
	// laptop sharing an identity.json — so it is a list, and everything
	// that goes to a person goes to all of them.
	conns   map[chat.PeerID][]*clientConn
	xfers   map[chat.TransferID]*relay
	listen  string
	host    string
	note    string
	started time.Time
	wg      sync.WaitGroup
}

// New builds a server over a store.
func New(store *chat.Store, disc chatwire.Discovery) *Server {
	if disc == nil {
		disc = chatwire.NoDiscovery()
	}
	host, _ := os.Hostname()
	return &Server{
		Store: store, Disc: disc,
		conns: map[chat.PeerID][]*clientConn{},
		xfers: map[chat.TransferID]*relay{},
		host:  strings.TrimSuffix(host, ".local"),
	}
}

// ID is the server's identity, which is also the name of the ordering its
// sequence numbers belong to. It is the peer ID in the server's own
// identity.json: one fewer file to keep, and it is already 16 random
// bytes that survive a restart.
func (s *Server) ID() chatwire.ServerID {
	return chatwire.ServerID(s.Store.Self().ID)
}

// DisplayName is what the server announces itself as.
func (s *Server) DisplayName() string {
	if n := strings.TrimSpace(s.Name); n != "" {
		return chat.ClipRunes(n, 48)
	}
	return chat.ClipRunes(s.Store.Self().Nick, 48)
}

// Listen is the "host:port" clients connect to, once Run has started.
func (s *Server) Listen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listen
}

// Started is when Run began.
func (s *Server) Started() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// Clients is how many client daemons are connected right now.
func (s *Server) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, list := range s.conns {
		n += len(list)
	}
	return n
}

func (s *Server) logf(format string, args ...any) {
	s.mu.Lock()
	fn := s.Log
	s.mu.Unlock()
	if fn != nil {
		fn(fmt.Sprintf(format, args...))
	}
}

// Run binds the listener, starts announcing and serves until ctx is
// cancelled. It returns once every goroutine it started has stopped.
func (s *Server) Run(ctx context.Context) error {
	port := s.Port
	switch {
	case port < 0:
		port = 0 // any free port: what the tests ask for
	case port == 0:
		port = chatwire.DefaultServerPort
		if p, err := strconv.Atoi(strings.TrimSpace(os.Getenv(chat.EnvPort))); err == nil && p > 0 {
			port = p
		}
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return fmt.Errorf("chat: server listener: %w", err)
	}
	actual := ln.Addr().(*net.TCPAddr).Port

	s.mu.Lock()
	s.listen = net.JoinHostPort(localAddress(), strconv.Itoa(actual))
	s.started = time.Now()
	s.mu.Unlock()
	s.logf("listening for clients on %s", s.Listen())

	s.Disc.Update(chatwire.Announcement{
		Version: chatwire.ProtocolVersion, ID: s.ID(),
		Name: s.DisplayName(), Host: s.host, Port: actual,
	})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handleConn(ctx, c)
			}()
		}
	}()

	discDone := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(discDone)
		err := s.Disc.Run(ctx, chatwire.DiscoveryEvents{
			Note: func(msg string) {
				s.mu.Lock()
				s.note = msg
				s.mu.Unlock()
				s.logf("%s", msg)
			},
		})
		if err != nil {
			s.mu.Lock()
			s.note = err.Error()
			s.mu.Unlock()
			s.logf("announcement stopped: %v", err)
		}
	}()

	// Housekeeping: an offer nobody answered is cancelled, and every
	// connection is asked whether it is still there.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.expireOffers()
				s.pingAll()
			}
		}
	}()

	<-ctx.Done()
	_ = ln.Close()
	s.closeAll()
	<-discDone
	s.wg.Wait()
	return nil
}

// Note is the last discovery problem, for the logs. Empty means none.
func (s *Server) Note() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.note
}

// ----------------------------------------------------------- connections

// clientConn is one enrolled client daemon.
type clientConn struct {
	peer   chat.PeerID
	conn   net.Conn
	out    chan chatwire.Frame
	closed chan struct{}
	once   sync.Once
}

func (c *clientConn) close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

// send queues a frame. It reports false when the connection is going
// away, so the caller carries on with the others rather than blocking.
func (c *clientConn) send(f chatwire.Frame) bool {
	select {
	case c.out <- f:
		return true
	case <-c.closed:
		return false
	case <-time.After(5 * time.Second):
		// A client that is not reading is worse than one that is gone: it
		// would otherwise wedge whichever goroutine is relaying to it.
		c.close()
		return false
	}
}

// outQueue is how many frames may wait for one client. File chunks make
// this matter: a sender should be slowed by the socket, not by a queue
// that grows until the machine notices.
const outQueue = 64

func (s *Server) handleConn(ctx context.Context, raw net.Conn) {
	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	hello, err := chatwire.ReadFrame(raw)
	if err != nil {
		_ = raw.Close()
		return
	}
	if err := chatwire.ValidHello(hello); err != nil {
		s.logf("refused %s: %v", raw.RemoteAddr(), err)
		_ = chatwire.WriteFrame(raw, chatwire.ErrorFrame(chatwire.ErrBadFrame, err.Error()))
		_ = raw.Close()
		return
	}
	_ = raw.SetDeadline(time.Time{})

	// Open enrolment: whoever asks is in. The server records who they say
	// they are and refuses nobody. See docs/security.md, which says
	// exactly how much protection that is.
	peer := chatwire.PeerFromHello(hello, raw.RemoteAddr().String())
	s.Store.PutPeer(peer)
	s.setRooms(peer.ID, chat.CanonRooms(hello.Rooms))

	// The welcome goes out before anything else, written straight to the
	// socket rather than queued: it is the frame the client is waiting
	// for, and it must not arrive behind a roster update that this
	// connection's own arrival caused.
	cursor := s.Store.Seq()
	reset := hello.Cursor > cursor || (hello.Server != "" && hello.Server != s.ID())
	self, _ := s.Store.Peer(peer.ID)
	if err := chatwire.WriteFrame(raw, chatwire.Frame{
		Type: chatwire.FrameWelcome, Version: chatwire.ProtocolVersion,
		Server: s.ID(), ServerName: s.DisplayName(), Host: s.host,
		Cursor: cursor, Reset: reset, Now: time.Now(), Peer: &self,
	}); err != nil {
		_ = raw.Close()
		return
	}

	c := &clientConn{
		peer: peer.ID, conn: raw,
		out: make(chan chatwire.Frame, outQueue), closed: make(chan struct{}),
	}
	s.register(c)
	s.logf("enrolled %s (%s) from %s", chat.ShortID(peer.ID), peer.Nick, raw.RemoteAddr())

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.writeLoop(c)
	}()

	defer func() {
		s.unregister(c)
		c.close()
		s.failTransfersOf(c.peer, "the other end disconnected")
		if s.connsOf(c.peer) == 0 {
			s.Store.SetPresence(c.peer, chat.PresenceOffline)
			s.broadcastPeer(c.peer)
		}
	}()

	s.backfill(c, hello.Cursor, reset, cursor)
	s.readLoop(ctx, c)
}

func (s *Server) register(c *clientConn) {
	s.mu.Lock()
	s.conns[c.peer] = append(s.conns[c.peer], c)
	s.mu.Unlock()
	s.broadcastPeer(c.peer)
}

func (s *Server) unregister(c *clientConn) {
	s.mu.Lock()
	list := s.conns[c.peer]
	out := list[:0]
	for _, x := range list {
		if x != c {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		delete(s.conns, c.peer)
	} else {
		s.conns[c.peer] = out
	}
	s.mu.Unlock()
}

func (s *Server) connsOf(id chat.PeerID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns[id])
}

// to is every live connection of one identity.
func (s *Server) to(id chat.PeerID) []*clientConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*clientConn(nil), s.conns[id]...)
}

func (s *Server) everyConn() []*clientConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*clientConn
	for _, list := range s.conns {
		out = append(out, list...)
	}
	return out
}

func (s *Server) closeAll() {
	for _, c := range s.everyConn() {
		select {
		case c.out <- chatwire.Frame{Type: chatwire.FrameBye, Reason: "the server is stopping"}:
			time.Sleep(10 * time.Millisecond)
		default:
		}
		c.close()
	}
}

func (s *Server) pingAll() {
	for _, c := range s.everyConn() {
		c.send(chatwire.Frame{Type: chatwire.FramePing})
	}
}

func (s *Server) writeLoop(c *clientConn) {
	defer c.close()
	for {
		select {
		case <-c.closed:
			return
		case f := <-c.out:
			_ = c.conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if err := chatwire.WriteFrame(c.conn, f); err != nil {
				return
			}
		}
	}
}

func (s *Server) readLoop(ctx context.Context, c *clientConn) {
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(idleTimeout))
		f, err := chatwire.ReadFrame(c.conn)
		if err != nil {
			return
		}
		if f.Type == chatwire.FrameBye {
			return
		}
		s.handle(c, f)
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// ------------------------------------------------------------- enrolment

// backfill says everything the client missed, after the welcome.
//
// The cursor is a position in this server's sequence. A cursor ahead of
// where the server has got to did not come from here — a client that was
// talking to a different server, or to this one before it was restored
// from a backup — so it is abandoned rather than believed, and the client
// is sent a window of recent history instead. So is a client so far
// behind that replaying its delta would be worse for it than starting
// from the recent past.
//
// A message relayed in the moment this runs may be sent twice, once live
// and once from the backlog. That is deliberate: a duplicate costs
// nothing, because a message carries its own id and both ends file it
// once, and the alternative is a window in which a message is sent
// neither way.
func (s *Server) backfill(c *clientConn, from uint64, reset bool, cursor uint64) {
	var missed []chat.Message
	if !reset {
		missed = s.visibleSince(c.peer, from)
		if len(missed) > MaxBacklog {
			reset = true
		}
	}
	if reset {
		missed = s.recentWindow(c.peer)
	}

	c.send(chatwire.Frame{Type: chatwire.FrameRoster, Peers: s.roster()})
	for _, m := range missed {
		if conv, ok := convFor(m, c.peer, s.roomsOf(c.peer)); ok {
			f := chatwire.MsgFrame(m)
			f.Conv = conv
			c.send(f)
		}
	}
	c.send(chatwire.Frame{Type: chatwire.FrameSynced, Cursor: cursor})
}

// visibleSince is everything after cursor that this client may see.
func (s *Server) visibleSince(id chat.PeerID, cursor uint64) []chat.Message {
	rooms := s.roomsOf(id)
	var out []chat.Message
	for _, m := range s.Store.Since(cursor, 0) {
		if _, ok := convFor(m, id, rooms); ok {
			out = append(out, m)
		}
	}
	return out
}

// recentWindow is the tail of every conversation this client can see. It
// is what a client gets when its cursor is no use.
func (s *Server) recentWindow(id chat.PeerID) []chat.Message {
	rooms := s.roomsOf(id)
	var out []chat.Message
	for _, conv := range s.Store.Convs() {
		if !s.convVisible(conv, id, rooms) {
			continue
		}
		out = append(out, s.Store.Messages(conv, BacklogWindow)...)
	}
	sortBySeq(out)
	return out
}

func (s *Server) convVisible(conv chat.ConversationID, id chat.PeerID, rooms []string) bool {
	if conv.IsRoom() {
		return hasRoom(rooms, conv.Room())
	}
	a, b, ok := conv.DirectPair()
	return ok && (a == id || b == id)
}

// convFor is the name this recipient calls a stored message's
// conversation, and whether they may see it at all. A room is a room to
// everybody in it; a one-to-one conversation is stored under both ids and
// called "the conversation with the other one" by each end.
func convFor(m chat.Message, id chat.PeerID, rooms []string) (chat.ConversationID, bool) {
	if m.Conv.IsRoom() {
		if !hasRoom(rooms, m.Conv.Room()) {
			return "", false
		}
		return m.Conv, true
	}
	a, b, ok := m.Conv.DirectPair()
	if !ok {
		return "", false
	}
	switch id {
	case a:
		return chat.DirectConv(b), true
	case b:
		return chat.DirectConv(a), true
	}
	return "", false
}

func hasRoom(rooms []string, room string) bool {
	room = chat.CanonRoom(room)
	for _, r := range rooms {
		if r == room {
			return true
		}
	}
	return false
}

func (s *Server) roomsOf(id chat.PeerID) []string {
	p, _ := s.Store.Peer(id)
	return p.Rooms
}

func (s *Server) setRooms(id chat.PeerID, rooms []string) {
	// The client's own list wins on enrolment, including when it is
	// empty: room membership belongs to the person, and the server is
	// keeping it for them rather than deciding it.
	s.Store.PutPeer(chat.Peer{ID: id, Rooms: append([]string{}, rooms...)})
}

func (s *Server) roster() []chat.Peer {
	peers := s.Store.Peers()
	for i := range peers {
		if s.connsOf(peers[i].ID) == 0 {
			peers[i].Presence = chat.PresenceOffline
		}
	}
	return peers
}

func sortBySeq(list []chat.Message) {
	// Insertion into the store already ordered each conversation; this
	// merges the conversations into one stream by the only thing that
	// orders them across conversations, the sequence number.
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Seq < list[j-1].Seq; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// localAddress is the address this machine is most likely reachable at,
// for the log line and for a client told to connect by name. Clients
// learn the real address from the packet they receive, so this is never
// used for routing.
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
