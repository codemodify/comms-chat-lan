package chatclientd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
)

// RPC is clientd's front door: one [Daemon], many local front ends on a
// Unix socket. It holds no state of its own beyond the set of
// connections, so a front end that reconnects sees exactly what one that
// never disconnected sees.
//
// Every method is answered from the daemon's cache. Nothing here waits on
// the server, which is what lets a window open and a conversation scroll
// while the LAN is having a bad afternoon.
type RPC struct {
	Daemon *Daemon
	Socket string
	// Version is reported by status.get.
	Version string

	mu      sync.Mutex
	conns   map[*rpcConn]struct{}
	serving bool
	// closed is set once the daemon is shutting down. A connection
	// accepted in the moment before that has to be dropped too — see
	// handleConn.
	closed bool
}

type rpcConn struct {
	net.Conn
	mu sync.Mutex
	w  *bufio.Writer
}

func (c *rpcConn) send(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return chat.WriteJSON(c.w, v)
}

// sendTimeout writes an event with a deadline and drops the client when it
// stops reading. A wedged front end must not be able to stall the fan-out
// to the others.
func (c *rpcConn) sendTimeout(v any, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.Conn.SetWriteDeadline(time.Now().Add(d))
	defer func() { _ = c.Conn.SetWriteDeadline(time.Time{}) }()
	if err := chat.WriteJSON(c.w, v); err != nil {
		_ = c.Conn.Close()
		return err
	}
	return nil
}

// NewRPC builds the socket server over a client daemon.
func NewRPC(d *Daemon, socket string) *RPC {
	if socket == "" {
		socket = chat.DefaultSocket()
	}
	return &RPC{Daemon: d, Socket: socket, Version: Version, conns: map[*rpcConn]struct{}{}}
}

// ListenAndServe binds the Unix socket, starts the client daemon and
// serves until ctx is cancelled. It is the whole of
// comms-chat-lan-clientd's main.
func ListenAndServe(ctx context.Context, socket string, d *Daemon) error {
	ln, lock, err := listenSocket(socket)
	if err != nil {
		return err
	}
	defer lock.release()
	defer func() { _ = os.Remove(socket + ".lock") }()

	srv := NewRPC(d, socket)
	d.OnEvent(srv.broadcast)

	daemonDone := make(chan error, 1)
	go func() { daemonDone <- d.Run(ctx) }()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	err = srv.Serve(ln)
	// The listener is closed, so no new front end can arrive; the ones
	// already connected are dropped too. Leaving them attached to a
	// daemon that is shutting down would leave each one waiting out its
	// call timeout instead of noticing at once and redialling.
	srv.closeClients()
	<-daemonDone
	return err
}

// Serve accepts local front ends on ln.
func (s *RPC) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.serving = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.serving = false
		s.mu.Unlock()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if isClosed(err) || !s.isServing() {
				return nil
			}
			return err
		}
		go s.handleConn(c)
	}
}

func (s *RPC) isServing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serving
}

func isClosed(err error) bool {
	if err == nil {
		return false
	}
	return err == net.ErrClosed || strings.Contains(err.Error(), "use of closed network connection")
}

// closeClients drops every connected front end and refuses any that are
// still being set up.
func (s *RPC) closeClients() {
	s.mu.Lock()
	s.closed = true
	conns := s.conns
	s.conns = map[*rpcConn]struct{}{}
	s.mu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
}

// Clients is how many front ends are connected, for status.get.
func (s *RPC) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *RPC) handleConn(raw net.Conn) {
	if ok, err := peerAllowed(raw); err != nil || !ok {
		// Another local user must not be able to read the history or send
		// messages as us.
		_ = raw.Close()
		return
	}
	c := &rpcConn{Conn: raw, w: bufio.NewWriter(raw)}
	s.mu.Lock()
	// Accept and registration are not one step: the listener hands the
	// connection to this goroutine, which may not be scheduled before the
	// daemon is asked to stop. A connection that registered after
	// closeClients ran would be left attached to a daemon that is gone,
	// and the front end would sit there believing it was connected.
	if s.closed {
		s.mu.Unlock()
		_ = raw.Close()
		return
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.Close()
	}()

	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 64*1024), chat.MaxRPCLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req chat.Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = c.send(chat.Response{JSONRPC: chat.RPCVersion, Error: &chat.RPCError{Code: chat.ErrParse, Message: err.Error()}})
			continue
		}
		resp := s.dispatch(req)
		if req.ID == nil {
			continue // a notification from the front end: no reply
		}
		resp.JSONRPC = chat.RPCVersion
		resp.ID = req.ID
		if err := c.send(resp); err != nil {
			return
		}
	}
}

// broadcast pushes one event to every connected front end. It is the
// daemon's [Daemon.OnEvent] sink, so it is called from the daemon's
// goroutines and must not block on a front end that has stopped reading.
func (s *RPC) broadcast(ev chat.Event) {
	req := chat.Request{JSONRPC: chat.RPCVersion, Method: ev.Kind}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	req.Params = raw

	s.mu.Lock()
	conns := make([]*rpcConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.sendTimeout(req, 5*time.Second)
	}
}

func result(v any) chat.Response {
	b, err := json.Marshal(v)
	if err != nil {
		return fail(chat.ErrInternal, err.Error())
	}
	return chat.Response{Result: b}
}

func fail(code int, msg string) chat.Response {
	return chat.Response{Error: &chat.RPCError{Code: code, Message: msg}}
}

func failErr(err error) chat.Response { return fail(chat.ErrApp, err.Error()) }

