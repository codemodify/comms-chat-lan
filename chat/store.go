package chat

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is identity, roster and history on disk. Both daemons keep one and
// they mean different things by it: the server's copy is the authority —
// the only place a message is ordered and the only copy that is complete —
// and a client daemon's copy is a cache of what the server has told it, so
// that a front end stays responsive and can still read a conversation
// while the server is unreachable.
//
// There is one implementation rather than an interface with several,
// because there is only one thing a chat store can usefully be. What varies
// is where it writes: a Store with an empty directory keeps everything in
// memory and is what the tests and the headless GUI fixtures use.
//
// History is an append-only NDJSON log per conversation. Appending a line
// is the only write on the hot path, a corrupt tail costs one message
// rather than the file, and the whole history is greppable with the tools
// already on the machine.
type Store struct {
	mu sync.RWMutex

	dir   string
	self  Identity
	peers map[PeerID]Peer

	msgs map[ConversationID][]Message
	// seen dedupes: the same message can arrive twice when a peer resends
	// one it never got an ack for.
	seen map[MessageID]bool
	// read is the count of messages in a conversation the user has seen.
	unread map[ConversationID]int
	notify NotifyPrefs
	// nextSeq is the sequence counter. On the server it is the global
	// order of every message on the LAN; a client daemon never mints one.
	// It only ever increases, including across restarts, where it is
	// restored from the highest sequence in the history.
	nextSeq uint64
}

// NewStore opens the store in dir, creating it if need be. An empty dir
// makes a memory-only store that persists nothing.
func NewStore(dir string) (*Store, error) {
	s := &Store{
		dir:    dir,
		peers:  map[PeerID]Peer{},
		msgs:   map[ConversationID][]Message{},
		seen:   map[MessageID]bool{},
		unread: map[ConversationID]int{},
		notify: DefaultNotifyPrefs(),
	}
	if dir == "" {
		s.self = normalizeIdentity(Identity{})
		return s, nil
	}
	if err := os.MkdirAll(filepath.Join(dir, "history"), 0o700); err != nil {
		return nil, err
	}
	id, idErr := LoadIdentityFrom(dir)
	s.self = id
	if err := s.loadRoster(); err != nil && !os.IsNotExist(err) {
		return s, err
	}
	if err := s.loadPrefs(); err != nil && !os.IsNotExist(err) {
		return s, err
	}
	if err := s.loadHistory(); err != nil {
		return s, err
	}
	return s, idErr
}

// NewMemoryStore is a Store that writes nothing. Tests and the headless GUI
// fixture use it; so does `comms-chatd -ephemeral`.
func NewMemoryStore() *Store {
	s, _ := NewStore("")
	return s
}

// Dir is where the store writes, or "" for a memory store.
func (s *Store) Dir() string { return s.dir }

// ---------------------------------------------------------------- identity

// Self is our own identity.
func (s *Store) Self() Identity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.self
}

// SetSelf replaces the identity, keeping the peer ID: an ID change would
// orphan every conversation on every other machine, so it is not offered.
func (s *Store) SetSelf(id Identity) (Identity, error) {
	s.mu.Lock()
	id.ID = s.self.ID
	if id.ID == "" {
		id.ID = NewPeerID()
	}
	s.self = normalizeIdentity(id)
	out := s.self
	dir := s.dir
	s.mu.Unlock()
	if dir == "" {
		return out, nil
	}
	return out, SaveIdentityTo(dir, out)
}

// NotifyPrefs is the shared notification setting.
func (s *Store) NotifyPrefs() NotifyPrefs {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.notify
}

// SetNotifyPrefs stores the notification setting.
func (s *Store) SetNotifyPrefs(p NotifyPrefs) (NotifyPrefs, error) {
	s.mu.Lock()
	s.notify = p
	dir := s.dir
	s.mu.Unlock()
	if dir == "" {
		return p, nil
	}
	return p, s.savePrefs()
}

// ------------------------------------------------------------------- peers

