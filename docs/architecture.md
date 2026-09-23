# Architecture

comms-chat-lan is four binaries around four packages. It is the reference
application for a family that all have this shape, so the shape matters as
much as the features.

```
                 ┌───────────────────────────────────────────────┐
                 │        comms-chat-lan-server                   │
  UDP multicast  │                                                │
 ◄───────────────│  chatwire.Beacon ──► announces "I am here"     │
   the beacon    │                                                │
                 │  chatserver.Server ◄──── chat.Store            │
 ─── TCP ───────►│      enrol, order, store, relay                │
                 └───────────────────────────────────────────────┘
                              ▲                  ▲
                              │ one TCP connection each
              ┌───────────────┴──────┐    ┌──────┴───────────────┐
              │ comms-chat-lan-clientd│    │ …-clientd elsewhere  │
              │                       │    └──────────────────────┘
              │  chatclientd.Daemon   │
              │    link ─► chat.Store │  the cache
              │  chatclientd.RPC ── JSON-RPC 2.0 (NDJSON)
              └───────────┬───────────┘
                          │  Unix socket, mode 0600
          ┌───────────────┴───────────────┐
          ▼                               ▼
 ┌──────────────────────────┐  ┌──────────────────────────┐
 │ comms-chat-lan-client-gui│  │ comms-chat-lan-client-tui│
 │ uitoolkit                │  │ tview / tcell            │
 └──────────────────────────┘  └──────────────────────────┘
```

## The rules

**One transport, and one place where messages are ordered.** Clients do
not talk to each other. Everything goes to the server, the server decides
the order, and that order is what every machine shows. A second path would
be a second ordering.

**The server is the authority; clientd is a cache.** The server's copy is
the complete one. A client daemon's copy is what it has been told, kept so
that a window opens instantly, a conversation scrolls while the network is
down, and a message typed into a disconnected client is not lost.

**The daemons own all state and all network. The front ends own nothing.**

Concretely:

* `go list -deps ./cmd/comms-chat-lan-server | grep uitoolkit` and the
  same for `comms-chat-lan-clientd` are both **empty**, and
  `TestNeitherDaemonLinksAUserInterface` fails the build if either stops
  being. Neither daemon links a toolkit, can open a window, or needs a
  display or a session bus.
* Neither front end opens a network socket. Neither parses a beacon,
  frames a message or touches the history files. If a front end can do
  something, there is a method for it in `chat/proto.go`.
* Both front ends can be open at the same time and agree with each other,
  because both are looking at the same client daemon and both hear the
  same events. Change your nickname in the terminal and the window's
  status bar changes.

The one place a desktop is reachable from `chat` at all is
`chat.DesktopNotifier`, a `func(title, body string)` variable. The daemons
leave it nil and only broadcast the `chat.notify` event; each front end
decides what to do with it. It is a plain function value rather than an
interface into a toolkit precisely so that no import of a UI package can
sneak into a daemon's dependency graph.

## Why a client daemon as well as a server

The server answers "who keeps the conversation". The client daemon
answers three other questions, and they are the reason it did not
disappear when the server arrived:

* **Presence without a window.** A chat application has to be running to
  be present. If the process that holds the connection is the window,
  then closing the window means going offline.
* **Responsiveness.** Every front-end call is answered from local disk.
  Opening a window does not wait for a round trip to the server, and a
  slow LAN never makes a frame drop.
* **The server being away.** History still reads, the conversation list
  still fills, and a message still composes. The front ends say the
  server is unreachable rather than showing an empty window.

It also means the desktop UI can be restarted, upgraded or crash without
dropping the connection or losing a message, and that the same state is
reachable over ssh through the terminal front end.

## The packages

### `chat` — what everything agrees on

The model, the store, and the local JSON-RPC. It imports neither daemon,
and both daemons and both front ends import it.

| file | what it holds |
|---|---|
| `types.go` | peers, messages, conversations, transfers, identity, conversation naming |
| `paths.go` | XDG paths, the environment variables, identity.json, the avatar palette |
| `store.go` | the roster, the rooms and the append-only history |
| `transfer.go` | transfer ids and limits, download paths, name sanitising |
| `proto.go` | the JSON-RPC method and event names, the parameter types, the event struct |
| `client.go` | the front ends' client, with reconnect |
| `notify.go` | the desktop-notification seam |
| `sample.go` | sample data, for the screenshot and the headless tests |

`Store` is one type used two ways, which is worth saying out loud: on the
server it is the authority and it mints the sequence numbers; on a client
it is a cache and it never mints one.

### `chatwire` — what the daemons say to each other

The beacon, the `Discovery` interface, the frame format and the frame
types. Specified in [protocol.md](protocol.md).

### `chatserver` — the server

