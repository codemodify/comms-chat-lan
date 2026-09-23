# The protocol

comms-chat-lan speaks three protocols. Two of them are between machines
and are specified here: the **beacon**, a UDP multicast announcement by
which a client finds the server, and the **session**, a TCP stream of JSON
frames carrying everything else. The third, the JSON-RPC between
`comms-chat-lan-clientd` and its front ends, is local to one machine and
is described in [architecture.md](architecture.md).

Everything here is version **2** (`chatwire.ProtocolVersion`). Version 1
was the peer-to-peer protocol this replaced; it is gone rather than
deprecated, and nothing in this repository speaks it. A server or client
announcing or enrolling with a different version is ignored rather than
half-understood: with one version in the wild there is nothing to
negotiate, and code that pretends to negotiate is code nobody has tested.

Forward compatibility is one rule: **unknown frame types are ignored.** A
future version may add frames, and this version will carry on talking to
it.

The shape of the whole thing in one paragraph: there is one server. Every
client daemon holds one connection to it. The server assigns every message
a sequence number, stores it, and relays it to everyone entitled to see
it; a client that was not connected asks for what it missed by naming the
last sequence number it saw. Everything else — presence, typing, rooms,
files — travels over that same connection.

---

## 1. The beacon

The server announces itself. Clients listen. That is the only direction:
a client never announces anything, and a beacon with nothing to announce
sends nothing at all.

### Why it is not mDNS

A correct DNS-SD responder is RFC 6762 plus RFC 6763 — probing and
conflict resolution, known-answer suppression, cache-flush semantics,
negative responses — and a partial one does not merely fall short, it
collides with the avahi daemon already running on the machine. What this
application needs from discovery is one sentence, repeated: *I am this
server, connect to me on this port.* So it sends one sentence, repeatedly,
in about a hundred lines we fully control.

### The cost, stated plainly

**The server is discoverable only by this application's own clients.**
`avahi-browse` will not show it. `dns-sd -B` will not show it. It
advertises no DNS-SD service type and answers no mDNS query.

Where multicast does not reach — another segment, a VPN, a container
network with no multicast route, a switch that drops it — the client is
**told** the address instead:

```sh
comms-chat-lan-clientd -server kestrel
comms-chat-lan-clientd -server 10.0.0.5:47772
UITK_CHAT_SERVER=10.0.0.5:47772 comms-chat-lan-clientd
```

A bare host name gets the default port, 47772. An address given this way
wins over discovery, and turns discovery off entirely: there is nothing
left to discover.

### Wire format

One UDP packet to **239.192.77.77:47771** — the IPv4 organisation-local
scope (RFC 2365), routed inside a site and never off it — with TTL 1. The
packet is the magic `CHATLAN/2 ` followed by one compact JSON object:

```
CHATLAN/2 {"v":2,"id":"<32 hex>","name":"the office","host":"kestrel","port":47772}
```

| field | meaning |
|---|---|
| `v` | protocol version; anything else is dropped unparsed |
| `id` | the server's id: 16 random bytes, hex, from its identity.json |
| `name` | what the server calls itself, display only, ≤ 48 runes |
| `host` | the server's host name, display only, never resolved |
| `port` | the TCP port to connect to |
| `bye` | present on the farewell packet sent at shutdown |

A packet is at most 1200 bytes, under the smallest MTU worth worrying
about, so an announcement is never fragmented.

**The address a client dials is the packet's source IP with the announced
port.** It is never read from the payload: an announcement may lie about
what it is called, but it cannot point a client at a third machine.

The magic carries the version, so a client of the peer-to-peer version of
this application and a server of this one never even parse each other's
packets.

### Timing

Announced every **12 seconds**; a server not heard from for **40 seconds**
is considered gone. Three missed announcements, because one lost packet on
a busy wireless network is routine and must not make the server blink out.
A goodbye packet at shutdown means a client does not have to wait out the
expiry.

### Several instances on one machine

The group port is bound with `SO_REUSEADDR` and `SO_REUSEPORT`, and
multicast loopback is on. A server and two clients on one machine is how
this is tried out for the first time, and it is how the tests work.

### Turning it off

`UITK_CHAT_NO_DISCOVERY=1`, or `-no-discovery` on either binary. The
server then announces nothing and every client has to be told where it is.

---

## 2. The session

### Framing

One TCP connection from the client daemon to the server, carrying
length-prefixed JSON:

```
uint32 big-endian length | length bytes of JSON
```

A length prefix rather than NDJSON because file chunks travel in the same
stream, and base64 in a line-oriented protocol makes the framing depend on
the payload. A length over **1 MiB** is an error and the connection is
dropped: the stream cannot be resynchronised, and trying is how a parser
becomes an attack surface.

Every frame is a flat object with a `t` field. Fields a type does not use
are omitted.

### Enrolment

The client sends `hello` and the server answers `welcome`. There is no
third step.

