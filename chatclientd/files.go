package chatclientd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatwire"
)

// File transfer, the client's half.
//
// The shape is the one it always was: the sender offers, the receiver
// accepts or declines, and only then do bytes move. Nothing is written to
// disk before a person — or their own auto-accept threshold — has said
// yes, and what is written is always a fresh name inside their own
// download directory.
//
// What changed is the path the bytes take. They go to the server and the
// server passes them on, because there is no longer any other way for two
// clients to reach each other. Both ends therefore have to be connected
// at the same time; an offer to somebody who is not is refused at once by
// the server, and an offer nobody answers is cancelled by the server
// after a couple of minutes rather than sitting in two front ends for
// ever.

// Offer offers a file to the other end of a conversation. The offer is
// announced in the conversation as a message, so it is in the history and
// in both front ends, and the transfer itself waits for an answer.
//
// Only direct conversations can carry a file: a room offer would mean n
// simultaneous transfers to people who each have to answer separately,
// and this version does not do that.
func (d *Daemon) Offer(conv chat.ConversationID, path string) (chat.Transfer, error) {
	if conv.IsRoom() {
		return chat.Transfer{}, fmt.Errorf("chat: files can only be sent to one person, not to a room")
	}
	peer := chat.ConvPeer(conv)
	if peer == "" {
		return chat.Transfer{}, fmt.Errorf("chat: %q is not a conversation with a person", conv)
	}
	if !d.Connected() {
		return chat.Transfer{}, fmt.Errorf("chat: the server is unreachable, so a file cannot be sent right now")
	}
	st, err := os.Stat(path)
	if err != nil {
		return chat.Transfer{}, err
	}
	if st.IsDir() {
		return chat.Transfer{}, fmt.Errorf("chat: %s is a directory", path)
	}
	if st.Size() > chat.MaxTransferBytes {
		return chat.Transfer{}, fmt.Errorf("chat: %s is %d bytes, over the %d limit", filepath.Base(path), st.Size(), chat.MaxTransferBytes)
	}
	if d.xfers.active(peer) >= chat.MaxOpenTransfers {
		return chat.Transfer{}, fmt.Errorf("chat: too many transfers already open with %s", chat.ShortID(peer))
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return chat.Transfer{}, err
	}

	tr := chat.NewTransfer(chat.NewTransferID(), conv, peer, filepath.Base(path), st.Size(), false, chat.TransferOffered)
	tr.SHA256 = sum
	tr.Path = path // the local source; never sent
	d.xfers.put(tr)

	self := d.Store.Self()
	m := chat.Message{
		ID: newMessageID(self.ID), Conv: conv, From: self.ID, FromNick: self.Nick,
		Body: "sent a file: " + tr.Name, Sent: time.Now(),
		Mine: true, Read: true, State: chat.StateQueued,
		Transfer: &chat.TransferRef{ID: tr.ID, Name: tr.Name, Size: tr.Size},
	}
	if _, err := d.Store.Append(m); err != nil {
		return chat.Transfer{}, err
	}
	d.emit(chat.Event{Kind: chat.EventMessage, Conv: conv, Msg: &m})
	d.sendMessage(m)
	d.emitTransfer(tr)

	if !d.write(chatwire.Frame{
		Type: chatwire.FrameOffer, Transfer: tr.ID, Conv: conv, To: peer,
		Name: tr.Name, Size: tr.Size, MIME: tr.MIME, SHA256: tr.SHA256,
	}) {
		return d.failTransfer(tr.ID, "the server connection dropped")
	}
	return tr, nil
}