| file | what it holds |
|---|---|
| `server.go` | the listener, the connections, enrolment, the resume |
| `handle.go` | one switch over every frame a client may send |
| `relay.go` | file transfer passing through |
| `start.go` | opening a server from the environment; the in-process one |

### `chatclientd` — the client daemon

| file | what it holds |
|---|---|
| `daemon.go` | the cache, composing, presence, rooms, typing, notification |
| `link.go` | finding the server, enrolling, resuming, staying connected |
| `files.go` | the client's half of file transfer |
| `cursor.go` | how far through the server's sequence this machine has got |
| `rpc.go` | the socket server and its one dispatch switch |
| `socklock.go` | the socket's hardening: lock, umask, SO_PEERCRED |
| `start.go` | opening a daemon from the environment; the in-process one |

### `chatui` and `chattui`

Unchanged in role. `chatui.Open(app, window, client)` returns a component;
the `session` struct behind it is the window's state and nothing more.
`transcript` is the only widget this application paints itself. `chattui`
is one `UI` struct over the same client, with `app.QueueUpdateDraw` where
the GUI uses `app.Post`.

## Extending it

### A second discovery

`Discovery` is three methods: `Run(ctx, events)`, `Update(announcement)`,
`Name()`. Implement it, hand it to `chatserver.New` or
`chatclientd.New`, and nothing else moves. An mDNS/DNS-SD implementation
would go here, as would "read the server's address from a file".

### A new RPC method

1. A constant and a line of documentation in `chat/proto.go`.
2. A case in the switch in `chatclientd/rpc.go`.
3. A method on `chat.Client`.
4. Both front ends, or neither.

If it also needs the server, it needs a frame in `chatwire/wire.go`, a
case in `chatserver/handle.go` and a line in
[protocol.md](protocol.md) — and if it does not fit that shape, it is
probably business logic trying to move into a front end.

### The fifth front end

Nothing in `chatclientd` knows how many front ends there are. A web
bridge, a command-line one-shot (`chat-send "room" "text"`), a bot — all
of them are `chat.Dial(socket)` and method calls.

## Environment

| variable | effect |
|---|---|
| `UITK_CHAT_HOME` | the data directory (history, roster, identity) |
| `UITK_CHAT_SOCK` | the client daemon's socket path |
| `UITK_CHAT_SERVER` | the server's address, for a client that cannot hear the beacon |
| `UITK_CHAT_PORT` | the port the server listens on |
| `UITK_CHAT_NICK` | override the nickname for one run |
| `UITK_CHAT_NO_DISCOVERY` | announce nothing, hear nothing |
| `UITK_CHAT_IFACE` | pin the beacon to one interface |
| `UITK_CHAT_NO_NOTIFY` | never raise a desktop notification |
| `UITK_CHAT_DOWNLOADS` | where accepted files land |
| `UITK_THEME` | any uitoolkit theme pack, for the GUI |

## Testing

Every `go test` in this repository goes through `tools/testenv.sh`, which
takes away the display, gives the process a private D-Bus session bus with
no service directory, and points the XDG directories at a temporary one.
This is not belt and braces: a uitoolkit test that reaches the real
session bus will raise a real notification and a real tray icon on the
desktop of whoever is running the tests.

```sh
tools/testenv.sh go test ./...
tools/testenv.sh go test -race ./...
```

The tests that matter most are the ones that hold the design in place
rather than the ones that cover a line:

* `TestNeitherDaemonLinksAUserInterface` — the architectural rule,
  asserted for both daemons.
* `TestEnrolmentIsOpen` and `TestTheServerOrdersAndRelaysAMessage` — the
  two decisions the rewrite was for.
* `TestAMessageWaitsForSomebodyWhoIsAway` and
  `TestALateJoinerReadsWhatWasSaidBeforeTheyArrived` — the two things the
  peer-to-peer version could not do.
* `TestAStaleCursorIsRefusedRatherThanBelieved`,
  `TestTwoClientsOnOneIdentityBothGetEverything`,
  `TestAServerRestartKeepsTheConversation`,
  `TestAResentMessageIsFiledOnce`, `TestAnOfferNobodyAnswersIsCancelled`
  — the awkward cases, each one a test rather than a paragraph.
* `TestTheCacheIsReadableWhileTheServerIsUnreachable` and
  `TestAMessageComposedWhileTheServerIsDownIsSentLater` — why there is a
  client daemon at all.
* `TestAClientFindsAServerOverMulticast` — a real beacon over loopback
  multicast, skipped where there is no multicast route.
* `TestTheWindowIsAccessible` and
  `TestTheWindowPaintsInVeryDifferentThemes` — the real window,
  offscreen, audited and painted in three theme packs.
* `TestTheTerminalUIDrawsOnASimulatedTerminal` — the real tview loop on
  tcell's simulation screen.
