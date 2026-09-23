package chatcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// File transfer, the node's half.
//
// The shape is: the sender offers, the receiver accepts or declines, and
// only then do bytes move. Nothing is written to disk before a person (or
// their auto-accept threshold) has said yes, and the file that is written
// is always a fresh name inside the download directory.

// Offer offers a file to the other end of a conversation. The offer is
// announced in the conversation as a message, so it is in the history and
// in both front ends, and the transfer itself waits for an answer.
//
// Only direct conversations can carry a file: a room offer would mean n
// simultaneous transfers to peers who each have to answer separately, and
// the first version of this app does not do that. The RPC rejects it.
func (n *Node) Offer(ctx context.Context, conv ConversationID, path string) (Transfer, error) {
	if conv.IsRoom() {
		return Transfer{}, fmt.Errorf("chat: files can only be sent to one peer, not to a room")
	}
	peer := convPeer(conv)
	if peer == "" {
		return Transfer{}, fmt.Errorf("chat: %q is not a peer conversation", conv)
	}
	st, err := os.Stat(path)
	if err != nil {
		return Transfer{}, err
	}
	if st.IsDir() {
		return Transfer{}, fmt.Errorf("chat: %s is a directory", path)
	}
	if st.Size() > maxTransferBytes {
		return Transfer{}, fmt.Errorf("chat: %s is %d bytes, over the %d limit", filepath.Base(path), st.Size(), maxTransferBytes)
	}
	if n.xfers.active(peer) >= maxOpenTransfers {
		return Transfer{}, fmt.Errorf("chat: too many transfers already open with %s", ShortID(peer))
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return Transfer{}, err
	}

	tr := newTransfer(NewTransferID(), conv, peer, filepath.Base(path), st.Size(), false, TransferOffered)
	tr.SHA256 = sum
	tr.Path = path // the local source; never sent
	n.xfers.put(tr)

	self := n.Store.Self()
	m := Message{
		ID: n.newMessageID(self.ID), Conv: conv, From: self.ID, FromNick: self.Nick,
		Body: "sent a file: " + tr.Name, Sent: time.Now(), Seq: n.Store.NextSeq(),
		Mine: true, Read: true, State: StateSending,
		Transfer: &TransferRef{ID: tr.ID, Name: tr.Name, Size: tr.Size},
	}
	if _, err := n.Store.Append(m); err != nil {
		return Transfer{}, err
	}
	n.emit(NodeEvent{Kind: EventMessage, Conv: conv, Msg: &m})
	n.emitTransfer(tr)

	if err := n.sendTo(ctx, peer, Frame{
		Type: FrameOffer, Transfer: tr.ID, Conv: conv, Name: tr.Name,
		Size: tr.Size, MIME: tr.MIME, SHA256: tr.SHA256,
	}); err != nil {
		tr, _ = n.xfers.update(tr.ID, func(t *Transfer) {
			t.State, t.Error = TransferFailed, err.Error()
		})
		n.emitTransfer(tr)
		n.setState(m.ID, StateFailed)
		return tr, err
	}
	return tr, nil
}

// onOffer records an incoming offer. It does not touch the disk: the
// receiver has not agreed to anything yet.
func (n *Node) onOffer(c *peerConn, f Frame) error {
	if f.Transfer == "" || f.Size < 0 || f.Size > maxTransferBytes {
		c.send(Frame{Type: FrameDecline, Transfer: f.Transfer, Reason: "bad offer"})
		return nil
	}
	if n.xfers.active(c.peer) >= maxOpenTransfers {
		c.send(Frame{Type: FrameDecline, Transfer: f.Transfer, Reason: "too many transfers open"})
		return nil
	}
	conv := DirectConv(c.peer)
	name := safeBaseName(f.Name)
	tr := newTransfer(f.Transfer, conv, c.peer, name, f.Size, true, TransferIncoming)
	tr.SHA256 = f.SHA256
	if f.MIME != "" {
		tr.MIME = clipRunes(f.MIME, 96)
	}
	n.xfers.put(tr)

	peer, _ := n.Store.Peer(c.peer)
	m := Message{
		ID: MessageID(string(f.Transfer) + "-offer"), Conv: conv, From: c.peer,
		FromNick: peer.Nick, Body: "offered a file: " + name,
		Sent: time.Now(), Received: time.Now(),
		Transfer: &TransferRef{ID: tr.ID, Name: name, Size: f.Size},
	}
	if added, err := n.Store.Append(m); err == nil && added {
		n.emit(NodeEvent{Kind: EventMessage, Conv: conv, Peer: c.peer, Msg: &m})
	}
	n.emitTransfer(tr)

	// An auto-accept threshold means small files land without a prompt.
	// The default is zero, which asks about everything.
	if limit := n.Store.Self().AutoAcceptFiles; limit > 0 && f.Size <= limit {
		go func() { _ = n.Accept(tr.ID) }()
		return nil
	}
	n.maybeNotify(Message{Conv: conv, From: c.peer, Body: "offered a file: " + name}, peer)
	return nil
}

