package chat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Environment variables. Everything the app needs can be overridden, which
// is what lets a test run a whole second instance beside the real one.
const (
	// EnvHome overrides the data directory (history, roster, identity).
	EnvHome = "UITK_CHAT_HOME"
	// EnvSock overrides clientd's Unix socket path.
	EnvSock = "UITK_CHAT_SOCK"
	// EnvServer tells a client where the server is ("host" or
	// "host:port"), for a network where the beacon does not cross the
	// segment. It overrides discovery.
	EnvServer = "UITK_CHAT_SERVER"
	// EnvPort pins the port the server listens on (default:
	// chatwire.DefaultServerPort).
	EnvPort = "UITK_CHAT_PORT"
	// EnvNick overrides the advertised nickname for this run.
	EnvNick = "UITK_CHAT_NICK"
	// EnvNoDiscovery=1 starts either daemon without the beacon: the server
	// announces nothing, a client listens for nothing and has to be told
	// where the server is. Tests use it, and so does anyone on a network
	// where multicast is unwelcome.
	EnvNoDiscovery = "UITK_CHAT_NO_DISCOVERY"
	// EnvIface pins the multicast interface by name.
	EnvIface = "UITK_CHAT_IFACE"
	// EnvNoNotify suppresses desktop notifications.
	EnvNoNotify = "UITK_CHAT_NO_NOTIFY"
)

// DataDir is where history, the roster and identity.json live:
// $UITK_CHAT_HOME, else $XDG_DATA_HOME/comms-chat-lan, else
// ~/.local/share/comms-chat-lan.
func DataDir() string {
	if p := strings.TrimSpace(os.Getenv(EnvHome)); p != "" {
		return p
	}
	if d := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); d != "" {
		return filepath.Join(d, "comms-chat-lan")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "comms-chat-lan-"+strconv.Itoa(os.Getuid()))
	}
	return filepath.Join(home, ".local", "share", "comms-chat-lan")
}

// DefaultSocket is the comms-chatd listen path: $UITK_CHAT_SOCK, else
// $XDG_RUNTIME_DIR/comms-chatd.sock, else a per-uid path under the temp
// directory.
func DefaultSocket() string {
	if p := strings.TrimSpace(os.Getenv(EnvSock)); p != "" {
		return p
	}
	if dir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); dir != "" {
		return filepath.Join(dir, "comms-chatd.sock")
	}
	return filepath.Join(os.TempDir(), "comms-chatd-"+strconv.Itoa(os.Getuid())+".sock")
}

var identityMu sync.Mutex

// identityPath is <dir>/identity.json. It is a function of the store's
// directory rather than of [DataDir], so a second instance pointed at a
// directory of its own — a test, the screenshot tool, a daemon started
// with -data — has an identity of its own too, and cannot overwrite the
// one belonging to the copy the user actually runs.
func identityPath(dir string) string { return filepath.Join(dir, "identity.json") }

// LoadIdentity reads the identity in [DataDir].
func LoadIdentity() (Identity, error) { return LoadIdentityFrom(DataDir()) }

// SaveIdentity writes the identity in [DataDir].
func SaveIdentity(id Identity) error { return SaveIdentityTo(DataDir(), id) }

// LoadIdentityFrom reads dir/identity.json, creating one on first run: a
// fresh random ID, the login name as a nickname and a colour derived from
// the ID so the same peer is the same colour on every machine.
func LoadIdentityFrom(dir string) (Identity, error) {
	identityMu.Lock()
	defer identityMu.Unlock()

	path := identityPath(dir)
	b, err := os.ReadFile(path)
	if err == nil {
		var id Identity
		if err := json.Unmarshal(b, &id); err != nil {
			return Identity{}, fmt.Errorf("chat: %s: %w", path, err)
		}
		if id.ID != "" {
			return normalizeIdentity(id), nil
		}
	} else if !os.IsNotExist(err) {
		return Identity{}, err
	}

	fresh := normalizeIdentity(Identity{ID: NewPeerID(), Nick: defaultNick()})
	if err := saveIdentityLocked(dir, fresh); err != nil {
		// A read-only home should not stop the app: run with an identity
		// that lives only for this process and say so.
		return fresh, fmt.Errorf("chat: identity is not persisted: %w", err)
	}
	return fresh, nil
}

// SaveIdentityTo writes dir/identity.json.
func SaveIdentityTo(dir string, id Identity) error {
	identityMu.Lock()
	defer identityMu.Unlock()
	return saveIdentityLocked(dir, normalizeIdentity(id))
}

func saveIdentityLocked(dir string, id Identity) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(identityPath(dir), append(b, '\n'), 0o600)
}

func normalizeIdentity(id Identity) Identity {
	if id.ID == "" {
		id.ID = NewPeerID()
	}
	if strings.TrimSpace(id.Nick) == "" {
		id.Nick = defaultNick()
	}
	if n := strings.TrimSpace(os.Getenv(EnvNick)); n != "" {
		id.Nick = n
	}
	id.Nick = ClipRunes(strings.TrimSpace(id.Nick), 32)
	if !ValidColor(id.Color) {
		id.Color = ColorForID(id.ID)
	}
	id.Presence = id.Presence.Valid()
	if id.Presence == PresenceOffline {
		id.Presence = PresenceOnline
	}
	id.Rooms = CanonRooms(id.Rooms)
	return id
}

// CanonRooms folds a room list to canonical, deduplicated names.
func CanonRooms(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, r := range in {
		r = CanonRoom(r)
		if r == "" || seen[r] || len(r) > 48 {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func defaultNick() string {
	if u, err := user.Current(); err == nil {
		if n := strings.TrimSpace(u.Username); n != "" {
			if host, err := os.Hostname(); err == nil && host != "" {
				return n + "@" + strings.TrimSuffix(host, ".local")
			}
			return n
		}
	}
	return "someone"
}

// NewPeerID mints a fresh random peer ID.
func NewPeerID() PeerID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on Linux; if it ever does, a
		// predictable ID is worse than stopping, so panic loudly.
		panic("chat: crypto/rand: " + err.Error())
	}
	return PeerID(hex.EncodeToString(b[:]))
}

// avatarPalette is the set of avatar colours. They are picked to stay
// legible as a filled circle with white text on both a light and a dark
// window background.
var avatarPalette = []string{
	"#2f6fd0", "#0f8f6f", "#b5651d", "#8b4fbf", "#c0392b",
	"#1f8fa8", "#7a8b1f", "#d0507f", "#4f5f8f", "#a07000",
}

// ColorForID is the avatar colour of a peer: a function of the ID alone, so
// every machine on the LAN draws the same peer in the same colour without
// anyone having to agree on it.
func ColorForID(id PeerID) string {
	var sum uint32
	for i := 0; i < len(id); i++ {
		sum = sum*31 + uint32(id[i])
	}
	return avatarPalette[int(sum%uint32(len(avatarPalette)))]
}

// ValidColor reports whether s is a "#rrggbb" colour.
func ValidColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	_, err := hex.DecodeString(s[1:])
	return err == nil
}

// ClipRunes truncates s to at most n runes (never mid-rune).
func ClipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// writeFileAtomic writes through a temp file in the same directory, so a
// crash mid-write leaves the old file rather than half the new one.
func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AvatarColors is the palette a person may pick from in Preferences. It
// is the same list every peer's colour is derived from, so a chosen
// colour and a derived one are drawn the same way.
func AvatarColors() []string {
	return append([]string(nil), avatarPalette...)
}
