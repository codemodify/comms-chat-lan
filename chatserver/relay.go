package chatserver

import (
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatwire"
)

// File transfer, relayed.
//
// The bytes go through the server, because that is the only path there
// is: clients do not connect to each other any more. The server does not
// keep the file. It checks that the transfer was offered and accepted,
// that chunks come from the side that offered, that they arrive in order
// and that they do not add up to more than was offered — and then it
// passes them on. What lands on disk is decided by the receiver, as
// before.
//
// Both ends have to be connected. An offer to somebody who is not is
// refused at once rather than held: a file is a live thing, and a person
// watching a progress bar deserves to be told now.

type relayState string

const (
	relayOffered relayState = "offered"
	relayRunning relayState = "running"
)

// relay is one transfer passing through the server.
type relay struct {
	ID    chat.TransferID
	From  chat.PeerID
	To    chat.PeerID
	Name  string
	Size  int64
	Sent  int64
	State relayState
	At    time.Time
}

func (s *Server) onOffer(c *clientConn, f chatwire.Frame) {
	to := f.To
	if to == "" {
		to = chat.ConvPeer(f.Conv)
	}
	decline := func(reason string) {
		c.send(chatwire.Frame{
			Type: chatwire.FrameDecline, Transfer: f.Transfer,
			Conv: chat.DirectConv(to), From: to, Reason: reason,
		})
	}
	if f.Transfer == "" || f.Size < 0 || f.Size > chat.MaxTransferBytes {
		decline("that offer is not one the server will carry")
		return
	}
	if to == "" || to == c.peer {
		decline("a file goes to one person, not to a room")
		return
	}
	if _, ok := s.Store.Peer(to); !ok {
		decline("nobody with that id has ever enrolled here")
		return
	}
	if s.connsOf(to) == 0 {
		// Honest and immediate. The alternative is a progress bar that
		// never moves and a sender who finds out tomorrow.
		decline("they are not connected, so a file cannot be sent right now")
		return
	}

	s.mu.Lock()
	if _, exists := s.xfers[f.Transfer]; exists {
		s.mu.Unlock()
		return
	}
	open := 0
	for _, r := range s.xfers {
		if r.From == c.peer || r.To == c.peer {
			open++
		}
	}
	if open >= chat.MaxOpenTransfers {
		s.mu.Unlock()
		decline("too many transfers are already open")
		return
	}
	s.xfers[f.Transfer] = &relay{
		ID: f.Transfer, From: c.peer, To: to, Name: chat.SafeBaseName(f.Name),
		Size: f.Size, State: relayOffered, At: time.Now(),
	}
	s.mu.Unlock()

	out := f
	out.From = c.peer
	out.To = to
	out.Conv = chat.DirectConv(c.peer)
	s.sendTo(to, out)
}

func (s *Server) onAccept(c *clientConn, f chatwire.Frame) {
	s.mu.Lock()
	r, ok := s.xfers[f.Transfer]
	if !ok || r.To != c.peer || r.State != relayOffered {
		s.mu.Unlock()
		return
	}
	r.State = relayRunning
	from := r.From
	s.mu.Unlock()
	s.sendTo(from, chatwire.Frame{
		Type: chatwire.FrameAccept, Transfer: f.Transfer,
		From: c.peer, Conv: chat.DirectConv(c.peer),
	})
}

func (s *Server) onDecline(c *clientConn, f chatwire.Frame) {
	s.mu.Lock()
	r, ok := s.xfers[f.Transfer]
	if !ok || (r.From != c.peer && r.To != c.peer) {
		s.mu.Unlock()
		return
	}
	other := r.From
	if other == c.peer {
		other = r.To
	}
	delete(s.xfers, f.Transfer)
	s.mu.Unlock()
	s.sendTo(other, chatwire.Frame{
		Type: chatwire.FrameDecline, Transfer: f.Transfer, From: c.peer,
		Conv: chat.DirectConv(c.peer), Reason: chat.ClipRunes(f.Reason, 120),
	})
}

// onChunk passes one slice on. The checks are the ones that stop the
// relay being a way to write bytes nobody asked for: the right sender,
// an accepted transfer, in order, and never more than was offered.
func (s *Server) onChunk(c *clientConn, f chatwire.Frame) {
	s.mu.Lock()
	r, ok := s.xfers[f.Transfer]
	if !ok || r.From != c.peer || r.State != relayRunning {
		s.mu.Unlock()
		return
	}
	if f.Offset != r.Sent || r.Sent+int64(len(f.Data)) > r.Size {
		to := r.To
		delete(s.xfers, f.Transfer)
		s.mu.Unlock()
		reason := "the sender did not follow the transfer protocol"
		c.send(chatwire.Frame{Type: chatwire.FrameDecline, Transfer: f.Transfer, Reason: reason})
		s.sendTo(to, chatwire.Frame{Type: chatwire.FrameDecline, Transfer: f.Transfer, From: c.peer, Reason: reason})
		return
	}
	r.Sent += int64(len(f.Data))
	to := r.To
	s.mu.Unlock()

	out := f
	out.From = c.peer
	s.sendTo(to, out)
}

func (s *Server) onDone(c *clientConn, f chatwire.Frame) {
	s.mu.Lock()
	r, ok := s.xfers[f.Transfer]
	if !ok || r.From != c.peer {
		s.mu.Unlock()
		return
	}
	to := r.To
	delete(s.xfers, f.Transfer)
	s.mu.Unlock()
	out := f
	out.From = c.peer
	s.sendTo(to, out)
}

// expireOffers cancels offers nobody answered. A receiver who never says
// yes or no is the ordinary case — they are away from the machine — and
// both ends are told rather than left with a row that never changes.
func (s *Server) expireOffers() {
	cutoff := time.Now().Add(-OfferTimeout)
	var gone []*relay
	s.mu.Lock()
	for id, r := range s.xfers {
		if r.State == relayOffered && r.At.Before(cutoff) {
			gone = append(gone, r)
			delete(s.xfers, id)
		}
	}
	s.mu.Unlock()
	for _, r := range gone {
		const reason = "the offer was not answered"
		s.sendTo(r.From, chatwire.Frame{Type: chatwire.FrameDecline, Transfer: r.ID, From: r.To, Reason: reason})
		s.sendTo(r.To, chatwire.Frame{Type: chatwire.FrameDecline, Transfer: r.ID, From: r.From, Reason: reason})
	}
}

// failTransfersOf ends everything one client was part of when it goes
// away, and tells the other side.
func (s *Server) failTransfersOf(id chat.PeerID, reason string) {
	var gone []*relay
	s.mu.Lock()
	for tid, r := range s.xfers {
		if r.From == id || r.To == id {
			gone = append(gone, r)
			delete(s.xfers, tid)
		}
	}
	s.mu.Unlock()
	for _, r := range gone {
		other := r.From
		if other == id {
			other = r.To
		}
		s.sendTo(other, chatwire.Frame{Type: chatwire.FrameDecline, Transfer: r.ID, From: id, Reason: reason})
	}
}

func (s *Server) sendTo(id chat.PeerID, f chatwire.Frame) {
	for _, conn := range s.to(id) {
		conn.send(f)
	}
}