// dispatch runs one method. Every method is here, in one switch, on
// purpose: the protocol is small enough to read in one sitting and a
// reviewer can see the whole surface a front end may reach.
func (s *RPC) dispatch(req chat.Request) chat.Response {
	d, store := s.Daemon, s.Daemon.Store

	switch req.Method {
	case chat.MethodPing:
		return result(map[string]string{"pong": s.Version})

	case chat.MethodStatusGet:
		self := store.Self()
		peers := store.Peers()
		online := 0
		for _, p := range peers {
			if p.Presence != chat.PresenceOffline {
				online++
			}
		}
		server := d.ServerAddr()
		if server == "" {
			server = "not found yet"
		} else if name := d.ServerName(); name != "" {
			server = name + " (" + server + ")"
		}
		return result(chat.DaemonStatus{
			Version: s.Version, Socket: s.Socket, Self: self.Peer(),
			Server: server, Connected: d.Connected(), Discovery: d.Discovery(),
			Cursor: d.Cursor(), Peers: len(peers), Online: online,
			Started: d.Started(), DataDir: store.Dir(), Clients: s.Clients(),
		})

	case chat.MethodIdentityGet:
		return result(store.Self())

	case chat.MethodIdentitySet:
		id, err := chat.DecodeParams[chat.Identity](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		out, err := store.SetSelf(id)
		if err != nil {
			return failErr(err)
		}
		d.Readvertise()
		s.broadcast(chat.Event{Kind: chat.EventPeers})
		return result(out)

	case chat.MethodPresenceSet:
		p, err := chat.DecodeParams[chat.PresenceParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		out, err := d.SetPresence(p.Presence)
		if err != nil {
			return failErr(err)
		}
		s.broadcast(chat.Event{Kind: chat.EventPeers})
		return result(out)

	case chat.MethodPeersList:
		return result(store.Peers())

	case chat.MethodPeersBlock:
		p, err := chat.DecodeParams[chat.BlockParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		if !store.BlockPeer(p.ID, p.Blocked) {
			return fail(chat.ErrApp, "no such peer")
		}
		s.broadcast(chat.Event{Kind: chat.EventPeers, Peer: p.ID})
		return result(chat.OKResult{OK: true})

	case chat.MethodConvList:
		return result(store.Conversations())

	case chat.MethodConvGet:
		p, err := chat.DecodeParams[chat.ConvParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		conv, ok := store.Conversation(p.Conv)
		if !ok {
			return fail(chat.ErrApp, "no such conversation")
		}
		view := chat.ConvView{Conv: conv, Typing: d.Typing(p.Conv)}
		if conv.Room != "" {
			view.Peers = store.RoomMembers(conv.Room)
		} else if peer, ok := store.Peer(conv.Peer); ok {
			view.Peers = []chat.Peer{peer}
		}
		return result(view)

	case chat.MethodMessagesList:
		p, err := chat.DecodeParams[chat.ListParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		return result(store.Messages(p.Conv, p.Limit))

	case chat.MethodMessagesSend:
		p, err := chat.DecodeParams[chat.SendParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		m, err := d.Send(p.Conv, p.Body)
		if err != nil {
			return failErr(err)
		}
		return result(m)

	case chat.MethodMessagesRead:
		p, err := chat.DecodeParams[chat.ConvParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		cleared := store.MarkRead(p.Conv)
		if cleared > 0 {
			// Every front end updates its unread badge, not just the one
			// whose window the messages were read in.
			s.broadcast(chat.Event{Kind: chat.EventPeers, Conv: p.Conv})
		}
		return result(chat.CountResult{Count: cleared})

	case chat.MethodTypingSet:
		p, err := chat.DecodeParams[chat.TypingParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		d.SetTyping(p.Conv, p.Typing)
		return result(chat.OKResult{OK: true})

	case chat.MethodRoomsList:
		return result(store.Rooms())

	case chat.MethodRoomsJoin:
		p, err := chat.DecodeParams[chat.RoomParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		room, changed, err := d.JoinRoom(p.Room)
		if err != nil {
			return failErr(err)
		}
		if changed {
			s.broadcast(chat.Event{Kind: chat.EventPeers, Conv: chat.RoomConv(room)})
		}
		return result(chat.OKResult{OK: changed, Room: room})

	case chat.MethodRoomsLeave:
		p, err := chat.DecodeParams[chat.RoomParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		left, err := d.LeaveRoom(p.Room)
		if err != nil {
			return failErr(err)
		}
		if left {
			s.broadcast(chat.Event{Kind: chat.EventPeers, Conv: chat.RoomConv(p.Room)})
		}
		return result(chat.OKResult{OK: left, Room: chat.CanonRoom(p.Room)})

	case chat.MethodFilesOffer:
		p, err := chat.DecodeParams[chat.OfferParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		tr, err := d.Offer(p.Conv, p.Path)
		if err != nil {
			return failErr(err)
		}
		return result(tr)

	case chat.MethodFilesAccept:
		p, err := chat.DecodeParams[chat.TransferParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		if err := d.Accept(p.Transfer); err != nil {
			return failErr(err)
		}
		return result(chat.OKResult{OK: true})

	case chat.MethodFilesDecline:
		p, err := chat.DecodeParams[chat.TransferParams](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		if err := d.Decline(p.Transfer, p.Reason); err != nil {
			return failErr(err)
		}
		return result(chat.OKResult{OK: true})

	case chat.MethodFilesList:
		return result(d.Transfers())

	case chat.MethodNotifyGet:
		return result(store.NotifyPrefs())

	case chat.MethodNotifySet:
		p, err := chat.DecodeParams[chat.NotifyPrefs](req.Params)
		if err != nil {
			return fail(chat.ErrBadParams, err.Error())
		}
		out, err := store.SetNotifyPrefs(p)
		if err != nil {
			return failErr(err)
		}
		return result(out)

	default:
		return fail(chat.ErrNoMethod, fmt.Sprintf("chat: no method %q", req.Method))
	}
}
