package chatclientd

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/codemodify/comms-chat-lan/chatwire"
)

// The cursor: how far through the server's sequence this machine has got.
//
// It is kept beside the history rather than inferred from it, because the
// two are not the same thing. The history is what we were told; the
// cursor is what we were told we had been told, and after a reset or a
// room backfill the highest sequence in the history is not it.
//
// It is stored with the server it belongs to. A position in one server's
// sequence means nothing in another's, so finding a different server
// throws the number away rather than carrying it across.

type cursorFile struct {
	Server chatwire.ServerID `json:"server"`
	Seq    uint64            `json:"seq"`
}

func cursorPath(dir string) string { return filepath.Join(dir, "cursor.json") }

// loadCursor reads the cursor, or returns a zero one — which is not an
// error, it is a client that has never enrolled anywhere.
func loadCursor(dir string) (uint64, chatwire.ServerID) {
	if dir == "" {
		return 0, ""
	}
	b, err := os.ReadFile(cursorPath(dir))
	if err != nil {
		return 0, ""
	}
	var c cursorFile
	if err := json.Unmarshal(b, &c); err != nil {
		return 0, ""
	}
	return c.Seq, c.Server
}

// saveCursor writes the cursor. It is best effort: losing it costs one
// resume that replays a window instead of a delta, which is the same
// conversation either way.
func (d *Daemon) saveCursor() {
	dir := d.Store.Dir()
	if dir == "" {
		return
	}
	d.mu.Lock()
	c := cursorFile{Server: d.serverID, Seq: d.cursor}
	d.mu.Unlock()
	if c.Server == "" {
		return
	}
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = os.WriteFile(cursorPath(dir), append(b, '\n'), 0o600)
}