```json
{"t":"hello","v":2,"from":"<32 hex>","nick":"sam","color":"#2f6fd0",
 "host":"box","rooms":["general"],"pres":"online","cur":812,"srv":"<32 hex>"}
```

```json
{"t":"welcome","v":2,"srv":"<32 hex>","srvname":"the office","host":"kestrel",
 "cur":840,"reset":false,"now":"2026-09-23T10:14:00Z","peer":{...}}
```

**Enrolment is open.** The server records who joined and refuses nobody.
There is no code, no passphrase and no approval, and the server keeps no
list of who is allowed — because there is no such list to keep. The only
thing a hello can be refused for is speaking the wrong protocol or
sending a malformed id. What that means for you is in
[security.md](security.md), which does not soften it.

The client keeps its own id and nickname; the server takes them as given.
`rooms` is the client's own membership and wins, including when it is
empty: room membership belongs to the person, and the server is keeping it
for them rather than deciding it.

The `welcome` is written before anything else on the connection, so it is
always the first frame a client sees.

### The cursor, and resuming

`cur` in the hello is **the last sequence number the client saw**. `srv`
is the server it belongs to.

After the welcome the server sends, in this order:

1. `roster` — everyone it has ever enrolled, with who is connected now;
2. every message with a sequence number above the cursor that this client
   may see, oldest first;
3. `synced`, carrying the server's current sequence number.

That is the whole of "what did I miss". There is no queue, no retry and no
separate offline-message mechanism: a message for somebody who is away is
an ordinary stored message whose sequence number is above their cursor.

Three ways the cursor is not used as given, all of them answered with
`"reset":true` and a window of the most recent 200 messages of every
conversation the client can see instead of a delta:

* **A cursor ahead of the server.** It did not come from here — a server
  restored from a backup, or a client that was talking to a different
  one.
* **A cursor from another server.** The client says which server its
  number belongs to; if it is not this one, the number means nothing
  here. A position in one sequence is not a position in another.
* **A cursor so far behind** that the delta is over 2000 messages.
  Somebody who has been away a month wants the recent conversation, not a
  month of replay before they can type.

A reset does not discard anything the client already has. Nothing the
server says makes what the client was told yesterday untrue; the history
stays, the cursor is what restarts.

A message may arrive twice — once relayed live, once from the backlog, in
the moment between the two. That is deliberate. A duplicate costs nothing,
because every message carries its own id and both ends file it once; the
alternative is a window in which a message arrives neither way.

### Frames

Client → server:

| `t` | carries |
|---|---|
| `hello` | enrolment and cursor, above |
| `msg` | one composed message: `id`, `conv`, `body`, `sent`, and a file reference if it announces one |
| `presence` | `pres`, and a changed `nick` or `color` |
| `typing` | `conv`, `typing` |
| `join` / `leave` | `room` |
| `offer` `accept` `decline` `chunk` `done` | file transfer, below |
| `ping` / `pong` | liveness |
| `bye` | a clean close |

Server → client:

| `t` | carries |
|---|---|
| `welcome` | the answer to a hello |
| `msg` | one sequenced message, `conv` written as *this* recipient names it |
| `seq` | `id` and `seq`: the sequence your message was given |
| `roster` | `peers` (all) or `peer` (one changed entry) |
| `typing` | `from`, `conv`, `typing` |
| `synced` | `cur`, and `room` when it ends a join's backfill |
| `offer` `accept` `decline` `chunk` `done` | relayed, with `from` stamped |
| `error` | `code` and a `reason` a person could read |
| `ping` / `pong` / `bye` | as above |

Error codes: `1` bad frame, `2` not enrolled, `3` no such person, `4` not
in that room, `5` refused (a limit). An `error` never closes the
connection by itself.

### What a client may and may not assert

**The sender is the identity that enrolled on the connection**, never the
`from` in the frame. The server overwrites it. A client cannot file a
message as somebody else however it is spelled.

**The conversation is recomputed, not taken on trust.** A client may name
a room it has joined, or the one-to-one conversation between itself and
one other enrolled person. Anything else is refused with `error`.

The nickname shown against a message is the sender's nickname **as the
server has it at the time**, not what the frame says. History keeps what
somebody was called then, not what they are called now.

### Ordering

The server assigns each accepted message a **sequence number** from one
counter, and that number is the order of the conversation on every machine
that holds it. The sender's clock (`sent`) is shown beside the message and
sorts nothing.

This is the one thing a server is for. Two people whose clocks disagree
used to see the same conversation in two orders, and there was nobody to
ask which was right. The counter survives a restart — it is restored from
the highest sequence in the stored history — because a counter that
restarted would reorder everything already said.

A message the client has composed but the server has not yet sequenced has
no number, and sorts **last**: it is the line the person just typed, and
it belongs at the bottom until the server says where it really goes.

### Delivery, and a server that is not there

