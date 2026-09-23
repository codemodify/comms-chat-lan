package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tempHome points the whole package at a directory of its own, so a test
// can never read or write the running user's real history.
func tempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "xdg"))
	return dir
}

// The server's sequence number is the order, not anybody's clock. This
// is the whole reason the application grew a server: two machines whose
// clocks disagree used to show the same conversation in two orders, and
// there was nobody to ask which was right.
func TestStoreOrdersMessagesByTheServersSequenceNotByAnyClock(t *testing.T) {
	tempHome(t)
	s := NewMemoryStore()
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	conv := RoomConv("general")

	// Deliberately out of order, and with one sender whose clock is an
	// hour fast and another whose clock is a minute slow.
	in := []Message{
		{ID: "c", Conv: conv, Sent: base.Add(-time.Minute), Seq: 3},
		{ID: "a", Conv: conv, Sent: base.Add(time.Hour), Seq: 1},
		{ID: "d", Conv: conv, Sent: base, Seq: 4},
		{ID: "b", Conv: conv, Sent: base, Seq: 2},
	}
	for _, m := range in {
		if _, err := s.Append(m); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, m := range s.Messages(conv, 0) {
		got = append(got, string(m.ID))
	}
	if want := "a b c d"; strings.Join(got, " ") != want {
		t.Fatalf("order %q, want %q", strings.Join(got, " "), want)
	}
}

// A message that has not reached the server has no sequence number, and
// belongs at the bottom until the server says where it really goes.
func TestAnUnsequencedMessageSitsAtTheEndUntilItIsConfirmed(t *testing.T) {
	tempHome(t)
	s := NewMemoryStore()
	conv := RoomConv("general")
	for i, seq := range []uint64{7, 8} {
		if _, err := s.Append(Message{
			ID: MessageID(string(rune('a' + i))), Conv: conv, Seq: seq,
			Sent: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Composed while the server was down: no sequence, oldest clock.
	if _, err := s.Append(Message{
		ID: "mine", Conv: conv, Mine: true, State: StateQueued,
		Sent: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	list := s.Messages(conv, 0)
	if list[len(list)-1].ID != "mine" {
		t.Fatalf("the unsent message is not last: %v", ids(list))
	}

	// The server accepts it, and it takes its real place.
	if _, ok := s.Confirm("mine", 9); !ok {
		t.Fatal("confirm found nothing to confirm")
	}
	list = s.Messages(conv, 0)
	if got := strings.Join(ids(list), " "); got != "a b mine" {
		t.Fatalf("order after confirmation %q", got)
	}
	if list[2].State != StateDelivered {
		t.Fatalf("state after confirmation %q", list[2].State)
	}
	if s.Seq() != 9 {
		t.Fatalf("the store's sequence is %d, want 9", s.Seq())
	}
}

func ids(list []Message) []string {
	var out []string
	for _, m := range list {
		out = append(out, string(m.ID))
	}
	return out
}

func TestStoreIgnoresADuplicateDelivery(t *testing.T) {
	tempHome(t)
	s := NewMemoryStore()
	m := Message{ID: "x", Conv: RoomConv("general"), Body: "hello", Sent: time.Now()}
	added, err := s.Append(m)
	if err != nil || !added {
		t.Fatalf("first append: added=%v err=%v", added, err)
	}
	// A resend after a lost ack must be free rather than doubled.
	added, err = s.Append(m)
	if err != nil || added {
		t.Fatalf("second append: added=%v err=%v", added, err)
	}
	if n := len(s.Messages(m.Conv, 0)); n != 1 {
		t.Fatalf("%d messages, want 1", n)
	}
}

func TestStoreCountsUnreadUntilTheConversationIsRead(t *testing.T) {
	tempHome(t)
	s := NewMemoryStore()
	conv := DirectConv("aaaa")
	for i := 0; i < 3; i++ {
		if _, err := s.Append(Message{ID: MessageID(string(rune('a' + i))), Conv: conv, Sent: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	// Our own messages and the app's own system lines never count.
	_, _ = s.Append(Message{ID: "mine", Conv: conv, Mine: true, Sent: time.Now()})
	_, _ = s.Append(Message{ID: "sys", Conv: conv, System: true, Sent: time.Now()})

	if got := s.Unread(conv); got != 3 {
		t.Fatalf("unread %d, want 3", got)
	}
	if got := s.MarkRead(conv); got != 3 {
		t.Fatalf("markRead cleared %d, want 3", got)
	}
	if got := s.Unread(conv); got != 0 {
		t.Fatalf("unread after read %d, want 0", got)
	}
}

func TestStoreSurvivesARestart(t *testing.T) {
	home := tempHome(t)
	dir := filepath.Join(home, "data")
	conv := DirectConv("beef")

	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(Message{
		ID: "one", Conv: conv, Body: "still here", Sent: time.Now(), Mine: true,
		Seq: 7, State: StateSending,
	}); err != nil {
		t.Fatal(err)
	}
	s.PutPeer(Peer{ID: "beef", Nick: "sam", Presence: PresenceOnline, Addr: "10.0.0.5:47772"})

	again, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	msgs := again.Messages(conv, 0)
	if len(msgs) != 1 || msgs[0].Body != "still here" {
		t.Fatalf("reloaded %#v", msgs)
	}
	// A message that was on the wire when the daemon stopped is queued
	// again rather than quietly lost.
	if msgs[0].State != StateQueued {
		t.Fatalf("state %q, want %q", msgs[0].State, StateQueued)
	}
	// The sequence counter must not restart, or a restart would mint ids
	// that sort before messages already sent.
	if next := again.NextSeq(); next <= 7 {
		t.Fatalf("next sequence %d, want more than 7", next)
	}
	p, ok := again.Peer("beef")
	if !ok || p.Nick != "sam" {
		t.Fatalf("peer %#v ok=%v", p, ok)
	}
	// Presence and address are facts about right now: a roster read back
	// tomorrow must not claim everyone is online at yesterday's address.
	if p.Presence != PresenceOffline || p.Addr != "" {
		t.Fatalf("stale liveness survived: presence=%q addr=%q", p.Presence, p.Addr)
	}
}

func TestHistoryFileNameCannotEscapeTheHistoryDirectory(t *testing.T) {
	home := tempHome(t)
	dir := filepath.Join(home, "data")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A room name arrives over an unauthenticated multicast socket, so it
	// gets the treatment a file name from the network deserves.
	nasty := ConversationID("room:../../../../tmp/owned")
	if _, err := s.Append(Message{ID: "x", Conv: nasty, Sent: time.Now()}); err != nil {
		t.Fatal(err)
	}
	path := s.historyPath(nasty)
	if filepath.Dir(path) != filepath.Join(dir, "history") {
		t.Fatalf("history file landed at %s", path)
	}
	if strings.Contains(filepath.Base(path), "/") || strings.Contains(filepath.Base(path), "..") {
		t.Fatalf("unsafe base name %q", filepath.Base(path))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("history file not written: %v", err)
	}
}

func TestSafeBaseNameDefangsAnIncomingFileName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":             "report.pdf",
		"../../../.bashrc":       "bashrc",
		"/etc/passwd":            "passwd",
		`..\..\windows\me.exe`:   "me.exe",
		"...":                    "file",
		"":                       "file",
		"with\nnewline.txt":      "withnewline.txt",
		"C:notes.txt":            "C_notes.txt",
		strings.Repeat("a", 400): strings.Repeat("a", 120),
	}
	for in, want := range cases {
		if got := SafeBaseName(in); got != want {
			t.Errorf("SafeBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAcceptedFileNeverOverwritesAnExistingOne(t *testing.T) {
	dir := t.TempDir()
	first, err := UniquePath(dir, "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	second, err := UniquePath(dir, "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both transfers landed on %s", first)
	}
	if filepath.Base(second) != "photo (2).png" {
		t.Fatalf("second file is %q", filepath.Base(second))
	}
}

func TestRoomsAreTheSameRoomWhateverTheCase(t *testing.T) {
	tempHome(t)
	s := NewMemoryStore()
	if _, _, err := s.JoinRoom("  General "); err != nil {
		t.Fatal(err)
	}
	_, changed, err := s.JoinRoom("GENERAL")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("joining the same room twice made a second room")
	}
	if got := RoomConv("General"); got != RoomConv("general ") {
		t.Fatalf("%q and %q are different conversations", got, RoomConv("general "))
	}
	if n := len(s.Rooms()); n != 1 {
		t.Fatalf("%d rooms, want 1", n)
	}
}

func TestAvatarColourIsTheSameOnEveryMachine(t *testing.T) {
	// It is a pure function of the id, which is the whole point: no peer
	// has to be asked what colour it is.
	id := PeerID("0123456789abcdef0123456789abcdef")
	if a, b := ColorForID(id), ColorForID(id); a != b {
		t.Fatalf("%s != %s", a, b)
	}
	if !ValidColor(ColorForID(id)) {
		t.Fatalf("%q is not a colour", ColorForID(id))
	}
}
