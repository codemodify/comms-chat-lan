package chat

import (
	"bufio"
	"encoding/json"
)

// The local protocol: JSON-RPC 2.0 over a Unix socket, one JSON object
// per line (NDJSON). comms-chat-lan-clientd answers it;
// comms-chat-lan-client-gui and comms-chat-lan-client-tui are clients of
// the same methods. Notifications — the events clientd pushes — have no
// "id".
//
// It is unchanged in shape from the version of this application that had
// no server, because nothing a front end asks for has changed: the
// difference is where clientd gets the answer from. See docs/protocol.md
// for what clientd says to the server.
//
// Neither front end holds state of its own beyond what it is showing. Any
// change a front end makes is a method call, and every front end hears
// about it through the same event, so two of them open at once agree.
//
// Every method is answered from clientd's local cache, so a front end
// keeps working — reading history, scrolling, composing — while the
// server is unreachable. status.get says whether it is.
//
// Methods:
//
//	ping
//	status.get
//	identity.get
//	identity.set        Identity (the id is ignored; it never changes)
//	presence.set        {presence}
//	peers.list
//	peers.block         {id, blocked}
//	conversations.list
//	conversations.get   {conv}
//	messages.list       {conv, limit?}
//	messages.send       {conv, body}
//	messages.markRead   {conv}
//	typing.set          {conv, typing}
//	rooms.list
//	rooms.join          {room}
//	rooms.leave         {room}
//	files.offer         {conv, path}   // path is read by the DAEMON's user
//	files.accept        {transfer}
//	files.decline       {transfer, reason?}
//	files.list
//	notify.get
//	notify.set          NotifyPrefs
//
// Events (daemon → client, no id):
//
//	chat.message        {conv, peer?, msg}
//	chat.state          {conv, msg}       // a delivery state changed
//	chat.peers          {peer?}           // the roster changed
//	chat.typing         {conv, peer, typing}
//	chat.transfer       {conv, peer, transfer}
//	chat.notify         {conv, peer, title, body}
//	chat.status         {text}
const RPCVersion = "2.0"

// Method names.
const (
	MethodPing         = "ping"
	MethodStatusGet    = "status.get"
	MethodIdentityGet  = "identity.get"
	MethodIdentitySet  = "identity.set"
	MethodPresenceSet  = "presence.set"
	MethodPeersList    = "peers.list"
	MethodPeersBlock   = "peers.block"
	MethodConvList     = "conversations.list"
	MethodConvGet      = "conversations.get"
	MethodMessagesList = "messages.list"
	MethodMessagesSend = "messages.send"
	MethodMessagesRead = "messages.markRead"
	MethodTypingSet    = "typing.set"
	MethodRoomsList    = "rooms.list"
	MethodRoomsJoin    = "rooms.join"
	MethodRoomsLeave   = "rooms.leave"
	MethodFilesOffer   = "files.offer"
	MethodFilesAccept  = "files.accept"
	MethodFilesDecline = "files.decline"
	MethodFilesList    = "files.list"
	MethodNotifyGet    = "notify.get"
	MethodNotifySet    = "notify.set"
)

// Event names. They are also the [NodeEvent.Kind] values, so the node's
// event and the wire notification are the same thing under one name.
const (
	EventMessage  = "chat.message"
	EventState    = "chat.state"
	EventPeers    = "chat.peers"
	EventTyping   = "chat.typing"
	EventTransfer = "chat.transfer"
	EventNotify   = "chat.notify"
	EventStatus   = "chat.status"
)

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// JSON-RPC error codes. The negative ones are the standard set; the
// application codes start at -32000.
const (
	ErrParse      = -32700
	ErrInvalidReq = -32600
	ErrNoMethod   = -32601
	ErrBadParams  = -32602
	ErrInternal   = -32603
	ErrApp        = -32000
	ErrClosed     = -32001
)

// The parameter and result types. They are exported because the two ends
// of this socket are now in two packages — [Client] here, the dispatch
// switch in chatclientd — and a shared struct beats two hand-written JSON
// objects that have to agree.

type ConvParams struct {
	Conv ConversationID `json:"conv"`
}

type ListParams struct {
	Conv  ConversationID `json:"conv"`
	Limit int            `json:"limit,omitempty"`
}

type SendParams struct {
	Conv ConversationID `json:"conv"`
	Body string         `json:"body"`
}

type TypingParams struct {
	Conv   ConversationID `json:"conv"`
	Typing bool           `json:"typing"`
}

type BlockParams struct {
	ID      PeerID `json:"id"`
	Blocked bool   `json:"blocked"`
}

type RoomParams struct {
	Room string `json:"room"`
}

type PresenceParams struct {
	Presence Presence `json:"presence"`
}

type OfferParams struct {
	Conv ConversationID `json:"conv"`
	Path string         `json:"path"`
}

type TransferParams struct {
	Transfer TransferID `json:"transfer"`
	Reason   string     `json:"reason,omitempty"`
}

type CountResult struct {
	Count int `json:"count"`
}

type OKResult struct {
	OK   bool   `json:"ok"`
	Room string `json:"room,omitempty"`
}

// Event is something clientd's clients should hear about: a message, a
// changed delivery state, a changed roster, a typing indicator, a
// transfer, a notification or a line for the status bar. One struct
// rather than seven: the RPC turns it into one JSON notification and the
// front ends switch on Kind.
type Event struct {
	Kind string `json:"kind"`
	// Conv is set for anything that belongs to a conversation.
	Conv ConversationID `json:"conv,omitempty"`
	Peer PeerID         `json:"peer,omitempty"`

	Msg      *Message  `json:"msg,omitempty"`
	Transfer *Transfer `json:"transfer,omitempty"`
	Typing   bool      `json:"typing,omitempty"`

	// Title and Body are set on EventNotify.
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	// Text is a plain line for the status bar and the log.
	Text string `json:"text,omitempty"`
}

// ConvView is one conversation with everything a front end needs to draw
// its header in a single round trip: the conversation, who is in it and
// who is typing right now.
type ConvView struct {
	Conv   Conversation `json:"conv"`
	Peers  []Peer       `json:"peers,omitempty"`
	Typing []PeerID     `json:"typing,omitempty"`
}

// MaxRPCLine bounds one line on the local socket. Nothing a front end
// sends is large — a file is named by path and read by clientd, never
// shipped over the local socket — so this is generous.
const MaxRPCLine = 4 * 1024 * 1024

// WriteJSON writes one JSON object and a newline, and flushes. Both ends
// of the socket frame the same way.
func WriteJSON(w *bufio.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}

// DecodeParams unmarshals a request's params, tolerating an absent one.
func DecodeParams[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	err := json.Unmarshal(raw, &v)
	return v, err
}
