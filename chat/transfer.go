package chat

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File transfers: the parts both daemons agree on — the identifier, the
// limits, where an accepted file may land and what an incoming name is
// allowed to become.
//
// A transfer is a live thing, so the record of one lives in memory and
// dies with the daemon that holds it; what persists is the chat message
// that announced it and, if it was accepted, the file on disk. A transfer
// interrupted by a restart, of either daemon or of the server, is not
// resumed — it is shown as failed and the sender is asked again.
// Resumption would need the receiver to persist partial state, the sender
// to keep the file unchanged and the server to remember both, and none of
// it is worth it for the sizes this app is for.

// NewTransferID mints an identifier for one transfer.
func NewTransferID() TransferID {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("chat: crypto/rand: " + err.Error())
	}
	return TransferID(hex.EncodeToString(b[:]))
}

// MaxOpenTransfers is how many transfers one person may have open at
// once. Anyone enrolled can send offers; without a cap, one of them can
// fill a conversation and a transfer list with them.
const MaxOpenTransfers = 8

// MaxTransferBytes is the largest file this application will carry. It is
// not a judgement about disk space: it is the point past which "send it
// over chat" is the wrong tool, and it stops a sender from naming an
// arbitrary size and having it believed.
const MaxTransferBytes = 2 << 30 // 2 GiB

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

// SafeBaseName reduces a file name from the network to something that can
// only ever name a file directly inside the download directory. A peer
// supplies this string, so "../../.bashrc", an absolute path, a name that
// is all dots and a name with a newline in it all have to come out the
// other side as an ordinary file name.
func SafeBaseName(name string) string {
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
	return ClipRunes(out, 120)
}

// UniquePath is dir/name, with " (2)", " (3)" and so on appended until it
// names a file that does not exist. An incoming file never overwrites
// something already on disk.
func UniquePath(dir, name string) (string, error) {
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

// guessMIME is a content type from the extension alone. Nothing sniffs
// the bytes and nothing acts on the type; it is shown to the user so they
// can decide; nothing branches on it.
func guessMIME(name string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		return strings.SplitN(t, ";", 2)[0]
	}
	return "application/octet-stream"
}

// NewTransfer is a fresh record with the timestamp and the guessed
// content type filled in.
func NewTransfer(id TransferID, conv ConversationID, peer PeerID, name string, size int64, incoming bool, state TransferState) Transfer {
	return Transfer{
		ID: id, Conv: conv, Peer: peer, Name: name, Size: size,
		MIME: guessMIME(name), Incoming: incoming, State: state, At: time.Now(),
	}
}
