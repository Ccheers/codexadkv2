// Package jsonrpc implements the JSON-RPC 2.0 framing, correlation, and
// transport layer for the Codex app-server protocol.
//
// It is internal on purpose: the wire representation is an implementation
// detail of the codex package, and keeping it unexported leaves it free to
// change without breaking users.
//
// Note that app-server omits the "jsonrpc":"2.0" header on the wire, so this
// package does too.
package jsonrpc

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Message is a decoded frame. Exactly which fields are populated determines
// what kind of message it is, and the four kinds are distinguished the same way
// the protocol does it:
//
//	id + method  -> a request (server -> client, must be answered)
//	id, no method -> a response to one of our requests
//	method, no id -> a notification (one-way)
type Message struct {
	ID     *ID             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`

	// EmittedAtMs is the server's emission timestamp, present on notification
	// envelopes from newer servers and absent on older ones.
	EmittedAtMs *int64 `json:"emittedAtMs,omitempty"`
}

// IsResponse reports whether m answers a request we sent.
func (m *Message) IsResponse() bool { return m.ID != nil && m.Method == "" }

// IsRequest reports whether m is a server-initiated request that we must answer.
func (m *Message) IsRequest() bool { return m.ID != nil && m.Method != "" }

// IsNotification reports whether m is a one-way notification.
func (m *Message) IsNotification() bool { return m.ID == nil && m.Method != "" }

// ID is a JSON-RPC request id. The protocol allows either a string or a number,
// so it is stored in whichever form it arrived in and echoed back identically.
// Echoing a number as a string (or the reverse) breaks correlation on some
// servers, so the original representation is preserved.
type ID struct {
	num   int64
	str   string
	isStr bool
}

// NumberID returns a numeric request id.
func NumberID(n int64) ID { return ID{num: n} }

// StringID returns a string request id.
func StringID(s string) ID { return ID{str: s, isStr: true} }

// String renders the id for logging and for use as a map key. The prefix keeps
// the numeric id 1 and the string id "1" from colliding.
func (id ID) String() string {
	if id.isStr {
		return "s" + id.str
	}
	return fmt.Sprintf("n%d", id.num)
}

// MarshalJSON implements json.Marshaler.
func (id ID) MarshalJSON() ([]byte, error) {
	if id.isStr {
		return json.Marshal(id.str)
	}
	return json.Marshal(id.num)
}

// UnmarshalJSON implements json.Unmarshaler.
func (id *ID) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		id.num, id.isStr = n, false
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		id.str, id.isStr = s, true
		return nil
	}
	return fmt.Errorf("jsonrpc: request id must be a string or a number, got %s", data)
}

// Error is a JSON-RPC error object.
//
// Code and Data are retained deliberately. A client that keeps only Message is
// forced to recover intent by pattern-matching human-readable text, which
// breaks the moment the server rewords anything.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("jsonrpc error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// JSON-RPC and app-server specific error codes worth branching on.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// CodeServerOverloaded is app-server specific: it is returned when the
	// request ingress queue is full. The documented remedy is to retry with
	// exponential backoff and jitter.
	CodeServerOverloaded = -32001
)

// Sentinel errors for the codes a caller is most likely to handle. Match them
// with errors.Is; the underlying *Error remains reachable with errors.As.
var (
	ErrMethodNotFound   = errors.New("method not found")
	ErrServerOverloaded = errors.New("server overloaded")
	ErrInvalidParams    = errors.New("invalid params")
	ErrInternalError    = errors.New("internal server error")
)

// Is lets errors.Is match an *Error against the sentinels above.
func (e *Error) Is(target error) bool {
	switch e.Code {
	case CodeMethodNotFound:
		return target == ErrMethodNotFound
	case CodeServerOverloaded:
		return target == ErrServerOverloaded
	case CodeInvalidParams:
		return target == ErrInvalidParams
	case CodeInternalError:
		return target == ErrInternalError
	}
	return false
}
