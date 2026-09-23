# Architecture

comms-chat-lan is three binaries around one package. It is the reference
application for a family that all have this shape, so the shape matters as
much as the features.

```
                  ┌─────────────────────────────────────────────┐
                  │            comms-chatd  (daemon)            │
   UDP multicast  │                                             │
  ◄──────────────►│  chatcore.Beacon ──► chatcore.Discovery     │
    the beacon    │                            │                │
                  │                            ▼                │
   TCP to peers   │  chatcore.Node ◄────── chatcore.Store       │
  ◄──────────────►│       │                (history, roster,    │
                  │       │                 identity, on disk)  │
                  │       ▼                                     │
                  │  chatcore.Server ── JSON-RPC 2.0 (NDJSON)   │
                  └───────────┬─────────────────────────────────┘
                              │  Unix socket, mode 0600
              ┌───────────────┴───────────────┐
              ▼                               ▼
   ┌────────────────────┐          ┌────────────────────┐
   │  comms-chat  (GUI) │          │ comms-chat-tui     │
   │  uitoolkit         │          │ tview / tcell      │
   └────────────────────┘          └────────────────────┘
```

## The rule

**The daemon owns all state and all network. The front ends own nothing.**

Concretely:

* `go list -deps ./cmd/comms-chatd | grep uitoolkit` is **empty**, and
  `TestTheDaemonLinksNoUserInterface` fails the build if it stops being.
  The daemon does not link a toolkit, cannot open a window, and does not
  need a display or a session bus to run.
* Neither front end opens a network socket. Neither parses a beacon,
  frames a message or touches the history files. If a front end can do
  something, there is a method for it in `chatcore/proto.go`.
* Both front ends can be open at the same time and agree with each other,
  because both are looking at the same daemon and both hear the same
  events. Change your nickname in the terminal and the window's status bar
  changes.

The one place a desktop is reachable from `chatcore` at all is
`chatcore.DesktopNotifier`, a `func(title, body string)` variable. The
daemon leaves it nil and only broadcasts the `chat.notify` event; each
front end decides what to do with it. It is a plain function value rather
than an interface into a toolkit precisely so that no import of a UI
package can sneak into the daemon's dependency graph.

## Why a daemon at all

A chat app has to be running to receive anything. If the process that
receives messages is the window, then closing the window means going
offline, and being visible on the LAN means leaving a window open all day.
Separating them means:

* the daemon can run from a systemd user unit and you are simply present;
* a message arriving while nothing is open is stored, and notified;
* the desktop UI can be restarted, upgraded or crash without dropping a
  connection or losing a message;
* the same state is reachable over ssh through the terminal front end.

## The packages

### `chatcore`

Everything. It is one package on purpose: splitting a 4000-line core into
six packages whose contents all refer to each other buys directory
structure and costs the ability to read it.

| file | what it holds |
|---|---|
| `types.go` | the model: peers, messages, conversations, transfers, identity |
| `paths.go` | XDG paths, the environment variables, identity.json, the avatar palette |
| `store.go` | the roster, the rooms and the append-only history |
| `transfer.go` | the live transfer table, download paths, name sanitising |
| `discovery.go` | the `Discovery` interface and the announcement |
| `beacon.go` | the UDP multicast implementation of it |
| `wire.go` | the peer-to-peer frame format |
| `node.go` | connections, sending, queueing, presence, typing |
| `node_files.go` | the file-transfer half of the node |
| `proto.go` | the JSON-RPC method and event names, and the wire params |
| `server.go` | the daemon's socket server and its one dispatch switch |
| `client.go` | the front ends' client, with reconnect |
| `socklock.go` | the socket's hardening: lock, umask, SO_PEERCRED |
| `sock.go` | opening a node from the environment; the in-process daemon |
| `sample.go` | sample data, for the screenshot and the headless tests |

### `chatui`

The uitoolkit window. `Open(app, window, client)` returns a component; the
`session` struct behind it is the window's state and nothing more. Daemon
events are posted to the UI goroutine (`session.post`) and every call to
the daemon goes through `session.async` so a slow daemon never blocks a
frame.

`transcript` is the only widget this app paints itself; `keys.go` holds the
window-wide key handling, which needs a component in the tree because the
toolkit has no window-level shortcut registration.

### `chattui`

The tview front end. One `UI` struct, the same client, `app.QueueUpdateDraw`
where the GUI uses `app.Post`.

## Extending it

### A second discovery

`Discovery` is three methods: `Run(ctx, events)`, `Update(announcement)`,
`Name()`. Implement it, hand it to `NewNode`, and the node, the store and
the RPC do not change. An mDNS/DNS-SD implementation would go here, as
would "read a list of hosts from a file" for a network where multicast is
blocked outright.

### A new RPC method

1. A constant and a line of documentation in `proto.go`.
2. A case in the switch in `server.go`.
3. A method on `Client` in `client.go`.
4. Both front ends, or neither.

If a change does not fit that shape, it is probably business logic trying
to move into a front end, and it belongs in the node or the store instead.

### The fourth front end

Nothing in `chatcore` knows how many clients there are. A web bridge, a
command-line one-shot (`chat-send "room" "text"`), a bot — all of them are
`chatcore.Dial(socket)` and method calls.

## Environment

| variable | effect |
|---|---|
| `UITK_CHAT_HOME` | the data directory (history, roster, identity) |
| `UITK_CHAT_SOCK` | the daemon's socket path |
| `UITK_CHAT_PORT` | pin the TCP port peers dial |
| `UITK_CHAT_NICK` | override the advertised nickname for one run |
| `UITK_CHAT_NO_DISCOVERY` | announce nothing, hear nothing |
| `UITK_CHAT_IFACE` | pin the beacon to one interface |
| `UITK_CHAT_NO_NOTIFY` | never raise a desktop notification |
| `UITK_CHAT_DOWNLOADS` | where accepted files land |
| `UITK_THEME` | any uitoolkit theme pack, for the GUI |

## Testing

Every `go test` in this repository goes through `tools/testenv.sh`, which
takes away the display, gives the process a private D-Bus session bus with
no service directory, and points the XDG directories at a temporary one.
This is not belt and braces: a uitoolkit test that reaches the real session
bus will raise a real notification and a real tray icon on the desktop of
whoever is running the tests.

```sh
tools/testenv.sh go test ./...
tools/testenv.sh go test -race ./...
```

The tests that matter most are the ones that hold the design in place
rather than the ones that cover a line:

* `TestTheDaemonLinksNoUserInterface` — the architectural rule, asserted.
* `TestTwoPeersExchangeAMessageAndAcknowledgeIt` and the queue, race and
  unknown-peer tests — two real nodes over loopback TCP.
* `TestTwoBeaconsFindEachOther` — two real beacons over loopback multicast,
  skipped where there is no multicast route.
* `TestTheWindowIsAccessible` and `TestTheWindowPaintsInVeryDifferentThemes`
  — the real window, offscreen, audited and painted in three theme packs.
* `TestTheTerminalUIDrawsOnASimulatedTerminal` — the real tview loop on
  tcell's simulation screen.
