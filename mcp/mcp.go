// Package mcp is the slice of the Model Context Protocol this project uses,
// written out rather than depended on.
//
// # Why this exists
//
// termcp's MCP surface is one tool and one prompt. For that it pulled in a
// library whose type package alone costs ~2.5 MB — JSON Schema validation, type
// coercion, URI templates — none of which this project calls. Holding a
// dependency whose useful fraction is three structs made every integration pay
// for the rest.
//
// # What is here
//
// Exactly what this project needs, and no more:
//
//   - the request/response shapes of a tool call and a prompt get;
//   - the content blocks a result carries (text only — termcp has no images or
//     audio, and inventing support would be inventing behaviour);
//   - tool and prompt descriptors, with the option setters used at registration;
//   - the JSON-RPC envelopes both transports move.
//
// # What is deliberately absent
//
// Resources (URI templates), sampling, elicitation, tasks, schema validation and
// the annotation vocabulary. Adding one is a small change when something needs
// it — the point is that the absence is a decision rather than an oversight.
//
// The list is about SURFACES, not about the details of the ones that exist. What
// this package does implement it implements properly: a request can be cancelled,
// a silent session expires, the version header is checked, a call can report
// progress, a tool can carry a title and an output schema, and completion/complete
// is answered when a provider is registered (issue #327). Those were gaps of the
// second kind — received and ignored, which a client sees as a silent error.
//
// # Compatibility
//
// The names mirror the vocabulary of the MCP specification, so a reader who knows
// the spec can find their way here without a translation table. Where this
// project renamed something, the comment says which spec field it is.
package mcp

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ProtocolVersion is what this server answers an initialize with.
//
// It is the version this implementation speaks, not a range: a client that asks
// for something else is answered with this one, which the specification allows
// and which is honest about what the server can do.
const ProtocolVersion = "2025-06-18"

// JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// --- wire envelopes ---

// Request is one inbound JSON-RPC frame.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is one outbound JSON-RPC frame: result or error, never both.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Notification is a frame with no id: the sender wants no answer.
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RPCError is the error member of a failed Response.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// IDString renders a request id for a map key. A JSON id is either a number or a
// string, and the two spellings of the same value are different ids.
func IDString(id json.RawMessage) string {
	if len(id) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(id))
	if s == "null" {
		return ""
	}
	if unq, err := strconv.Unquote(s); err == nil {
		return unq
	}
	return s
}

// --- content ---

// TextContent is one block of a tool result or prompt message.
//
// It is CONCRETE rather than the spec's Content union on purpose. Every block this
// package produces is text, and an interface with one implementation would be a
// name for a signature — worse, it would make a result impossible to decode from
// JSON without a custom unmarshaller, because an interface cannot be filled in.
// The day an image block is needed, this becomes the union it describes.
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MarshalJSON keeps Type populated even when a caller built the block by hand,
// which is the common case here: every result this project produces is text.
func (t TextContent) MarshalJSON() ([]byte, error) {
	type plain struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	return json.Marshal(plain{Type: "text", Text: t.Text})
}

// NewTextContent builds a text block.
func NewTextContent(text string) TextContent { return TextContent{Type: "text", Text: text} }

// --- tool calls ---

// CallToolRequest is an inbound tools/call.
type CallToolRequest struct {
	Params CallToolParams `json:"params"`
}

// CallToolParams is the parameters of a tools/call: which tool, and its arguments.
type CallToolParams struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments,omitempty"`
	// Meta is the request's _meta object: per-request metadata, of which this
	// package reads exactly one member, progressToken (see ProgressFrom).
	Meta map[string]any `json:"_meta,omitempty"`
}

// ProgressToken is the client's handle for one request's progress reports.
//
// It is whatever JSON value the client chose — a number, a string — so it travels
// as a raw message and is echoed back unchanged. Anything else would report a token
// the client does not recognise.
type ProgressToken = json.RawMessage

