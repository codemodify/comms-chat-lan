package chatcore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Client is the front ends' side of the daemon socket. Both the GUI and
// the TUI use it, and neither has any other way to reach the network: if
// something is not a method on this type, no front end can do it.
//
// A Client reconnects by itself. comms-chatd being restarted under a
// running UI is an ordinary event — an upgrade, a crash, a `systemctl
// --user restart` — and a front end that has to be restarted with it is a
// front end nobody leaves open.
type Client struct {
	Socket string

	mu       sync.Mutex
	conn     net.Conn
	w        *bufio.Writer
	pending  map[uint64]chan Response
	onEvent  func(NodeEvent)
	onState  func(bool)
	nextID   atomic.Uint64
	closed   atomic.Bool
	upNow    atomic.Bool
	redialOn bool
}

// Dial connects to comms-chatd.
func Dial(socket string) (*Client, error) {
	if socket == "" {
		socket = DefaultSocket()
	}
	c := &Client{Socket: socket, pending: map[uint64]chan Response{}}
	if err := c.connect(); err != nil {
		return nil, err
	}
	return c, nil
}

// DialWait retries until the daemon appears or wait elapses. It is what
// the front ends use at startup, so starting the daemon and the UI in the
// same breath works.
func DialWait(socket string, wait time.Duration) (*Client, error) {
	deadline := time.Now().Add(wait)
	var last error
	for {
		cli, err := Dial(socket)
		if err == nil {
			return cli, nil
		}
		last = err
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if last == nil {
		last = fmt.Errorf("chat: timeout dialling %s", socket)
	}
	return nil, last
}

func (c *Client) connect() error {
	conn, err := net.DialTimeout("unix", c.Socket, 2*time.Second)
	if err != nil {
		return fmt.Errorf("chat: dial %s: %w (is comms-chatd running?)", c.Socket, err)
	}
	c.mu.Lock()
	c.conn = conn
	c.w = bufio.NewWriter(conn)
	c.mu.Unlock()
	c.upNow.Store(true)
	go c.readLoop(conn)
	return nil
}

// Connected reports whether the socket is up right now.
func (c *Client) Connected() bool { return c.upNow.Load() && !c.closed.Load() }

// AutoReconnect makes the client redial for ever, in the background, when
// the daemon goes away. onState is called with false when the connection
// drops and true when it comes back, so a front end can say so in its
// status bar and reload what it is showing.
func (c *Client) AutoReconnect(onState func(up bool)) {
	c.mu.Lock()
	c.onState = onState
	if c.redialOn {
		c.mu.Unlock()
		return
	}
	c.redialOn = true
	c.mu.Unlock()
}

// Close drops the socket and stops reconnecting.
func (c *Client) Close() error {
	c.closed.Store(true)
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// OnEvent registers the handler for daemon events. It is called from the
// client's reader goroutine, so a GUI handler must hop to the UI thread.
func (c *Client) OnEvent(fn func(NodeEvent)) {
	c.mu.Lock()
	c.onEvent = fn
	c.mu.Unlock()
}

func (c *Client) readLoop(conn net.Conn) {
	defer c.dropped(conn)
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), maxRPCLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var peek struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &peek); err != nil {
			continue
		}
		if peek.Method != "" && len(peek.ID) == 0 {
			var req Request
			if err := json.Unmarshal(line, &req); err != nil {
				continue
			}
			var ev NodeEvent
			if len(req.Params) > 0 {
				_ = json.Unmarshal(req.Params, &ev)
			}
			ev.Kind = req.Method
			c.mu.Lock()
			fn := c.onEvent
			c.mu.Unlock()
			if fn != nil {
				fn(ev)
			}
			continue
		}
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		c.deliver(resp)
	}
}

