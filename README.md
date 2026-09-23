# comms-chat-lan

Chat with the people on your network. No server, no accounts, no sign-up,
no configuration — start it and the other machines running it appear.

It is three programs: a daemon that owns the network and the history, a
desktop window, and a terminal front end for the machines you only reach
over ssh. Both front ends talk to the same daemon, so you can have both
open at once and they agree with each other.

```
┌─ Conversations ──┬─────────────────────────────────────────────────┐
│ # general   3    │  #general                      3 here           │
│ • nadia     (2)  │ ─────────────────────────────────────────────── │
│ ◦ theo           │  NK  nadia          09:14  delivered            │
│                  │      Morning. The build machine is back up —    │
│                  │      it was the switch in the cupboard.         │
│                  │                                                 │
│                  │  S   sam            09:16  delivered            │
│                  │      That explains the two of us staring at     │
│                  │      logs yesterday.                            │
│                  │ ─────────────────────────────────────────────── │
│                  │  Write a message…             [Attach…] [Send]  │
├──────────────────┴─────────────────────────────────────────────────┤
│ sam — online   │  2 here of 3 known  │  not encrypted — LAN only   │
└────────────────────────────────────────────────────────────────────┘
```

## Build and run

```sh
go build ./...

./comms-chatd &          # the daemon: owns everything
./comms-chat             # the desktop window
./comms-chat-tui         # or the terminal one, over ssh
```

Nothing to configure. On first run the daemon mints an identity — 16 random
bytes, your login name as a nickname, an avatar colour derived from the id
— and starts announcing itself. Other people on the same network appear
within a few seconds.

```sh
comms-chatd -nick sam           # a different name for this run
comms-chatd -no-discovery       # announce nothing, hear nothing
comms-chatd -ephemeral          # keep everything in memory
comms-chat -light               # the light appearance
UITK_THEME=win95 comms-chat     # any uitoolkit theme pack
comms-chat -sample              # a window with sample data in it, no real daemon
```

Full list of environment variables: [docs/architecture.md](docs/architecture.md).

## What it does

* **One-to-one and rooms.** A room is a name. Anyone who joins a room of
  the same name is in it with you; there is nobody to ask.
* **Presence.** Who is here, who is away, who is typing.
* **History, locally.** Every conversation is an append-only NDJSON file
  under `~/.local/share/comms-chat-lan/`. Grep it with the tools you
  already have.
* **Files and images**, offered and accepted or declined. Nothing is
  written to your disk until you say yes.
* **Messages wait.** A message to somebody who is asleep is queued and
  goes out by itself when they come back.
* **Notifications**, a tray icon, and a window that is keyboard-reachable
  throughout and correct in every uitoolkit theme pack.

## Architecture, in a paragraph

`comms-chatd` owns all the state and all the network: the UDP beacon that
finds peers, the TCP connections that carry conversations, the roster and
the message history. It exposes them over a Unix socket as JSON-RPC 2.0,
one object per line. `comms-chat` (uitoolkit) and `comms-chat-tui` (tview)
are both clients of that socket and nothing more — they open no network
socket, parse no protocol and hold no state beyond what they are showing,
so every change either makes is a method call and every change either hears
is an event. The daemon links no user interface at all:
`go list -deps ./cmd/comms-chatd | grep uitoolkit` is empty, and a test
fails the build if that stops being true. See
[docs/architecture.md](docs/architecture.md).

## Discovery: what it is, and what it is not

Peers find each other with a **UDP multicast beacon of this application's
own**: a JSON line on `239.192.77.77:47771`, every twelve seconds, saying
who you are and which TCP port to dial. Peers that go quiet for forty
seconds are dropped.

**This is not mDNS, and the cost is real: comms-chat-lan is discoverable
only by other copies of comms-chat-lan.** `avahi-browse` will not show it.
`dns-sd -B` will not show it. It advertises no DNS-SD service type and
answers no mDNS query.

That is deliberate. A correct DNS-SD responder is RFC 6762 plus RFC 6763 —
probing and conflict resolution, known-answer suppression, cache-flush
semantics — and a partial one does not merely fall short, it collides with
the avahi daemon already running on the machine. What this app needs from
discovery is one sentence repeated, so it sends one sentence, repeatedly,
in about a hundred lines we fully control. Discovery sits behind a
three-method interface, so a real mDNS implementation can be added later as
a second one without anything else moving.

If nobody ever appears, multicast is probably blocked on your network, or a
firewall is dropping UDP 47771. See [docs/protocol.md](docs/protocol.md).

## Security: read this

**Nothing is encrypted and nobody is authenticated.** Messages and files
cross the network in plain sight, and a peer's name and identity are
whatever that peer says they are. Treat it the way you would treat talking
out loud in the same room.

What it *does* do: nothing leaves your machine until you send it; nothing
is written to your disk from a file offer until you accept it, and then
only under a fresh name inside your download directory; the daemon's socket
is mode 0600 and checks every connection against your own user id, so
another user on the machine cannot read your history or send as you.

The full position, including what the SHA-256 on a file transfer is and
what it is not, is in [docs/security.md](docs/security.md). It is short and
worth the two minutes.

## Documentation

| | |
|---|---|
| [docs/protocol.md](docs/protocol.md) | the beacon packet, the TCP frames, ordering, delivery, file transfer |
| [docs/architecture.md](docs/architecture.md) | the three binaries, the packages, how to extend it, testing |
| [docs/security.md](docs/security.md) | what it protects against and what it does not |

## Built on

[uitoolkit](https://github.com/codemodify/uitoolkit) — the Go desktop
toolkit the window is written against, including its accessibility tree
and its theme packs.

The terminal front end uses [tview](https://github.com/rivo/tview) (MIT)
over [tcell](https://github.com/gdamore/tcell) (Apache 2.0). Everything
else is the Go standard library, `golang.org/x/net/ipv4` for multicast and
`golang.org/x/sys/unix` for the socket options. All dependencies are
permissively licensed.

## Testing

Every test goes through `tools/testenv.sh`, which takes away the display
and gives the process a private D-Bus bus. A uitoolkit test that reaches
the real session bus opens a real window on whoever is running it.

```sh
tools/testenv.sh go test ./...
```

## Licence

The Free License — see [LICENSE](LICENSE). Use it. No restrictions.
