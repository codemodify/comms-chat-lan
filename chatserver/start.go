package chatserver

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatwire"
)

// Open builds the server comms-chat-lan-server runs from the environment:
// the authoritative store in dir (or memory, when dir is empty) and the
// beacon, unless UITK_CHAT_NO_DISCOVERY says otherwise.
//
// The server's own identity.json gives it the 16 random bytes it
// announces itself by, and which name the ordering its sequence numbers
// belong to. It is the same file format a client keeps, because it is the
// same question — who am I, across restarts — and one answer is enough.
func Open(dir string) (*Server, error) {
	store, err := chat.NewStore(dir)
	if err != nil && store == nil {
		return nil, err
	}
	var disc chatwire.Discovery = chatwire.NewBeacon()
	if v := strings.TrimSpace(os.Getenv(chat.EnvNoDiscovery)); v != "" && v != "0" {
		disc = chatwire.NoDiscovery()
	}
	return New(store, disc), err
}

// StartInProcess runs a whole server on a free port, with whatever
// discovery it is given, and returns the address clients should dial and
// a stop function. The tests use it; it is not how the server is run.
func StartInProcess(ctx context.Context, disc chatwire.Discovery) (addr string, srv *Server, stop func(), err error) {
	dir, err := os.MkdirTemp("", "comms-chat-lan-server-")
	if err != nil {
		return "", nil, nil, err
	}
	store, err := chat.NewStore(dir)
	if err != nil && store == nil {
		_ = os.RemoveAll(dir)
		return "", nil, nil, err
	}
	srv = New(store, disc)
	srv.Port = -1 // any free port: a test must not fight the real server

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.Listen(); a != "" {
			stop = func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
				}
				_ = os.RemoveAll(dir)
			}
			return loopback(a), srv, stop, nil
		}
		select {
		case err := <-done:
			cancel()
			_ = os.RemoveAll(dir)
			if err == nil {
				err = fmt.Errorf("chat: the server exited before it listened")
			}
			return "", nil, nil, err
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	_ = os.RemoveAll(dir)
	return "", nil, nil, fmt.Errorf("chat: the server never listened")
}

// loopback is the listen address with the host replaced by 127.0.0.1. The
// address a server advertises is the machine's outbound one, which a test
// has no business depending on.
func loopback(listen string) string {
	if i := strings.LastIndex(listen, ":"); i > 0 {
		return "127.0.0.1" + listen[i:]
	}
	return listen
}