A composed message is stored locally at once as `queued` and the front end
returns. It is then `sending` while it is on the wire and `delivered` when
the server's `seq` comes back. Nothing optimistically assumes delivery.

If the server is unreachable the message stays `queued` — on disk, in the
conversation, visible to the person who typed it — and goes out when the
link is back, after the client has caught up, so it lands behind what it
missed rather than in front of it. That resend is the same code path as
the first send.

**A resend is free.** The message id is minted by the sender, so a message
composed while the server was down already has its final identity. The
server recognises an id it already holds, stores nothing, and answers with
the sequence number the message was given the first time.

### A server restart with clients still connected

The clients' connections die and they reconnect with backoff. The server
is the same server: its id and its history are on disk, so the cursors
every client is holding are still positions in the same sequence, and each
client is sent only what it missed while the process was down.

An `-ephemeral` server is a different matter: it keeps nothing, so its
sequence starts again and every client's cursor is ahead of it. Each one
is told `reset` and sent what little there is. That is the correct
behaviour for a server that has genuinely forgotten, and it is why
`-ephemeral` is for trying things out rather than for running anything.

### Two clients on one identity

A desktop and a laptop sharing an `identity.json` are both that person.
The server keeps a list of connections per identity, and everything that
goes to a person goes to all of them, including the echo of what either
one sends and the `seq` for it. Presence is whether any of them is
connected. The last one to enrol asserts the room list.

Nothing stops a *stranger* enrolling under somebody else's id either. That
is the same sentence as "enrolment is open", and [security.md](security.md)
says what it costs.

### Rooms

A room is a name, folded to lower case and trimmed, 1–48 characters.
`join` puts the sender in it; the server answers with the room's most
recent 200 messages and a `synced` naming the room.

**That is the late joiner.** Somebody who joins `#general` on Thursday
reads Monday's conversation, because the server kept it and their cursor
is simply behind. It is the same relay path as everything else.

A room message reaches only the people the server has in that room, and a
client that has not joined a room cannot send to it — `error` code 4.
Leaving stops the messages; the history stays, on the server and in the
cache, and rejoining shows it again.

### Typing indicators

Relayed, never stored, never replayed. An indicator that arrives late is
worse than one that never arrives. They expire after six seconds without
a refresh, on the receiving client, so somebody who starts typing and
walks away does not type for ever.

### Presence

What a client says it is doing — online, away, busy — relayed to everybody
as a roster update. What the server *reports* is what it can see: an
identity with no connection is offline whatever it last claimed, so
somebody whose laptop lost power is offline without having said so.

When a client loses the link it marks its whole cached roster offline. It
cannot see anybody, and a status bar that implied otherwise would be
telling the one lie it must not tell.

### File transfer

The bytes go through the server, because clients do not connect to each
other any more.

```
sender                server                receiver
  ── offer ─────────────► (record) ───────────► offer
                                             ◄── accept ──
  ◄──────────────────────── accept ───────────
  ── chunk (n) ─────────► (check) ────────────► chunk
  ── done ──────────────►             ───────► done
```

* An `offer` names the file, its size, its type and a SHA-256. Nothing
  moves and nothing is written until the receiver accepts.
* **Both ends must be connected.** An offer to somebody who is not is
  refused by the server at once with a `decline` saying so. A file is a
  live thing, and a progress bar that never moves is worse than a
  sentence.
* **An offer nobody answers is cancelled** by the server after two
  minutes, and both ends are told. A receiver who has walked away is the
  ordinary case, not an exception.
* The server checks what it relays: only the offering side may send
  chunks, only for an accepted transfer, only in order, and never more
  in total than the offer claimed. A sender that breaks any of those ends
  the transfer for both.
* The server keeps nothing. It does not spool the file and it does not
  see it again once the last chunk is passed on.
* The receiver chooses the path, inside its own download directory, from
  a sanitised base name. The sender and the server have no say in it.
* At most 8 transfers open per person; at most 2 GiB per file.
* The SHA-256 is recomputed on arrival and a file that does not match is
  deleted. **That catches a truncated or corrupted transfer, including a
  relay that lost its place. It is not a security check** — anyone who can
  change the bytes can change the digest with them, and the bytes pass
  through the server.
* A transfer does not survive a dropped link. It is marked failed and the
  sender is asked again; resumption would need the receiver to persist
  partial state, the sender to keep the file unchanged and the server to
  remember both.

---

## 3. What this protocol does not do

* **No encryption.** Every frame, including every file, crosses the LAN in
  the clear.
* **No authentication.** Enrolment is open and an identity is whatever a
  client says it is. Nothing prevents somebody enrolling as your
  colleague.
* **No authorisation.** Anyone enrolled can join any room and read
  everything said in it from then on.
* **No federation.** One server, one LAN. Two servers do not know about
  each other, and a cursor from one means nothing to the other.
* **No delivery receipt from the other person.** `delivered` means the
  server took it, not that anybody read it.

[security.md](security.md) is the whole position, in two minutes.
