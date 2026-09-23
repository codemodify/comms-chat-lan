package chat

import "time"

// Sample data. It exists for two things only: the screenshot in the
// README, and the headless tests that need a window with something in it.
// The real app never seeds anything — a first run is genuinely empty, and
// pretending otherwise would be the first lie the app told.
//
// The sequence numbers here stand in for the server's: a store the server
// has never spoken to still has to order a conversation, and this is what
// that looks like.

// SeedSample fills a store with one room, two peers and a short
// conversation, dated relative to now so a screenshot never looks stale.
func SeedSample(s *Store) {
	self := s.Self()
	self.Nick = "sam"
	self.Color = "#2f6fd0"
	if out, err := s.SetSelf(self); err == nil {
		self = out
	}
	_, _, _ = s.JoinRoom("general")

	nadia := PeerID("11112222333344445555666677778888")
	theo := PeerID("99990000aaaabbbbccccddddeeeeffff")
	s.PutPeer(Peer{
		ID: nadia, Nick: "nadia", Color: ColorForID(nadia), Host: "kestrel",
		Addr: "192.168.1.24:47772", Rooms: []string{"general"},
		Presence: PresenceOnline, LastSeen: time.Now(),
	})
	s.PutPeer(Peer{
		ID: theo, Nick: "theo", Color: ColorForID(theo), Host: "workshop",
		Addr: "192.168.1.31:47772", Rooms: []string{"general"},
		Presence: PresenceAway, LastSeen: time.Now().Add(-4 * time.Minute),
	})

	now := time.Now()
	room := RoomConv("general")
	lines := []struct {
		from PeerID
		nick string
		body string
		ago  time.Duration
	}{
		{nadia, "nadia", "Morning. The build machine is back up — it was the switch in the cupboard, not the machine.", 46 * time.Minute},
		{self.ID, "sam", "That explains the two of us staring at logs yesterday.", 44 * time.Minute},
		{theo, "theo", "I'm going to put a label on that switch so the next person doesn't lose an afternoon to it.", 41 * time.Minute},
		{nadia, "nadia", "Please do. I'll send over the photos from the rack while I'm at it — there are four, none of them large.", 12 * time.Minute},
		{self.ID, "sam", "Go on then.", 11 * time.Minute},
	}
	for i, l := range lines {
		mine := l.from == self.ID
		state := MessageState("")
		if mine {
			state = StateDelivered
		}
		_, _ = s.Append(Message{
			ID:   MessageID("sample-" + string(rune('a'+i))),
			Conv: room, From: l.from, FromNick: l.nick, Body: l.body,
			Sent: now.Add(-l.ago), Seq: uint64(i + 1),
			Mine: mine, Read: true, State: state,
		})
	}
	_, _ = s.Append(Message{
		ID: "sample-direct", Conv: DirectConv(nadia), From: nadia, FromNick: "nadia",
		Body: "Separately: are you around at four? Ten minutes on the release plan, no slides.",
		Sent: now.Add(-6 * time.Minute), Seq: 9,
	})
}
