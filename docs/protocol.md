# The protocol

comms-chat-lan speaks three protocols. Two of them are between machines and
are specified here: the **beacon**, a UDP multicast announcement that finds
peers, and the **conversation**, a TCP stream of JSON frames that carries
everything else. The third, the JSON-RPC between the daemon and its front
ends, is local to one machine and is described in
[architecture.md](architecture.md).

Everything here is version **1** (`chatcore.ProtocolVersion`). A peer
announcing or handshaking a different version is ignored rather than
half-understood: with one version in the wild there is nothing to
negotiate, and code that pretends to negotiate is code nobody has tested.

Forward compatibility is one rule: **unknown frame types are ignored.** A
future version may add frames, and this version will carry on talking to it.

---

## 1. The beacon

### Why it is not mDNS

The obvious answer to "find other copies of this app on the LAN" is
DNS-SD over mDNS, and we are not doing it. A correct mDNS responder is RFC
6762 and RFC 6763 — probing and conflict resolution, known-answer
suppression, cache-flush bits, negative responses, the shared/unique record
distinction — and a partial one is not merely incomplete: it *collides*
with the avahi daemon already running on the machine and misbehaves on the
wire for everybody else on the segment.

What this app needs from discovery is one sentence, repeated: *I am this
id, I am called this, dial me on this port.* That is about a hundred lines
we fully control, and that is what the beacon is.

### The cost, stated plainly

**comms-chat-lan is discoverable only by other copies of comms-chat-lan.**
`avahi-browse -a` will not show it. `dns-sd -B` will not show it. It
advertises no DNS-SD service type and answers no mDNS query. If you need
this app to appear in generic service-discovery tooling, that is a second
`Discovery` implementation somebody has to write; the interface is there
for it (see [architecture.md](architecture.md)).

### Wire format

One UDP datagram, sent to the multicast group:

| | |
|---|---|
| group | `239.192.77.77` |
| port | `47771` |
| TTL | `1` |
| maximum size | 1200 bytes |

`239.192.0.0/14` is the IPv4 organisation-local scope (RFC 2365): routed
inside a site, never off it. A TTL of 1 keeps a packet on the local
segment.

The datagram is the magic string `CHATLAN/1 ` (ten bytes, note the trailing
space) followed by one compact JSON object:

```
CHATLAN/1 {"v":1,"id":"5f3a…","nick":"sam","color":"#2f6fd0","host":"kestrel","port":47772,"rooms":["general"],"pres":"online"}
```

| field | type | meaning |
|---|---|---|
| `v` | int | protocol version; must be 1 |
| `id` | string | the peer's id: 32 lowercase hex digits (16 random bytes) |
| `nick` | string | display name, at most 32 runes after trimming |
| `color` | string | avatar colour, `#rrggbb` |
| `host` | string | the announcer's host name, for display only |
| `port` | int | the TCP port the peer accepts conversations on |
| `rooms` | []string | rooms the peer has joined, lower-cased |
| `pres` | string | `online`, `away` or `busy` |
| `bye` | bool | present and true only in the farewell packet |

**The peer's address is the packet's source IP plus the announced `port`.**
It is never read from the payload. An announcement can lie about its
nickname; it cannot point anyone at a third machine.

A packet that fails any of these checks is dropped silently: wrong magic,
wrong version, malformed id, port outside 1–65535, an over-long nickname or
host, more than 32 rooms, more than 1200 bytes.

### Timing

| | |
|---|---|
| announce every | 12 seconds |
| expire a peer after | 40 seconds of silence |

Forty seconds is three missed announcements plus a margin. One lost packet
on a busy wireless network is routine and must not make somebody blink out
of the roster.

On a clean shutdown a peer sends one packet with `"bye":true` and
`"pres":"offline"`. It is best effort — a `kill -9` sends nothing — and
the only cost of losing it is that the other side waits out the expiry.

### Several instances on one machine

The beacon socket is bound with `SO_REUSEADDR` and `SO_REUSEPORT`, and
multicast loopback is on. Running two copies of the app side by side is the
first thing anybody trying a LAN chat app does, and without this the second
one fails to bind.

A beacon ignores announcements carrying its own peer id, so a machine never
sees itself as a peer.

### Turning it off

`UITK_CHAT_NO_DISCOVERY=1` (or `comms-chatd -no-discovery`) starts the
daemon with a `Discovery` that announces nothing and hears nothing. No
multicast leaves the machine. The app still works between peers that have
already been added, and this is what the tests use.

`UITK_CHAT_IFACE=<name>` pins the beacon to one interface. By default it
announces on every interface that is up, multicast-capable and has an IPv4
address — a laptop on wifi with a docker bridge up would otherwise announce
on whichever one the routing table prefers, which is rarely the one the
other people are on.

---

## 2. The conversation

Two peers talk over one plain TCP connection. Either may open it; there is
no client and no server.

### Framing

```
+--------+----------------------------+
| uint32 |  that many bytes of JSON   |
| BE len |                            |
+--------+----------------------------+
```

Length-prefixed rather than line-delimited because file chunks share the
stream, and base64 inside a line-oriented protocol makes the framing depend
on the payload. The maximum frame is **1 MiB**; a larger length is an error
and the connection is dropped. It is not resynchronised — trying to
resynchronise a stream after a framing error is how a parser becomes an
attack surface.

JSON rather than a packed binary encoding because the whole protocol fits
on this page, and a chat app is never limited by its frame encoder.

### The handshake

Both sides send `hello` immediately on connecting. Neither waits for the
other's first.

```json
{"t":"hello","v":1,"from":"5f3a…","nick":"sam","color":"#2f6fd0","host":"kestrel","port":47772,"rooms":["general"]}
```

