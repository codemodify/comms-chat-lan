package chatcore

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTwoBeaconsFindEachOther is the discovery test that actually uses the
// network stack: two beacons on a private group and port, on this machine,
// over loopback multicast. It is the case the app is tried out in first —
// two copies side by side — and it is the one that exercises SO_REUSEPORT,
// the multicast join and the packet round trip together.
//
// A machine or container with no multicast route cannot run it, so a join
// failure skips rather than fails.
func TestTwoBeaconsFindEachOther(t *testing.T) {
	tempHome(t)
	const group, port = "239.192.77.200", 47799

	newBeacon := func(nick string, id PeerID) *Beacon {
		b := &Beacon{
			Group: group, Port: port, Loopback: true,
			Interval: 100 * time.Millisecond, Expiry: time.Second,
		}
		b.Update(Announcement{ID: id, Nick: nick, Port: 47000, Presence: PresenceOnline})
		return b
	}

	aliceID := PeerID(strings.Repeat("a", 32))
	bobID := PeerID(strings.Repeat("b", 32))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	heardByAlice := map[PeerID]Announcement{}
	leftByAlice := map[PeerID]bool{}
	var runErr error

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := newBeacon("alice", aliceID).Run(ctx, DiscoveryEvents{
			Appeared: func(a Announcement) {
				mu.Lock()
				heardByAlice[a.ID] = a
				mu.Unlock()
			},
			Left: func(id PeerID) {
				mu.Lock()
				leftByAlice[id] = true
				mu.Unlock()
			},
		})
		mu.Lock()
		runErr = err
		mu.Unlock()
	}()

	bobCtx, stopBob := context.WithCancel(ctx)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = newBeacon("bob", bobID).Run(bobCtx, DiscoveryEvents{})
	}()

	found := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		a, ok := heardByAlice[bobID]
		err := runErr
		mu.Unlock()
		if err != nil {
			stopBob()
			cancel()
			wg.Wait()
			t.Skipf("no multicast here: %v", err)
		}
		if ok {
			if a.Nick != "bob" {
				t.Fatalf("heard %#v", a)
			}
			if a.Addr == "" {
				t.Fatalf("the announcement carries no address: %#v", a)
			}
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		stopBob()
		cancel()
		wg.Wait()
		t.Skip("no announcement arrived; this machine has no multicast route")
	}

	// A peer that says goodbye is gone at once, rather than after the
	// expiry: that is what the farewell packet is for.
	stopBob()
	gone := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		gone = leftByAlice[bobID]
		mu.Unlock()
		if gone {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if !gone {
		t.Fatal("alice never noticed bob leave, by goodbye or by expiry")
	}
}

// A beacon never reports the machine it is running on as a peer, however
// many packets loop back to it.
func TestABeaconIgnoresItsOwnAnnouncements(t *testing.T) {
	tempHome(t)
	id := PeerID(strings.Repeat("c", 32))
	b := &Beacon{
		Group: "239.192.77.201", Port: 47798, Loopback: true,
		Interval: 50 * time.Millisecond, Expiry: time.Second,
	}
	b.Update(Announcement{ID: id, Nick: "solo", Port: 47000, Presence: PresenceOnline})

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	var heard []PeerID
	_ = b.Run(ctx, DiscoveryEvents{Appeared: func(a Announcement) {
		mu.Lock()
		heard = append(heard, a.ID)
		mu.Unlock()
	}})
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 0 {
		t.Fatalf("a beacon heard itself: %v", heard)
	}
}
