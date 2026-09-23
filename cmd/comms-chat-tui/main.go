// Command comms-chat-tui is comms-chat-lan's terminal front end. It talks
// to comms-chatd over the same Unix socket and the same JSON-RPC as the
// desktop UI, so the two can be open at once and agree with each other.
//
// It is meant for a machine you are on over ssh, where the desktop UI is
// not an option:
//
//	comms-chatd &
//	comms-chat-tui
//
//	UITK_CHAT_SOCK=/tmp/chat.sock comms-chat-tui
//
// Ctrl+G lists every key and every command.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/codemodify/comms-chat-lan/chatcore"
	"github.com/codemodify/comms-chat-lan/chattui"
)

func main() {
	sock := flag.String("socket", chatcore.DefaultSocket(), "comms-chatd Unix socket")
	wait := flag.Duration("wait", 3*time.Second, "how long to wait for the daemon at startup")
	flag.Parse()

	cli, err := chatcore.DialWait(*sock, *wait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "comms-chat-tui: %v\nStart the daemon first:  comms-chatd\n", err)
		os.Exit(1)
	}
	defer func() { _ = cli.Close() }()

	if err := chattui.New(cli).Run(); err != nil {
		log.Fatalf("comms-chat-tui: %v", err)
	}
}