// dropped releases every in-flight call and, if asked, starts redialling.
func (c *Client) dropped(conn net.Conn) {
	c.mu.Lock()
	if c.conn != conn {
		// Already replaced by a reconnect; nothing to do.
		c.mu.Unlock()
		return
	}
	pending := c.pending
	c.pending = map[uint64]chan Response{}
	onState, redial := c.onState, c.redialOn
	c.mu.Unlock()

	c.upNow.Store(false)
	for _, ch := range pending {
		select {
		case ch <- Response{Error: &RPCError{Code: ErrClosed, Message: "comms-chatd: the connection closed"}}:
		default:
		}
	}
	if c.closed.Load() {
		return
	}
	if onState != nil {
		onState(false)
	}
	if !redial {
		return
	}
	go c.redial()
}

// redial backs off from 100ms to 5s. A daemon restarted by hand is back
// within a second; one that is not coming back must not spin a core.
func (c *Client) redial() {
	wait := 100 * time.Millisecond
	for !c.closed.Load() {
		time.Sleep(wait)
		if wait < 5*time.Second {
			wait *= 2
		}
		if c.closed.Load() {
			return
		}
		if err := c.connect(); err != nil {
			continue
		}
		c.mu.Lock()
		onState := c.onState
		c.mu.Unlock()
		if onState != nil {
			onState(true)
		}
		return
	}
}

func (c *Client) deliver(resp Response) {
	id, ok := jsonNumber(resp.ID)
	if !ok {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- resp
	}
}

func jsonNumber(v any) (uint64, bool) {
	switch n := v.(type) {
	case float64:
		return uint64(n), true
	case json.Number:
		u, err := n.Int64()
		return uint64(u), err == nil
	case int:
		return uint64(n), true
	case int64:
		return uint64(n), true
	case uint64:
		return n, true
	default:
		return 0, false
	}
}

// callTimeout is how long one method may take. Everything in this protocol
// is local and fast: the daemon never blocks a reply on the network, so a
// slow call means something is wrong rather than something is busy.
func callTimeout(method string) time.Duration {
	switch method {
	case MethodFilesOffer:
		// The daemon hashes the file before it offers it, and that is
		// proportional to its size.
		return 5 * time.Minute
	default:
		return 20 * time.Second
	}
}

func (c *Client) call(method string, params any, out any) error {
	if c.closed.Load() {
		return fmt.Errorf("chat: the client is closed")
	}
	id := c.nextID.Add(1)
	req := Request{JSONRPC: RPCVersion, ID: id, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = raw
	}

	ch := make(chan Response, 1)
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return fmt.Errorf("chat: not connected to comms-chatd")
	}
	c.pending[id] = ch
	if err := writeJSON(c.w, req); err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	timer := time.NewTimer(callTimeout(method))
	defer timer.Stop()
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if out == nil || len(resp.Result) == 0 || string(resp.Result) == "null" {
			return nil
		}
		return json.Unmarshal(resp.Result, out)
	case <-timer.C:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("chat: timeout on %s", method)
	}
}

// ------------------------------------------------------------- the methods

// Ping checks the daemon is answering and returns its version.
func (c *Client) Ping() (string, error) {
	var out map[string]string
	if err := c.call(MethodPing, nil, &out); err != nil {
		return "", err
	}
	return out["pong"], nil
}

// Status is everything a front end needs for its status bar.
func (c *Client) Status() (DaemonStatus, error) {
	var out DaemonStatus
	err := c.call(MethodStatusGet, nil, &out)
	return out, err
}

// Identity is who we are on the LAN.
func (c *Client) Identity() (Identity, error) {
	var out Identity
	err := c.call(MethodIdentityGet, nil, &out)
	return out, err
}

// SetIdentity changes the nickname, colour, presence or auto-accept limit.
// The peer ID is not changeable and is ignored.
func (c *Client) SetIdentity(id Identity) (Identity, error) {
	var out Identity
	err := c.call(MethodIdentitySet, id, &out)
	return out, err
}

// SetPresence advertises online, away or busy.
func (c *Client) SetPresence(p Presence) (Identity, error) {
	var out Identity
	err := c.call(MethodPresenceSet, presenceParams{Presence: p}, &out)
	return out, err
}