// Accept accepts an incoming offer and opens the file the bytes will land
// in. The path is chosen here, by us, from a sanitised base name: the
// sender has no say in where its file goes.
func (n *Node) Accept(id TransferID) error {
	tr, ok := n.xfers.get(id)
	if !ok {
		return fmt.Errorf("chat: no such transfer %s", id)
	}
	if !tr.Incoming || tr.State != TransferIncoming {
		return fmt.Errorf("chat: transfer %s cannot be accepted in state %q", id, tr.State)
	}
	path, err := uniquePath(DownloadDir(), safeBaseName(tr.Name))
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n.xfers.openFile(id, f)
	tr, _ = n.xfers.update(id, func(t *Transfer) {
		t.State, t.Path, t.Done = TransferRunning, path, 0
	})
	n.emitTransfer(tr)

	c, ok := n.conn(tr.Peer)
	if !ok {
		return n.failTransfer(id, "the peer is no longer connected")
	}
	c.send(Frame{Type: FrameAccept, Transfer: id})
	return nil
}

// Decline refuses an incoming offer, or cancels one of our own. Either
// way the other side is told, and the conversation records it.
func (n *Node) Decline(id TransferID, reason string) error {
	tr, ok := n.xfers.get(id)
	if !ok {
		return fmt.Errorf("chat: no such transfer %s", id)
	}
	if f := n.xfers.takeFile(id); f != nil {
		_ = f.Close()
	}
	state := TransferCancelled
	if !tr.Incoming {
		state = TransferCancelled
	}
	tr, _ = n.xfers.update(id, func(t *Transfer) {
		t.State, t.Error = state, reason
		if t.Incoming && t.Path != "" {
			// Nothing was agreed to, so nothing stays on disk.
			_ = os.Remove(t.Path)
			t.Path = ""
		}
	})
	n.emitTransfer(tr)
	if c, ok := n.conn(tr.Peer); ok {
		c.send(Frame{Type: FrameDecline, Transfer: id, Reason: clipRunes(reason, 120)})
	}
	n.systemMessage(tr.Conv, "declined "+tr.Name)
	return nil
}

// onAccept starts pushing the file. It runs on its own goroutine so the
// read loop keeps answering while a large file is in flight.
func (n *Node) onAccept(ctx context.Context, c *peerConn, f Frame) {
	tr, ok := n.xfers.get(f.Transfer)
	if !ok || tr.Incoming || tr.State != TransferOffered || tr.Peer != c.peer {
		return
	}
	tr, _ = n.xfers.update(f.Transfer, func(t *Transfer) { t.State = TransferRunning })
	n.emitTransfer(tr)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.pushFile(ctx, c, tr)
	}()
}

func (n *Node) onDecline(c *peerConn, f Frame) {
	tr, ok := n.xfers.get(f.Transfer)
	if !ok || tr.Peer != c.peer {
		return
	}
	if fh := n.xfers.takeFile(f.Transfer); fh != nil {
		_ = fh.Close()
	}
	tr, _ = n.xfers.update(f.Transfer, func(t *Transfer) {
		t.State, t.Error = TransferDeclined, clipRunes(f.Reason, 120)
		if t.Incoming && t.Path != "" {
			_ = os.Remove(t.Path)
			t.Path = ""
		}
	})
	n.emitTransfer(tr)
	n.systemMessage(tr.Conv, tr.Name+" was declined")
}

