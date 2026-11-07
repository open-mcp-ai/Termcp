package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
)

// Middleware wraps a tool handler. The outermost is applied last, so the first
// one passed in is the closest to the handler.
//
// It is a plain function rather than an interface because that is what it is: one
// call in, one call out. A type would be a name for a signature.
type Middleware func(ToolHandler) ToolHandler

// ToolHandler answers one tools/call.
//
// ctx carries whatever the transport lifted out of the HTTP request (see the
// Server's context function), which is how a handler learns the address the
// client actually reached.
//
// TWO WAYS TO FAIL, and they mean different things. A non-nil error is a
// PROTOCOL failure: the call did not reach the tool's logic, and the client gets a
// JSON-RPC error. A result with IsError set is a TOOL failure: the tool ran and is
// reporting its own outcome, which the client shows as the tool's answer. Neither
// is a panic — see the middleware in this package for that.
type ToolHandler func(ctx context.Context, req CallToolRequest) (*CallToolResult, error)

// PromptHandler answers one prompts/get.
type PromptHandler func(ctx context.Context, req GetPromptRequest) (*GetPromptResult, error)

// Hooks are the points where a caller wants to observe or amend protocol
// traffic. Each is optional; a nil slice is the common case.
type Hooks struct {
	// OnRegisterSession fires when a transport creates a session, before the
	// client has said anything. The session has an id and nothing else yet.
	OnRegisterSession []func(ctx context.Context, session Session)
	// OnUnregisterSession fires when a session ends.
	OnUnregisterSession []func(ctx context.Context, session Session)
	// OnAfterInitialize fires once the handshake is answered, so a caller can
	// record what the client said about itself.
	OnAfterInitialize []func(ctx context.Context, req *InitializeRequest, res *InitializeResult)
	// OnAfterListTools fires after tools/list is built, so a caller can amend the
	// listing per request — which is how a per-request address reaches a client.
	OnAfterListTools []func(ctx context.Context, req *ListToolsRequest, res *ListToolsResult)
}

// Session is one connected client.
//
// ID is opaque to callers; it is what a registry keys on to know who is attached.
type Session interface {
	SessionID() string
	// Notify delivers an unsolicited notification to this client. A transport
	// that cannot deliver returns an error rather than dropping it silently.
	Notify(ctx context.Context, method string, params any) error
	// GetClientInfo returns what the client called itself in initialize. It is
	// zero-valued for a client that said nothing, which is legal: clientInfo is
	// optional in the handshake.
	GetClientInfo() Implementation
}

// ToolFuncs is the registration surface: what this server can do.
type ToolFuncs struct {
	Tools   map[string]Tool
	Prompts map[string]Prompt

	toolHandlers   map[string]ToolHandler
	promptHandlers map[string]PromptHandler
}

// Server is a protocol endpoint: it turns JSON-RPC frames into handler calls.
//
// It owns no listener and no HTTP. A transport calls Handle with a frame and a
// Session and gets back a frame, which is what keeps SSE, streamable HTTP and any
// future transport sharing one dispatch path instead of three copies of it.
type Server struct {
	name         string
	version      string
	instructions string

	tools   map[string]Tool
	prompts map[string]Prompt

	toolHandlers   map[string]ToolHandler
	promptHandlers map[string]PromptHandler

	hooks Hooks

	// middleware wraps every tool handler. Applied in order at call time rather
	// than at registration, so a caller can add one after the tools exist.
	middleware []Middleware

	// inbound answers requests a CLIENT sends to this server beyond the ones
	// modelled here (resources/*, elicitation/*, tasks/*, completions/*).
	//
	// A table rather than built-in implementations, for the reason the whole
	// package exists: an unregistered method costs nothing, and a registered one
	// is code somebody actually runs. Built-in support for a surface no caller
	// uses would be an implementation nobody verifies against a moving spec.
	inbound map[string]InboundHandler

	sessions   sync.Map // session id -> Session
	seq        atomic.Uint64
	closed     atomic.Bool
	toolChange atomic.Bool
}

