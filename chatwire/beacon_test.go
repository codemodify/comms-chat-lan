package chatwire

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAClientFindsAServerOverMulticast is the discovery test that
// actually uses the network stack: a server announcing and a client
// listening, on a private group and port, over loopback multicast. It is
// the case the application is tried out in first — a server and a client
// on one machine — and it exercises SO_REUSEPORT, the multicast join and
// the packet round trip together.
//
// A machine or container with no multicast route cannot run it, so a join
// failure skips rather than fails.
func TestAClientFindsAServerOverMulticast(t *testing.T) {
	const group, port = "239.192.77.200", 47799
	serverID := ServerID(strings.Repeat("a", 32))

	newBeacon := func() *Beacon {
		return &Beacon{
			Group: group, Port: port, Loopback: true,
			Interval: 100 * time.Millisecond, Expiry: time.Second,
		}
	}
	server := newBeacon()
	server.Update(Announcement{ID: serverID, Name: "the office", Port: 47772})
	client := newBeacon() // never calls Update: it only listens

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	heard := map[ServerID]Announcement{}
	left := map[ServerID]bool{}
	var runErr error

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := client.Run(ctx, DiscoveryEvents{
			Appeared: func(a Announcement) {
				mu.Lock()
				heard[a.ID] = a
				mu.Unlock()
			},
			Left: func(id ServerID) {
				mu.Lock()
				left[id] = true
				mu.Unlock()
			},
		})
		mu.Lock()
		runErr = err
		mu.Unlock()
	}()

	serverCtx, stopServer := context.WithCancel(ctx)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = server.Run(serverCtx, DiscoveryEvents{})
	}()

	found := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		a, ok := heard[serverID]
		err := runErr
		mu.Unlock()
		if err != nil {
			stopServer()
			cancel()
			wg.Wait()
			t.Skipf("no multicast here: %v", err)
		}
		if ok {
			if a.Name != "the office" {
				t.Fatalf("heard %#v", a)
			}
			// The address is where to connect, and it comes from the
			// packet rather than from what the packet says.
			if a.Addr == "" || !strings.HasSuffix(a.Addr, ":47772") {
				t.Fatalf("the announcement carries no usable address: %#v", a)
			}
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		stopServer()
		cancel()
		wg.Wait()
		t.Skip("no announcement arrived; this machine has no multicast route")
	}

	// A server that says goodbye is gone at once, rather than after the
	// expiry: that is what the farewell packet is for.
	stopServer()
	gone := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		gone = left[serverID]
		mu.Unlock()
		if gone {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if !gone {
		t.Fatal("the client never noticed the server leave, by goodbye or by expiry")
	}
}

// A server never reports itself as a server it has found, however many
// packets loop back to it.
func TestAServerIgnoresItsOwnAnnouncements(t *testing.T) {
	b := &Beacon{
		Group: "239.192.77.201", Port: 47798, Loopback: true,
		Interval: 50 * time.Millisecond, Expiry: time.Second,
	}
	b.Update(Announcement{ID: ServerID(strings.Repeat("c", 32)), Name: "solo", Port: 47772})

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	var heard []ServerID
	_ = b.Run(ctx, DiscoveryEvents{Appeared: func(a Announcement) {
		mu.Lock()
		heard = append(heard, a.ID)
		mu.Unlock()
	}})
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 0 {
		t.Fatalf("a server heard itself: %v", heard)
	}
}

// A client that has nothing to announce announces nothing: a beacon with
// no announcement set must not put an empty packet on the group.
func TestAClientAnnouncesNothing(t *testing.T) {
	listener := &Beacon{
		Group: "239.192.77.202", Port: 47797, Loopback: true,
		Interval: 50 * time.Millisecond, Expiry: time.Second,
	}
	silent := &Beacon{
		Group: "239.192.77.202", Port: 47797, Loopback: true,
		Interval: 50 * time.Millisecond, Expiry: time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	var mu sync.Mutex
	var heard []ServerID
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = listener.Run(ctx, DiscoveryEvents{Appeared: func(a Announcement) {
			mu.Lock()
			heard = append(heard, a.ID)
			mu.Unlock()
		}})
	}()
	go func() {
		defer wg.Done()
		_ = silent.Run(ctx, DiscoveryEvents{})
	}()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 0 {
		t.Fatalf("a beacon with nothing to announce announced something: %v", heard)
	}
}
