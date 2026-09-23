// Command comms-chat-lan-client-tui is comms-chat-lan's terminal front
// end. It talks to comms-chat-lan-clientd over the same Unix socket and
// the same JSON-RPC as the desktop UI, so the two can be open at once and
// agree with each other.
//
// It is meant for a machine you are on over ssh, where the desktop UI is
// not an option:
//
//	comms-chat-lan-clientd &
//	comms-chat-lan-client-tui
//
//	UITK_CHAT_SOCK=/tmp/chat.sock comms-chat-lan-client-tui
//
// Ctrl+G lists every key and every command.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chattui"
)

func main() {
	sock := flag.String("socket", chat.DefaultSocket(), "comms-chat-lan-clientd Unix socket")
	wait := flag.Duration("wait", 3*time.Second, "how long to wait for the daemon at startup")
	flag.Parse()

	cli, err := chat.DialWait(*sock, *wait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "comms-chat-lan-client-tui: %v\nStart the daemon first:  comms-chat-lan-clientd\n", err)
		os.Exit(1)
	}
	defer func() { _ = cli.Close() }()

	if err := chattui.New(cli).Run(); err != nil {
		log.Fatalf("comms-chat-lan-client-tui: %v", err)
	}
}
