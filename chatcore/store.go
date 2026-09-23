package chatcore

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

// Store is everything comms-chatd remembers: the identity, the roster of
// peers it has seen and the history of every conversation.
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
	// nextSeq is our own send counter. It only ever increases, including
	// across restarts (it is restored from our own last message).
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
	id, idErr := LoadIdentity()
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

// Backend names the store for status.get.
func (s *Store) Backend() string {
	if s.dir == "" {
		return "memory"
	}
	return "disk"
}

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
	return out, SaveIdentity(out)
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
		merged.Nick = clipRunes(p.Nick, 32)
	}
	if p.Color != "" && validColor(p.Color) {
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
		merged.Rooms = canonRooms(p.Rooms)
	}
	if p.Presence != "" {
		merged.Presence = p.Presence.Valid()
	}
	if !p.LastSeen.IsZero() {
		merged.LastSeen = p.LastSeen
	}
	merged.Known = merged.Known || p.Known
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
// hear about this". LastSeen is excluded: it ticks on every mDNS
// announcement and would otherwise make every peer "changed" twice a
// minute, waking every front end for nothing.
func samePeer(a, b Peer) bool {
	if a.Nick != b.Nick || a.Color != b.Color || a.Host != b.Host || a.Addr != b.Addr ||
		a.Presence != b.Presence || a.Known != b.Known || a.Blocked != b.Blocked {
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
	return name, true, SaveIdentity(self)
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
	return found, SaveIdentity(self)
}

// --------------------------------------------------------------- messages

// NextSeq mints our next send sequence number.
func (s *Store) NextSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSeq++
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
// Ordering is (Sent, Seq, ID): every peer that holds the same set of
// messages displays them in the same order, whatever order they arrived in,
// without anyone having to be the server.
func (s *Store) Append(m Message) (bool, error) {
	if m.ID == "" || m.Conv == "" {
		return false, fmt.Errorf("chat: message needs an id and a conversation")
	}
	if m.Sent.IsZero() {
		m.Sent = time.Now()
	}
	m.Body = clipRunes(m.Body, maxBodyRunes)

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
	if m.Mine && m.Seq > s.nextSeq {
		s.nextSeq = m.Seq
	}
	dir := s.dir
	s.mu.Unlock()

	if dir == "" {
		return true, nil
	}
	return true, s.appendLine(m.Conv, m)
}

// maxBodyRunes bounds one message. It is generous for chat and small
// enough that a peer cannot make us hold an unbounded string.
const maxBodyRunes = 8000

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

func lessMessage(a, b Message) bool {
	if !a.Sent.Equal(b.Sent) {
		return a.Sent.Before(b.Sent)
	}
	if a.Seq != b.Seq {
		return a.Seq < b.Seq
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
// already a safe file name (a hex peer ID or a folded room name), but it
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
	return clipRunes(out, 120)
}

// logLine is one line of a history file. A message line carries "m"; the
// other kinds amend an earlier line without rewriting the file.
type logLine struct {
	Kind  string       `json:"k"`
	Msg   *Message     `json:"m,omitempty"`
	ID    MessageID    `json:"id,omitempty"`
	State MessageState `json:"s,omitempty"`
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
			if m.Mine && m.Seq > s.nextSeq {
				s.nextSeq = m.Seq
			}
		case "s":
			states[l.ID] = l.State
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
	for i := range list {
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
		if !validColor(p.Color) {
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

// hasQueuedFor reports whether anything is waiting to go to one peer. It
// is the cheap check the node makes on every announcement, before it
// spends a goroutine on a flush that would usually find nothing.
func (s *Store) hasQueuedFor(id PeerID) bool {
	conv := DirectConv(id)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.msgs[conv] {
		if m.Mine && (m.State == StateQueued || m.State == StateSending) {
			return true
		}
	}
	return false
}