// NewServer builds a protocol endpoint.
func NewServer(name, version, instructions string, hooks Hooks) *Server {
	return &Server{
		name:           name,
		version:        version,
		instructions:   instructions,
		tools:          map[string]Tool{},
		prompts:        map[string]Prompt{},
		toolHandlers:   map[string]ToolHandler{},
		promptHandlers: map[string]PromptHandler{},
		inbound:        map[string]InboundHandler{},
		hooks:          hooks,
	}
}

// AddTool registers a tool and the handler that answers it.
func (s *Server) AddTool(t Tool, h ToolHandler) {
	s.tools[t.Name] = t
	s.toolHandlers[t.Name] = h
}

// AddPrompt registers a prompt and the handler that renders it.
func (s *Server) AddPrompt(p Prompt, h PromptHandler) {
	s.prompts[p.Name] = p
	s.promptHandlers[p.Name] = h
}

// InboundHandler answers a method this package does not model.
//
// It is the escape hatch for the rest of the specification: resources, elicitation,
// tasks, completions. Registering one makes it reachable; NOT registering it leaves
// the method refused with method-not-found, which is what a client should see from a
// server that does not offer it.
type InboundHandler func(ctx context.Context, params json.RawMessage) (any, *RPCError)

// Use appends a middleware. It applies to every tool call, including ones whose
// tool was registered before the middleware.
func (s *Server) Use(m Middleware) {
	s.middleware = append(s.middleware, m)
}

// OnMethod registers a handler for one method name.
//
// Names are matched exactly, including the namespace, so "resources/list" and
// "resources/read" are separate registrations. A caller that wants to serve a whole
// namespace registers each method it implements — an unimplemented one is refused
// rather than answered with a half-truth.
func (s *Server) OnMethod(method string, h InboundHandler) {
	s.inbound[method] = h
}

// SetToolListChanged records that the tool list can change at runtime, so the
// initialize answer advertises the notification a client would wait for.
func (s *Server) SetToolListChanged(v bool) { s.toolChange.Store(v) }

// Sessions returns the attached sessions, in id order so a caller that renders
// them produces a stable list.
func (s *Server) Sessions() []Session {
	out := make([]Session, 0, 8)
	s.sessions.Range(func(_, v any) bool {
		if sess, ok := v.(Session); ok {
			out = append(out, sess)
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID() < out[j].SessionID() })
	return out
}

// registerSession records a new session and fires the hook.
func (s *Server) registerSession(ctx context.Context, sess Session) {
	s.sessions.Store(sess.SessionID(), sess)
	for _, fn := range s.hooks.OnRegisterSession {
		fn(ctx, sess)
	}
}

// unregisterSession drops a session and fires the hook.
func (s *Server) unregisterSession(ctx context.Context, sess Session) {
	s.sessions.Delete(sess.SessionID())
	for _, fn := range s.hooks.OnUnregisterSession {
		fn(ctx, sess)
	}
}

// Close ends every session. It is idempotent.
func (s *Server) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	s.sessions.Range(func(k, _ any) bool {
		s.sessions.Delete(k)
		return true
	})
}

// Handle answers one inbound frame.
//
// A notification (no id) is handled and answered with nil: the sender wants no
// reply, and sending one is a protocol violation. An error in a REQUEST is a
// Response carrying the error, never a nil return, because a client that gets
// nothing back waits forever.
func (s *Server) Handle(ctx context.Context, raw []byte, sess Session) []byte {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return s.encodeError(nil, CodeParseError, "parse error: "+err.Error())
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return s.encodeError(req.ID, CodeInvalidRequest, "jsonrpc must be \"2.0\"")
	}

	// An answer to something this server asked (sampling) is not a request: it
	// has an id and no method. Routing it to the waiter is the whole point of
	// keeping a pending table, and dispatching it would answer "method not found"
	// to a client that just told us what we wanted to know.
	if req.Method == "" {
		if c, ok := sess.(interface{ deliver(*Response) bool }); ok {
			var res Response
			if err := json.Unmarshal(raw, &res); err == nil {
				c.deliver(&res)
			}
		}
		return nil
	}

	// A frame with no id is a notification: no answer is sent, ever.
	isNotification := IDString(req.ID) == ""

	result, rpcErr := s.dispatch(WithSessionContext(ctx, sess), &req, sess)
	if isNotification {
		return nil
	}
	if rpcErr != nil {
		return s.encodeError(req.ID, rpcErr.Code, rpcErr.Message)
	}
	return s.encodeResult(req.ID, result)
}

