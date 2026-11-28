package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/open-mcp-ai/termcp/mcp"
)

// SUBSCRIBING TO WHAT THE EXTENSIONS SAY.
//
// The main endpoint could push exactly two frames before this: progress for the call in
// flight, and cancellation. Everything a downstream peer says on its own stopped at the
// pump, because /ext/<name>/stream is the only door that delivers it — so a client that
// wants to watch several extensions needed one MCP session per extension, and one that
// wants to know when ANY of them speaks had no way to say so.
//
// Here is the way to say so, and it is the narrow half of #275: an explicit subscription
// over the existing endpoint, no replay and no queue, so that a plugin can hold ONE
// session and be told what the extensions said.
//
//	termcp/subscribe    {ext: "<name>"}  or {ext: "*"}   → {subscribed: [...]}
//	termcp/unsubscribe  {ext: "<name>"}  or {ext: "*"}
//	termcp/subscriptions                                 → {subscribed: [...], delivered, dropped}
//
// Delivery is notifications/termcp/ext with {extension, method, params}: the peer's own
// method name stays visible, so a subscriber filters by what the peer called it rather
// than by parsing a payload it has to guess was relayed.
//
// WHY AN OBJECT AND NOT THE PEER'S FRAME VERBATIM. A subscriber can be attached to
// several extensions at once, so the frame must say which one spoke; and a client that
// receives `notifications/resources/updated` for a URI it never subscribed to has been
// told something false about its own subscriptions. The envelope is three fields and
// the payload is untouched inside it.
//
// WHAT IS DELIBERATELY ABSENT:
//   - no replay. A subscriber that was not attached missed it, which the subscribe reply
//     says in words; the alternative is a queue per subscriber that outlives the client
//     and a memory bill nobody asked for.
//   - no server-initiated requests. A peer's request frame is answered with an error at
//     the extension endpoint (nothing there accepts one), so it never reaches this path.
//   - no capability claim. Custom methods need none, and claiming `resources.subscribe`
//     here would promise the spec's semantics (a resource document behind the update)
//     which is not what this delivers.
//
// WHY THE SUBSCRIPTION IS KEYED BY SESSION AND HOLDS THE *SESSION*: the delivery is
// per-client by definition, and the session object is the only thing that can write to
// one. It is dropped by the unregister hook, so a client that disconnects leaves nothing
// behind.

const (
	// extEventMethod is the notification a subscriber receives.
	extEventMethod = "notifications/termcp/ext"
	// extSubscribeMethod, extUnsubscribeMethod and extListSubsMethod are the three
	// inbound methods that manage the table.
	extSubscribeMethod   = "termcp/subscribe"
	extUnsubscribeMethod = "termcp/unsubscribe"
	extListSubsMethod    = "termcp/subscriptions"
	// extSubWildcard subscribes to every extension, including ones created later —
	// which is the shape a plugin wants: it starts before the peers do.
	extSubWildcard = "*"
)

// extEventSubs is the subscription table.
//
// NO MUTEX (AGENTS.md): each entry's name list is an immutable snapshot behind an atomic
// pointer, published with CompareAndSwap, and the map itself is a sync.Map. A subscribe
// and a delivery therefore cannot block each other, and a delivery never holds anything
// while it writes to a client.
type extEventSubs struct {
	sessions sync.Map // session id → *extEventSub
	// delivered and dropped are counters, so they are atomics. A drop is a client that
	// was not reading (the transport's own bound) or has just gone; a subscriber reading
	// these numbers can tell "nothing happened" from "I was not keeping up".
	delivered atomic.Int64
	dropped   atomic.Int64
}

// extEventSub is one session's subscriptions.
type extEventSub struct {
	sess  mcp.Session
	names atomic.Pointer[[]string]
}

func newExtEventSubs() *extEventSubs { return &extEventSubs{} }

// names is the snapshot for one session (nil when it has no subscription).
func (t *extEventSubs) names(sessionID string) []string {
	if v, ok := t.sessions.Load(sessionID); ok {
		return *v.(*extEventSub).names.Load()
	}
	return nil
}

