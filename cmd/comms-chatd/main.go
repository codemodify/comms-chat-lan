// Command comms-chatd is the comms-chat-lan daemon. It owns everything:
// the identity, the message history, the UDP beacon that finds peers on
// the LAN and the TCP connections to them. It listens on a Unix socket and
// speaks JSON-RPC 2.0 (NDJSON) to its front ends.
//
// It has no user interface and no dependency on one — `go list -deps
// ./cmd/comms-chatd | grep uitoolkit` is empty, and there is a test that
// says so. Start it, then start comms-chat (the desktop UI) or
// comms-chat-tui (the terminal one), or both at once.
//
//	comms-chatd
//	comms-chatd -nick sam -port 47772
//	UITK_CHAT_NO_DISCOVERY=1 comms-chatd     # no multicast leaves the machine
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

	"github.com/codemodify/comms-chat-lan/chatcore"
)

func main() {
	var (
		sock    = flag.String("socket", chatcore.DefaultSocket(), "Unix socket to listen on")
		dir     = flag.String("data", chatcore.DataDir(), "data directory (history, roster, identity)")
		nick    = flag.String("nick", "", "nickname to advertise (overrides identity.json for this run)")
		port    = flag.Int("port", 0, "TCP port peers dial (0: any free port)")
		noDisc  = flag.Bool("no-discovery", false, "do not announce or listen on the multicast group")
		mem     = flag.Bool("ephemeral", false, "keep everything in memory: nothing is written to disk")
		verbose = flag.Bool("v", false, "log peers, connections and queue activity")
	)
	flag.Parse()

	if *nick != "" {
		// Before the store opens, so the identity it loads already has it.
		_ = os.Setenv(chatcore.EnvNick, *nick)
	}
	if *noDisc {
		_ = os.Setenv(chatcore.EnvNoDiscovery, "1")
	}
	data := *dir
	if *mem {
		data = ""
	}

	node, err := chatcore.OpenNode(data)
	if err != nil && node == nil {
		log.Fatalf("comms-chatd: %v", err)
	}
	if err != nil {
		// A read-only home should not stop the app; it should say so.
		fmt.Fprintf(os.Stderr, "comms-chatd: %v (serving anyway)\n", err)
	}
	node.Port = *port
	if *verbose {
		node.Log = func(s string) { log.Println(s) }
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	self := node.Store.Self()
	fmt.Printf("comms-chatd  %s  nick=%q  id=%s\n", chatcore.Version, self.Nick, self.ID)
	fmt.Printf("  socket    %s  (mode 0600, this user only)\n", *sock)
	fmt.Printf("  storage   %s\n", storageName(node.Store.Dir()))
	fmt.Printf("  discovery %s\n", node.Disc.Name())
	fmt.Println("  no encryption and no authentication on the LAN: see docs/security.md")

	if err := chatcore.ListenAndServe(ctx, *sock, node); err != nil {
		// A lock clash means another comms-chatd already owns the socket.
		// Saying so beats silently taking it over.
		log.Fatalf("comms-chatd: %v", err)
	}
}

func storageName(dir string) string {
	if dir == "" {
		return "memory (nothing is written to disk)"
	}
	return dir
}