// Peers is the roster, most recently seen first.
func (s *Store) Peers() []Peer {
	s.mu.RLock()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		oi, oj := out[i].Presence != PresenceOffline, out[j].Presence != PresenceOffline
		if oi != oj {
			return oi
		}
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].DisplayName() < out[j].DisplayName()
	})
	return out
}

// Peer looks one peer up.
func (s *Store) Peer(id PeerID) (Peer, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.peers[id]
	return p, ok
}

// PutPeer merges what we have just learned about a peer into the roster.
// Fields the caller left empty keep the value they already had, so a
// presence update from the wire does not erase an address learned from
// the beacon. It reports whether anything actually changed.
func (s *Store) PutPeer(p Peer) bool {
	if p.ID == "" {
		return false
	}
	s.mu.Lock()
	old, had := s.peers[p.ID]
	merged := old
	merged.ID = p.ID
	if p.Nick != "" {
		merged.Nick = ClipRunes(p.Nick, 32)
	}
	if p.Color != "" && ValidColor(p.Color) {
		merged.Color = p.Color
	}
	if merged.Color == "" {
		merged.Color = ColorForID(p.ID)
	}
	if p.Host != "" {
		merged.Host = p.Host
	}
	if p.Addr != "" {
		merged.Addr = p.Addr
	}
	if p.Rooms != nil {
		merged.Rooms = CanonRooms(p.Rooms)
	}
	if p.Presence != "" {
		merged.Presence = p.Presence.Valid()
	}
	if !p.LastSeen.IsZero() {
		merged.LastSeen = p.LastSeen
	}
	merged.Blocked = merged.Blocked || p.Blocked
	changed := !had || !samePeer(old, merged)
	s.peers[p.ID] = merged
	dir := s.dir
	s.mu.Unlock()
	if changed && dir != "" {
		_ = s.saveRoster()
	}
	return changed
}

// samePeer compares two roster entries for the purpose of "should the UI
// hear about this". LastSeen is excluded: it ticks on every roster update
// and would otherwise make every peer "changed" whenever anyone typed,
// waking every front end for nothing.
func samePeer(a, b Peer) bool {
	if a.Nick != b.Nick || a.Color != b.Color || a.Host != b.Host || a.Addr != b.Addr ||
		a.Presence != b.Presence || a.Blocked != b.Blocked {
		return false
	}
	if len(a.Rooms) != len(b.Rooms) {
		return false
	}
	for i := range a.Rooms {
		if a.Rooms[i] != b.Rooms[i] {
			return false
		}
	}
	return true
}

// SetPresence records a peer's presence and reports whether it changed.
func (s *Store) SetPresence(id PeerID, pr Presence) bool {
	return s.PutPeer(Peer{ID: id, Presence: pr, LastSeen: time.Now()})
}

