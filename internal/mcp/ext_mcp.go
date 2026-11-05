package mcp

// MCP over HTTP, PER EXTENSION (issue #234).
//
// The 18765 port already serves termcp's OWN tool surface at /stream. That surface
// has one tool (`term`) whose whole vocabulary is this project's directive language,
// so an MCP client that does not know the language cannot reach anything an
// extension provides. The extension's own MCP server — its tools, its instructions,
// its serverInfo — is reachable only from inside termcp, through `call <peer>`.
//
// /ext/{name}/stream closes that gap without teaching anybody the language: the
// client points its MCP config at the URL and talks to the EXTENSION, transparently.
// termcp stays the lifecycle owner (an on-demand extension is started by the first
// request and reaped by the same idle rule as everything else) and the policy owner
// (the capability whitelist and review gate are the extension's own, unchanged).
//
// WHAT IS DELIBERATELY NOT HERE:
//
//   - No listening GET stream. Streamable HTTP allows the server to push frames
//     outside a request/response pair; an extension on a line-delimited channel
//     answers inside the response to a call and does not initiate. A GET therefore
//     says so (405 + a sentence) instead of holding a socket open that could never
//     carry anything.
//   - No shared resident channel. A successful POST allocates an isolated
//     extension instance and returns a random `Mcp-Session-Id`; every later frame
//     must present that id under the same authenticated principal and credential.
//     DELETE releases only that caller's instance.
//   - No MCP semantics. Every frame is forwarded verbatim and the peer's answer is
//     returned verbatim, which is what makes `initialize` honest: the serverInfo the
//     client reads is the extension's, not ours.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/authorization"
	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/ns"
)

const (
	// extMCPStartWait is how long a request waits for an on-demand extension to
	// come up before answering. It is bounded because an HTTP client is holding a
	// connection: a target that takes minutes to answer must not turn into a
	// request that never answers, or the client's own timeout fires with no
	// explanation and it blames the network.
	extMCPStartWait = 20 * time.Second
	// extMCPCallWindow is the reply window for one forwarded frame.
	extMCPCallWindow = 60.0 // seconds, as termArgs.TimeoutSeconds
	// extMCPMaxBody bounds the request body. An MCP frame is a JSON-RPC message,
	// not a payload: 8 MiB is far past any real tools/call argument and small
	// enough that a stuck upload cannot exhaust memory.
	extMCPMaxBody = 8 << 20
	// extMCPLeaseIdle bounds a protocol session that outlives the client process
	// without an explicit DELETE.
	extMCPLeaseIdle = 30 * time.Minute
)

const extMCPSessionHeader = "Mcp-Session-Id"

type extMCPLease struct {
	id      string
	owner   string
	name    string
	status  extensions.Status
	request sync.Mutex
	timerMu sync.Mutex
	timer   *time.Timer
	lastUse time.Time
}

// ExtMCPHandler serves one extension's MCP server over streamable HTTP.
//
// The route is registered as `POST /ext/{name}/stream`; the name is read from the
// path rather than a query parameter so a client can configure it as an ordinary
// MCP URL.
func (s *Server) ExtMCPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(r.PathValue("name"))
		if name == "" {
			http.Error(w, "no extension named in the path: the route is POST /ext/{name}/stream", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodPost:
			s.extMCPPost(w, r, name)
		case http.MethodGet:
			// An extension on a stdio channel never initiates a message, so a
			// listening stream would be a socket that can only ever stay empty.
			// Saying so is more useful than holding it open.
			w.Header().Set("Allow", "POST, DELETE")
			http.Error(w, "this extension's MCP server answers inside the response to a call and "+
				"never sends a message on its own, so there is no server-initiated stream to listen on. "+
				"POST your JSON-RPC frames to this same URL.", http.StatusMethodNotAllowed)
		case http.MethodDelete:
			s.extMCPDelete(w, r, name)
		default:
			w.Header().Set("Allow", "POST, DELETE")
			http.Error(w, "an MCP endpoint takes POST (JSON-RPC) and DELETE (end of session)", http.StatusMethodNotAllowed)
		}
	})
}

