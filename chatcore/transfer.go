package chatcore

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// File transfers.
//
// A transfer is a live thing, so the record of one lives in memory and
// dies with the daemon; what persists is the chat message that announced
// it and, if it was accepted, the file on disk. A transfer interrupted by
// a restart is not resumed — it is shown as failed and the sender is asked
// again. Resumption would need the receiver to persist partial state and
// the sender to keep the file unchanged, and neither is worth it for the
// sizes this app is for.

// transfers is the live transfer table. It is a separate struct from Store
// so that the store's lock is not held across file I/O.
type transfers struct {
	mu   sync.Mutex
	byID map[TransferID]*Transfer
	// open is the receiving file handle of a running incoming transfer.
	open map[TransferID]*os.File
}

func newTransfers() *transfers {
	return &transfers{byID: map[TransferID]*Transfer{}, open: map[TransferID]*os.File{}}
}

// NewTransferID mints an identifier for one transfer.
func NewTransferID() TransferID {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("chat: crypto/rand: " + err.Error())
	}
	return TransferID(hex.EncodeToString(b[:]))
}

func (t *transfers) put(tr Transfer) *Transfer {
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := tr
	t.byID[tr.ID] = &cp
	return &cp
}

func (t *transfers) get(id TransferID) (Transfer, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr, ok := t.byID[id]
	if !ok {
		return Transfer{}, false
	}
	return *tr, true
}

// update applies fn to a transfer under the lock and returns the result.
func (t *transfers) update(id TransferID, fn func(*Transfer)) (Transfer, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr, ok := t.byID[id]
	if !ok {
		return Transfer{}, false
	}
	fn(tr)
	return *tr, true
}

func (t *transfers) list() []Transfer {
	t.mu.Lock()
	out := make([]Transfer, 0, len(t.byID))
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
func (t *transfers) active(peer PeerID) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, tr := range t.byID {
		if tr.Peer != peer {
			continue
		}
		switch tr.State {
		case TransferOffered, TransferIncoming, TransferRunning:
			n++
		}
	}
	return n
}

// maxOpenTransfers is how many offers one peer may have outstanding. Any
// peer on the LAN can send offers; without a cap, one can fill the
// conversation and the transfer list with them.
const maxOpenTransfers = 8

// maxTransferBytes is the largest file this app will accept. It is not a
// judgement about disk space: it is the point past which "send it over
// chat" is the wrong tool, and it stops a peer from being able to name an
// arbitrary size and have us believe it.
const maxTransferBytes = 2 << 30 // 2 GiB

func (t *transfers) openFile(id TransferID, f *os.File) {
	t.mu.Lock()
	t.open[id] = f
	t.mu.Unlock()
}

func (t *transfers) takeFile(id TransferID) *os.File {
	t.mu.Lock()
	f := t.open[id]
	delete(t.open, id)
	t.mu.Unlock()
	return f
}

func (t *transfers) file(id TransferID) *os.File {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.open[id]
}

// closeAll releases every open receiving file. It runs at shutdown.
func (t *transfers) closeAll() {
	t.mu.Lock()
	open := t.open
	t.open = map[TransferID]*os.File{}
	t.mu.Unlock()
	for _, f := range open {
		_ = f.Close()
	}
}

// DownloadDir is where accepted files land: $XDG_DOWNLOAD_DIR if it names
// an existing directory, else ~/Downloads, else the data directory. It is
// created if it does not exist.
func DownloadDir() string {
	if d := strings.TrimSpace(os.Getenv("UITK_CHAT_DOWNLOADS")); d != "" {
		return d
	}
	if d := strings.TrimSpace(os.Getenv("XDG_DOWNLOAD_DIR")); d != "" {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		d := filepath.Join(home, "Downloads")
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return filepath.Join(DataDir(), "files")
}

// safeBaseName reduces a file name from the network to something that can
// only ever name a file directly inside the download directory. A peer
// supplies this string, so "../../.bashrc", an absolute path, a name that
// is all dots and a name with a newline in it all have to come out the
// other side as an ordinary file name.
func safeBaseName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(filepath.Clean("/" + name))
	if name == "/" || name == "." {
		// An empty name, or one that was nothing but separators.
		return "file"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// control characters, including the newline that would let a
			// name forge a second line in a log
		case r == '/' || r == '\\' || r == ':':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimLeft(out, ".")
	if out == "" {
		out = "file"
	}
	return clipRunes(out, 120)
}

// uniquePath is dir/name, with " (2)", " (3)" and so on appended until it
// names a file that does not exist. An incoming file never overwrites
// something already on disk.
func uniquePath(dir, name string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		candidate := filepath.Join(dir, name)
		if i > 1 {
			candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		}
		f, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return candidate, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("chat: cannot find a free name for %q in %s", name, dir)
}

// guessMIME is a content type from the extension alone. The daemon never
// sniffs the bytes and never acts on the type; it is shown to the user so
// they can decide; nothing branches on it.
func guessMIME(name string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		return strings.SplitN(t, ";", 2)[0]
	}
	return "application/octet-stream"
}

// newTransfer is a fresh record with the timestamp filled in.
func newTransfer(id TransferID, conv ConversationID, peer PeerID, name string, size int64, incoming bool, state TransferState) Transfer {
	return Transfer{
		ID: id, Conv: conv, Peer: peer, Name: name, Size: size,
		MIME: guessMIME(name), Incoming: incoming, State: state, At: time.Now(),
	}
}
