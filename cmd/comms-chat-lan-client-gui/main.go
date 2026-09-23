// Command comms-chat-lan-client-gui is comms-chat-lan's desktop user
// interface. It connects to comms-chat-lan-clientd over a Unix socket and
// never touches the LAN itself: no multicast, no connection to the
// server, no message store.
//
//	comms-chat-lan-clientd &     # the daemon owns everything
//	comms-chat-lan-client-gui
//
//	UITK_CHAT_SOCK=/tmp/chat.sock comms-chat-lan-client-gui
//	UITK_THEME=win95 comms-chat-lan-client-gui   # any uitoolkit theme pack
//
// Built on uitoolkit: https://github.com/codemodify/uitoolkit
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/codemodify/comms-chat-lan/chat"
	"github.com/codemodify/comms-chat-lan/chatclientd"
	"github.com/codemodify/comms-chat-lan/chatui"
	"github.com/codemodify/uitoolkit"
	"github.com/codemodify/uitoolkit/icons"
	"github.com/codemodify/uitoolkit/platform"
	"github.com/codemodify/uitoolkit/style"
)

func main() {
	var (
		sock     = flag.String("socket", chat.DefaultSocket(), "comms-chat-lan-clientd Unix socket")
		wait     = flag.Duration("wait", 3*time.Second, "how long to wait for the daemon at startup")
		light    = flag.Bool("light", false, "start with the light appearance")
		headless = flag.Bool("headless", false, "paint one frame offscreen, write chat.png and exit")
		sample   = flag.Bool("sample", false, "run against a private daemon seeded with sample data and no server")
		out      = flag.String("png", "chat.png", "where -headless writes its frame")
	)
	flag.Parse()

	socket := *sock
	if *sample {
		// A window with something in it, for a screenshot or a first
		// look, without touching the real daemon, the real history or
		// any server at all.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s, stop, err := chatclientd.StartSample(ctx)
		if err != nil {
			log.Fatalf("comms-chat-lan-client-gui: %v", err)
		}
		defer stop()
		socket = s
	}

	cli, err := chat.DialWait(socket, *wait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "comms-chat-lan-client-gui: %v\nStart the daemon first:  comms-chat-lan-clientd\n", err)
		os.Exit(1)
	}
	defer func() { _ = cli.Close() }()

	look := style.PreferredLook()
	if *light {
		look = style.WithTheme(look, style.ThemeLight)
	}
	a := uitoolkit.New(uitoolkit.Options{Look: look, Headless: *headless, WatchLook: true})
	a.SetIcon(icons.AppIconRGB("message-circle", 0x2f, 0x6f, 0xd0)...)

	win, err := a.NewWindow(platform.WindowOptions{
		Title: "Chat", Width: 1100, Height: 720, MinWidth: 720, MinHeight: 460,
		Headless: *headless,
	})
	if err != nil {
		log.Fatalf("comms-chat-lan-client-gui: %v", err)
	}
	win.SetContent(chatui.Open(a, win, cli))

	if *headless {
		a.PumpOnce()
		if err := win.WritePNG(*out); err != nil {
			log.Fatalf("comms-chat-lan-client-gui: %v", err)
		}
		fmt.Println("wrote " + *out)
		return
	}
	if err := a.Run(); err != nil {
		log.Fatalf("comms-chat-lan-client-gui: %v", err)
	}
}