// BlockPeer sets or clears a peer's block flag.
func (s *Store) BlockPeer(id PeerID, blocked bool) bool {
	s.mu.Lock()
	p, ok := s.peers[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	p.Blocked = blocked
	s.peers[id] = p
	dir := s.dir
	s.mu.Unlock()
	if dir != "" {
		_ = s.saveRoster()
	}
	return true
}

// Blocked reports whether a peer is blocked.
func (s *Store) Blocked(id PeerID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peers[id].Blocked
}

// ------------------------------------------------------------------ rooms

// Rooms is every room we have joined, with how many reachable peers also
// advertise it.
func (s *Store) Rooms() []Conversation {
	s.mu.RLock()
	joined := append([]string(nil), s.self.Rooms...)
	members := map[string]int{}
	for _, p := range s.peers {
		if p.Presence == PresenceOffline || p.Blocked {
			continue
		}
		for _, r := range p.Rooms {
			members[r]++
		}
	}
	s.mu.RUnlock()

	out := make([]Conversation, 0, len(joined))
	for _, r := range joined {
		conv := RoomConv(r)
		out = append(out, Conversation{
			ID: conv, Title: "#" + r, Room: r,
			Members: members[r] + 1, // us
			Unread:  s.Unread(conv),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Room < out[j].Room })
	return out
}

// JoinRoom adds a room to our identity and advertises it. It reports the
// canonical room name and whether anything changed.
func (s *Store) JoinRoom(name string) (string, bool, error) {
	name = CanonRoom(name)
	if name == "" || len(name) > 48 {
		return "", false, fmt.Errorf("chat: room name must be 1-48 characters")
	}
	s.mu.Lock()
	for _, r := range s.self.Rooms {
		if r == name {
			s.mu.Unlock()
			return name, false, nil
		}
	}
	s.self.Rooms = append(s.self.Rooms, name)
	sort.Strings(s.self.Rooms)
	self := s.self
	dir := s.dir
	s.mu.Unlock()
	if dir == "" {
		return name, true, nil
	}
	return name, true, SaveIdentityTo(dir, self)
}

// LeaveRoom drops a room. History stays on disk.
func (s *Store) LeaveRoom(name string) (bool, error) {
	name = CanonRoom(name)
	s.mu.Lock()
	out := s.self.Rooms[:0]
	found := false
	for _, r := range s.self.Rooms {
		if r == name {
			found = true
			continue
		}
		out = append(out, r)
	}
	s.self.Rooms = append([]string(nil), out...)
	self := s.self
	dir := s.dir
	s.mu.Unlock()
	if !found || dir == "" {
		return found, nil
	}
	return found, SaveIdentityTo(dir, self)
}

// --------------------------------------------------------------- messages

// NextSeq mints the next sequence number. Only the server calls it: it is
// the one place in the application where the order of a conversation is
// decided, which is the whole reason there is a server.
func (s *Store) NextSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSeq++
	return s.nextSeq
}

// Seq is the sequence number last handed out: on the server, how far the
// conversation of the whole LAN has got, and the cursor a client that is
// up to date holds.
func (s *Store) Seq() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nextSeq
}

// Have reports whether we already hold a message. It is how a duplicate
// delivery is dropped.
func (s *Store) Have(id MessageID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.seen[id]
}

// Append files a message. A message we already have is ignored and false
// comes back, so a resend after a lost ack is free.
//
// Ordering is the server's sequence number, then the sender's clock, then
// the ID. Everyone who holds the same set of messages displays them in the
// same order, whatever order they arrived in and whatever their clocks
// say, because one machine decided that order.
func (s *Store) Append(m Message) (bool, error) {
	if m.ID == "" || m.Conv == "" {
		return false, fmt.Errorf("chat: message needs an id and a conversation")
	}
	if m.Sent.IsZero() {
		m.Sent = time.Now()
	}
	m.Body = ClipRunes(m.Body, MaxBodyRunes)

	s.mu.Lock()
	if s.seen[m.ID] {
		s.mu.Unlock()
		return false, nil
	}
	s.seen[m.ID] = true
	s.msgs[m.Conv] = insertOrdered(s.msgs[m.Conv], m)
	if !m.Mine && !m.Read && !m.System {
		s.unread[m.Conv]++
	}
	if m.Seq > s.nextSeq {
		s.nextSeq = m.Seq
	}
	dir := s.dir
	s.mu.Unlock()

	if dir == "" {
		return true, nil
	}
	return true, s.appendLine(m.Conv, m)
}

// MaxBodyRunes bounds one message. It is generous for chat and small
// enough that a peer cannot make us hold an unbounded string.
const MaxBodyRunes = 8000

// insertOrdered puts m in its place in an already-ordered slice. Messages
// almost always arrive in order, so the common case is an append.
func insertOrdered(list []Message, m Message) []Message {
	if len(list) == 0 || lessMessage(list[len(list)-1], m) {
		return append(list, m)
	}
	i := sort.Search(len(list), func(i int) bool { return !lessMessage(list[i], m) })
	list = append(list, Message{})
	copy(list[i+1:], list[i:])
	list[i] = m
	return list
}

// lessMessage is the order every copy of the application displays. A
// message that has not reached the server yet has no sequence number and
// sorts last: it is the one the person just typed, and it belongs at the
// bottom until the server says where it really goes.
func lessMessage(a, b Message) bool {
	if (a.Seq == 0) != (b.Seq == 0) {
		return b.Seq == 0
	}
	if a.Seq != b.Seq {
		return a.Seq < b.Seq
	}
	if !a.Sent.Equal(b.Sent) {
		return a.Sent.Before(b.Sent)
	}
	return a.ID < b.ID
}