// extMCPPost forwards one JSON-RPC frame to the extension and writes the answer.
func (s *Server) extMCPPost(w http.ResponseWriter, r *http.Request, name string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, extMCPMaxBody+1))
	if err != nil {
		http.Error(w, "cannot read the request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > extMCPMaxBody {
		http.Error(w, fmt.Sprintf("the request body is larger than %d bytes; an MCP frame is a JSON-RPC message, not a payload", extMCPMaxBody), http.StatusRequestEntityTooLarge)
		return
	}
	frame := strings.TrimSpace(string(body))
	if frame == "" {
		http.Error(w, "empty request body: POST one JSON-RPC message", http.StatusBadRequest)
		return
	}
	if !json.Valid([]byte(frame)) {
		http.Error(w, "the body is not JSON; this endpoint forwards one JSON-RPC message per request", http.StatusBadRequest)
		return
	}

	// THE REQUEST SHAPE IS CHECKED BEFORE THE SERVER'S OWN CAPABILITIES: a body that
	// is not a JSON-RPC message is malformed on any instance, and answering it with
	// "extensions are not available here" would send the caller looking in the wrong
	// place. The method was already dispatched, so GET/DELETE never reach this.
	if s.extensions == nil {
		http.Error(w, "extensions are not available on this instance", http.StatusServiceUnavailable)
		return
	}

	binding, err := s.authorizeExtensionMCP(r.Context(), name, frameID(frame) != "")
	if err != nil {
		auth.RecordAudit(r.Context(), auth.AuditEvent{
			Action: "mcp.ext.authorize", TargetType: "extension", TargetID: name,
			Result: "deny", Reason: err.Error(),
		})
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	lease, err := s.extMCPLeaseForRequest(w, r, name, binding)
	if err != nil {
		return
	}
	shellID := lease.status.ShellID
	if shellID == "" {
		// A template with no channel (a read-only file window, a forwards-only
		// tunnel) has no MCP server to talk to. Naming which shape it is beats
		// "not found", which would send the caller looking for a typo.
		http.Error(w, "extension "+name+" has no channel, so it has no MCP server to talk to: "+
			"its template declares no command. Add --command (or --url) to give it one.", http.StatusConflict)
		return
	}

	ctx := r.Context()
	if binding != nil {
		sess := (toolGuard{server: s}).lookupShellID(shellID)
		if sess == nil {
			http.Error(w, "extension "+name+": acquired channel is unavailable", http.StatusConflict)
			return
		}
		info := sess.Info()
		if !ns.IsExtensions(info.Namespace) {
			http.Error(w, "extension "+name+": acquired channel is not owned by the extension layer", http.StatusConflict)
			return
		}
		ctx = withExtensionMCPExecution(ctx, info.Namespace, shellID)
	}
	ctx = withMCPStreamSession(ctx, lease.id)
	lease.request.Lock()
	defer lease.request.Unlock()
	s.touchExtMCPLease(lease)
	stmt := directive.Statement{Op: "mcp", Kind: "mcp", Target: shellID}
	win := termArgs{TimeoutSet: true, TimeoutSeconds: extMCPCallWindow}
	line := s.lineLock(shellID)
	line.Lock()

	if id := frameID(frame); id != "" {
		if _, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", shellID, "text", frame)); fail != nil {
			line.Unlock()
			s.extMCPFail(w, ctx, stmt, fail)
			return
		}
		if _, fail := s.callTool(ctx, "shell_key", argsOf("shell_id", shellID, "key", "enter")); fail != nil {
			line.Unlock()
			s.extMCPFail(w, ctx, stmt, fail)
			return
		}
		line.Unlock()
		slot := s.mcpAwaitReply(ctx, stmt, shellID, id, win, map[string]string{})
		s.extMCPWriteSlot(w, r, slot)
		return
	}

	// A notification has no answer, and waiting for one would hold the client's
	// request open until the window expired to report nothing.
	if _, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", shellID, "text", frame)); fail != nil {
		line.Unlock()
		s.extMCPFail(w, ctx, stmt, fail)
		return
	}
	if _, fail := s.callTool(ctx, "shell_key", argsOf("shell_id", shellID, "key", "enter")); fail != nil {
		line.Unlock()
		s.extMCPFail(w, ctx, stmt, fail)
		return
	}
	line.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) extMCPDelete(w http.ResponseWriter, r *http.Request, name string) {
	if len(r.Header.Values(extMCPSessionHeader)) == 0 {
		http.Error(w, "DELETE requires one Mcp-Session-Id header", http.StatusBadRequest)
		return
	}
	session, ok := extMCPSessionID(r)
	if !ok {
		http.Error(w, "DELETE requires one Mcp-Session-Id header", http.StatusBadRequest)
		return
	}
	value, found := s.extMCPLeases.Load(session)
	if !found {
		http.Error(w, "MCP session is unknown or already ended", http.StatusNotFound)
		return
	}
	lease := value.(*extMCPLease)
	if lease.owner != extMCPOwner(r.Context()) {
		http.Error(w, "MCP session belongs to a different credential", http.StatusForbidden)
		return
	}
	if lease.name != name {
		http.Error(w, "MCP session belongs to a different extension", http.StatusNotFound)
		return
	}
	s.removeExtMCPLease(lease)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) extMCPLeaseForRequest(w http.ResponseWriter, r *http.Request, name string, binding *extensions.AuthorizationBinding) (*extMCPLease, error) {
	session, ok := extMCPSessionID(r)
	if !ok {
		http.Error(w, "ambiguous Mcp-Session-Id", http.StatusBadRequest)
		return nil, fmt.Errorf("ambiguous extension MCP session")
	}
	owner := extMCPOwner(r.Context())
	if session != "" {
		value, found := s.extMCPLeases.Load(session)
		if !found {
			http.Error(w, "MCP session is unknown or expired; initialize again", http.StatusNotFound)
			return nil, fmt.Errorf("unknown extension MCP session")
		}
		lease := value.(*extMCPLease)
		if lease.owner != owner {
			http.Error(w, "MCP session belongs to a different credential", http.StatusForbidden)
			return nil, fmt.Errorf("extension MCP session owner mismatch")
		}
		if lease.name != name {
			http.Error(w, "MCP session belongs to a different extension", http.StatusNotFound)
			return nil, fmt.Errorf("extension MCP session name mismatch")
		}
		w.Header().Set(extMCPSessionHeader, lease.id)
		return lease, nil
	}

	var st extensions.Status
	var err error
	if binding != nil {
		st, err = s.extensions.AcquireBoundFresh(*binding, extMCPStartWait)
	} else {
		st, err = s.extensions.AcquireFresh(name, "", extMCPStartWait)
	}
	if err != nil {
		http.Error(w, "extension "+name+": "+err.Error(), http.StatusNotFound)
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		id, err := newExtMCPSessionID()
		if err != nil {
			http.Error(w, "could not create an MCP session", http.StatusInternalServerError)
			return nil, err
		}
		lease := &extMCPLease{id: id, owner: owner, name: name, status: st, lastUse: time.Now()}
		if _, loaded := s.extMCPLeases.LoadOrStore(id, lease); loaded {
			continue
		}
		s.scheduleExtMCPLeaseExpiry(lease)
		w.Header().Set(extMCPSessionHeader, id)
		return lease, nil
	}
	if st.ShellID != "" {
		_, _ = s.extensions.Release(name, st.ShellID)
	}
	http.Error(w, "could not allocate a unique MCP session", http.StatusInternalServerError)
	return nil, fmt.Errorf("extension MCP session id collision")
}

