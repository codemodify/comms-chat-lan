package chatcore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OpenNode builds the node comms-chatd runs from the environment: the
// store in [DataDir] (or memory, when dir is empty) and the beacon, unless
// UITK_CHAT_NO_DISCOVERY says otherwise.
func OpenNode(dir string) (*Node, error) {
	store, err := NewStore(dir)
	if err != nil && store == nil {
		return nil, err
	}
	var disc Discovery = NewBeacon()
	if v := strings.TrimSpace(os.Getenv(EnvNoDiscovery)); v != "" && v != "0" {
		disc = NoDiscovery()
	}
	return NewNode(store, disc), err
}

// StartInProcess runs a whole daemon — store, node and RPC — on a private
// socket in a temporary directory, and returns the socket path and a stop
// function. It is what the tests, the screenshot tool and the
// single-process demo use; it is not how the app is normally run.
//
// Discovery is off unless disc says otherwise, so a test run never
// announces itself on the real network and never sees the real one.
func StartInProcess(ctx context.Context, disc Discovery) (socket string, stop func(), err error) {
	return startInProcess(ctx, disc, nil)
}

// startSeeded is StartInProcess with discovery off and a store the caller
// fills in first.
func startSeeded(ctx context.Context, seed func(*Store)) (socket string, stop func(), err error) {
	return startInProcess(ctx, NoDiscovery(), seed)
}

func startInProcess(ctx context.Context, disc Discovery, seed func(*Store)) (socket string, stop func(), err error) {
	dir, err := os.MkdirTemp("", "comms-chatd-")
	if err != nil {
		return "", nil, err
	}
	store, err := NewStore(filepath.Join(dir, "data"))
	if err != nil && store == nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	if disc == nil {
		disc = NoDiscovery()
	}
	if seed != nil {
		seed(store)
	}
	node := NewNode(store, disc)

	socket = filepath.Join(dir, "chatd.sock")
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, socket, node) }()

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
				err = fmt.Errorf("chat: comms-chatd exited before it listened")
			}
			return "", nil, err
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	_ = os.RemoveAll(dir)
	return "", nil, fmt.Errorf("chat: comms-chatd socket never appeared")
}
