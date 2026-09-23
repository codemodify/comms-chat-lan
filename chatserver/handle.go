package chatserver

import (
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatwire"
)

// handle runs one frame from one client. Every case is here, in one
// switch, on purpose: this is the whole of what a client on the LAN can
// make the server do, and a reviewer should be able to read it in one
// sitting.
func (s *Server) handle(c *clientConn, f chatwire.Frame) {
	switch f.Type {
	case chatwire.FrameMsg:
		s.onMessage(c, f)

	case chatwire.FramePresence:
		pr := f.Presence.Valid()
		if s.Store.PutPeer(chat.Peer{ID: c.peer, Presence: pr, LastSeen: time.Now()}) {
			s.broadcastPeer(c.peer)
		}

	case chatwire.FrameTyping:
		s.onTyping(c, f)

	case chatwire.FrameJoin:
		s.onJoin(c, f)

	case chatwire.FrameLeave:
		s.onLeave(c, f)

	case chatwire.FrameOffer:
		s.onOffer(c, f)

	case chatwire.FrameAccept:
		s.onAccept(c, f)

	case chatwire.FrameDecline:
		s.onDecline(c, f)

	case chatwire.FrameChunk:
		s.onChunk(c, f)

	case chatwire.FrameDone:
		s.onDone(c, f)

	case chatwire.FramePing:
		c.send(chatwire.Frame{Type: chatwire.FramePong})

	case chatwire.FramePong:
		// Nothing to do: reading it was the point.

	default:
		// Forward compatibility: a newer client may send frames this
		// build has never heard of, and the conversation carries on.
	}
}

// onMessage is where the order of the LAN's conversation is decided.
//
// The sender is the identity that enrolled on this connection, never the
// "from" in the frame. The conversation is recomputed the same way: a
// client may name a room it has joined, or the one-to-one conversation
// between itself and one other person, and nothing else.
func (s *Server) onMessage(c *clientConn, f chatwire.Frame) {
	m, ok := chatwire.MessageFromFrame(f, c.peer)
	if !ok {
		c.send(chatwire.ErrorFrame(chatwire.ErrBadFrame, "malformed message"))
		return
	}
	conv, recipients, errFrame := s.route(c.peer, f.Conv)
	if errFrame != nil {
		c.send(*errFrame)
		return
	}
	m.Conv = conv
	sender, _ := s.Store.Peer(c.peer)
	m.FromNick = sender.Nick

	// A resend is free. A client that composed while the server was down,
	// or that never saw the sequence it was given, sends the same message
	// again with the same id; it is filed once and sequenced once.
	if s.Store.Have(m.ID) {
		if had, ok := s.Store.Message(m.ID); ok {
			c.send(chatwire.Frame{Type: chatwire.FrameSeq, ID: m.ID, Seq: had.Seq})
		}
		return
	}
	m.Seq = s.Store.NextSeq()
	if _, err := s.Store.Append(m); err != nil {
		c.send(chatwire.ErrorFrame(chatwire.ErrRefused, err.Error()))
		return
	}

	s.fanOut(m, recipients)
	for _, conn := range s.to(c.peer) {
		conn.send(chatwire.Frame{Type: chatwire.FrameSeq, ID: m.ID, Seq: m.Seq})
	}
}

// route is who a message goes to, and what the server calls the
// conversation it belongs to.
func (s *Server) route(sender chat.PeerID, conv chat.ConversationID) (chat.ConversationID, []chat.PeerID, *chatwire.Frame) {
	if conv.IsRoom() {
		room := chat.CanonRoom(conv.Room())
		if room == "" || !hasRoom(s.roomsOf(sender), room) {
			f := chatwire.ErrorFrame(chatwire.ErrNotInRoom, "you are not in #"+room)
			return "", nil, &f
		}
		var to []chat.PeerID
		for _, p := range s.Store.RoomMembers(room) {
			to = append(to, p.ID)
		}
		return chat.RoomConv(room), to, nil
	}
	other := chat.ConvPeer(conv)
	if other == "" || other == sender {
		f := chatwire.ErrorFrame(chatwire.ErrBadFrame, "that is not a conversation you can send to")
		return "", nil, &f
	}
	if _, ok := s.Store.Peer(other); !ok {
		f := chatwire.ErrorFrame(chatwire.ErrNoSuchPeer, "nobody with that id has ever enrolled here")
		return "", nil, &f
	}
	return chat.DirectKey(sender, other), []chat.PeerID{sender, other}, nil
}

