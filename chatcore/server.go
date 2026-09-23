package chatcore

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
)

// Server is comms-chatd's front door: one [Node], one [Store], many local
// clients on a Unix socket. It holds no state of its own beyond the set of
// connections, so a front end that reconnects sees exactly what one that
// never disconnected sees.
type Server struct {
	Node   *Node
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
	return writeJSON(c.w, v)
}

// sendTimeout writes an event with a deadline and drops the client when it
// stops reading. A wedged front end must not be able to stall the daemon's
// fan-out to the others.
func (c *rpcConn) sendTimeout(v any, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.Conn.SetWriteDeadline(time.Now().Add(d))
	defer func() { _ = c.Conn.SetWriteDeadline(time.Time{}) }()
	if err := writeJSON(c.w, v); err != nil {
		_ = c.Conn.Close()
		return err
	}
	return nil
}

// maxRPCLine bounds one request. Nothing a front end sends is large — a
// file is named by path and read by the daemon, never shipped over the
// local socket — so this is generous.
const maxRPCLine = 4 * 1024 * 1024

// NewServer builds the RPC server over a node.
func NewServer(node *Node, socket string) *Server {
	if socket == "" {
		socket = DefaultSocket()
	}
	return &Server{Node: node, Socket: socket, Version: Version, conns: map[*rpcConn]struct{}{}}
}

// Version is what status.get reports. It is bumped by hand at a release.
const Version = "0.1.0"

// ListenAndServe binds the Unix socket, starts the node and serves until
// ctx is cancelled. It is the whole of comms-chatd's main.
func ListenAndServe(ctx context.Context, socket string, node *Node) error {
	ln, lock, err := listenSocket(socket)
	if err != nil {
		return err
	}
	defer lock.release()
	defer func() { _ = os.Remove(socket + ".lock") }()

	srv := NewServer(node, socket)
	node.OnEvent(srv.broadcast)

	nodeDone := make(chan error, 1)
	go func() { nodeDone <- node.Run(ctx) }()

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
	<-nodeDone
	return err
}