// ProgressFrom reads _meta.progressToken off a request. ok is false when the client
// did not ask for progress, which is the common case and means "send nothing".
func ProgressFrom(meta map[string]any) (ProgressToken, bool) {
	if meta == nil {
		return nil, false
	}
	raw, ok := meta["progressToken"]
	if !ok || raw == nil {
		return nil, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	return ProgressToken(b), true
}

// NewProgressNotification builds one notifications/progress frame.
//
// `total` is optional in the spec, so a caller that does not know the end omits it
// rather than inventing one: a percentage derived from a guess is worse than a
// count with no denominator.
func NewProgressNotification(token ProgressToken, progress float64, total *float64, message string) Notification {
	params := map[string]any{
		"progressToken": token,
		"progress":      progress,
	}
	if total != nil {
		params["total"] = *total
	}
	if message != "" {
		params["message"] = message
	}
	return Notification{JSONRPC: "2.0", Method: "notifications/progress", Params: params}
}

// CallToolResult is the answer to a tools/call.
type CallToolResult struct {
	Content []TextContent `json:"content"`
	// StructuredContent is the machine-readable twin of Content, when a caller
	// asked for one. This project sets it only where it already had JSON.
	StructuredContent any `json:"structuredContent,omitempty"`
	// IsError marks a result the client should show as a failure. The MCP layer
	// distinguishes this from a JSON-RPC error: the call reached the tool, and the
	// tool reports failure in its own vocabulary.
	IsError bool `json:"isError,omitempty"`
}

// GetArguments returns the arguments map, or nil when the client sent none.
//
// THE TYPE ASSERTION IS THE WHOLE POINT: arguments arrive as whatever the client
// encoded, and a handler that assumed map[string]any would panic on a
// non-object. nil is the answer that makes a caller check.
func (r CallToolRequest) GetArguments() map[string]any {
	if args, ok := r.Params.Arguments.(map[string]any); ok {
		return args
	}
	return nil
}

// NewToolResultText is the common answer: one text block.
func NewToolResultText(text string) *CallToolResult {
	return &CallToolResult{Content: []TextContent{NewTextContent(text)}}
}

// NewToolResultError is a result the client must read as a failure.
func NewToolResultError(text string) *CallToolResult {
	return &CallToolResult{Content: []TextContent{NewTextContent(text)}, IsError: true}
}

// --- tool descriptors ---

// Tool is one entry of tools/list.
type Tool struct {
	Name string `json:"name"`
	// Title is a human-readable display name. A client that shows `term` in a menu
	// has only the machine name otherwise, and the two are not the same thing
	// (issue #327).
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema ToolInputSchema `json:"inputSchema"`
	// OutputSchema describes structuredContent when the tool produces one. It is
	// passed through as given: this package does not validate against it, because a
	// client that wants to validate has the schema and a server that enforced it
	// here would be validating its own output against its own claim (issue #327).
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	// Annotations is the spec's optional display hints. Passed through as given.
	Annotations *ToolAnnotation `json:"annotations,omitempty"`
}

// ToolInputSchema is a JSON Schema object describing a tool's arguments.
type ToolInputSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties,omitempty"`
	Required   []string       `json:"required,omitempty"`
	Extra      map[string]any `json:"-"`
}

// MarshalJSON keeps any schema keywords this struct does not model.
func (s ToolInputSchema) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(s.Extra)+3)
	for k, v := range s.Extra {
		out[k] = v
	}
	out["type"] = s.Type
	if len(s.Properties) > 0 {
		out["properties"] = s.Properties
	}
	if len(s.Required) > 0 {
		out["required"] = s.Required
	}
	return json.Marshal(out)
}

// ToolAnnotation is the spec's optional annotation object for a tool.
type ToolAnnotation struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ToolOption customises a Tool at construction.
type ToolOption func(*Tool)

// PropertyOption customises one property of a tool's input schema.
type PropertyOption func(map[string]any)

// NewTool builds a tool descriptor with a `type: object` input schema.
func NewTool(name string, opts ...ToolOption) Tool {
	t := Tool{
		Name:        name,
		InputSchema: ToolInputSchema{Type: "object", Properties: map[string]any{}},
	}
	for _, opt := range opts {
		opt(&t)
	}
	return t
}

// WithDescription sets the tool's description.
func WithDescription(s string) ToolOption {
	return func(t *Tool) { t.Description = s }
}

// WithTitle sets the human-readable display name.
func WithTitle(s string) ToolOption {
	return func(t *Tool) { t.Title = s }
}

// WithOutputSchema attaches a JSON Schema for structuredContent.
//
// IT IS PASSED THROUGH, NOT VALIDATED. A server validating its own output against
// its own claim proves nothing, and a client that wants the guarantee has the schema
// and can check. An invalid schema is therefore the caller's mistake to see, and the
// only thing checked here is that it is JSON at all — a string that is not JSON would
// produce a tools/list no client can parse.
func WithOutputSchema(schema string) ToolOption {
	return func(t *Tool) {
		if json.Valid([]byte(schema)) {
			t.OutputSchema = json.RawMessage(schema)
		}
	}
}

// WithToolAnnotation attaches the annotation object.
func WithToolAnnotation(a ToolAnnotation) ToolOption {
	return func(t *Tool) { t.Annotations = &a }
}

// WithString declares a string argument.
func WithString(name string, opts ...PropertyOption) ToolOption {
	return withProperty(name, "string", opts...)
}

// WithNumber declares a numeric argument.
func WithNumber(name string, opts ...PropertyOption) ToolOption {
	return withProperty(name, "number", opts...)
}

// WithBoolean declares a boolean argument.
func WithBoolean(name string, opts ...PropertyOption) ToolOption {
	return withProperty(name, "boolean", opts...)
}

// WithArray declares an array argument.
func WithArray(name string, opts ...PropertyOption) ToolOption {
	return withProperty(name, "array", opts...)
}

