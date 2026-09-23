# Security and privacy

This document exists because the alternative is that you work it out later.

**comms-chat-lan does not encrypt anything and does not authenticate
anybody.** Treat it the way you would treat talking out loud in the same
room: fine for "the build machine is back up", wrong for anything you would
not say where you could be overheard.

## What it does not protect against

### Anyone who can watch the network

Messages, file names and file contents cross the LAN in the clear. Another
machine on the same wifi, anything spanning the switch, a router you do not
control, a guest network that is not really isolated — all of them see
everything. There is no TLS, no key exchange and no message encryption.

### Anyone who claims to be someone else

A peer's id is 16 random bytes it generated itself. Its nickname and colour
are whatever it says they are. **Nothing proves that the peer claiming an
id is the one that generated it**, and nothing stops a second peer using
your colleague's nickname. There is no key, no fingerprint and nothing to
verify.

The one thing the app does do is refuse to let a *connected* peer claim to
be a *different* peer: a message is always filed under the id established
by the handshake on that connection, never under the id in the frame, and a
peer may not file messages into a conversation between two other people.
That is internal consistency, not authentication.

### Anyone who can reach your machine

Any host that can open a TCP connection to the advertised port can start a
conversation and can offer you files. An unannounced peer is accepted —
refusing them would mean the app stops working on a network where multicast
is blocked — but it is marked, and both front ends say "this peer connected
without announcing itself". You can block a peer, which drops it at accept.

### Traffic analysis, of the simplest kind

The beacon says, twelve times a minute, that you are here, what you call
yourself, and which rooms you are in, to everyone on the segment. That is
the point of it. `UITK_CHAT_NO_DISCOVERY=1` turns it off; you then have no
discovery.

## What it does do

### Nothing leaves this machine until you send it

There is no server, no account, no cloud and no telemetry. The app talks to
the machines on your LAN and to nothing else. No history is uploaded
anywhere; it is a directory of text files on your disk.

### Nothing is written to disk from a file offer until you accept it

An offer is a message and a row in a list. Only after an explicit accept
(or an auto-accept threshold you set yourself, which is zero by default)
does the daemon open a file at all.

When it does:

* the path is chosen by **the receiver**, inside its own download
  directory. The sender has no say in it.
* an incoming name is reduced to a single path component with control
  characters removed: `../../../.bashrc` becomes `bashrc`, `/etc/passwd`
  becomes `passwd`, a name that is nothing but dots becomes `file`.
* an existing file is never overwritten — a clash becomes `photo (2).png`.
* the daemon refuses more bytes than the offer claimed, so a "small" file
  cannot fill a disk, and a file over 2 GiB is refused outright.

There is a test for each of those sentences.

### The local socket is yours alone

The daemon exposes the whole history, the roster and the send path over its
Unix socket with no authentication of its own, so the socket **is** the
authorisation boundary and is treated as one:

* the directory holding it is created 0700 and its owner checked;
* the socket is bound under umask 0177 and chmodded 0600, so there is no
  window in which another user could connect;
* a lock file stops a second daemon stealing the path from a running one,
  rather than the older habit of removing the socket and hoping;
* every accepted connection is checked with `SO_PEERCRED` and dropped
  unless it comes from your own uid (or root, which can read the files
  anyway).

Another user on the same machine cannot read your history or send messages
as you.

### File integrity, and what that is not

Every transfer carries a SHA-256, recomputed on arrival, and a file that
does not match is deleted. **This catches a truncated or corrupted
transfer. It is not a security check.** Somebody who can change the bytes
in flight can change the digest with them. It is documented here rather
than allowed to look like protection.

### No invented cryptography

There is none. The only `crypto/*` used is `crypto/rand` for identifiers
and `crypto/sha256` for that integrity check. Nothing in this repository
implements a cipher, a key exchange or a protocol of its own devising, and
nothing should: if encryption is added it will be a reviewed library doing
a named protocol, and this document will say which.

## If you need more than this

You need a different tool, or this one over something that already provides
the guarantees: a WireGuard link, an SSH tunnel, a VLAN you trust. The app
has no opinion about what it runs over; it has an opinion about not
claiming to be what it is not.

## Reporting something

Open an issue at
<https://github.com/codemodify/comms-chat-lan>. Given the above, "messages
are not encrypted" is not a finding — it is the design, stated here. A
frame that escapes the checks in [protocol.md](protocol.md), a file that
lands outside the download directory, or a socket another user can reach,
are.