// Messages is the tail of a conversation: the last limit messages, oldest
// first. A limit of 0 or less means every message.
func (s *Store) Messages(conv ConversationID, limit int) []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.msgs[conv]
	if limit > 0 && len(list) > limit {
		list = list[len(list)-limit:]
	}
	return append([]Message(nil), list...)
}

// Message looks one message up.
func (s *Store) Message(id MessageID) (Message, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, list := range s.msgs {
		for _, m := range list {
			if m.ID == id {
				return m, true
			}
		}
	}
	return Message{}, false
}

// SetState records the delivery state of a message we sent. The change is
// journalled as a state line, so a restart still shows what was delivered.
func (s *Store) SetState(id MessageID, st MessageState) (Message, bool) {
	s.mu.Lock()
	var found Message
	ok := false
	for _, list := range s.msgs {
		for i := range list {
			if list[i].ID != id {
				continue
			}
			list[i].State = st
			found, ok = list[i], true
			break
		}
		if ok {
			break
		}
	}
	dir := s.dir
	s.mu.Unlock()
	if ok && dir != "" {
		_ = s.appendState(found.Conv, id, st)
	}
	return found, ok
}

// Queued is every outgoing message still waiting for its peer, oldest
// first. The node walks it whenever a peer comes back.
func (s *Store) Queued(conv ConversationID) []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Message
	pick := func(list []Message) {
		for _, m := range list {
			if m.Mine && (m.State == StateQueued || m.State == StateSending) {
				out = append(out, m)
			}
		}
	}
	if conv != "" {
		pick(s.msgs[conv])
	} else {
		for _, list := range s.msgs {
			pick(list)
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessMessage(out[i], out[j]) })
	return out
}