// WithObject declares an object argument.
func WithObject(name string, opts ...PropertyOption) ToolOption {
	return withProperty(name, "object", opts...)
}

func withProperty(name, typ string, opts ...PropertyOption) ToolOption {
	return func(t *Tool) {
		if t.InputSchema.Properties == nil {
			t.InputSchema.Properties = map[string]any{}
		}
		prop := map[string]any{"type": typ}
		for _, opt := range opts {
			opt(prop)
		}
		t.InputSchema.Properties[name] = prop
	}
}

// WithRequired is the option that actually records a required argument. It is
// separate from Required because the requirement belongs to the TOOL, not to one
// property's schema.
func WithRequired(name string) ToolOption {
	return func(t *Tool) { t.InputSchema.Required = append(t.InputSchema.Required, name) }
}

// Description sets a property's description.
func Description(s string) PropertyOption {
	return func(p map[string]any) { p["description"] = s }
}

// Enum restricts a property to the listed values.
func Enum(values ...string) PropertyOption {
	return func(p map[string]any) {
		any := make([]any, len(values))
		for i, v := range values {
			any[i] = v
		}
		p["enum"] = any
	}
}

// DefaultString sets a property's default.
func DefaultString(s string) PropertyOption {
	return func(p map[string]any) { p["default"] = s }
}

// DefaultNumber sets a property's numeric default.
func DefaultNumber(n float64) PropertyOption {
	return func(p map[string]any) { p["default"] = n }
}

// DefaultBool sets a property's boolean default.
func DefaultBool(b bool) PropertyOption {
	return func(p map[string]any) { p["default"] = b }
}

// Min sets a property's minimum.
func Min(n float64) PropertyOption {
	return func(p map[string]any) { p["minimum"] = n }
}

// Items describes an array property's element type.
func Items(items map[string]any) PropertyOption {
	return func(p map[string]any) { p["items"] = items }
}

// AdditionalProperties sets the schema keyword of the same name: either a bool
// or a schema that every extra property must match.
func AdditionalProperties(v any) PropertyOption {
	return func(p map[string]any) { p["additionalProperties"] = v }
}

// ArgumentDescription names one argument in a prompt's descriptor.
type ArgumentDescription struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// WithArgument adds an argument to a prompt descriptor.
//
// The name is passed separately from the description because it is the key a
// prompt handler looks up. Both being strings, a positional (name, description)
// pair is one transposition away from a bug that compiles.
func WithArgument(name string, arg ArgumentDescription) PromptOption {
	return func(p *Prompt) {
		arg.Name = name
		p.Arguments = append(p.Arguments, arg)
	}
}

// WithPromptDescription sets a prompt's description.
func WithPromptDescription(s string) PromptOption {
	return func(p *Prompt) { p.Description = s }
}

// --- prompts ---

// Prompt is one entry of prompts/list.
type Prompt struct {
	Name        string                `json:"name"`
	Description string                `json:"description,omitempty"`
	Arguments   []ArgumentDescription `json:"arguments,omitempty"`
}

// PromptOption customises a Prompt at construction.
type PromptOption func(*Prompt)

// NewPrompt builds a prompt descriptor.
func NewPrompt(name string, opts ...PromptOption) Prompt {
	p := Prompt{Name: name}
	for _, opt := range opts {
		opt(&p)
	}
	return p
}

// RoleUser and RoleAssistant are the two roles a prompt message may carry.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// PromptMessage is one message of a prompt result.
type PromptMessage struct {
	Role    string      `json:"role"`
	Content TextContent `json:"content"`
}

// GetPromptRequest is an inbound prompts/get.
type GetPromptRequest struct {
	Params GetPromptParams `json:"params"`
}

// GetPromptParams is the parameters of a prompts/get.
type GetPromptParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

// GetPromptResult is the answer to a prompts/get.
type GetPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// --- initialize ---

// InitializeRequest is an inbound initialize.
type InitializeRequest struct {
	Params InitializeParams `json:"params"`
}

// InitializeParams is the client's half of the handshake.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ClientInfo      Implementation `json:"clientInfo"`
}

// InitializeResult is the server's half of the handshake.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// Implementation names a client or a server.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ServerCapabilities says which optional surfaces this server offers.
type ServerCapabilities struct {
	Tools   *ListChangedCapability `json:"tools,omitempty"`
	Prompts *ListChangedCapability `json:"prompts,omitempty"`
	// Completions says this server answers completion/complete. Claimed only when a
	// completion provider is registered, because a client that reads the claim will
	// ask (issue #327).
	Completions *ListChangedCapability `json:"completions,omitempty"`
}

// ListChangedCapability is the only capability field this project sets.
type ListChangedCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// --- list results ---

// ListToolsRequest is an inbound tools/list.
type ListToolsRequest struct {
	Params struct {
		Cursor string `json:"cursor,omitempty"`
	} `json:"params,omitempty"`
}

// ListToolsResult is the answer to a tools/list.
type ListToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}
