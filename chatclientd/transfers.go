package chatclientd

import (
	"os"
	"sort"
	"sync"

	"github.com/codemodify/comms-chat-lan/chat"
)

// transfers is clientd's live transfer table: what has been offered to us
// and by us, and the file an accepted incoming transfer is landing in. It
// is a separate struct from the store so that the store's lock is never
// held across file I/O.
type transfers struct {
	mu   sync.Mutex
	byID map[chat.TransferID]*chat.Transfer
	// open is the receiving file handle of a running incoming transfer.
	open map[chat.TransferID]*os.File
}

func newTransfers() *transfers {
	return &transfers{byID: map[chat.TransferID]*chat.Transfer{}, open: map[chat.TransferID]*os.File{}}
}

func (t *transfers) put(tr chat.Transfer) *chat.Transfer {
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := tr
	t.byID[tr.ID] = &cp
	return &cp
}

func (t *transfers) get(id chat.TransferID) (chat.Transfer, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr, ok := t.byID[id]
	if !ok {
		return chat.Transfer{}, false
	}
	return *tr, true
}

// update applies fn to a transfer under the lock and returns the result.
func (t *transfers) update(id chat.TransferID, fn func(*chat.Transfer)) (chat.Transfer, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr, ok := t.byID[id]
	if !ok {
		return chat.Transfer{}, false
	}
	fn(tr)
	return *tr, true
}

func (t *transfers) list() []chat.Transfer {
	t.mu.Lock()
	out := make([]chat.Transfer, 0, len(t.byID))
	for _, tr := range t.byID {
		out = append(out, *tr)
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// active reports how many transfers are moving bytes or waiting for an
// answer. It bounds how many a peer may have open at once.
func (t *transfers) active(peer chat.PeerID) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, tr := range t.byID {
		if tr.Peer != peer {
			continue
		}
		switch tr.State {
		case chat.TransferOffered, chat.TransferIncoming, chat.TransferRunning:
			n++
		}
	}
	return n
}

func (t *transfers) openFile(id chat.TransferID, f *os.File) {
	t.mu.Lock()
	t.open[id] = f
	t.mu.Unlock()
}

func (t *transfers) takeFile(id chat.TransferID) *os.File {
	t.mu.Lock()
	f := t.open[id]
	delete(t.open, id)
	t.mu.Unlock()
	return f
}

func (t *transfers) file(id chat.TransferID) *os.File {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.open[id]
}

// closeAll releases every open receiving file. It runs at shutdown.
func (t *transfers) closeAll() {
	t.mu.Lock()
	open := t.open
	t.open = map[chat.TransferID]*os.File{}
	t.mu.Unlock()
	for _, f := range open {
		_ = f.Close()
	}
}
