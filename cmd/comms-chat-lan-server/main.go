// Command comms-chat-lan-server is the machine everybody's client daemon
// connects to. It enrols whoever asks, decides the order of every
// message, stores every conversation, relays what it stores, and keeps
// what an absent client missed until it comes back and asks for it.
//
// Run one of these somewhere on the LAN — a desktop that is always on, a
// small server, a systemd unit — and start comms-chat-lan-clientd on
// every machine that wants to talk.
//
//	comms-chat-lan-server
//	comms-chat-lan-server -port 47772 -name "the office"
//	UITK_CHAT_NO_DISCOVERY=1 comms-chat-lan-server   # announce nothing
//
// It has no user interface and no dependency on one — `go list -deps
// ./cmd/comms-chat-lan-server | grep uitoolkit` is empty, and there is a
// test that says so.
//
// See docs/architecture.md and docs/protocol.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/codemodify/comms-chat-lan/chatserver"
)

func main() {
	var (
		dir     = flag.String("data", defaultDir(), "data directory (history, roster, identity)")
		name    = flag.String("name", "", "what this server calls itself when it announces")
		port    = flag.Int("port", 0, "TCP port clients connect to (0: the default, 47772)")
		noDisc  = flag.Bool("no-discovery", false, "do not announce on the multicast group")
		mem     = flag.Bool("ephemeral", false, "keep everything in memory: nothing is written to disk")
		verbose = flag.Bool("v", false, "log enrolments, connections and relays")
	)
	flag.Parse()

	if *noDisc {
		_ = os.Setenv("UITK_CHAT_NO_DISCOVERY", "1")
	}
	data := *dir
	if *mem {
		data = ""
	}

	srv, err := chatserver.Open(data)
	if err != nil && srv == nil {
		log.Fatalf("comms-chat-lan-server: %v", err)
	}
	if err != nil {
		// A read-only home should not stop the server; it should say so.
		fmt.Fprintf(os.Stderr, "comms-chat-lan-server: %v (serving anyway)\n", err)
	}
	srv.Port = *port
	srv.Name = *name
	if *verbose {
		srv.Log = func(s string) { log.Println(s) }
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("comms-chat-lan-server  %s  name=%q  id=%s\n", chatserver.Version, srv.DisplayName(), srv.ID())
	fmt.Printf("  storage   %s\n", storageName(data))
	fmt.Printf("  discovery %s\n", srv.Disc.Name())
	fmt.Println("  enrolment is open: anyone who can reach this port is in")
	fmt.Println("  nothing is encrypted and nobody is authenticated: see docs/security.md")

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("comms-chat-lan-server: %v", err)
	}
}

func defaultDir() string {
	if d := os.Getenv("UITK_CHAT_HOME"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d + "/comms-chat-lan-server"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return os.TempDir() + "/comms-chat-lan-server"
	}
	return home + "/.local/share/comms-chat-lan-server"
}

func storageName(dir string) string {
	if dir == "" {
		return "memory (nothing is written to disk)"
	}
	return dir
}
