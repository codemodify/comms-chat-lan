// Command comms-chat-lan-clientd is the daemon on your own machine. It
// finds the comms-chat-lan-server on your network, enrols with it, keeps
// a local copy of everything it is told, and exposes that copy to the
// front ends over a Unix socket as JSON-RPC 2.0 (NDJSON).
//
// It is the only thing in this application that talks to the server, and
// it has no user interface and no dependency on one — `go list -deps
// ./cmd/comms-chat-lan-clientd | grep uitoolkit` is empty, and there is a
// test that says so. Start it, then start comms-chat-lan-client-gui (the
// desktop window) or comms-chat-lan-client-tui (the terminal one), or
// both at once.
//
//	comms-chat-lan-clientd
//	comms-chat-lan-clientd -nick sam
//	comms-chat-lan-clientd -server kestrel        # where multicast does not reach
//	UITK_CHAT_SERVER=10.0.0.5:47772 comms-chat-lan-clientd
//
// Because it caches, it is useful before it has found anything: the front
// ends open, the history is there, and a message composed while the
// server is unreachable is kept and sent when it is not.
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

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatclientd"
)

func main() {
	var (
		sock    = flag.String("socket", chat.DefaultSocket(), "Unix socket to listen on")
		dir     = flag.String("data", chat.DataDir(), "data directory (history, roster, identity)")
		server  = flag.String("server", "", "the server's address (\"host\" or \"host:port\"); empty means listen for its beacon")
		nick    = flag.String("nick", "", "nickname to use (overrides identity.json for this run)")
		noDisc  = flag.Bool("no-discovery", false, "do not listen on the multicast group")
		mem     = flag.Bool("ephemeral", false, "keep everything in memory: nothing is written to disk")
		verbose = flag.Bool("v", false, "log the link, enrolment and queue activity")
	)
	flag.Parse()

	if *nick != "" {
		// Before the store opens, so the identity it loads already has it.
		_ = os.Setenv(chat.EnvNick, *nick)
	}
	if *noDisc {
		_ = os.Setenv(chat.EnvNoDiscovery, "1")
	}
	data := *dir
	if *mem {
		data = ""
	}

	d, err := chatclientd.Open(data, *server)
	if err != nil && d == nil {
		log.Fatalf("comms-chat-lan-clientd: %v", err)
	}
	if err != nil {
		// A read-only home should not stop the app; it should say so.
		fmt.Fprintf(os.Stderr, "comms-chat-lan-clientd: %v (serving anyway)\n", err)
	}
	if *verbose {
		d.Log = func(s string) { log.Println(s) }
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	self := d.Store.Self()
	fmt.Printf("comms-chat-lan-clientd  %s  nick=%q  id=%s\n", chatclientd.Version, self.Nick, self.ID)
	fmt.Printf("  socket    %s  (mode 0600, this user only)\n", *sock)
	fmt.Printf("  storage   %s\n", storageName(d.Store.Dir()))
	fmt.Printf("  server    %s\n", serverName(*server))
	fmt.Println("  no encryption and no authentication on the LAN: see docs/security.md")

	if err := chatclientd.ListenAndServe(ctx, *sock, d); err != nil {
		// A lock clash means another clientd already owns the socket.
		// Saying so beats silently taking it over.
		log.Fatalf("comms-chat-lan-clientd: %v", err)
	}
}

func serverName(addr string) string {
	if addr == "" {
		return "whichever one announces itself on the LAN"
	}
	return addr
}

func storageName(dir string) string {
	if dir == "" {
		return "memory (nothing is written to disk)"
	}
	return dir
}
