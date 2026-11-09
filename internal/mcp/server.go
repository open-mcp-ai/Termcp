package mcp

import (
	"context"
	"io/fs"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sftpengine"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/task"
	"github.com/open-mcp-ai/termcp/mcp"
)

// Server wraps the MCP SSE server, streamable HTTP handler, and tool handlers.
type Server struct {
	mcpServer    *mcp.Server
	sseServer    *mcp.SSEServer
	streamServer *mcp.StreamableHTTPServer
	sessMgr      *session.Manager
	msgMgr       *message.Manager
	sshConfigs   *sshconfig.Store
	forwardMgr   *forward.ForwardManager
	extensions   *extensions.Reconciler
	baseURL      string // http://host:port, set from Start()
	docsFS       fs.FS  // embedded agent docs, set via SetDocsFS
	NoInternal   bool   // when true, hide and refuse the built-in loopback profile
	// ManageSSHConfigs enables the profile write actions (create/edit/copy/delete)
	// of the `profile` directive; main sets it from --mcp-manage-ssh-configs.
	ManageSSHConfigs bool

	// uiNotify delivers a user-facing notification to the Web UI (the notify_user
	// tool). Set by main to webui.Handler.BroadcastUINotify so this package stays
	// decoupled from webui. Returns the number of open tabs that received it.
	uiNotify func(level, title, message, sessionID string, durationSec int) int

	// connections tracks the MCP clients currently attached (see connections.go).
	// It is fed by the mcp package's session hooks, which is the only place a
	// client attaching or detaching is observable.
	connections *connRegistry

	// tasks tracks the asynchronous file transfers this process started (#158).
	// Transfers are always async: a caller behind a tool-call watchdog that fires
	// at 30s cannot synchronously move a 30 MB binary, and an interrupted transfer
	// used to leave a truncated file with no error anywhere.
	tasks *task.Manager
	// connListener is called when that set changes, so the Web UI can refresh
	// without polling. Set by main to the same broadcast the session list uses.
	connListener func()

	// aliases is this instance's alias table: the runtime entries a caller defines
	// with `alias <name> …` plus the saved ones under the data directory.
	//
	// PER SERVER, NOT PACKAGE-WIDE. An alias changes what a NAME means, so it is
	// configuration, and configuration belongs to the instance that holds it: two
	// termcp instances in one process (an embedder runs them this way) must not share
	// a vocabulary, and neither should one instance's tests.
	aliases *aliasTable

	// recvCursors is where each channel's `mcp recv` has read up to.
	//
	// It has to live here, per handle, because a recv is a QUEUE CONSUMER: the
	// caller reads one frame at a time out of a stream that keeps growing, and the
	// position it has reached is the only thing that stops the next call from
	// re-reading the first frame or skipping the ones in between. shell_output's
	// own cursor cannot serve: with no reader_id it hands out a NEW reader at the
	// current end of the stream, so a byte window that ended mid-frame made recv
	// report 204 and then skip the frames behind it on the next call — silently.
	// recvCursors is where each channel's `mcp recv` has read up to.
	//
	// A caller reads one frame at a time out of a growing stream, and the position
	// it reached is the only thing that stops the next call from re-reading the
	// first frame or skipping the ones in between. Independent entries, so sync.Map.
	recvCursors sync.Map // handle string -> int64

	// recvReaders gives a handle one reader at a time, held for the WHOLE read
	// (#183).
	//
	// The cursor above is a read POSITION, and a position does not say who is
	// reading it: two concurrent recvs both start at the same offset, both find the
	// same frame, and both return it. Duplicate delivery is what JSON-RPC id pairing
	// cannot survive — two `id:3` and no `id:4` leaves the client waiting for an
	// answer another reader already consumed.
	//
	// Per HANDLE, because a recv can wait out the caller's whole window.
	// A TOKEN, NOT A MUTEX (issue #224): Mutex.Lock takes no context, so a second
	// recv could not leave when its own window expired. The one-slot channel is the
	// same exclusivity with a door.
	recvReaders sync.Map // handle string -> chan struct{}

	// recvPending holds frames a `call` read off the stream while waiting for its
	// OWN id (issue #301).
	//
	// `call` used to hand the foreign frame back with a note, which made the next
	// call answer the previous question — a 200 whose body is not what was asked.
	// Skipping the frame instead would lose it, so the skipped frames wait here and
	// `read <channel> --frame` hands them out, oldest first, before reading the
	// stream again. A waiter also looks here for its own id: a reply can be skipped
	// into the queue by a waiter for a different id.
	//
	// Per handle, touched only while holding that handle's recvReaders token — the
	// same single-reader rule that keeps the cursor honest, because the queue and
	// the cursor are two halves of one position.
	recvPending sync.Map // handle string -> *frameQueue

	// lineLocks serialises the two writes that make ONE line on a channel (#212).
	//
	// `run` is two calls into the shell layer — the text, then the enter that ends it
	// — and nothing used to keep another `run` from landing between them. Two
	// concurrent runs produced `{"id":1}{"id":2}\n\n`: not two lines and not one
	// valid line either, so a line-protocol peer read it as corrupt and exited.
	//
	// PER HANDLE and held ONLY around the writes: the wait for output that follows
	// must not hold it, or a run would park every later command on the channel.
	lineLocks sync.Map // handle string -> *sync.Mutex

	// closedReads is where each CLOSED channel's plain `read` has read up to (#188).
	//
	// A live channel's position lives in its ring-buffer reader, which is the only
	// place it can live while bytes are still arriving. A closed one has no such
	// reader — its bytes come from the persisted log — so the position is kept here
	// or the default read has no way to mean "what is new since last time", and it
	// answered with the tail of the history on every call (#159 C1).
	//
	// ponytail: one int64 per (session, channel) ever read after closing, never
	// evicted. Bounded by the number of channels this process has read; an LRU is
	// the right shape if a daemon ever outlives thousands of them.
	closedReads sync.Map // key string -> int64

	// ptyQuiet is a bounded learner per channel (see pty_quiet.go). It is dropped
	// on channel close and on session DEAD.
	ptyQuiet *quietTrackers

	// sftpEngines keeps one SFTP subsystem per session, so a file operation does not
	// pay three round trips to open its own engine every call (see
	// internal/sftpengine, issue #244).
	sftpEngines *sftpengine.Pool
}