// Peers is the roster.
func (c *Client) Peers() ([]Peer, error) {
	var out []Peer
	err := c.call(MethodPeersList, nil, &out)
	return out, err
}

// Block sets or clears a peer's block flag.
func (c *Client) Block(id PeerID, blocked bool) error {
	return c.call(MethodPeersBlock, blockParams{ID: id, Blocked: blocked}, nil)
}

// Conversations is the whole sidebar in one call.
func (c *Client) Conversations() ([]Conversation, error) {
	var out []Conversation
	err := c.call(MethodConvList, nil, &out)
	return out, err
}

// Conversation is one conversation with its members and who is typing.
func (c *Client) Conversation(id ConversationID) (ConvView, error) {
	var out ConvView
	err := c.call(MethodConvGet, convParams{Conv: id}, &out)
	return out, err
}

// Messages is the tail of a conversation, oldest first.
func (c *Client) Messages(conv ConversationID, limit int) ([]Message, error) {
	var out []Message
	err := c.call(MethodMessagesList, listParams{Conv: conv, Limit: limit}, &out)
	return out, err
}

// Send composes a message. It returns once the message is stored; delivery
// arrives later as a chat.state event.
func (c *Client) Send(conv ConversationID, body string) (Message, error) {
	var out Message
	err := c.call(MethodMessagesSend, sendParams{Conv: conv, Body: body}, &out)
	return out, err
}

// MarkRead clears a conversation's unread count.
func (c *Client) MarkRead(conv ConversationID) (int, error) {
	var out countResult
	err := c.call(MethodMessagesRead, convParams{Conv: conv}, &out)
	return out.Count, err
}

// SetTyping tells the other side we are composing.
func (c *Client) SetTyping(conv ConversationID, typing bool) error {
	return c.call(MethodTypingSet, typingParams{Conv: conv, Typing: typing}, nil)
}

// Rooms is every room we have joined.
func (c *Client) Rooms() ([]Conversation, error) {
	var out []Conversation
	err := c.call(MethodRoomsList, nil, &out)
	return out, err
}

// JoinRoom joins a room and returns its canonical name.
func (c *Client) JoinRoom(name string) (string, error) {
	var out okResult
	err := c.call(MethodRoomsJoin, roomParams{Room: name}, &out)
	return out.Room, err
}

// LeaveRoom leaves a room. Its history stays on disk.
func (c *Client) LeaveRoom(name string) error {
	return c.call(MethodRoomsLeave, roomParams{Room: name}, nil)
}

// OfferFile offers a local file to a peer. The path is opened by the
// daemon, which runs as the same user as the front end — the socket makes
// sure of that — so this is not a way to read somebody else's files.
func (c *Client) OfferFile(conv ConversationID, path string) (Transfer, error) {
	var out Transfer
	err := c.call(MethodFilesOffer, offerParams{Conv: conv, Path: path}, &out)
	return out, err
}

// AcceptFile accepts an incoming offer.
func (c *Client) AcceptFile(id TransferID) error {
	return c.call(MethodFilesAccept, transferParams{Transfer: id}, nil)
}

// DeclineFile refuses an offer or cancels one of ours.
func (c *Client) DeclineFile(id TransferID, reason string) error {
	return c.call(MethodFilesDecline, transferParams{Transfer: id, Reason: reason}, nil)
}

// Transfers is every transfer the daemon knows about.
func (c *Client) Transfers() ([]Transfer, error) {
	var out []Transfer
	err := c.call(MethodFilesList, nil, &out)
	return out, err
}

// NotifyPrefs is the shared notification setting.
func (c *Client) NotifyPrefs() (NotifyPrefs, error) {
	var out NotifyPrefs
	err := c.call(MethodNotifyGet, nil, &out)
	return out, err
}

// SetNotifyPrefs stores the notification setting for every front end.
func (c *Client) SetNotifyPrefs(p NotifyPrefs) (NotifyPrefs, error) {
	var out NotifyPrefs
	err := c.call(MethodNotifySet, p, &out)
	return out, err
}
