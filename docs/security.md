# Security and privacy

This document exists because the alternative is that you work it out
later.

**comms-chat-lan does not encrypt anything and does not authenticate
anybody. Enrolment with the server is open: whoever can reach it is in.**
Treat it the way you would treat talking out loud in the same room: fine
for "the build machine is back up", wrong for anything you would not say
where you could be overheard.

The peer-to-peer version of this application said the same thing about a
different design. What changed is that there is now one place that holds
everybody's conversations, and one door into it that is not locked. Both
of those are worth being precise about.

## What it does not protect against

### Anyone who can watch the network

Messages, file names and file contents cross the LAN in the clear, twice
now — once from the sender to the server, once from the server to each
recipient. Another machine on the same wifi, anything spanning the switch,
a router you do not control, a guest network that is not really isolated —
all of them see everything. There is no TLS, no key exchange and no
message encryption.

### Anyone who can reach the server

**Enrolment is open, by design.** There is no code, no passphrase and no
approval; the server records who joined and refuses nobody. Anyone who can
open a TCP connection to port 47772 can therefore:

* be on the LAN's roster, under any name and colour they like;
* join any room, and read everything said in it from that moment on —
  including the room's recent history, which is sent to whoever joins;
* send messages to anybody enrolled;
* offer files to anybody connected.

**This is exactly as much protection as the LAN itself.** If your network
is a flat office wifi with a guest password on a sticker, that is who can
join. The server is only as private as the segment it is on, and nothing
in this application changes that. Put it behind the boundary you actually
trust — a VLAN, a WireGuard link, a machine that is not on the guest
network — or accept that the boundary is the building.

### Anyone who claims to be someone else

An id is 16 random bytes a client generated itself. Its nickname and
colour are whatever it says they are. **Nothing proves that the client
claiming an id is the one that generated it**, and nothing stops a second
client using your colleague's nickname, or their id. There is no key, no
fingerprint and nothing to verify.

Two clients enrolling under one identity is a supported case — a desktop
and a laptop sharing an `identity.json` — so the server cannot treat it as
suspicious, because it is not. Which means somebody who guesses or copies
an id receives that person's one-to-one messages from then on.

What the server *does* do is refuse to let a connected client speak as
anybody else: a message is filed under the identity that enrolled on that
connection, never the one in the frame, and a client may only name a room
it has joined or a conversation it is one half of. That is internal
consistency, not authentication.

### Whoever runs the server

Every message and every file passes through it. Every conversation is
stored on its disk, in plain text, and it can read all of them. If that is
not obvious from "there is a server", it is stated here.

The server does keep files only in flight: it relays the bytes and does
not spool them or keep a copy.

### Traffic analysis, of the simplest kind

The server announces, five times a minute, that it exists, what it is
called and which port to connect to, to everyone on the segment.
Clients announce nothing at all, which is one thing the peer-to-peer
version could not say. `UITK_CHAT_NO_DISCOVERY=1` turns the announcement
off; every client then has to be told the address.

### Losing the server

It is a single point of failure and there is no second one. If the machine
running it is off, nobody can send anything new. What each person keeps is
their own cache: the history they have already been told about, readable
with the server switched off, which is most of what people actually want
when the server is down.

## What it does do

### Nothing leaves this machine until you send it

There is no account, no cloud and no telemetry. The application talks to
one server on your own network and to nothing else. No history is uploaded
anywhere; it is a directory of text files on your disk and another on the
server's.

### Nothing is written to disk from a file offer until you accept it

An offer is a message and a row in a list. Only after an explicit accept —
or an auto-accept threshold you set yourself, which is zero by default —
does the client daemon open a file at all.

When it does:

* the path is chosen by **the receiver**, inside its own download
  directory. Neither the sender nor the server has any say in it.
* an incoming name is reduced to a single path component with control
  characters removed: `../../../.bashrc` becomes `bashrc`, `/etc/passwd`
  becomes `passwd`, a name that is nothing but dots becomes `file`.
* an existing file is never overwritten — a clash becomes `photo (2).png`.
* more bytes than the offer claimed are refused, by the server on the way
  through and by the receiver on arrival, so a "small" file cannot fill a
  disk; a file over 2 GiB is refused outright.

There is a test for each of those sentences.

### The local socket is yours alone

The client daemon exposes the whole history, the roster and the send path
over its Unix socket with no authentication of its own, so the socket
**is** the authorisation boundary and is treated as one:

* the directory holding it is created 0700 and its owner checked;
* the socket is bound under umask 0177 and chmodded 0600, so there is no
  window in which another user could connect;
* a lock file stops a second daemon stealing the path from a running one,
  rather than the older habit of removing the socket and hoping;
* every accepted connection is checked with `SO_PEERCRED` and dropped
  unless it comes from your own uid (or root, which can read the files
  anyway).

Another user on the same machine cannot read your history or send
messages as you.

### File integrity, and what that is not

Every transfer carries a SHA-256, recomputed on arrival, and a file that
does not match is deleted. **This catches a truncated or corrupted
transfer, including a relay that lost its place. It is not a security
check.** Anyone who can change the bytes in flight can change the digest
with them, and the bytes go through the server. It is documented here
rather than allowed to look like protection.

### No invented cryptography

There is none. The only `crypto/*` used is `crypto/rand` for identifiers
and `crypto/sha256` for that integrity check. Nothing in this repository
implements a cipher, a key exchange or a protocol of its own devising, and
nothing should: if encryption is added it will be a reviewed library doing
a named protocol, and this document will say which.

## If you need more than this

You need a different tool, or this one over something that already
provides the guarantees: a WireGuard link, an SSH tunnel, a VLAN you
trust. Put the server inside that boundary and the open enrolment becomes
"anyone inside the boundary", which may be exactly what you want.

The application has no opinion about what it runs over; it has an opinion
about not claiming to be what it is not.

## Reporting something

Open an issue at <https://github.com/codemodify/comms-chat-lan>. Given the
above, "messages are not encrypted" and "anyone can enrol" are not
findings — they are the design, stated here. A frame that escapes the
checks in [protocol.md](protocol.md), a client that can make the server
file a message as somebody else, a file that lands outside the download
directory, or a socket another user can reach, are.