// SFTPPool exposes the shared SFTP engine pool, so a second surface over the same
// sessions (the Web UI's file panel) reuses the engine a tool call already opened
// instead of paying the handshake again (issue #244).
//
// It is an accessor rather than a construction option because there is exactly one
// pool per process and it must be the one whose DEAD hook evicts entries: two pools
// would mean two subsystems per session and one of them never evicted.
func (s *Server) SFTPPool() *sftpengine.Pool { return s.sftpEngines }

// Option customizes a Server at construction time. Options keep New's signature
// stable for callers (and tests) that do not care about the newer knobs.
type Option func(*Server)

// SetUINotifier wires the Web UI notification delivery end (the notify_user tool).
//
// It is a setter rather than a construction option because webui imports this
// package, so this package cannot name its type: main hands over the function.
func (s *Server) SetUINotifier(fn func(level, title, message, sessionID string, durationSec int) int) {
	s.uiNotify = fn
}

// SetDataDir tells the server where this installation keeps its state, which is
// what makes `create alias` (and `edit alias`) persist: the saved table lives in
// `<dir>/aliases/<name>.toml`.
//
// A SERVER GIVEN NO DIRECTORY KEEPS ALIASES IN MEMORY, which is exactly what the
// plain `alias` verb promises and what an embedder without a data directory wants.
func (s *Server) SetDataDir(dir string) {
	if s.aliases == nil {
		s.aliases = &aliasTable{}
	}
	s.aliases.saved = newAliasStore(dir)
}

// SetExtensionReconciler wires the extension layer. Without it the extension
// tool reports not_configured rather than offering a surface with no backing
// state, so a deployment that does not run the reconciler fails loudly instead
// of silently accepting templates it never connects.
func (s *Server) SetExtensionReconciler(r *extensions.Reconciler) {
	s.extensions = r
	// A TEMPLATE'S `alias` TABLE IS A COMMAND SOURCE, resolved on demand rather than
	// copied into the table: editing a template (or upgrading it) then takes effect
	// on the next command, with nothing to invalidate and no second copy to drift.
	if r == nil || s.aliases == nil {
		return
	}
	store := r.Store()
	s.aliases.ext = func(name string) (map[string]string, bool) {
		ext, err := store.Load(name)
		if err != nil || ext == nil || !ext.IsEnabled() {
			return nil, false
		}
		return ext.Alias, true
	}
}

// MCP exposes the protocol endpoint itself, so an embedder can register its own
// tools and prompts on the same server termcp's are on.
//
// It returns *mcp.Server, which lives outside internal/ and is already the
// registration surface (AddTool, AddPrompt, OnMethod, Use). Exposing it is how
// an integration adds a tool without termcp growing a plugin system: there is
// nothing to wrap, because the object that takes registrations is public.
//
// The returned server is the live one. Registering after Start is safe for
// tools registered before a client's first tools/list, and is what an embedder
// doing setup in New-then-Start order does naturally.
func (s *Server) MCP() *mcp.Server { return s.mcpServer }

// SSEHandler exposes the MCP SSE endpoint for mounting on a shared mux.
func (s *Server) SSEHandler() http.Handler {
	return s.sseServer.SSEHandler()
}

// MessageHandler exposes the MCP JSON-RPC message endpoint for mounting on a shared mux.
func (s *Server) MessageHandler() http.Handler {
	return s.sseServer.MessageHandler()
}

// StreamableHTTPHandler exposes the MCP streamable-HTTP endpoint (POST/GET/DELETE on one path).
// Mount at "/stream" (or another path with a matching wrapper); clients use e.g. http://host:port/stream.
func (s *Server) StreamableHTTPHandler() http.Handler {
	return s.streamServer
}

// Start begins serving MCP over SSE on the given address.
func (s *Server) Start(addr string) error {
	host, port, _ := net.SplitHostPort(addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "8080"
	}
	s.baseURL = "http://" + net.JoinHostPort(host, port)
	// Docs resources/prompts need baseURL, so they are registered here (once)
	// rather than in New(). registerDocs is idempotent for a single Start.
	s.registerDocs()
	// The transports are mounted on the shared mux (see core), so there is no
	// listener of their own to start. The base URL is resolved above because the
	// docs prompt needs it.
	return nil
}

// Stop gracefully shuts down the SSE server.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.streamServer != nil {
		_ = s.streamServer.Shutdown(ctx)
	}
	return s.sseServer.Shutdown(ctx)
}
