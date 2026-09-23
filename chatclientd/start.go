package chatclientd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatwire"
)

// Open builds the client daemon comms-chat-lan-clientd runs from the
// environment: the cache in dir (or memory, when dir is empty) and the
// beacon listener, unless UITK_CHAT_NO_DISCOVERY says otherwise or an
// address was given, in which case there is nothing to discover.
func Open(dir, addr string) (*Daemon, error) {
	store, err := chat.NewStore(dir)
	if err != nil && store == nil {
		return nil, err
	}
	var disc chatwire.Discovery = chatwire.NewBeacon()
	off := strings.TrimSpace(os.Getenv(chat.EnvNoDiscovery))
	if (off != "" && off != "0") || strings.TrimSpace(addr) != "" {
		disc = chatwire.NoDiscovery()
	}
	d := New(store, disc)
	d.Addr = strings.TrimSpace(addr)
	return d, err
}

// StartInProcess runs a whole client daemon — cache, link and RPC — on a
// private socket in a temporary directory, and returns the socket path
// and a stop function. It is what the tests, the screenshot tool and the
// single-process demo use; it is not how the application is normally run.
//
// Discovery is off unless disc says otherwise, so a test run never
// announces itself on the real network and never sees the real one.
func StartInProcess(ctx context.Context, disc chatwire.Discovery) (socket string, stop func(), err error) {
	return startInProcess(ctx, disc, "", nil)
}

// StartAgainst is StartInProcess pointed at one server address, with no
// discovery at all. It is how a test runs a whole client beside a whole
// server without either of them touching the network they are on.
func StartAgainst(ctx context.Context, addr string) (socket string, stop func(), err error) {
	return startInProcess(ctx, chatwire.NoDiscovery(), addr, nil)
}

// StartSample runs a whole client daemon in this process over a cache
// seeded with sample data, on a private socket, with no server to talk to
// at all. The screenshot tool and the headless UI tests use it; nothing
// else should.
//
// It is also the honest demonstration of the cache: there is no server
// anywhere, and the window is still a working window over a real
// conversation.
func StartSample(ctx context.Context) (socket string, stop func(), err error) {
	return startInProcess(ctx, chatwire.NoDiscovery(), "", chat.SeedSample)
}

func startInProcess(ctx context.Context, disc chatwire.Discovery, addr string, seed func(*chat.Store)) (socket string, stop func(), err error) {
	dir, err := os.MkdirTemp("", "comms-chat-lan-clientd-")
	if err != nil {
		return "", nil, err
	}
	store, err := chat.NewStore(filepath.Join(dir, "data"))
	if err != nil && store == nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	if seed != nil {
		seed(store)
	}
	d := New(store, disc)
	d.Addr = addr

	socket = filepath.Join(dir, "clientd.sock")
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, socket, d) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			stop = func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
				}
				_ = os.RemoveAll(dir)
			}
			return socket, stop, nil
		}
		select {
		case err := <-done:
			cancel()
			_ = os.RemoveAll(dir)
			if err == nil {
				err = fmt.Errorf("chat: comms-chat-lan-clientd exited before it listened")
			}
			return "", nil, err
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	_ = os.RemoveAll(dir)
	return "", nil, fmt.Errorf("chat: the comms-chat-lan-clientd socket never appeared")
}
