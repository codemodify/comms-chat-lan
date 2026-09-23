package chatclientd

import (
	"bufio"
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

// The link to the server: find one, enrol, resume, and keep it up.
//
// Everything in this file is written on the assumption that the server
// will go away — a restart, a reboot, a laptop closed on the wrong side
// of the building — because it will, and because the whole point of a
// client daemon is that the person does not have to care when it does.

// dialTimeout is how long one attempt to reach the server may take.
const dialTimeout = 5 * time.Second

// serverConn is the live connection, with the lock that keeps two
// goroutines from interleaving frames on it.
type serverConn struct {
	conn net.Conn
	mu   sync.Mutex
	w    *bufio.Writer
	done chan struct{}
	once sync.Once
}

func (c *serverConn) send(f chatwire.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	if err := chatwire.WriteFrame(c.w, f); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *serverConn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

// write sends one frame to the server if the link is up, and reports
// whether it went. A false is not an error anybody has to handle: the
// caller has already stored whatever it was, and the queue carries it.
func (d *Daemon) write(f chatwire.Frame) bool {
	d.mu.Lock()
	c := d.conn
	d.mu.Unlock()
	if c == nil {
		return false
	}
	if err := c.send(f); err != nil {
		d.logf("link: %v", err)
		c.close()
		return false
	}
	return true
}

// linkLoop keeps one session running for as long as it can, and tries
// again when it cannot. The backoff runs from 250ms to 10s and is reset
// by a beacon announcement, so a server that has just come up is reached
// in the moment it announces rather than at the end of a long sleep.
func (d *Daemon) linkLoop(ctx context.Context) {
	wait := 250 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		addr := d.serverTarget()
		if addr == "" {
			d.setNote("looking for a server")
		} else if err := d.session(ctx, addr); err != nil {
			d.logf("server %s: %v", addr, err)
			d.emit(chat.Event{Kind: chat.EventStatus, Text: "server: " + err.Error()})
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
			wait = 250 * time.Millisecond
		case <-time.After(wait):
			if wait < 10*time.Second {
				wait *= 2
			}
		}
	}
}

func (d *Daemon) setNote(s string) {
	d.mu.Lock()
	d.discNote = s
	d.mu.Unlock()
}

// serverTarget is where to try: what we were told, else what the
// environment says, else what the beacon last heard.
func (d *Daemon) serverTarget() string {
	if a := normalizeAddr(d.Addr); a != "" {
		return a
	}
	if a := normalizeAddr(os.Getenv(chat.EnvServer)); a != "" {
		return a
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.heard
}

// normalizeAddr accepts "host", "host:port" and an empty string. A bare
// host gets the default port: somebody typing -server kestrel should not
// have to know one.
func normalizeAddr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s
	}
	return net.JoinHostPort(s, strconv.Itoa(chatwire.DefaultServerPort))
}

// session runs one connection from dial to close. It returns when the
// link is gone, with the reason if there was one.
func (d *Daemon) session(ctx context.Context, addr string) error {
	dialer := net.Dialer{Timeout: dialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	c := &serverConn{conn: raw, w: bufio.NewWriter(raw), done: make(chan struct{})}
	defer c.close()

	// Enrol. Open enrolment means this cannot be refused for who we are,
	// only for speaking the wrong protocol.
	d.mu.Lock()
	cursor, srv := d.cursor, d.serverID
	d.mu.Unlock()
	_ = raw.SetDeadline(time.Now().Add(dialTimeout))
	if err := c.send(chatwire.HelloFrame(d.Store.Self(), d.host, cursor, srv)); err != nil {
		return err
	}
	welcome, err := chatwire.ReadFrame(raw)
	if err != nil {
		return err
	}
	if err := chatwire.ValidWelcome(welcome); err != nil {
		return err
	}
	_ = raw.SetDeadline(time.Time{})

	d.mu.Lock()
	if welcome.Server != d.serverID || welcome.Reset {
		// A different server, or the same one telling us our cursor is no
		// use to it. Either way the number we were holding means nothing
		// here; the history we already have is kept, because nothing the
		// server says makes what we were told yesterday untrue.
		d.cursor = 0
	}
	d.serverID = welcome.Server
	d.srvName = chat.ClipRunes(welcome.ServerName, 48)
	d.server = addr
	d.conn = c
	d.connected = true
	d.mu.Unlock()

	d.logf("enrolled with %s at %s", welcome.ServerName, addr)
	d.emit(chat.Event{Kind: chat.EventStatus, Text: "connected to " + addr})
	d.emit(chat.Event{Kind: chat.EventPeers})

	go func() {
		<-ctx.Done()
		c.close()
	}()

	err = d.readLoop(c)

	d.mu.Lock()
	d.conn = nil
	d.connected = false
	d.mu.Unlock()
	d.saveCursor()
	d.markEveryoneOffline()
	d.clearAllTyping()
	d.failLiveTransfers("the server connection dropped")
	d.emit(chat.Event{Kind: chat.EventStatus, Text: "the server is unreachable — showing what this machine already has"})
	d.emit(chat.Event{Kind: chat.EventPeers})
	return err
}

// idleTimeout drops a link that has said nothing at all, not even a pong.
// The server pings every fifteen seconds, so silence for this long means
// the connection is dead however alive TCP believes it to be.
const idleTimeout = 2 * time.Minute

func (d *Daemon) readLoop(c *serverConn) error {
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(idleTimeout))
		f, err := chatwire.ReadFrame(c.conn)
		if err != nil {
			return err
		}
		switch f.Type {
		case chatwire.FrameBye:
			return fmt.Errorf("the server said goodbye: %s", f.Reason)
		case chatwire.FramePing:
			_ = c.send(chatwire.Frame{Type: chatwire.FramePong})
		case chatwire.FramePong:
		default:
			d.apply(f)
		}
	}
}