// onOffer records an incoming offer. It does not touch the disk: the
// receiver has not agreed to anything yet. The message that announces the
// offer arrives separately, relayed like any other, so the offer is in
// the history in the same order as everything else.
func (d *Daemon) onOffer(f chatwire.Frame) {
	if f.Transfer == "" || f.From == "" || f.Size < 0 || f.Size > chat.MaxTransferBytes {
		return
	}
	if d.Store.Blocked(f.From) {
		d.write(chatwire.Frame{Type: chatwire.FrameDecline, Transfer: f.Transfer, Reason: "blocked"})
		return
	}
	if d.xfers.active(f.From) >= chat.MaxOpenTransfers {
		d.write(chatwire.Frame{Type: chatwire.FrameDecline, Transfer: f.Transfer, Reason: "too many transfers open"})
		return
	}
	conv := chat.DirectConv(f.From)
	name := chat.SafeBaseName(f.Name)
	tr := chat.NewTransfer(f.Transfer, conv, f.From, name, f.Size, true, chat.TransferIncoming)
	tr.SHA256 = f.SHA256
	if f.MIME != "" {
		tr.MIME = chat.ClipRunes(f.MIME, 96)
	}
	d.xfers.put(tr)
	d.emitTransfer(tr)

	// An auto-accept threshold means small files land without a prompt.
	// The default is zero, which asks about everything.
	if limit := d.Store.Self().AutoAcceptFiles; limit > 0 && f.Size <= limit {
		go func() { _ = d.Accept(tr.ID) }()
		return
	}
	peer, _ := d.Store.Peer(f.From)
	d.maybeNotify(chat.Message{Conv: conv, From: f.From, Body: "offered a file: " + name}, peer)
}

// Accept accepts an incoming offer and opens the file the bytes will land
// in. The path is chosen here, by us, from a sanitised base name: neither
// the sender nor the server has any say in where the file goes.
func (d *Daemon) Accept(id chat.TransferID) error {
	tr, ok := d.xfers.get(id)
	if !ok {
		return fmt.Errorf("chat: no such transfer %s", id)
	}
	if !tr.Incoming || tr.State != chat.TransferIncoming {
		return fmt.Errorf("chat: transfer %s cannot be accepted in state %q", id, tr.State)
	}
	path, err := chat.UniquePath(chat.DownloadDir(), chat.SafeBaseName(tr.Name))
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	d.xfers.openFile(id, f)
	tr, _ = d.xfers.update(id, func(t *chat.Transfer) {
		t.State, t.Path, t.Done = chat.TransferRunning, path, 0
	})
	d.emitTransfer(tr)

	if !d.write(chatwire.Frame{Type: chatwire.FrameAccept, Transfer: id, To: tr.Peer}) {
		_, err := d.failTransfer(id, "the server connection dropped")
		return err
	}
	return nil
}

// Decline refuses an incoming offer, or cancels one of our own. Either
// way the other end is told, through the server, and the conversation
// records it.
func (d *Daemon) Decline(id chat.TransferID, reason string) error {
	tr, ok := d.xfers.get(id)
	if !ok {
		return fmt.Errorf("chat: no such transfer %s", id)
	}
	if f := d.xfers.takeFile(id); f != nil {
		_ = f.Close()
	}
	tr, _ = d.xfers.update(id, func(t *chat.Transfer) {
		t.State, t.Error = chat.TransferCancelled, reason
		if t.Incoming && t.Path != "" {
			// Nothing was agreed to, so nothing stays on disk.
			_ = os.Remove(t.Path)
			t.Path = ""
		}
	})
	d.emitTransfer(tr)
	d.write(chatwire.Frame{
		Type: chatwire.FrameDecline, Transfer: id, To: tr.Peer,
		Reason: chat.ClipRunes(reason, 120),
	})
	d.systemMessage(tr.Conv, "declined "+tr.Name)
	return nil
}

// onAccepted starts pushing the file: our offer was taken up.
func (d *Daemon) onAccepted(f chatwire.Frame) {
	tr, ok := d.xfers.get(f.Transfer)
	if !ok || tr.Incoming || tr.State != chat.TransferOffered {
		return
	}
	tr, _ = d.xfers.update(f.Transfer, func(t *chat.Transfer) { t.State = chat.TransferRunning })
	d.emitTransfer(tr)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.pushFile(tr)
	}()
}

