# comms-chat-lan

Chat with the people on your network. No accounts, no sign-up, no cloud —
run one server somewhere on the LAN and everyone else just starts the
client.

It is four programs: a server that holds the conversation, a client daemon
on each machine that talks to it and keeps a local copy, a desktop window,
and a terminal front end for the machines you only reach over ssh. Both
front ends talk to the same client daemon, so you can have both open at
once and they agree with each other.

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

## What it needs that it used to not

**It needs a server running.** An earlier version of this application had
none: every copy found every other copy and they talked directly. That was
genuinely nicer to start — nothing to set up, nothing to keep running —
and it could not do two things people expect of a chat application:

* a message to somebody whose machine is asleep never arrived, because
  the only copy of it was on the sender's machine;
* somebody who joined a room saw nothing that had been said in it before
  they arrived, because nobody was keeping it.

Both are the same missing thing: nowhere for a conversation to live except
in the machines that happened to be awake. So there is now one machine
that keeps it. What that costs is honest and worth saying plainly: one
machine has to be on, and if it is off, nobody can send anything new.
What each person keeps is their own cache, so history still reads with the
server switched off.

## Build and run

```sh
go build ./...

./comms-chat-lan-server &        # once, on a machine that stays on
./comms-chat-lan-clientd &       # on each machine: the cache and the link
./comms-chat-lan-client-gui      # the desktop window
./comms-chat-lan-client-tui      # or the terminal one, over ssh
```

Nothing to configure. The server announces itself and the clients find it
within a few seconds. On first run a client mints an identity — 16 random
bytes, your login name as a nickname, an avatar colour derived from the id
— enrols with whatever server it finds, and that is the whole of setting
up.

```sh
comms-chat-lan-server -name "the office"       # what it calls itself
comms-chat-lan-server -port 47772              # the default
comms-chat-lan-clientd -server kestrel         # where multicast does not reach
comms-chat-lan-clientd -nick sam               # a different name for this run
comms-chat-lan-clientd -no-discovery           # listen for nothing
comms-chat-lan-client-gui -light               # the light appearance
UITK_THEME=win95 comms-chat-lan-client-gui     # any uitoolkit theme pack
comms-chat-lan-client-gui -sample              # a window with sample data, no server at all
```

Full list of environment variables:
[docs/architecture.md](docs/architecture.md).

## What it does

* **One-to-one and rooms.** A room is a name. Anyone who joins a room of
  the same name is in it with you; there is nobody to ask.
* **Presence.** Who is here, who is away, who is typing.
* **Messages wait.** A message to somebody who is asleep is on the server
  when they wake up, and arrives without anybody resending anything.
* **Rooms have history.** Join `#general` today and read what was said in
  it yesterday.
* **History, locally too.** Every conversation is an append-only NDJSON
  file under `~/.local/share/comms-chat-lan/`, on your own machine as
  well as the server's. Grep it with the tools you already have — and
  read it when the server is off.
* **Files and images**, offered and accepted or declined, relayed through
  the server. Nothing is written to your disk until you say yes.
* **Notifications**, a tray icon, and a window that is keyboard-reachable
  throughout and correct in every uitoolkit theme pack.

## Architecture, in a paragraph

`comms-chat-lan-server` is the authority: it enrols whoever asks, gives
every message a sequence number that is the order of the conversation
everywhere, stores it, and relays it to everyone entitled to see it.
`comms-chat-lan-clientd` is the only thing that talks to it: it finds the
server, enrols, says what it last saw and is told what it missed, and
keeps a local copy so the front ends stay responsive and can read history
while the server is unreachable. It exposes that copy over a Unix socket
as JSON-RPC 2.0, one object per line. `comms-chat-lan-client-gui`
(uitoolkit) and `comms-chat-lan-client-tui` (tview) are clients of that
socket and nothing more — they open no network socket, parse no protocol
and hold no state beyond what they are showing. Neither daemon links a
user interface at all: `go list -deps` on either, piped through
`grep uitoolkit`, is empty, and a test fails the build if that stops being
true. See [docs/architecture.md](docs/architecture.md).

## Discovery: what it is, and what it is not

The server announces itself with a **UDP multicast beacon of this
application's own**: a JSON line on `239.192.77.77:47771`, every twelve
seconds, saying who it is and which TCP port to connect to. Clients only
listen; they announce nothing.

**This is not mDNS, and the cost is real: the server is discoverable only
by this application's own clients.** `avahi-browse` will not show it.
`dns-sd -B` will not show it. It advertises no DNS-SD service type and
answers no mDNS query.

That is deliberate. A correct DNS-SD responder is RFC 6762 plus RFC 6763 —
probing and conflict resolution, known-answer suppression, cache-flush
semantics — and a partial one does not merely fall short, it collides with
the avahi daemon already running on the machine. What this application
needs from discovery is one sentence repeated, so it sends one sentence,
repeatedly, in about a hundred lines we fully control. Discovery sits
behind a three-method interface, so a real mDNS implementation can be
added later as a second one without anything else moving.

Where multicast does not cross the segment, tell the client instead:

```sh
comms-chat-lan-clientd -server kestrel
UITK_CHAT_SERVER=10.0.0.5:47772 comms-chat-lan-clientd
```

## Security: read this

**Nothing is encrypted, nobody is authenticated, and enrolment is open:
whoever can reach the server is in.** Messages and files cross the network
in plain sight, a name and an identity are whatever a client says they
are, and anyone who can open a connection to the server can join any room
and read everything said in it from then on. That is exactly as much
protection as the LAN itself — put the server inside whatever boundary you
actually trust.

What it *does* do: nothing leaves your machine until you send it; nothing
is written to your disk from a file offer until you accept it, and then
only under a fresh name inside your download directory; the client
daemon's socket is mode 0600 and checks every connection against your own
user id, so another user on the machine cannot read your history or send
as you.

The full position, including what the SHA-256 on a file transfer is and
what it is not, is in [docs/security.md](docs/security.md). It is short
and worth the two minutes.

## Documentation

| | |
|---|---|
| [docs/protocol.md](docs/protocol.md) | the beacon, the session, enrolment, resuming, ordering, file transfer |
| [docs/architecture.md](docs/architecture.md) | the four binaries, the packages, how to extend it, testing |
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