// apply is one frame from the server, folded into the cache. Everything
// here comes from the server and is believed: it is the authority, and a
// client that second-guessed it would be a second ordering.
func (d *Daemon) apply(f chatwire.Frame) {
	switch f.Type {
	case chatwire.FrameMsg:
		d.onMessage(f)

	case chatwire.FrameSeq:
		d.advance(f.Seq)
		if m, ok := d.Store.Confirm(f.ID, f.Seq); ok {
			d.emit(chat.Event{Kind: chat.EventState, Conv: m.Conv, Msg: &m})
		}

	case chatwire.FrameRoster:
		d.onRoster(f)

	case chatwire.FrameTyping:
		if f.From != "" {
			d.onTyping(f.From, f.Conv, f.Typing)
		}

	case chatwire.FrameSynced:
		d.advance(f.Cursor)
		d.saveCursor()
		// Anything composed while the link was down goes out now, behind
		// whatever we had missed rather than in front of it.
		d.flushQueue()
		d.emit(chat.Event{Kind: chat.EventPeers})

	case chatwire.FrameOffer:
		d.onOffer(f)
	case chatwire.FrameAccept:
		d.onAccepted(f)
	case chatwire.FrameDecline:
		d.onDeclined(f)
	case chatwire.FrameChunk:
		d.onChunk(f)
	case chatwire.FrameDone:
		d.onDone(f)

	case chatwire.FrameError:
		d.logf("the server refused something: %s", f.Reason)
		d.emit(chat.Event{Kind: chat.EventStatus, Text: "server: " + f.Reason})

	default:
		// Forward compatibility: a newer server may send frames this
		// build has never heard of, and the session carries on.
	}
}

func (d *Daemon) onMessage(f chatwire.Frame) {
	if f.From == "" {
		return
	}
	d.advance(f.Seq)
	if d.Store.Blocked(f.From) {
		// The server relays everyone; blocking is this machine's own
		// decision and is applied here, where the person made it.
		return
	}
	m, ok := chatwire.MessageFromFrame(f, f.From)
	if !ok {
		return
	}
	self := d.Store.Self().ID
	m.Mine = m.From == self
	if m.Mine {
		// Our own message, coming back with the sequence it was given —
		// our copy is already in the store, so this only fixes its place.
		if _, ok := d.Store.Confirm(m.ID, m.Seq); ok {
			return
		}
		m.Read = true
		m.State = chat.StateDelivered
	}
	added, err := d.Store.Append(m)
	if err != nil || !added {
		return
	}
	d.clearTyping(m.From, m.Conv)
	d.emit(chat.Event{Kind: chat.EventMessage, Conv: m.Conv, Peer: m.From, Msg: &m})
	if !m.Mine {
		peer, _ := d.Store.Peer(m.From)
		d.maybeNotify(m, peer)
	}
}

func (d *Daemon) onRoster(f chatwire.Frame) {
	self := d.Store.Self().ID
	changed := false
	put := func(p chat.Peer) {
		if p.ID == "" || p.ID == self {
			return
		}
		if d.Store.PutPeer(p) {
			changed = true
		}
	}
	if f.Peer != nil {
		put(*f.Peer)
		if changed {
			d.emit(chat.Event{Kind: chat.EventPeers, Peer: f.Peer.ID})
		}
		return
	}
	for _, p := range f.Peers {
		put(p)
	}
	if changed {
		d.emit(chat.Event{Kind: chat.EventPeers})
	}
}

// markEveryoneOffline is what the cache should say when the link drops:
// we can no longer see anybody, and claiming otherwise would be the one
// lie a status bar must not tell.
func (d *Daemon) markEveryoneOffline() {
	for _, p := range d.Store.Peers() {
		d.Store.SetPresence(p.ID, chat.PresenceOffline)
	}
}

// advance moves the cursor forward. It only ever moves forward: backfill
// of an old room conversation arrives with sequence numbers far behind
// the live stream, and must not rewind what we have already seen.
func (d *Daemon) advance(seq uint64) {
	if seq == 0 {
		return
	}
	d.mu.Lock()
	if seq > d.cursor {
		d.cursor = seq
	}
	d.mu.Unlock()
}