// onDeclined is the other end saying no, or the server saying it cannot
// carry this one — a receiver who is not connected, an offer nobody
// answered, a sender that did not follow the protocol. All of them reach
// a person as the same sentence: it did not happen, and here is why.
func (d *Daemon) onDeclined(f chatwire.Frame) {
	tr, ok := d.xfers.get(f.Transfer)
	if !ok {
		return
	}
	if fh := d.xfers.takeFile(f.Transfer); fh != nil {
		_ = fh.Close()
	}
	tr, _ = d.xfers.update(f.Transfer, func(t *chat.Transfer) {
		t.State, t.Error = chat.TransferDeclined, chat.ClipRunes(f.Reason, 120)
		if t.Incoming && t.Path != "" {
			_ = os.Remove(t.Path)
			t.Path = ""
		}
	})
	d.emitTransfer(tr)
	why := tr.Name + " was declined"
	if f.Reason != "" {
		why = tr.Name + ": " + chat.ClipRunes(f.Reason, 120)
	}
	d.systemMessage(tr.Conv, why)
	d.setStateForTransfer(f.Transfer, chat.StateFailed)
}

// pushFile streams an accepted file to the server, which passes it on.
// Progress is reported on every chunk, throttled so that a fast local
// transfer does not drown the front ends in events.
func (d *Daemon) pushFile(tr chat.Transfer) {
	fh, err := os.Open(tr.Path)
	if err != nil {
		_, _ = d.failTransfer(tr.ID, err.Error())
		return
	}
	defer func() { _ = fh.Close() }()

	buf := make([]byte, chatwire.ChunkSize)
	var off int64
	lastEmit := time.Now()
	for {
		if cur, ok := d.xfers.get(tr.ID); !ok || cur.State != chat.TransferRunning {
			return // declined or cancelled while in flight
		}
		nr, rerr := fh.Read(buf)
		if nr > 0 {
			chunk := make([]byte, nr)
			copy(chunk, buf[:nr])
			if !d.write(chatwire.Frame{
				Type: chatwire.FrameChunk, Transfer: tr.ID, To: tr.Peer,
				Offset: off, Data: chunk,
			}) {
				_, _ = d.failTransfer(tr.ID, "the server connection dropped mid-transfer")
				return
			}
			off += int64(nr)
			cur, _ := d.xfers.update(tr.ID, func(t *chat.Transfer) { t.Done = off })
			if time.Since(lastEmit) > 150*time.Millisecond {
				lastEmit = time.Now()
				d.emitTransfer(cur)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_, _ = d.failTransfer(tr.ID, rerr.Error())
			return
		}
	}
	d.write(chatwire.Frame{
		Type: chatwire.FrameDone, Transfer: tr.ID, To: tr.Peer,
		SHA256: tr.SHA256, Size: off,
	})
	done, _ := d.xfers.update(tr.ID, func(t *chat.Transfer) { t.State, t.Done = chat.TransferDone, off })
	d.emitTransfer(done)
}

// onChunk writes one slice of an accepted incoming file. The checks are
// the same ones as before and exist for the same reason: bytes nobody
// accepted are never written, and a sender may not exceed what it
// offered, whatever the stream says.
func (d *Daemon) onChunk(f chatwire.Frame) {
	tr, ok := d.xfers.get(f.Transfer)
	if !ok || !tr.Incoming || tr.State != chat.TransferRunning {
		return
	}
	fh := d.xfers.file(f.Transfer)
	if fh == nil {
		_, _ = d.failTransfer(f.Transfer, "no open file for this transfer")
		return
	}
	if f.Offset != tr.Done {
		_, _ = d.failTransfer(f.Transfer, fmt.Sprintf("a chunk arrived at %d, expected %d", f.Offset, tr.Done))
		return
	}
	if tr.Done+int64(len(f.Data)) > tr.Size {
		// More than was offered. Believing the stream over the offer is
		// how a "small" file fills a disk.
		_, _ = d.failTransfer(f.Transfer, "more bytes arrived than were offered")
		return
	}
	if _, err := fh.Write(f.Data); err != nil {
		_, _ = d.failTransfer(f.Transfer, err.Error())
		return
	}
	cur, _ := d.xfers.update(f.Transfer, func(t *chat.Transfer) { t.Done += int64(len(f.Data)) })
	d.emitTransferThrottled(cur)
}

// onDone closes an incoming file and checks the digest. The digest
// catches a truncated or corrupted transfer, including one the relay lost
// its place in; it is not a security check, because anyone who can change
// the bytes can change the digest with them.
func (d *Daemon) onDone(f chatwire.Frame) {
	tr, ok := d.xfers.get(f.Transfer)
	if !ok || !tr.Incoming {
		return
	}
	fh := d.xfers.takeFile(f.Transfer)
	if fh != nil {
		_ = fh.Sync()
		_ = fh.Close()
	}
	if tr.SHA256 != "" && tr.Path != "" {
		got, err := fileSHA256(tr.Path)
		if err != nil || got != tr.SHA256 {
			_ = os.Remove(tr.Path)
			_, _ = d.failTransfer(f.Transfer, "the file did not match the sender's checksum")
			return
		}
	}
	done, _ := d.xfers.update(f.Transfer, func(t *chat.Transfer) { t.State, t.Done = chat.TransferDone, t.Size })
	d.emitTransfer(done)
	d.systemMessage(tr.Conv, "saved "+tr.Name+" to "+done.Path)
}

func (d *Daemon) failTransfer(id chat.TransferID, why string) (chat.Transfer, error) {
	if fh := d.xfers.takeFile(id); fh != nil {
		_ = fh.Close()
	}
	tr, ok := d.xfers.update(id, func(t *chat.Transfer) { t.State, t.Error = chat.TransferFailed, why })
	if !ok {
		return chat.Transfer{}, fmt.Errorf("chat: no such transfer %s", id)
	}
	d.emitTransfer(tr)
	d.setStateForTransfer(id, chat.StateFailed)
	return tr, fmt.Errorf("chat: transfer %s failed: %s", id, why)
}

// failLiveTransfers ends everything in flight when the link drops. A
// transfer is a live thing and does not survive the connection it was
// travelling over.
func (d *Daemon) failLiveTransfers(why string) {
	for _, tr := range d.xfers.list() {
		switch tr.State {
		case chat.TransferRunning, chat.TransferOffered, chat.TransferIncoming:
			_, _ = d.failTransfer(tr.ID, why)
		}
	}
}

// setStateForTransfer moves the delivery state of the message that
// announced a transfer, so the conversation shows what happened without a
// second kind of row.
func (d *Daemon) setStateForTransfer(id chat.TransferID, st chat.MessageState) {
	tr, ok := d.xfers.get(id)
	if !ok {
		return
	}
	for _, m := range d.Store.Messages(tr.Conv, 200) {
		if m.Mine && m.Transfer != nil && m.Transfer.ID == id {
			d.setState(m.ID, st)
			return
		}
	}
}

// Transfers is every transfer this daemon knows about, newest first.
func (d *Daemon) Transfers() []chat.Transfer { return d.xfers.list() }

// Transfer looks one transfer up.
func (d *Daemon) Transfer(id chat.TransferID) (chat.Transfer, bool) { return d.xfers.get(id) }

func (d *Daemon) emitTransfer(tr chat.Transfer) {
	cp := tr
	d.emit(chat.Event{Kind: chat.EventTransfer, Conv: tr.Conv, Peer: tr.Peer, Transfer: &cp})
}

// emitTransferThrottled reports progress at most every 150ms. A 64 KiB
// chunk over a local network arrives hundreds of times a second, and a
// front end that repaints on each one does nothing else.
func (d *Daemon) emitTransferThrottled(tr chat.Transfer) {
	d.mu.Lock()
	last := d.lastProgress[tr.ID]
	now := time.Now()
	due := now.Sub(last) > 150*time.Millisecond || tr.Done >= tr.Size
	if due {
		d.lastProgress[tr.ID] = now
	}
	d.mu.Unlock()
	if due {
		d.emitTransfer(tr)
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