The connection is dropped if the frame is not a `hello`, the version is not
1, or the id is not 32 hex digits. A dialler also drops the connection if
the answering peer's id is not the one it meant to dial — the address it
had may belong to somebody else now, and filing the conversation under the
wrong peer would be worse than failing.

### Frames

| `t` | fields | meaning |
|---|---|---|
| `hello` | `v`, `from`, `nick`, `color`, `host`, `port`, `rooms` | the handshake, both directions |
| `msg` | `id`, `conv`, `body`, `sent`, `seq`, `nick` | one message |
| `ack` | `id` | the message was stored |
| `typing` | `conv`, `typing` | advisory, never stored |
| `presence` | `pres`, `nick`, `color`, `rooms` | a change between beacons |
| `offer` | `tid`, `name`, `size`, `mime`, `sha256` | a file is offered |
| `accept` | `tid` | the offer is accepted |
| `decline` | `tid`, `reason` | the offer is refused, or a transfer cancelled |
| `chunk` | `tid`, `off`, `data` | one slice of a file; `data` is base64 |
| `done` | `tid`, `sha256`, `size` | the last chunk has been sent |
| `bye` | — | a clean close |

### What a peer may and may not assert

A frame is data from an unauthenticated stranger. Two things are
recomputed rather than believed:

* **The sender.** A message is filed under the peer id established by the
  handshake on this connection, never under the `from` in the frame. A peer
  cannot post as somebody else.
* **The conversation.** A peer may name a room, or name nothing. If it
  names a direct conversation, the only one it can possibly mean is the one
  between us and it, and that is what it gets.

A message whose body is over 8000 runes is refused. A room name is
lower-cased, trimmed and capped at 48 characters.

### Ordering

Messages are ordered by `(sent, seq, id)`:

* `sent` is the **sender's** wall clock. It is not trustworthy.
* `seq` is the sender's own monotonic counter, which breaks a tie.
* `id` breaks the remaining tie and is globally unique by construction:
  `<short peer id>-<nanoseconds>-<random>`.

Every peer holding the same set of messages therefore shows them in the
same order, whatever order they arrived in, with nobody being the server.
It is not necessarily the *true* order — with no clock anyone trusts, it
cannot be — and two peers whose clocks disagree will agree with each other
while both being wrong about reality.

### Delivery, and a peer that is offline

A message we send is stored at once as `queued` and the composer returns.
It is then:

1. `sending` while it is on the wire,
2. `delivered` when the `ack` comes back,
3. back to `queued` if the peer could not be reached.

Back to `queued`, not `failed`: a peer that is not answering is asleep, not
gone. When the beacon says it is back, the queue is flushed to it oldest
first, automatically. A daemon restart with a message still `sending`
reloads it as `queued`, so nothing is silently lost.

A duplicate is free: the receiver recognises the message id, drops it, and
still sends the `ack` — because a resend happens precisely when the first
ack was lost.

### Two peers dialling each other at once

Both open a connection, so the pair briefly has two. Both sides keep **the
connection whose dialler is the numerically smaller peer id** and drop the
other. It is computed from the only facts both sides have — the two ids —
so both reach the same answer, and the pair ends with exactly one
connection rather than none or two.

### A message from a peer nobody announced

Accepted, and marked. A network with broken multicast must not mean a dead
app, so an unsolicited inbound connection is honoured; but the peer's
roster entry records that we never heard it announce itself, and both front
ends say so. Anyone on the LAN can be one.

A blocked peer is refused at accept, before the handshake is believed.

### Rooms

A room is a name. Anyone who joins a room of the same name is in it; there
is nobody to ask. Membership is what peers advertise in their beacon.

A room message is sent to every peer that advertises the room **at the
moment of sending**. There is no history sync: a peer that was away does
not receive it later. With no server there is nobody to ask for what was
missed, and pretending otherwise would be the more dishonest design.

### Typing indicators

Best effort, never queued, never stored. A late typing indicator is worse
than none. An indicator that is not refreshed expires after 6 seconds, so
somebody who starts typing and then walks away does not appear to be typing
for ever.

### File transfer

```
sender                                receiver
  |  offer (tid, name, size, sha256)  |
  |---------------------------------->|
  |                                   |  a person decides
  |          accept (tid)             |
  |<----------------------------------|
  |  chunk (tid, off=0,       data)   |
  |---------------------------------->|
  |  chunk (tid, off=65536,   data)   |
  |---------------------------------->|
  |            …                      |
  |  done (tid, sha256, size)         |
  |---------------------------------->|
```

* **Nothing is written to disk before an accept.** The offer is a message
  in the conversation and a row in the transfer list; that is all.
* The receiver chooses the path, from a sanitised base name, inside its own
  download directory. The sender has no say in where its file goes. An
  incoming name is reduced to a single path component with control
  characters removed, and a name that already exists gets ` (2)`, ` (3)`
  and so on — an incoming file never overwrites anything.
* Chunks are 64 KiB before base64. A chunk at the wrong offset, or one that
  would take the total past the offered size, drops the connection: the
  peer is not following the protocol, and the alternative is writing bytes
  nobody agreed to.
* The SHA-256 is verified on arrival, and a file that does not match is
  deleted. **This catches corruption, not tampering.** Somebody who can
  change the bytes can change the digest with them.
* A file over 2 GiB is refused, and one peer may have at most 8 transfers
  open at a time.
* A transfer interrupted by a restart is not resumed. It is shown as
  failed and the sender is asked again.
* Files go to one peer, never to a room.

---

## 3. What this protocol does not do

It does not encrypt. It does not authenticate. It does not resist a
determined participant on your network. See
[security.md](security.md), which says exactly what that means.