// dispatch routes one method. It returns either a result to encode or an error.
func (s *Server) dispatch(ctx context.Context, req *Request, sess Session) (any, *RPCError) {
	switch req.Method {
	case "initialize":
		var p InitializeRequest
		// Empty params are legal: the spec's fields are all optional to SEND, and a
		// client that omits them is a client that wants the defaults. Only malformed
		// JSON is an error.
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p.Params); err != nil {
				return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid initialize params: " + err.Error()}
			}
		}
		return s.initialize(ctx, &p, sess), nil

	case "ping":
		return struct{}{}, nil

	case "tools/list":
		var p ListToolsRequest
		// An empty params object is legal; a malformed one is not worth failing the
		// call over, since the request carries nothing this server reads.
		_ = json.Unmarshal(req.Params, &p)
		return s.listTools(ctx, &p), nil

	case "tools/call":
		var p CallToolRequest
		if err := json.Unmarshal(req.Params, &p.Params); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid tools/call params: " + err.Error()}
		}
		return s.callTool(ctx, &p)

	case "prompts/list":
		return s.listPrompts(), nil

	case "prompts/get":
		var p GetPromptRequest
		if err := json.Unmarshal(req.Params, &p.Params); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid prompts/get params: " + err.Error()}
		}
		return s.getPrompt(ctx, &p)

	case "notifications/initialized", "notifications/cancelled", "notifications/roots/list_changed":
		return nil, nil

	default:
		// A registered escape hatch, then method-not-found. The order matters: the
		// table is how a caller serves resources/elicitation/tasks without this
		// package modelling them.
		if h, ok := s.inbound[req.Method]; ok {
			return h(ctx, req.Params)
		}
		// An unknown method is the client's mistake to see, not a silent no-op:
		// a client that mistyped a method would otherwise conclude the server is
		// broken.
		return nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

func (s *Server) initialize(ctx context.Context, p *InitializeRequest, sess Session) *InitializeResult {
	caps := ServerCapabilities{}
	if len(s.tools) > 0 {
		caps.Tools = &ListChangedCapability{ListChanged: s.toolChange.Load()}
	}
	if len(s.prompts) > 0 {
		caps.Prompts = &ListChangedCapability{}
	}
	res := &InitializeResult{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    caps,
		ServerInfo:      Implementation{Name: s.name, Version: s.version},
		Instructions:    s.instructions,
	}
	// The client's self-description is per session, so it is recorded on the one
	// this request arrived on. A client that sends no clientInfo leaves the zero
	// value, which a display layer must render as "unnamed" rather than as a name.
	// An interface check rather than a *client assertion: each transport wraps the
	// shared state in its own type, and the method is promoted through embedding.
	if withInfo, ok := sess.(interface{ noteClientInfo(Implementation) }); ok {
		withInfo.noteClientInfo(p.Params.ClientInfo)
	}
	for _, fn := range s.hooks.OnAfterInitialize {
		fn(ctx, p, res)
	}
	return res
}

func (s *Server) listTools(ctx context.Context, p *ListToolsRequest) *ListToolsResult {
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	res := &ListToolsResult{Tools: make([]Tool, 0, len(names))}
	for _, name := range names {
		res.Tools = append(res.Tools, s.tools[name])
	}
	for _, fn := range s.hooks.OnAfterListTools {
		fn(ctx, p, res)
	}
	return res
}