func extMCPSessionID(r *http.Request) (string, bool) {
	values := r.Header.Values(extMCPSessionHeader)
	if len(values) > 1 {
		return "", false
	}
	if len(values) == 0 {
		return "", true
	}
	value := strings.TrimSpace(values[0])
	if len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
		return "", false
	}
	return value, true
}

func extMCPOwner(ctx context.Context) string {
	actor, _ := auth.ActorFromContext(ctx)
	return actor.ID + "\x00" + auth.CredentialIDFromContext(ctx)
}

func newExtMCPSessionID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *Server) scheduleExtMCPLeaseExpiry(lease *extMCPLease) {
	lease.timerMu.Lock()
	defer lease.timerMu.Unlock()
	lease.timer = time.AfterFunc(extMCPLeaseIdle, func() {
		s.expireExtMCPLease(lease)
	})
}

func (s *Server) touchExtMCPLease(lease *extMCPLease) {
	lease.timerMu.Lock()
	defer lease.timerMu.Unlock()
	lease.lastUse = time.Now()
	if lease.timer != nil {
		lease.timer.Reset(extMCPLeaseIdle)
	}
}

func (s *Server) expireExtMCPLease(lease *extMCPLease) {
	lease.request.Lock()
	lease.timerMu.Lock()
	remaining := extMCPLeaseIdle - time.Since(lease.lastUse)
	if remaining > 0 && lease.timer != nil {
		lease.timer.Reset(remaining)
		lease.timerMu.Unlock()
		lease.request.Unlock()
		return
	}
	if lease.timer != nil {
		lease.timer.Stop()
		lease.timer = nil
	}
	lease.timerMu.Unlock()
	detached := s.detachExtMCPLease(lease)
	lease.request.Unlock()
	if detached {
		s.releaseExtMCPLease(lease)
	}
}