// subscribe adds one name to a session's set and returns the new snapshot.
func (t *extEventSubs) subscribe(sess mcp.Session, name string) []string {
	v, _ := t.sessions.LoadOrStore(sess.SessionID(), &extEventSub{sess: sess})
	sub := v.(*extEventSub)
	for {
		cur := sub.names.Load()
		var list []string
		if cur != nil {
			list = *cur
		}
		next := append(append([]string(nil), list...), name)
		sort.Strings(next)
		// DEDUPE AFTER SORT: subscribing twice is legal and must not deliver twice.
		next = compactNames(next)
		if sub.names.CompareAndSwap(cur, &next) {
			return next
		}
	}
}

// unsubscribe removes one name (or every name, for the wildcard) and reports what is
// left.
func (t *extEventSubs) unsubscribe(sessionID, name string) []string {
	v, ok := t.sessions.Load(sessionID)
	if !ok {
		return nil
	}
	sub := v.(*extEventSub)
	for {
		cur := sub.names.Load()
		if cur == nil {
			return nil
		}
		var next []string
		if name != extSubWildcard {
			for _, n := range *cur {
				if n != name {
					next = append(next, n)
				}
			}
		}
		if sub.names.CompareAndSwap(cur, &next) {
			return next
		}
	}
}

// forget drops a session's subscriptions, called from the unregister hook.
func (t *extEventSubs) forget(sessionID string) { t.sessions.Delete(sessionID) }

// targets returns the sessions that asked for this extension's events.
//
// THE WILDCARD IS MATCHED HERE, not expanded at subscribe time, so an extension created
// after the subscription is covered — which is the difference between "watch everything"
// and "watch what existed when I asked".
func (t *extEventSubs) targets(extName string) []*extEventSub {
	var out []*extEventSub
	t.sessions.Range(func(_, v any) bool {
		sub := v.(*extEventSub)
		if list := sub.names.Load(); list != nil {
			for _, n := range *list {
				if n == extName || n == extSubWildcard {
					out = append(out, sub)
					break
				}
			}
		}
		return true
	})
	return out
}

// publishExtEvent hands one notification from an extension to every subscriber.
//
// IT IS CALLED FROM THE PUMP, with the channel's context: the frame's own bytes are
// forwarded to the extension endpoint unchanged (see ext_pump.route), and this is the
// second destination — the same event, wrapped so the receiver knows whose it is.
func (s *Server) publishExtEvent(ctx context.Context, extName, method string, params json.RawMessage) {
	if s.extEvents == nil || extName == "" || method == "" {
		return
	}
	targets := s.extEvents.targets(extName)
	if len(targets) == 0 {
		return
	}
	body := map[string]any{"extension": extName, "method": method}
	if len(params) > 0 {
		body["params"] = params
	}
	for _, sub := range targets {
		if err := sub.sess.Notify(ctx, extEventMethod, body); err != nil {
			s.extEvents.dropped.Add(1)
			continue
		}
		s.extEvents.delivered.Add(1)
	}
}

