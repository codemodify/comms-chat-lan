package chatcore

import "encoding/json"

// The daemon protocol: JSON-RPC 2.0 over a Unix socket, one JSON object
// per line (NDJSON). comms-chatd is the server; comms-chat (the GUI) and
// comms-chat-tui (the terminal front end) are clients of the same methods.
// Notifications — the events the daemon pushes — have no "id".
//
// Neither front end holds state of its own beyond what it is showing. Any
// change a front end makes is a method call, and every front end hears
// about it through the same event, so two of them open at once agree.
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

// Wire parameter and result types. They are exported where a front end has
// to name one; the rest are internal to the RPC.

type convParams struct {
	Conv ConversationID `json:"conv"`
}

type listParams struct {
	Conv  ConversationID `json:"conv"`
	Limit int            `json:"limit,omitempty"`
}

type sendParams struct {
	Conv ConversationID `json:"conv"`
	Body string         `json:"body"`
}

type typingParams struct {
	Conv   ConversationID `json:"conv"`
	Typing bool           `json:"typing"`
}

type blockParams struct {
	ID      PeerID `json:"id"`
	Blocked bool   `json:"blocked"`
}

type roomParams struct {
	Room string `json:"room"`
}

type presenceParams struct {
	Presence Presence `json:"presence"`
}

type offerParams struct {
	Conv ConversationID `json:"conv"`
	Path string         `json:"path"`
}

type transferParams struct {
	Transfer TransferID `json:"transfer"`
	Reason   string     `json:"reason,omitempty"`
}

type countResult struct {
	Count int `json:"count"`
}

type okResult struct {
	OK   bool   `json:"ok"`
	Room string `json:"room,omitempty"`
}

// ConvView is one conversation with everything a front end needs to draw
// its header in a single round trip: the conversation, who is in it and
// who is typing right now.
type ConvView struct {
	Conv   Conversation `json:"conv"`
	Peers  []Peer       `json:"peers,omitempty"`
	Typing []PeerID     `json:"typing,omitempty"`
}

func decodeParams[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	err := json.Unmarshal(raw, &v)
	return v, err
}