func (s *Server) callTool(ctx context.Context, p *CallToolRequest) (any, *RPCError) {
	h, ok := s.toolHandlers[p.Params.Name]
	if !ok {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "unknown tool: " + p.Params.Name}
	}
	// The innermost panic guard, so a middleware that panics is also caught. The
	// middleware chain is composed per call because it may grow after registration.
	//
	// A panic is answered as an error rather than killing the process: one bad call
	// must not take down every attached session.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("mcp: panic in tool %q: %v", p.Params.Name, r)
		}
	}()
	for i := len(s.middleware) - 1; i >= 0; i-- {
		h = s.middleware[i](h)
	}
	res, err := h(ctx, *p)
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: err.Error()}
	}
	if res == nil {
		return NewToolResultText(""), nil
	}
	return res, nil
}

func (s *Server) listPrompts() any {
	names := make([]string, 0, len(s.prompts))
	for name := range s.prompts {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Prompt, 0, len(names))
	for _, name := range names {
		out = append(out, s.prompts[name])
	}
	return struct {
		Prompts []Prompt `json:"prompts"`
	}{Prompts: out}
}

func (s *Server) getPrompt(ctx context.Context, p *GetPromptRequest) (any, *RPCError) {
	h, ok := s.promptHandlers[p.Params.Name]
	if !ok {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "unknown prompt: " + p.Params.Name}
	}
	res, err := h(ctx, *p)
	if err != nil {
		return nil, &RPCError{Code: CodeInternalError, Message: err.Error()}
	}
	return res, nil
}

func (s *Server) encodeResult(id json.RawMessage, result any) []byte {
	b, err := json.Marshal(Response{JSONRPC: "2.0", ID: id, Result: result})
	if err != nil {
		return s.encodeError(id, CodeInternalError, "encode result: "+err.Error())
	}
	return b
}

func (s *Server) encodeError(id json.RawMessage, code int, msg string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, err := json.Marshal(Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}})
	if err != nil {
		// Nothing left to do: a response that cannot be encoded is a bug in this
		// package, and the client will time out. Log so it is not silent.
		log.Printf("mcpv1: cannot encode error response: %v", err)
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":null,"error":{"code":%d,"message":"internal error"}}`, CodeInternalError))
	}
	return b
}

// sessionKey carries the Session through a request's context.
//
// A handler needs the session for the two things only it knows: who is attached
// (a per-request fact) and how to reach that client unprompted. Putting it on the
// context is what makes the transports interchangeable — the SSE one, the
// streamable one and an in-process test all hand the handler the same shape.
type sessionKey struct{}

// WithSessionContext returns a context carrying sess.
func WithSessionContext(ctx context.Context, sess Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, sess)
}

// SessionFromContext returns the session this request arrived on, or nil.
//
// nil is a real answer, not an error: an in-process call (a test, the REST
// entrance) has no attached client, and a handler that cannot work without one
// must say so rather than assume a zero value.
func SessionFromContext(ctx context.Context) Session {
	sess, _ := ctx.Value(sessionKey{}).(Session)
	return sess
}

// Tools returns the registered tools, in name order.
//
// It is the read half of AddTool, for a caller that needs to inspect what this
// server offers without going through a transport — a test asserting a schema, or
// a wrapper forwarding the listing somewhere else.
func (s *Server) Tools() []Tool {
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Tool, 0, len(names))
	for _, name := range names {
		out = append(out, s.tools[name])
	}
	return out
}

// Tool returns one registered tool and whether it exists.
func (s *Server) Tool(name string) (Tool, bool) {
	t, ok := s.tools[name]
	return t, ok
}

// NotifyAll delivers a notification to every attached client.
//
// It is best-effort by design: one client that has stopped reading must not stop
// the others from hearing something, and the caller of a broadcast has no useful
// way to handle per-client errors. The count returned says how many accepted it.
func (s *Server) NotifyAll(ctx context.Context, method string, params any) int {
	delivered := 0
	s.sessions.Range(func(_, v any) bool {
		sess, ok := v.(Session)
		if !ok {
			return true
		}
		if err := sess.Notify(ctx, method, params); err == nil {
			delivered++
		}
		return true
	})
	return delivered
}