// pushFile streams an accepted file. Progress is reported on every chunk,
// throttled so a fast local transfer does not drown the front ends in
// events.
func (n *Node) pushFile(ctx context.Context, c *peerConn, tr Transfer) {
	fh, err := os.Open(tr.Path)
	if err != nil {
		_ = n.failTransfer(tr.ID, err.Error())
		return
	}
	defer func() { _ = fh.Close() }()

	buf := make([]byte, ChunkSize)
	var off int64
	lastEmit := time.Now()
	for {
		select {
		case <-ctx.Done():
			_ = n.failTransfer(tr.ID, "shutting down")
			return
		default:
		}
		if cur, ok := n.xfers.get(tr.ID); !ok || cur.State != TransferRunning {
			return // declined or cancelled while in flight
		}
		nr, rerr := fh.Read(buf)
		if nr > 0 {
			chunk := make([]byte, nr)
			copy(chunk, buf[:nr])
			if !c.send(Frame{Type: FrameChunk, Transfer: tr.ID, Offset: off, Data: chunk}) {
				_ = n.failTransfer(tr.ID, "the connection closed mid-transfer")
				return
			}
			off += int64(nr)
			cur, _ := n.xfers.update(tr.ID, func(t *Transfer) { t.Done = off })
			if time.Since(lastEmit) > 150*time.Millisecond {
				lastEmit = time.Now()
				n.emitTransfer(cur)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = n.failTransfer(tr.ID, rerr.Error())
			return
		}
	}
	c.send(Frame{Type: FrameDone, Transfer: tr.ID, SHA256: tr.SHA256, Size: off})
	done, _ := n.xfers.update(tr.ID, func(t *Transfer) { t.State, t.Done = TransferDone, off })
	n.emitTransfer(done)
	n.setStateForTransfer(tr.ID, StateDelivered)
}

// onChunk writes one slice of an accepted incoming file.
func (n *Node) onChunk(c *peerConn, f Frame) error {
	tr, ok := n.xfers.get(f.Transfer)
	if !ok || !tr.Incoming || tr.State != TransferRunning || tr.Peer != c.peer {
		// A chunk for something we did not accept is the one case where
		// dropping the connection is right: the peer is not following the
		// protocol, and the alternative is writing bytes nobody agreed to.
		return fmt.Errorf("unexpected chunk for transfer %s", f.Transfer)
	}
	fh := n.xfers.file(f.Transfer)
	if fh == nil {
		return fmt.Errorf("transfer %s has no open file", f.Transfer)
	}
	if f.Offset != tr.Done {
		return fmt.Errorf("transfer %s: chunk at %d, expected %d", f.Transfer, f.Offset, tr.Done)
	}
	if tr.Done+int64(len(f.Data)) > tr.Size {
		// The sender is sending more than it offered. Believing the
		// stream over the offer is how a "small" file fills a disk.
		return fmt.Errorf("transfer %s: more bytes than the %d offered", f.Transfer, tr.Size)
	}
	if _, err := fh.Write(f.Data); err != nil {
		_ = n.failTransfer(f.Transfer, err.Error())
		return nil
	}
	cur, _ := n.xfers.update(f.Transfer, func(t *Transfer) { t.Done += int64(len(f.Data)) })
	n.emitTransferThrottled(cur)
	return nil
}

// onDone closes an incoming file and checks the digest. The digest catches
// a truncated or corrupted transfer; it is not a security check, because a
// peer that can change the bytes can change the digest with them.
func (n *Node) onDone(c *peerConn, f Frame) error {
	tr, ok := n.xfers.get(f.Transfer)
	if !ok || !tr.Incoming || tr.Peer != c.peer {
		return nil
	}
	fh := n.xfers.takeFile(f.Transfer)
	if fh != nil {
		_ = fh.Sync()
		_ = fh.Close()
	}
	if tr.SHA256 != "" && tr.Path != "" {
		got, err := fileSHA256(tr.Path)
		if err != nil || got != tr.SHA256 {
			_ = os.Remove(tr.Path)
			_ = n.failTransfer(f.Transfer, "the file did not match the sender's checksum")
			return nil
		}
	}
	done, _ := n.xfers.update(f.Transfer, func(t *Transfer) { t.State, t.Done = TransferDone, t.Size })
	n.emitTransfer(done)
	n.systemMessage(tr.Conv, "saved "+tr.Name+" to "+done.Path)
	return nil
}

func (n *Node) failTransfer(id TransferID, why string) error {
	if fh := n.xfers.takeFile(id); fh != nil {
		_ = fh.Close()
	}
	tr, ok := n.xfers.update(id, func(t *Transfer) { t.State, t.Error = TransferFailed, why })
	if !ok {
		return fmt.Errorf("chat: no such transfer %s", id)
	}
	n.emitTransfer(tr)
	n.setStateForTransfer(id, StateFailed)
	return fmt.Errorf("chat: transfer %s failed: %s", id, why)
}

// failRunningTransfers marks everything in flight with a peer as failed
// when its connection goes away.
func (n *Node) failRunningTransfers(peer PeerID) {
	for _, tr := range n.xfers.list() {
		if tr.Peer != peer {
			continue
		}
		switch tr.State {
		case TransferRunning, TransferOffered, TransferIncoming:
			_ = n.failTransfer(tr.ID, "the peer disconnected")
		}
	}
}

// setStateForTransfer moves the delivery state of the message that
// announced a transfer, so the conversation shows what happened without a
// second kind of row.
func (n *Node) setStateForTransfer(id TransferID, st MessageState) {
	tr, ok := n.xfers.get(id)
	if !ok {
		return
	}
	for _, m := range n.Store.Messages(tr.Conv, 200) {
		if m.Mine && m.Transfer != nil && m.Transfer.ID == id {
			n.setState(m.ID, st)
			return
		}
	}
}

// Transfers is every transfer this daemon knows about, newest first.
func (n *Node) Transfers() []Transfer { return n.xfers.list() }

// Transfer looks one transfer up.
func (n *Node) Transfer(id TransferID) (Transfer, bool) { return n.xfers.get(id) }

func (n *Node) emitTransfer(tr Transfer) {
	cp := tr
	n.emit(NodeEvent{Kind: EventTransfer, Conv: tr.Conv, Peer: tr.Peer, Transfer: &cp})
}

// emitTransferThrottled reports progress at most every 150ms. A 64 KiB
// chunk over loopback arrives thousands of times a second, and a front end
// that repaints on each one does nothing else.
func (n *Node) emitTransferThrottled(tr Transfer) {
	n.mu.Lock()
	last := n.lastProgress[tr.ID]
	now := time.Now()
	due := now.Sub(last) > 150*time.Millisecond || tr.Done >= tr.Size
	if due {
		if n.lastProgress == nil {
			n.lastProgress = map[TransferID]time.Time{}
		}
		n.lastProgress[tr.ID] = now
	}
	n.mu.Unlock()
	if due {
		n.emitTransfer(tr)
	}
}

// systemMessage files a line the app wrote itself: a decline, a saved
// file. It is in the history like anything else, and never notifies.
func (n *Node) systemMessage(conv ConversationID, text string) {
	if conv == "" {
		return
	}
	m := Message{
		ID: n.newMessageID(n.Store.Self().ID), Conv: conv, From: n.Store.Self().ID,
		Body: text, Sent: time.Now(), Seq: n.Store.NextSeq(),
		System: true, Read: true, Mine: true, State: StateDelivered,
	}
	if added, err := n.Store.Append(m); err == nil && added {
		n.emit(NodeEvent{Kind: EventMessage, Conv: conv, Msg: &m})
	}
}

// fileSHA256 is the digest of a whole file, hex.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// convPeer is the peer a direct conversation belongs to ("" for a room).
func convPeer(c ConversationID) PeerID {
	if c.IsRoom() {
		return ""
	}
	if len(c) > 5 && c[:5] == "peer:" {
		return PeerID(c[5:])
	}
	return ""
}