func (s *Server) removeExtMCPLease(lease *extMCPLease) {
	lease.request.Lock()
	lease.timerMu.Lock()
	if lease.timer != nil {
		lease.timer.Stop()
		lease.timer = nil
	}
	lease.timerMu.Unlock()
	detached := s.detachExtMCPLease(lease)
	lease.request.Unlock()
	if detached {
		s.releaseExtMCPLease(lease)
	}
}

func (s *Server) detachExtMCPLease(lease *extMCPLease) bool {
	if _, loaded := s.extMCPLeases.LoadAndDelete(lease.id); !loaded {
		return false
	}
	if lease.status.ShellID != "" {
		s.dropMCPStreamSession(lease.id, lease.status.ShellID)
	}
	return true
}

func (s *Server) releaseExtMCPLease(lease *extMCPLease) {
	if s.extensions != nil && lease.status.ShellID != "" {
		_, _ = s.extensions.Release(lease.name, lease.status.ShellID)
	}
}

// authorizeExtensionMCP resolves the extension before it is started. A verified
// Agent needs ext.get for this direct MCP door, and the returned binding freezes
// the template and target profile so a concurrent edit cannot redirect launch.
// Approval-required ext.get is refused here because a raw MCP frame cannot be
// safely replayed into a later JSON-RPC response; the Agent can use the normal
// extension/DSL surface, whose whole operation is replayable.
func (s *Server) authorizeExtensionMCP(ctx context.Context, name string, needsReply bool) (*extensions.AuthorizationBinding, error) {
	p, restricted, err := auth.PolicyFromContext(ctx)
	if !restricted {
		if actor, ok := auth.ActorFromContext(ctx); ok && actor.Role == auth.RoleAgent {
			return nil, fmt.Errorf("Agent policy is unavailable")
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plan := authorization.Plan{Requirements: []authorization.Requirement{
		{Operation: capability.ExtGet},
		{Operation: "shell.input"},
		{Operation: "shell.key", Evidence: authorization.Evidence{Keys: []string{"enter"}, Repeat: 1}},
	}}
	if needsReply {
		plan.Requirements = append(plan.Requirements, authorization.Requirement{Operation: "shell.output"})
	}
	decision, err := evaluateAgentPlan(p, plan)
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		return nil, fmt.Errorf("Agent permission denied: %s", decision.Reason)
	}
	if decision.NeedsApproval {
		return nil, fmt.Errorf("the direct extension MCP frame requires approval; it cannot be resumed after approval. Allow ext.get, shell.input, shell.key (enter), and shell.output when a reply is expected, or use the termcp MCP peer/DSL surface whose whole call can be replayed")
	}
	binding, err := s.agentExtensionBinding(p, name, "")
	if err != nil {
		return nil, fmt.Errorf("extension is outside this Agent's scope: %w", err)
	}
	return &binding, nil
}

// extensionMCPToolAuthorized limits the post-ext.get inner bypass to the exact
// shell that extensionMCPPost acquired. It is package-private context, not a
// request field, so a caller cannot manufacture it through MCP parameters.
func extensionMCPToolAuthorized(ctx context.Context, tool string, request mcpgo.CallToolRequest) bool {
	execution, ok := extensionMCPExecutionFromContext(ctx)
	if !ok {
		return false
	}
	g := lookupGate(tool)
	if !g.known || !g.ExtensionFrameWrite {
		return false
	}
	ids, _ := splitBatchIDs(getString(request.GetArguments(), "shell_id", ""))
	return len(ids) == 1 && ids[0] == execution.shellID
}

// extMCPFail turns a tool-call failure into an HTTP error whose body names what
// went wrong. The MCP client sees the status text, so it says the same thing the
// directive reply would.
func (s *Server) extMCPFail(w http.ResponseWriter, ctx context.Context, stmt directive.Statement, fail *toolFailure) {
	slot := s.slotFromFailure(ctx, stmt, fail)
	code := slot.Code
	if code == 0 || code == 200 {
		code = http.StatusBadGateway
	}
	http.Error(w, slot.Body, code)
}

// extMCPWriteSlot writes a directive slot as an MCP HTTP response.
//
// THE BODY IS THE PEER'S FRAME, UNCHANGED. Every sentence this project adds to a
// reply (a hint in the Hint header, a note appended to body text) exists for a
// human or an agent reading a terminal; an MCP client parses the body as JSON and
// would choke on an appended paragraph. So a body that is not a JSON-RPC message is
// reported as a server-side problem rather than sent as if it were one.
func (s *Server) extMCPWriteSlot(w http.ResponseWriter, r *http.Request, slot envelope.Slot) {
	switch {
	case slot.Code == http.StatusNoContent:
		w.WriteHeader(http.StatusAccepted)
		return
	case slot.Code >= 400:
		http.Error(w, slot.Body, slot.Code)
		return
	}
	body := strings.TrimSpace(slot.Body)
	var probe struct {
		JSONRPC string `json:"jsonrpc"`
	}
	if err := json.Unmarshal([]byte(body), &probe); err != nil || probe.JSONRPC == "" {
		http.Error(w, "the extension answered with something that is not a JSON-RPC message, "+
			"so it cannot be returned to an MCP client: "+truncateForError(body), http.StatusBadGateway)
		return
	}
	// The client asked for what it accepts; a streamable-HTTP server may answer a
	// POST with JSON or with an event stream. JSON is what a line-delimited peer
	// already gave us, so it is served as JSON unless the client refuses it.
	if acceptsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "event: message\ndata: "+body+"\n\n")
}

// acceptsJSON reports whether the client will take a plain JSON body. Absent an
// Accept header the answer is yes: a client that says nothing has not refused it.
func acceptsJSON(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	if accept == "" || strings.Contains(accept, "application/json") || strings.Contains(accept, "*/*") {
		return true
	}
	return false
}

// truncateForError bounds a non-JSON answer in an error body, so a peer that
// printed a whole log does not become the error message.
func truncateForError(body string) string {
	const limit = 300
	if len(body) <= limit {
		return body
	}
	return body[:limit] + "…"
}