// Serve accepts local clients on ln.
func (s *Server) Serve(ln net.Listener) error {
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

func (s *Server) isServing() bool {
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
func (s *Server) closeClients() {
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
func (s *Server) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *Server) handleConn(raw net.Conn) {
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
	sc.Buffer(make([]byte, 0, 64*1024), maxRPCLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = c.send(Response{JSONRPC: RPCVersion, Error: &RPCError{Code: ErrParse, Message: err.Error()}})
			continue
		}
		resp := s.dispatch(req)
		if req.ID == nil {
			continue // a notification from the client: no reply
		}
		resp.JSONRPC = RPCVersion
		resp.ID = req.ID
		if err := c.send(resp); err != nil {
			return
		}
	}
}

// broadcast pushes one event to every connected front end. It is the
// node's [Node.OnEvent] sink, so it is called from the node's goroutines
// and must not block on a client that has stopped reading.
func (s *Server) broadcast(ev NodeEvent) {
	req := Request{JSONRPC: RPCVersion, Method: ev.Kind}
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

func writeJSON(w *bufio.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}

func result(v any) Response {
	b, err := json.Marshal(v)
	if err != nil {
		return fail(ErrInternal, err.Error())
	}
	return Response{Result: b}
}

func fail(code int, msg string) Response {
	return Response{Error: &RPCError{Code: code, Message: msg}}
}

func failErr(err error) Response { return fail(ErrApp, err.Error()) }

// dispatch runs one method. Every method is here, in one switch, on
// purpose: the protocol is small enough to read in one sitting and a
// reviewer can see the whole surface a front end may reach.
func (s *Server) dispatch(req Request) Response {
	n, store := s.Node, s.Node.Store
	ctx := context.Background()

	switch req.Method {
	case MethodPing:
		return result(map[string]string{"pong": s.Version})

	case MethodStatusGet:
		self := store.Self()
		peers := store.Peers()
		online := 0
		for _, p := range peers {
			if p.Presence != PresenceOffline {
				online++
			}
		}
		disc := n.Disc.Name()
		if note := n.Note(); note != "" {
			disc += " (" + note + ")"
		}
		return result(DaemonStatus{
			Version: s.Version, Socket: s.Socket, Self: self.Peer(),
			Listen: n.Listen(), Discovery: disc, Peers: len(peers), Online: online,
			Started: n.Started(), DataDir: store.Dir(), Clients: s.Clients(),
		})

	case MethodIdentityGet:
		return result(store.Self())

	case MethodIdentitySet:
		id, err := decodeParams[Identity](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		out, err := store.SetSelf(id)
		if err != nil {
			return failErr(err)
		}
		n.Readvertise()
		s.broadcast(NodeEvent{Kind: EventPeers})
		return result(out)

	case MethodPresenceSet:
		p, err := decodeParams[presenceParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		self := store.Self()
		self.Presence = p.Presence.Valid()
		out, err := store.SetSelf(self)
		if err != nil {
			return failErr(err)
		}
		n.Readvertise()
		s.broadcast(NodeEvent{Kind: EventPeers})
		return result(out)

	case MethodPeersList:
		return result(store.Peers())

	case MethodPeersBlock:
		p, err := decodeParams[blockParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		if !store.BlockPeer(p.ID, p.Blocked) {
			return fail(ErrApp, "no such peer")
		}
		if p.Blocked {
			n.dropConn(p.ID, "blocked")
		}
		s.broadcast(NodeEvent{Kind: EventPeers, Peer: p.ID})
		return result(okResult{OK: true})

	case MethodConvList:
		return result(store.Conversations())

	case MethodConvGet:
		p, err := decodeParams[convParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		conv, ok := store.Conversation(p.Conv)
		if !ok {
			return fail(ErrApp, "no such conversation")
		}
		view := ConvView{Conv: conv, Typing: n.Typing(p.Conv)}
		if conv.Room != "" {
			for _, peer := range store.Peers() {
				if hasRoom(peer.Rooms, conv.Room) {
					view.Peers = append(view.Peers, peer)
				}
			}
		} else if peer, ok := store.Peer(conv.Peer); ok {
			view.Peers = []Peer{peer}
		}
		return result(view)

	case MethodMessagesList:
		p, err := decodeParams[listParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		return result(store.Messages(p.Conv, p.Limit))

	case MethodMessagesSend:
		p, err := decodeParams[sendParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		m, err := n.Send(ctx, p.Conv, p.Body)
		if err != nil {
			return failErr(err)
		}
		return result(m)

	case MethodMessagesRead:
		p, err := decodeParams[convParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		cleared := store.MarkRead(p.Conv)
		if cleared > 0 {
			// Every front end updates its unread badge, not just the one
			// whose window the messages were read in.
			s.broadcast(NodeEvent{Kind: EventPeers, Conv: p.Conv})
		}
		return result(countResult{Count: cleared})

	case MethodTypingSet:
		p, err := decodeParams[typingParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		n.SetTyping(p.Conv, p.Typing)
		return result(okResult{OK: true})

	case MethodRoomsList:
		return result(store.Rooms())

	case MethodRoomsJoin:
		p, err := decodeParams[roomParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		room, changed, err := store.JoinRoom(p.Room)
		if err != nil {
			return failErr(err)
		}
		if changed {
			n.Readvertise()
			s.broadcast(NodeEvent{Kind: EventPeers, Conv: RoomConv(room)})
		}
		return result(okResult{OK: changed, Room: room})

	case MethodRoomsLeave:
		p, err := decodeParams[roomParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		left, err := store.LeaveRoom(p.Room)
		if err != nil {
			return failErr(err)
		}
		if left {
			n.Readvertise()
			s.broadcast(NodeEvent{Kind: EventPeers, Conv: RoomConv(p.Room)})
		}
		return result(okResult{OK: left, Room: CanonRoom(p.Room)})

	case MethodFilesOffer:
		p, err := decodeParams[offerParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		tr, err := n.Offer(ctx, p.Conv, p.Path)
		if err != nil {
			return failErr(err)
		}
		return result(tr)

	case MethodFilesAccept:
		p, err := decodeParams[transferParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		if err := n.Accept(p.Transfer); err != nil {
			return failErr(err)
		}
		return result(okResult{OK: true})

	case MethodFilesDecline:
		p, err := decodeParams[transferParams](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		if err := n.Decline(p.Transfer, p.Reason); err != nil {
			return failErr(err)
		}
		return result(okResult{OK: true})

	case MethodFilesList:
		return result(n.Transfers())

	case MethodNotifyGet:
		return result(store.NotifyPrefs())

	case MethodNotifySet:
		p, err := decodeParams[NotifyPrefs](req.Params)
		if err != nil {
			return fail(ErrBadParams, err.Error())
		}
		out, err := store.SetNotifyPrefs(p)
		if err != nil {
			return failErr(err)
		}
		return result(out)

	default:
		return fail(ErrNoMethod, fmt.Sprintf("chat: no method %q", req.Method))
	}
}