// handleExtSubscribe answers termcp/subscribe.
func (s *Server) handleExtSubscribe(ctx context.Context, raw json.RawMessage) (any, *mcp.RPCError) {
	sess := mcp.SessionFromContext(ctx)
	if sess == nil {
		return nil, &mcp.RPCError{Code: mcp.CodeInternalError, Message: "no session on this request, so there is nothing to subscribe"}
	}
	name, rpcErr := s.extSubscriptionTarget(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	names := subNames(s.extEvents.subscribe(sess, name))
	// THE NOTE IS PART OF THE ANSWER, because the alternative to a replay is silence:
	// a client that assumes it will receive what happened while it was away waits for
	// events that will never come, which is the failure this whole area keeps producing.
	return map[string]any{
		"subscribed": names,
		"from_now":   "events are delivered from now on; nothing that happened before this call is replayed",
	}, nil
}

// handleExtUnsubscribe answers termcp/unsubscribe.
func (s *Server) handleExtUnsubscribe(ctx context.Context, raw json.RawMessage) (any, *mcp.RPCError) {
	sess := mcp.SessionFromContext(ctx)
	if sess == nil {
		return nil, &mcp.RPCError{Code: mcp.CodeInternalError, Message: "no session on this request, so there is nothing to unsubscribe"}
	}
	name, rpcErr := s.extSubscriptionTarget(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return map[string]any{"subscribed": subNames(s.extEvents.unsubscribe(sess.SessionID(), name))}, nil
}

// handleExtSubscriptions answers termcp/subscriptions: the set, plus the two counters
// that make "nothing happened" and "I lost events" different answers.
func (s *Server) handleExtSubscriptions(ctx context.Context, _ json.RawMessage) (any, *mcp.RPCError) {
	sess := mcp.SessionFromContext(ctx)
	if sess == nil {
		return nil, &mcp.RPCError{Code: mcp.CodeInternalError, Message: "no session on this request"}
	}
	return map[string]any{
		"subscribed": subNames(s.extEvents.names(sess.SessionID())),
		"delivered":  s.extEvents.delivered.Load(),
		"dropped":    s.extEvents.dropped.Load(),
	}, nil
}

// extSubscriptionTarget reads the one argument these methods take, and refuses a name
// that is not an extension.
//
// VALIDATED HERE, NOT AT DELIVERY. A typo would otherwise be a subscription that never
// fires and never says so — the exact silent failure that made the old `shell_notify`
// mechanism useless (see the CHANGELOG entry that removed it). An extension created
// later is fine as long as the name is right, which is what the wildcard is for.
func (s *Server) extSubscriptionTarget(raw json.RawMessage) (string, *mcp.RPCError) {
	var p struct {
		Ext string `json:"ext"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", &mcp.RPCError{Code: mcp.CodeInvalidParams,
				Message: "invalid params: " + err.Error() + `; send {"ext":"<name>"} or {"ext":"*"}`}
		}
	}
	name := strings.TrimSpace(p.Ext)
	if name == "" {
		return "", &mcp.RPCError{Code: mcp.CodeInvalidParams,
			Message: `ext is required: name one extension ({"ext":"my-peer"}) or every one of them ({"ext":"*"})`}
	}
	if name == extSubWildcard {
		return name, nil
	}
	if s.extensions == nil {
		return "", &mcp.RPCError{Code: mcp.CodeInvalidParams, Message: "extensions are not available on this instance"}
	}
	if _, ok := s.extensions.Status(name); !ok {
		known := s.knownExtensionNames()
		if len(known) == 0 {
			return "", &mcp.RPCError{Code: mcp.CodeInvalidParams,
				Message: "no extension is named " + quoteOrDash(name) + ", and this instance has none: `ls ext` is empty until one is created"}
		}
		return "", &mcp.RPCError{Code: mcp.CodeInvalidParams,
			Message: "no extension is named " + quoteOrDash(name) + "; this instance has " + strings.Join(known, ", ") +
				`. Use {"ext":"*"} to follow every extension, including ones created later`}
	}
	return name, nil
}

// subNames renders a snapshot for a reply. A nil snapshot is "nothing subscribed", which
// belongs in JSON as [] rather than null: a client reading `subscribed` should not have
// to treat two spellings of the empty set differently.
func subNames(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

// knownExtensionNames lists the CONFIGURED names for a refusal, bounded so a big store
// cannot turn one typo into a wall of text.
//
// THE STORE, NOT THE RUNTIME STATES: a disabled or never-started template is still a
// name a caller can subscribe to (and the one it is most likely to have got wrong), so
// answering "this instance has nothing" while `ls ext` shows a row would be false.
func (s *Server) knownExtensionNames() []string {
	const limit = 20
	names, err := s.extensions.Store().List()
	if err != nil {
		return nil
	}
	if len(names) > limit {
		return append(names[:limit:limit], "…")
	}
	return names
}

// compactNames removes adjacent duplicates from a sorted list.
func compactNames(sorted []string) []string {
	out := sorted[:0]
	for i, n := range sorted {
		if i > 0 && n == sorted[i-1] {
			continue
		}
		out = append(out, n)
	}
	return out
}