// fanOut relays a stored message to everyone entitled to it who is
// connected. Everyone who is not gets it from their cursor when they come
// back, which is the same path and not a special case.
func (s *Server) fanOut(m chat.Message, recipients []chat.PeerID) {
	for _, id := range recipients {
		conv, ok := convFor(m, id, s.roomsOf(id))
		if !ok {
			continue
		}
		f := chatwire.MsgFrame(m)
		f.Conv = conv
		for _, conn := range s.to(id) {
			conn.send(f)
		}
	}
}

// onTyping relays an indicator to the others in the conversation. It is
// never stored: a typing indicator that arrives late is worse than one
// that never arrives.
func (s *Server) onTyping(c *clientConn, f chatwire.Frame) {
	_, recipients, errFrame := s.route(c.peer, f.Conv)
	if errFrame != nil {
		return
	}
	for _, id := range recipients {
		if id == c.peer {
			continue
		}
		out := chatwire.Frame{Type: chatwire.FrameTyping, From: c.peer, Typing: f.Typing}
		if f.Conv.IsRoom() {
			out.Conv = chat.RoomConv(f.Conv.Room())
		} else {
			out.Conv = chat.DirectConv(c.peer)
		}
		for _, conn := range s.to(id) {
			conn.send(out)
		}
	}
}

// onJoin puts a client in a room and then tells it what has been said in
// there. This is the late joiner: somebody who joins #general on Thursday
// reads Monday's conversation, because the server kept it and their
// cursor is simply behind.
func (s *Server) onJoin(c *clientConn, f chatwire.Frame) {
	room := chat.CanonRoom(f.Room)
	if room == "" || len(room) > 48 {
		c.send(chatwire.ErrorFrame(chatwire.ErrBadFrame, "a room name is 1-48 characters"))
		return
	}
	rooms := s.roomsOf(c.peer)
	if !hasRoom(rooms, room) {
		s.setRooms(c.peer, append(append([]string{}, rooms...), room))
		s.broadcastPeer(c.peer)
	}
	conv := chat.RoomConv(room)
	for _, m := range s.Store.Messages(conv, BacklogWindow) {
		c.send(chatwire.MsgFrame(m))
	}
	c.send(chatwire.Frame{Type: chatwire.FrameSynced, Cursor: s.Store.Seq(), Room: room})
}

func (s *Server) onLeave(c *clientConn, f chatwire.Frame) {
	room := chat.CanonRoom(f.Room)
	var left []string
	for _, r := range s.roomsOf(c.peer) {
		if r != room {
			left = append(left, r)
		}
	}
	s.setRooms(c.peer, left)
	s.broadcastPeer(c.peer)
}

// broadcastPeer tells everybody about one changed roster entry. Presence
// is what the server can see — whether that identity has a connection —
// rather than what it last claimed, so somebody whose laptop lost power
// is offline without having said so.
func (s *Server) broadcastPeer(id chat.PeerID) {
	p, ok := s.Store.Peer(id)
	if !ok {
		return
	}
	if s.connsOf(id) == 0 {
		p.Presence = chat.PresenceOffline
	}
	f := chatwire.Frame{Type: chatwire.FrameRoster, Peer: &p}
	for _, conn := range s.everyConn() {
		conn.send(f)
	}
}