// Since is every message the store holds with a sequence number above
// cursor, in sequence order, at most limit of them (0 for all). It is how
// the server answers "what did I miss": one walk of the history, filtered
// by the caller to what that client may see.
func (s *Store) Since(cursor uint64, limit int) []Message {
	s.mu.RLock()
	var out []Message
	for _, list := range s.msgs {
		for _, m := range list {
			if m.Seq > cursor {
				out = append(out, m)
			}
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return lessMessage(out[i], out[j]) })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Convs is every conversation the store holds messages for.
func (s *Store) Convs() []ConversationID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ConversationID, 0, len(s.msgs))
	for conv := range s.msgs {
		out = append(out, conv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Confirm records that the server has accepted a message we composed: it
// gets the sequence number that fixes its place in the conversation, and
// the delivered state. It is the moment an optimistic local copy becomes
// the same message everybody else has.
func (s *Store) Confirm(id MessageID, seq uint64) (Message, bool) {
	s.mu.Lock()
	var found Message
	ok := false
	for conv, list := range s.msgs {
		for i := range list {
			if list[i].ID != id {
				continue
			}
			list[i].Seq = seq
			list[i].State = StateDelivered
			found, ok = list[i], true
			// The sequence number may move it: a message composed while
			// the server was down sorts after everything that reached the
			// server first.
			rest := append(list[:i:i], list[i+1:]...)
			s.msgs[conv] = insertOrdered(rest, found)
			break
		}
		if ok {
			break
		}
	}
	if seq > s.nextSeq {
		s.nextSeq = seq
	}
	dir := s.dir
	s.mu.Unlock()
	if ok && dir != "" {
		_ = s.appendLog(found.Conv, logLine{Kind: "q", ID: id, Seq: seq, State: StateDelivered})
	}
	return found, ok
}

// RoomMembers is every peer the roster has in a room.
func (s *Store) RoomMembers(room string) []Peer {
	room = CanonRoom(room)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Peer
	for _, p := range s.peers {
		for _, r := range p.Rooms {
			if r == room {
				out = append(out, p)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Unread is how many unread messages a conversation holds.
func (s *Store) Unread(conv ConversationID) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.unread[conv]
}

// MarkRead clears a conversation's unread count and returns what it was.
func (s *Store) MarkRead(conv ConversationID) int {
	s.mu.Lock()
	n := s.unread[conv]
	delete(s.unread, conv)
	for i := range s.msgs[conv] {
		s.msgs[conv][i].Read = true
	}
	dir := s.dir
	s.mu.Unlock()
	if n > 0 && dir != "" {
		_ = s.appendRead(conv)
	}
	return n
}

// Conversations is the conversation list: every peer we know plus every
// room we have joined, most recent first, with the last line and the
// unread count already filled in — one round trip builds the whole sidebar.
func (s *Store) Conversations() []Conversation {
	rooms := s.Rooms()
	peers := s.Peers()

	s.mu.RLock()
	out := make([]Conversation, 0, len(peers)+len(rooms))
	for _, p := range peers {
		conv := DirectConv(p.ID)
		c := Conversation{
			ID: conv, Title: p.DisplayName(), Peer: p.ID,
			Unread: s.unread[conv], Presence: p.Presence.Valid(), Color: p.Color,
		}
		if list := s.msgs[conv]; len(list) > 0 {
			last := list[len(list)-1]
			c.Last, c.LastAt = last.Body, last.Sent
		}
		// A peer we have never spoken to and that is not here right now is
		// not a conversation, it is a memory. Keep it out of the list.
		if c.Last == "" && p.Presence == PresenceOffline {
			continue
		}
		out = append(out, c)
	}
	for _, r := range rooms {
		if list := s.msgs[r.ID]; len(list) > 0 {
			last := list[len(list)-1]
			r.Last, r.LastAt = last.Body, last.Sent
		}
		out = append(out, r)
	}
	s.mu.RUnlock()

	sort.SliceStable(out, func(i, j int) bool {
		// Anything with unread first, then most recent, then rooms before
		// silent peers so the room list does not sink out of sight.
		ui, uj := out[i].Unread > 0, out[j].Unread > 0
		if ui != uj {
			return ui
		}
		if !out[i].LastAt.Equal(out[j].LastAt) {
			return out[i].LastAt.After(out[j].LastAt)
		}
		if (out[i].Room != "") != (out[j].Room != "") {
			return out[i].Room != ""
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// Conversation looks one conversation up, synthesising it from the roster
// or the room list when it holds no messages yet.
func (s *Store) Conversation(id ConversationID) (Conversation, bool) {
	for _, c := range s.Conversations() {
		if c.ID == id {
			return c, true
		}
	}
	if id.IsRoom() {
		room := id.Room()
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, r := range s.self.Rooms {
			if r == room {
				return Conversation{ID: id, Title: "#" + room, Room: room}, true
			}
		}
		return Conversation{}, false
	}
	pid := PeerID(strings.TrimPrefix(string(id), "peer:"))
	if p, ok := s.Peer(pid); ok {
		return Conversation{ID: id, Title: p.DisplayName(), Peer: pid, Color: p.Color, Presence: p.Presence}, true
	}
	return Conversation{}, false
}

// --------------------------------------------------------------- on disk

func (s *Store) rosterPath() string { return filepath.Join(s.dir, "roster.json") }
func (s *Store) prefsPath() string  { return filepath.Join(s.dir, "prefs.json") }

// historyPath is the log of one conversation. The conversation ID is
// already a safe file name (hex peer IDs or a folded room name), but it
// comes off the network, so it is sanitised rather than trusted.
func (s *Store) historyPath(conv ConversationID) string {
	return filepath.Join(s.dir, "history", safeFileName(string(conv))+".ndjson")
}

// safeFileName keeps [a-z0-9._-] and folds everything else to "_", so a
// conversation ID can never escape the history directory or name a device.
func safeFileName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ':' || r == '.':
			b.WriteByte('_')
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "_" {
		return "unnamed"
	}
	return ClipRunes(out, 120)
}

// logLine is one line of a history file. A message line carries "m"; the
// other kinds amend an earlier line without rewriting the file.
type logLine struct {
	Kind  string       `json:"k"`
	Msg   *Message     `json:"m,omitempty"`
	ID    MessageID    `json:"id,omitempty"`
	State MessageState `json:"s,omitempty"`
	// Seq amends a message the server has since sequenced, on a "q" line.
	// It is journalled rather than rewritten in place for the same reason
	// everything else here is: one append is the only write on the path.
	Seq uint64 `json:"q,omitempty"`
}

func (s *Store) appendLine(conv ConversationID, m Message) error {
	return s.appendLog(conv, logLine{Kind: "m", Msg: &m})
}

func (s *Store) appendState(conv ConversationID, id MessageID, st MessageState) error {
	return s.appendLog(conv, logLine{Kind: "s", ID: id, State: st})
}

func (s *Store) appendRead(conv ConversationID) error {
	return s.appendLog(conv, logLine{Kind: "r"})
}

func (s *Store) appendLog(conv ConversationID, l logLine) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.historyPath(conv), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (s *Store) loadHistory() error {
	dir := filepath.Join(s.dir, "history")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ndjson") {
			continue
		}
		if err := s.loadLog(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("chat: %s: %w", e.Name(), err)
		}
	}
	return nil
}

func (s *Store) loadLog(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var conv ConversationID
	states := map[MessageID]MessageState{}
	seqs := map[MessageID]uint64{}
	read := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var l logLine
		if err := json.Unmarshal(line, &l); err != nil {
			// A half-written last line is the normal shape of a crash.
			// Losing that one message beats refusing to start.
			continue
		}
		switch l.Kind {
		case "m":
			if l.Msg == nil || l.Msg.ID == "" {
				continue
			}
			m := *l.Msg
			conv = m.Conv
			s.seen[m.ID] = true
			s.msgs[m.Conv] = insertOrdered(s.msgs[m.Conv], m)
			if m.Seq > s.nextSeq {
				s.nextSeq = m.Seq
			}
		case "s":
			states[l.ID] = l.State
		case "q":
			seqs[l.ID] = l.Seq
			if l.State != "" {
				states[l.ID] = l.State
			}
			if l.Seq > s.nextSeq {
				s.nextSeq = l.Seq
			}
		case "r":
			read = true
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if conv == "" {
		return nil
	}
	list := s.msgs[conv]
	resort := false
	for i := range list {
		if seq, ok := seqs[list[i].ID]; ok && list[i].Seq != seq {
			list[i].Seq = seq
			resort = true
		}
		if st, ok := states[list[i].ID]; ok {
			list[i].State = st
		}
		// A message we sent that was still in flight when the daemon
		// stopped is queued again, not silently lost.
		if list[i].Mine && list[i].State == StateSending {
			list[i].State = StateQueued
		}
		if read {
			list[i].Read = true
		}
	}
	if resort {
		sort.Slice(list, func(i, j int) bool { return lessMessage(list[i], list[j]) })
	}
	if !read {
		n := 0
		for _, m := range list {
			if !m.Mine && !m.System {
				n++
			}
		}
		s.unread[conv] = n
	}
	return nil
}

func (s *Store) saveRoster() error {
	s.mu.RLock()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		// Presence and address are facts about right now; a roster read
		// back tomorrow must not claim everyone is online.
		p.Presence, p.Addr = PresenceOffline, ""
		out = append(out, p)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.rosterPath(), append(b, '\n'), 0o600)
}

func (s *Store) loadRoster() error {
	b, err := os.ReadFile(s.rosterPath())
	if err != nil {
		return err
	}
	var in []Peer
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	for _, p := range in {
		if p.ID == "" {
			continue
		}
		p.Presence = PresenceOffline
		p.Addr = ""
		if !ValidColor(p.Color) {
			p.Color = ColorForID(p.ID)
		}
		s.peers[p.ID] = p
	}
	return nil
}

func (s *Store) savePrefs() error {
	s.mu.RLock()
	p := s.notify
	s.mu.RUnlock()
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.prefsPath(), append(b, '\n'), 0o600)
}

func (s *Store) loadPrefs() error {
	b, err := os.ReadFile(s.prefsPath())
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.notify)
}
