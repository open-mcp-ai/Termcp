// The cdp door: what DevTools needs on top of a frame channel.
//
// `jrpc` is the honest layer — send a frame, get a frame — and it is enough for a
// peer whose protocol is request/response. A browser is not that peer, and the
// three things that make it different are exactly what a caller loses a browser
// lifecycle to discover:
//
//  1. A PAGE IS A SESSION. Everything useful (Page.*, Runtime.*, DOM.*) needs the
//     sessionId that Target.attachToTarget mints, and a frame without one is
//     addressed to the browser process instead — which does not fail, it answers
//     something else. Order matters too: attach BEFORE enabling, because the
//     sessionId does not exist until then, and a Page.enable sent to the browser
//     process succeeds and delivers nothing forever.
//
//  2. EVENTS ARE NEVER REPLAYED. Page.loadEventFired fires after Page.navigate has
//     already answered, so a reader that only looks at what a previous call caught
//     never sees it — and a reader that consumes it once can never look again. That
//     is what the frame log in internal/jrpc is for.
//
//  3. THE PROTOCOL IS THE PEER'S OWN. Chrome publishes its live signature at
//     /json/protocol; hardcoding a method table would be a second, drifting truth.
//
// So this door adds: a page address (<session> cdp:1), sessionId injection, a
// target listing, and the live protocol document. Everything else stays `call`.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/encoding"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/jrpc"
)

// cdpSession is one attached target: the sessionId plus what a caller needs to
// recognise it a turn later.
type cdpSession struct {
	ID      string
	Target  string
	Type    string
	Title   string
	URL     string
	Created time.Time
	// Transient marks a session opened by `cdp@targetId` for one call: it is never
	// recorded under a child handle, so it must never be listed or resolved.
	Transient bool
	// Domains records the `.enable` calls that have succeeded on this session, so
	// `get <session> cdp:1` can answer "is Page enabled?" without guessing. It is not
	// replayed: a reconnect invalidates the sessionId itself, and pretending
	// otherwise would be the silent failure this door exists to remove.
	Domains []string
}

// cdpState is the per-connection browser state: which targets are attached and under
// which session id.
//
// KEYED BY THE BROWSER'S OWN TARGET ID, not by a number this layer invents. The id is
// what `Target.getTargets` returns and what `Target.attachToTarget` takes, so the
// address a reply prints can be pasted straight back into the next frame — and two
// tabs can never collide on it the way a counter can after a detach.
type cdpState struct {
	sessions sync.Map // targetId string -> *cdpSession
	byID     sync.Map // sessionId string -> *cdpSession
}

func newCDPState() *cdpState {
	return &cdpState{}
}

// resolve maps a target id onto its attached state, or explains what exists.
func (c *cdpState) resolve(handle, target string) (*cdpSession, string) {
	v, ok := c.sessions.Load(target)
	if ok && v != nil {
		if sess, _ := v.(*cdpSession); sess != nil {
			return sess, ""
		}
	}
	live := c.live()
	if len(live) == 0 {
		return nil, handle + " has no attached target yet, so " + handle + "@" + target +
			" names nothing. Attach one: `open " + handle + " --target <targetId>` (target ids come from `ls " + handle + "`)."
	}
	return nil, "no target " + target + " attached to " + handle + ". Live ones: " + strings.Join(live, ", ") +
		". A target exists after `open " + handle + " --target <targetId>`."
}

// live reports the child indices currently attached.
func (c *cdpState) live() []string {
	var out []string
	c.sessions.Range(func(k, _ any) bool {
		if target, ok := k.(string); ok {
			out = append(out, target)
		}
		return true
	})
	sort.Strings(out)
	return out
}

// record remembers an attach.
func (c *cdpState) record(sess *cdpSession) {
	sess.Created = time.Now()
	c.sessions.Store(sess.Target, sess)
	c.byID.Store(sess.ID, sess)
}

// forget drops a session (detached, or destroyed by the page closing).
func (c *cdpState) forget(sessionID string) {
	v, ok := c.byID.LoadAndDelete(sessionID)
	if !ok {
		return
	}
	if sess, ok := v.(*cdpSession); ok && sess != nil {
		c.sessions.Delete(sess.Target)
	}
}

// noteEnabled records a successful domain enable, so the session's own state is
// readable instead of guessed.
func (c *cdpState) noteEnabled(sessionID, domain string) {
	v, ok := c.byID.Load(sessionID)
	if !ok {
		return
	}
	sess, _ := v.(*cdpSession)
	if sess == nil {
		return
	}
	for _, d := range sess.Domains {
		if d == domain {
			return
		}
	}
	sess.Domains = append(sess.Domains, domain)
}

// snapshot copies the sessions for rendering.
func (c *cdpState) snapshot() []*cdpSession {
	m := make(map[string]*cdpSession)
	var targets []string
	c.sessions.Range(func(k, v any) bool {
		target, ok := k.(string)
		sess, ok2 := v.(*cdpSession)
		if ok && ok2 {
			targets = append(targets, target)
			m[target] = sess
		}
		return true
	})
	sort.Strings(targets)
	out := make([]*cdpSession, 0, len(targets))
	for _, target := range targets {
		out = append(out, m[target])
	}
	return out
}

// cdpAttach implements `open <session> cdp --target <targetId>`: attach with flatten, mint
// the child handle, and REPORT the state a caller needs for the next call.
//
// The report is the point. A caller that has never read the CDP specification gets
// a handle, the target it names, and the sentence that says which calls need it —
// instead of a bare sessionId and a silent failure three calls later.
func (s *Server) cdpAttach(ctx context.Context, stmt directive.Statement, h *jrpcHandle, targetID string) envelope.Slot {
	handle := peerAddress(h.sessionID, h.kind, h.index)
	if targetID == "" {
		return badRequest(stmt, "open "+handle+" target needs a target id: `open "+handle+
			" --target <targetId>`. `ls "+handle+"` prints the ids.")
	}
	reply, err := h.ch.Call(ctx, map[string]any{
		"method": "Target.attachToTarget",
		"params": map[string]any{"targetId": targetID, "flatten": true},
	})
	if err != nil {
		return cdpFailure(stmt, "Target.attachToTarget", err)
	}
	var doc struct {
		Result struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(reply), &doc); err != nil {
		return cdpFailure(stmt, "Target.attachToTarget", fmt.Errorf("the reply is not JSON: %w", err))
	}
	if doc.Error != nil {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeInvalidArgument},
			Body: "the browser refused to attach " + targetID + ": " + doc.Error.Message +
				"\nA target id that is stale (the tab closed) is the usual cause; `ls " + handle + "` shows the live ones."}
	}
	if doc.Result.SessionID == "" {
		return cdpFailure(stmt, "Target.attachToTarget",
			fmt.Errorf("the browser answered without a sessionId: %s", truncateForReply(reply)))
	}

	sess := &cdpSession{ID: doc.Result.SessionID, Target: targetID}
	// The attach reply carries no title; ask once so `ls` and `get` can name the
	// page rather than print an opaque id. Best effort — a failure here must not
	// undo a successful attach.
	if info := s.cdpTargetInfo(ctx, h, targetID); info != nil {
		sess.Type, sess.Title, sess.URL = info.Type, info.Title, info.URL
	}
	h.cdp.record(sess)
	child := pageAddress(h, sess.Target)

	var b strings.Builder
	b.WriteString(child + " " + describeTarget(sess) + "\n")
	b.WriteString("attached; every frame for this page carries sessionId " + sess.ID + ", which `call " + child + "` fills in for you.\n")
	b.WriteString("\nNext: enable the domains you need, THEN navigate — a Page event never fires for a domain that was not enabled first:\n")
	b.WriteString("  call " + child + " '{\"method\":\"Page.enable\"}'\n")
	b.WriteString("  call " + child + " '{\"method\":\"Page.navigate\",\"params\":{\"url\":\"…\"}}'\n")
	b.WriteString("  read " + handle + " --since <n>   (n from `get " + handle + "`)")
	return envelope.Slot{
		Code:    200,
		Headers: map[string]string{"Path": stmt.Path, "Handle": child, "Session-Id": sess.ID},
		Body:    b.String(),
	}
}

// cdpDetach closes one attached session without closing the tab.
func (s *Server) cdpDetach(ctx context.Context, stmt directive.Statement, h *jrpcHandle, target string) envelope.Slot {
	handle := peerAddress(h.sessionID, h.kind, h.index)
	sess, problem := h.cdp.resolve(handle, target)
	if problem != "" {
		return notFoundSlot(stmt, CodeInvalidArgument, problem)
	}
	// Best effort: the tab may already be gone, and a detach that reports failure
	// for a session the browser forgot would leave the handle permanently stuck.
	_, _ = h.ch.Call(ctx, map[string]any{
		"sessionId": sess.ID,
		"method":    "Target.detachFromTarget",
		"params":    map[string]any{"sessionId": sess.ID},
	})
	h.cdp.forget(sess.ID)
	return noContent(stmt)
}

// cdpList implements `ls <peer>` for a browser: the live targets, which of them are
// attached, and under which handle.
//
// IT ASKS THE BROWSER, not a cache. A tab list that is one navigation stale is worse
// than no list, because a target id from it produces a silent attach failure.
func (s *Server) cdpList(ctx context.Context, stmt directive.Statement, h *jrpcHandle) envelope.Slot {
	handle := peerAddress(h.sessionID, h.kind, h.index)
	targets, err := s.cdpTargets(ctx, h)
	if err != nil {
		return cdpFailure(stmt, "Target.getTargets", err)
	}
	attached := map[string]string{} // targetId -> child handle
	for _, sess := range h.cdp.snapshot() {
		attached[sess.Target] = pageAddress(h, sess.Target)
	}

	var b strings.Builder
	b.WriteString(handle + "  " + h.ch.URL() + "  (" + h.ch.Transport().String() + ")\n\n")
	if len(targets) == 0 {
		b.WriteString("no targets. A headless browser with no page has none until something creates one:\n")
		b.WriteString("  call " + handle + " '{\"method\":\"Target.createTarget\",\"params\":{\"url\":\"about:blank\"}}'")
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
	}
	b.WriteString(fmt.Sprintf("%d target(s):\n", len(targets)))
	for _, t := range targets {
		line := "  " + t.ID + "  " + t.Type
		if child := attached[t.ID]; child != "" {
			line += "  ATTACHED as " + child
		}
		line += "\n      " + describeTargetFields(t.Type, t.Title, t.URL) + "\n"
		b.WriteString(line)
	}
	if len(attached) == 0 {
		b.WriteString("\nNothing is attached, so no page-level method works yet. Attach one:\n")
		b.WriteString("  open " + handle + " --target " + targets[0].ID)
	} else {
		b.WriteString("\nSessions live under this peer: " + strings.Join(h.cdp.live(), " ") +
			"  (address one as " + handle + ":N)")
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
}

// cdpTarget is the part of a target's description this door renders.
type cdpTarget struct {
	ID    string
	Type  string
	Title string
	URL   string
}

func describeTarget(t *cdpSession) string {
	return describeTargetFields(t.Type, t.Title, t.URL)
}

func describeTargetFields(typ, title, url string) string {
	if title == "" {
		title = "(untitled)"
	}
	line := title + " — " + url
	if typ != "" && typ != "page" {
		line = typ + ": " + line
	}
	return line
}

// cdpTargetInfo asks for one target's description (used right after an attach, so
// the child handle is not an opaque id in the reply).
func (s *Server) cdpTargetInfo(ctx context.Context, h *jrpcHandle, targetID string) *cdpTarget {
	targets, err := s.cdpTargets(ctx, h)
	if err != nil {
		return nil
	}
	for i := range targets {
		if targets[i].ID == targetID {
			return &targets[i]
		}
	}
	return nil
}

// cdpTargets reads the live target list.
func (s *Server) cdpTargets(ctx context.Context, h *jrpcHandle) ([]cdpTarget, error) {
	reply, err := h.ch.Call(ctx, map[string]any{"method": "Target.getTargets"})
	if err != nil {
		return nil, err
	}
	var doc struct {
		Result struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				Title    string `json:"title"`
				URL      string `json:"url"`
			} `json:"targetInfos"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(reply), &doc); err != nil {
		return nil, fmt.Errorf("the reply is not JSON: %w", err)
	}
	if doc.Error != nil {
		return nil, fmt.Errorf("%s", doc.Error.Message)
	}
	out := make([]cdpTarget, 0, len(doc.Result.TargetInfos))
	for _, t := range doc.Result.TargetInfos {
		out = append(out, cdpTarget{
			ID: t.TargetID, Type: t.Type,
			// Titles and URLs are the peer's own strings, still carrying the JSON
			// escapes the browser writes for anything non-ASCII.
			Title: encoding.UnescapeJSON(t.Title),
			URL:   encoding.UnescapeJSON(t.URL),
		})
	}
	return out, nil
}

// cdpDescribe implements `get <peer>` and `get <peer>:N`.
func (s *Server) cdpDescribe(stmt directive.Statement, h *jrpcHandle, target string) envelope.Slot {
	handle := peerAddress(h.sessionID, h.kind, h.index)
	if target == "" {
		var b strings.Builder
		b.WriteString(handle + "  " + h.ch.URL() + "  (" + h.ch.Transport().String() + ")\n")
		stats := h.ch.Latest()
		b.WriteString(fmt.Sprintf("frames received: %d  (read --since N pages the history; N is the number you last saw)\n", stats))
		sessions := h.cdp.snapshot()
		if len(sessions) == 0 {
			b.WriteString("\nno attached targets. `ls " + handle + "` lists the tabs; `open " + handle + " --target <targetId>` attaches one.")
			return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path, "Latest": fmt.Sprint(stats)}, Body: b.String()}
		}
		b.WriteString("\nattached targets:\n")
		for _, sess := range sessions {
			b.WriteString("  " + pageAddress(h, sess.Target) + "  " + describeTarget(sess) + "\n")
			if len(sess.Domains) > 0 {
				b.WriteString("      enabled: " + strings.Join(sess.Domains, ", ") + "\n")
			} else {
				b.WriteString("      enabled: none (Page events never fire until Page.enable)\n")
			}
		}
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path, "Latest": fmt.Sprint(stats)}, Body: b.String()}
	}
	sess, problem := h.cdp.resolve(handle, target)
	if problem != "" {
		return notFoundSlot(stmt, CodeInvalidArgument, problem)
	}
	var b strings.Builder
	b.WriteString(pageAddress(h, sess.Target) + "  " + describeTarget(sess) + "\n")
	b.WriteString("sessionId: " + sess.ID + "\n")
	b.WriteString("targetId:  " + sess.Target + "\n")
	b.WriteString("age:       " + time.Since(sess.Created).Round(time.Second).String() + "\n")
	if len(sess.Domains) > 0 {
		b.WriteString("enabled:   " + strings.Join(sess.Domains, ", ") + "\n")
	} else {
		b.WriteString("enabled:   none\n")
	}
	b.WriteString("\ncalls go through `call " + pageAddress(h, sess.Target) + " '<frame>'` — the sessionId is filled in for you.")
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path, "Session-Id": sess.ID}, Body: b.String()}
}

// cdpProtocol implements `get <peer> protocol [Domain]`: the LIVE signature, read
// from the browser rather than from a table in this binary.
//
// A hardcoded method list is a second truth that drifts the moment the browser
// updates, and the drift is silent — a caller fills in parameters that no longer
// exist and reads the resulting error as their own mistake. The browser publishes
// its own document at /json/protocol; asking it is both smaller and correct.
func (s *Server) cdpProtocol(ctx context.Context, stmt directive.Statement, h *jrpcHandle, domain string) envelope.Slot {
	handle := peerAddress(h.sessionID, h.kind, h.index)
	if h.httpBase == "" {
		return badRequest(stmt, "this peer was opened on a bare ws:// URL, so its protocol document has no address to read.\n"+
			"Reopen it through the http endpoint and the signature comes with it: `open <session> cdp "+h.ch.URL()+"` "+
			"or `open <session> cdp http://<host>:<port>`.")
	}
	raw, err := fetchOverSession(ctx, h.httpBase+"/json/protocol", h.dial)
	if err != nil {
		return cdpFailure(stmt, "GET /json/protocol", err)
	}
	var doc struct {
		// THE VERSION IS AN OBJECT, not a string: {"major":"1","minor":"3"}.
		// Typing it as a string failed the whole document, which is how a
		// self-describing endpoint turned into "not JSON" for a field nobody reads.
		Version struct {
			Major string `json:"major"`
			Minor string `json:"minor"`
		} `json:"version"`
		Domains []struct {
			Domain      string `json:"domain"`
			Description string `json:"description"`
			Commands    []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Parameters  []struct {
					Name     string `json:"name"`
					Type     string `json:"type"`
					Optional bool   `json:"optional"`
					Desc     string `json:"description"`
				} `json:"parameters"`
			} `json:"commands"`
			Events []struct {
				Name string `json:"name"`
			} `json:"events"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return cdpFailure(stmt, "GET /json/protocol", fmt.Errorf("the document is not JSON: %w", err))
	}
	var b strings.Builder
	if domain == "" {
		version := strings.Trim(doc.Version.Major+"."+doc.Version.Minor, ".")
		if version == "" {
			version = "(unknown)"
		}
		b.WriteString(fmt.Sprintf("CDP %s — %d domains. `get %s protocol <Domain>` shows one domain's commands.\n\n",
			version, len(doc.Domains), handle))
		for _, d := range doc.Domains {
			b.WriteString(fmt.Sprintf("  %-22s %2d commands  %2d events\n", d.Domain, len(d.Commands), len(d.Events)))
		}
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
	}
	for _, d := range doc.Domains {
		if !strings.EqualFold(d.Domain, domain) {
			continue
		}
		b.WriteString(d.Domain + " — " + firstLineSummary(d.Description) + "\n\ncommands:\n")
		for _, c := range d.Commands {
			b.WriteString("  " + c.Name + "(")
			for i, p := range c.Parameters {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(p.Name)
				if p.Optional {
					b.WriteString("?")
				}
				b.WriteString(": " + p.Type)
			}
			b.WriteString(")\n")
		}
		if len(d.Events) > 0 {
			b.WriteString("\nevents:\n")
			for _, e := range d.Events {
				b.WriteString("  " + e.Name + "\n")
			}
		}
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
	}
	names := make([]string, 0, len(doc.Domains))
	for _, d := range doc.Domains {
		names = append(names, d.Domain)
	}
	return notFoundSlot(stmt, CodeInvalidArgument,
		"no domain "+domain+" in the live protocol. Available: "+strings.Join(names, ", "))
}

// cdpEvents implements `read <peer> --since N`: the frame history from a cursor.
//
// IT DOES NOT CONSUME. A frame log is history, not a queue: the caller pages
// forward by passing the last number it saw, and asking again from the same cursor
// gives the same answer. That is what makes it usable for the pattern it exists for
// — record the latest number, click, ask for what happened after it.
func (s *Server) cdpEvents(ctx context.Context, stmt directive.Statement, h *jrpcHandle, target string, since int64, grep string, win termArgs) envelope.Slot {
	handle := peerAddress(h.sessionID, h.kind, h.index)
	var re *regexp.Regexp
	if grep != "" {
		compiled, err := regexp.Compile(grep)
		if err != nil {
			return badRequest(stmt, "grep: "+err.Error())
		}
		re = compiled
	}
	// ONE PAGE, OR ALL OF THEM. A browser multiplexes every attached page's frames
	// onto the one socket this peer holds, tagged with their sessionId — so a page
	// address narrows the window rather than opening a second one. Filtering happens
	// BEFORE the page cap (the recorder's contract), so a busy sibling page cannot
	// push this page's frames out of the reply.
	sessionID := ""
	if target != "" {
		sess, problem := h.cdp.resolve(handle, target)
		if problem != "" {
			return notFoundSlot(stmt, CodeInvalidArgument, problem)
		}
		sessionID = sess.ID
	}
	// A page can emit thousands of frames per navigation, and a reply carrying all
	// of them is the failure mode the cursor exists to avoid. The cap is a page
	// size, not a filter: the reply says how to ask for the rest.
	const page = 200
	frames, stats := h.ch.History(since, func(f jrpc.Framed) bool {
		if sessionID != "" && !frameBelongsTo(f.Data, sessionID) {
			return false
		}
		if re == nil {
			return true
		}
		return re.MatchString(f.Data)
	}, page)
	if len(frames) == 0 {
		// NOTHING AFTER `--since` IS A 204 THAT DID NOT DELIVER, and Latest is what tells a
		// caller whether it fell behind or asked past the end. Both facts ride in the
		// Hint header, because a 204 with a body contradicts its own status line (see
		// noContentSaying).
		return noContentSaying(map[string]string{
			"Path": stmt.Path, "Latest": fmt.Sprint(stats.Seq),
		}, "no frames after "+fmt.Sprint(since)+" (latest is "+fmt.Sprint(stats.Seq)+")")
	}
	var b strings.Builder
	for _, f := range frames {
		kind := "answer"
		if f.Event {
			kind = "event"
		}
		b.WriteString(fmt.Sprint(f.N) + " " + kind + " " + encoding.UnescapeJSON(f.Data) + "\n")
	}
	last := frames[len(frames)-1].N
	headers := map[string]string{
		"Path":   stmt.Path,
		"Frames": fmt.Sprintf("%d-%d", frames[0].N, last),
		"Latest": fmt.Sprint(stats.Seq),
	}
	if last < stats.Seq {
		b.WriteString("\n…more follow. Continue with `read " + handle + " --since " + fmt.Sprint(last) + "`.")
	}
	if stats.Dropped > 0 && since < stats.Seq-int64(stats.Size) {
		b.WriteString(fmt.Sprintf("\n(the window holds the newest %d frames; %d older ones were dropped. --since is a cursor, not an archive.)",
			stats.Size, stats.Dropped))
	}
	_ = win
	return envelope.Slot{Code: 200, Headers: headers, Body: b.String()}
}

// cdpSend writes one frame without waiting for an answer.
func (s *Server) cdpSend(ctx context.Context, stmt directive.Statement, h *jrpcHandle, target string, frame map[string]any) envelope.Slot {
	handle := peerAddress(h.sessionID, h.kind, h.index)
	if target != "" {
		sess, problem := h.cdp.resolve(handle, target)
		if problem != "" {
			return notFoundSlot(stmt, CodeInvalidArgument, problem)
		}
		if _, ok := frame["sessionId"]; !ok {
			frame["sessionId"] = sess.ID
		}
	}
	if err := h.ch.Send(ctx, frame); err != nil {
		return cdpFailure(stmt, frameMethod(frame), err)
	}
	return envelope.Slot{Code: 202, Headers: map[string]string{"Path": stmt.Path},
		Body: "sent; read the answer or events with `read " + handle + " --since <n>`"}
}

// cdpCall runs one frame against a browser, filling in the sessionId the target
// address names.
//
// THE INJECTION IS THE WHOLE FEATURE. Writing the sessionId by hand in every frame
// is both noisy and the one place a mistake is silent: a frame without one is not
// rejected, it is addressed to the browser process, so `Page.navigate` answers with
// a frameId and the page does not move.
//
// A TRANSIENT TARGET (`cdp@<targetId>`, no connection index) is the one-shot form:
// attach, call, detach, so a single command needs no attachment to keep. Naming the
// connection too (`cdp:1@<targetId>`) means the target is already attached, and the
// call just uses that session. It is NOT for multi-step work — the
// domain enablement a page event depends on dies with the session — so the reply
// says that when the frame looks like a first step.
func (s *Server) cdpCall(ctx context.Context, stmt directive.Statement, h *jrpcHandle, win termArgs, target string, frame map[string]any) envelope.Slot {
	handle := peerAddress(h.sessionID, h.kind, h.index)
	if transient := cdpTransientTarget(stmt.Key); transient != "" {
		sess, slot, ok := s.cdpTransientSession(ctx, stmt, h, transient)
		if !ok {
			return slot
		}
		// Detached even when the call fails: leaving a one-shot session behind would
		// leak a handle the caller never learned the name of. A REUSED session is not
		// ours, though — detaching it would tear down work this call knows nothing
		// about, which is the silent damage the reuse rule exists to prevent.
		if sess.Transient {
			defer s.cdpDetachQuietly(ctx, h, sess.ID)
		}
		if _, exists := frame["sessionId"]; !exists {
			frame["sessionId"] = sess.ID
		}
		reply, err := h.ch.Call(ctx, frame)
		if err != nil {
			return cdpFailure(stmt, frameMethod(frame), err)
		}
		return s.frameReply(ctx, stmt, h, reply, win)
	}
	if target != "" {
		sess, problem := h.cdp.resolve(handle, target)
		if problem != "" {
			return notFoundSlot(stmt, CodeInvalidArgument, problem)
		}
		if _, ok := frame["sessionId"]; !ok {
			frame["sessionId"] = sess.ID
		}
		// Record the enable as it succeeds, so `get` can answer "what is on" rather
		// than the caller having to remember.
		if method, _ := frame["method"].(string); strings.HasSuffix(method, ".enable") {
			defer func() {
				h.cdp.noteEnabled(sess.ID, strings.TrimSuffix(method, ".enable"))
			}()
		}
	}
	reply, err := h.ch.Call(ctx, frame)
	if err != nil {
		return cdpFailure(stmt, frameMethod(frame), err)
	}
	return s.frameReply(ctx, stmt, h, reply, win)
}

// cdpTransientTarget returns the targetId in `cdp@ABC123`, or "".
//
// THE `@` FORM EXISTS FOR ONE-SHOT COMMANDS. Attaching a session to reload a tab is
// two calls and an address to remember; `call a1f3 cdp@ABC123 '{…Page.reload}'` is
// one call and no state. It is deliberately NOT a session: domain enablement dies
// with it, so anything multi-step must attach and keep the child address.
func cdpTransientTarget(key string) string {
	if !strings.HasPrefix(key, "@") || len(key) == 1 {
		return ""
	}
	return key[1:]
}

// cdpTransientSession attaches for one call, REUSING an existing session on that
// target when there is one.
//
// Reuse is not an optimisation, it is what keeps a one-shot from destroying
// somebody else's handle: attaching twice to the same target in flatten mode gives a
// second session, and detaching ours would be fine — but tearing down a session a
// caller is still holding would break work this call knows nothing about.
func (s *Server) cdpTransientSession(ctx context.Context, stmt directive.Statement, h *jrpcHandle, targetID string) (*cdpSession, envelope.Slot, bool) {
	for _, sess := range h.cdp.snapshot() {
		if sess.Target == targetID {
			return sess, envelope.Slot{}, true
		}
	}
	reply, err := h.ch.Call(ctx, map[string]any{
		"method": "Target.attachToTarget",
		"params": map[string]any{"targetId": targetID, "flatten": true},
	})
	if err != nil {
		return nil, cdpFailure(stmt, "Target.attachToTarget", err), false
	}
	var doc struct {
		Result struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(reply), &doc); err != nil {
		return nil, cdpFailure(stmt, "Target.attachToTarget", fmt.Errorf("the reply is not JSON: %w", err)), false
	}
	if doc.Error != nil {
		return nil, notFoundSlot(stmt, CodeInvalidArgument,
			"cannot attach "+targetID+": "+encoding.UnescapeJSON(doc.Error.Message)+
				"\nA stale target id is the usual cause; `ls` lists the live ones."), false
	}
	if doc.Result.SessionID == "" {
		return nil, cdpFailure(stmt, "Target.attachToTarget",
			fmt.Errorf("the browser answered without a sessionId: %s", truncateForReply(reply))), false
	}
	sess := &cdpSession{ID: doc.Result.SessionID, Target: targetID, Transient: true}
	if info := s.cdpTargetInfo(ctx, h, targetID); info != nil {
		sess.Type, sess.Title, sess.URL = info.Type, info.Title, info.URL
	}
	return sess, envelope.Slot{}, true
}

// detachQuietly ends a one-shot session: tell the browser, then forget it.
//
// Failure is ignored on purpose — the tab may already be gone, and the caller never
// held this handle, so there is nothing to report it to.
func (s *Server) cdpDetachQuietly(ctx context.Context, h *jrpcHandle, sessionID string) {
	_, _ = h.ch.Call(ctx, map[string]any{
		"method": "Target.detachFromTarget",
		"params": map[string]any{"sessionId": sessionID},
	})
	h.cdp.forget(sessionID)
}

// cdpFailure renders a transport or protocol failure with the sentence that says
// what to do about it, instead of a bare error string.
func cdpFailure(stmt directive.Statement, what string, err error) envelope.Slot {
	return envelope.Slot{Code: 500, Headers: map[string]string{
		"Path": stmt.Path, "Error-Code": CodeConnectionFailed,
	}, Body: what + ": " + err.Error()}
}

func frameMethod(frame map[string]any) string {
	if m, ok := frame["method"].(string); ok {
		return m
	}
	return "call"
}

func firstLineSummary(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func truncateForReply(s string) string {
	s = encoding.UnescapeJSON(s)
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

// frameReply renders one frame as a reply, applying the two transformations every
// frame deserves on its way out: the peer's JSON escapes become characters, and an
// oversized payload becomes a receipt.
func (s *Server) frameReply(ctx context.Context, stmt directive.Statement, h *jrpcHandle, reply string, win termArgs) envelope.Slot {
	// A PAYLOAD THAT DOES NOT FIT IS A RECEIPT, NOT A WALL OF TEXT.
	//
	// The space window would truncate it, and a truncated JSON document is the worst
	// of both worlds: it is too big to have been worth the tokens and too broken to
	// use. So the frame stays in the log (it is already there — every frame on the
	// wire is recorded) and the reply names it. `read --at <n> --offset <b>` pages it
	// out a slice at a time, which is also the only way to look at the middle of a
	// 46 KB DOM dump.
	//
	// "DOES NOT FIT" MEANS THE CALLER'S WINDOW, NOT A CONSTANT (issue #204).
	//
	// This was a hardcoded 4096, and the two doors to a peer each had their own copy of
	// it. So `call … --bytes 32768` answered with a 512-byte preview and no header
	// saying why — on a tool whose own documentation promises the opposite ("NEITHER
	// HAS A HARD CAP … a larger value is honoured", term.go). The size of a reply is
	// the caller's decision to make: it is the party paying for the tokens and the only
	// one that knows whether it is reading a title or a DOM dump.
	//
	// The receipt is still the fallback for the case it was written for — a frame
	// larger than the window the caller chose — so no truncated JSON can appear either
	// way. What changed is who picks the number.
	inlineLimit := win.BytesLimit
	if inlineLimit <= 0 {
		// 0 means "hand back the channel, consume nothing", which cannot also mean
		// "inline everything": the only window left is the default.
		inlineLimit = defaultBytesLimit
	}
	if body := encoding.UnescapeJSON(reply); len(body) > inlineLimit {
		if rec, ok := h.ch.LastAnswer(); ok {
			// Bytes and Decoded-Bytes mean the same thing here as they do in `read --at`:
			// the frame in the log, and the same frame as text. A receipt that measured
			// the text and a pager that measured the log was one header with two meanings.
			return envelope.Slot{Code: 200, Headers: map[string]string{
				"Path": stmt.Path, "Frame": fmt.Sprint(rec.N),
				"Bytes": fmt.Sprint(len(rec.Data)), "Decoded-Bytes": fmt.Sprint(len(body)),
			}, Body: receiptBody(rec.N, body, len(rec.Data), inlineLimit, stmt, h)}
		}
	}
	// A PEER THAT ANSWERED WITH AN ERROR DID NOT SUCCEED. A 200 whose body happens to
	// contain {"error":…} is the silent failure this door exists to remove: the caller
	// reads the status, sees 2xx, and concludes the browser did what it asked. The
	// status is what a caller branches on, so it has to say so.
	if slot, isError := peerError(stmt, h, reply); isError {
		return slot
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: encoding.UnescapeJSON(reply)}
}

// peerError turns a JSON-RPC error frame into a failing reply, with the sentence
// that says what went wrong in terms of the tree rather than the protocol.
//
// THE TWO HINTS THAT MATTER are the two silent traps. A page-level method sent to
// the browser process is answered with "wasn't found" — not with a refusal — so it
// looks like a typo when it is really "this belongs to a page". A sessionId that
// stopped existing because the tab navigated or closed reads as an ordinary error
// unless the reply says to re-attach.
func peerError(stmt directive.Statement, h *jrpcHandle, reply string) (envelope.Slot, bool) {
	return peerErrorOr(stmt, h, reply, 400, CodeInvalidArgument)
}

func peerErrorOr(stmt directive.Statement, h *jrpcHandle, reply string, code int, errorCode string) (envelope.Slot, bool) {
	var doc struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(reply), &doc); err != nil || doc.Error == nil {
		return envelope.Slot{}, false
	}
	msg := encoding.UnescapeJSON(doc.Error.Message)
	index := stmt.Key
	hasChild := index != ""
	var hint string
	switch {
	case strings.Contains(msg, "wasn't found") || strings.Contains(msg, "was not found"):
		if !hasChild || index == "" {
			hint = "\nThat is a PAGE-level method, and this frame went to the browser process, which does not have it. " +
				"Address the attached page instead: `call <session> " + h.kind + ":N …` (`ls <session> " + h.kind + "` lists them)."
		}
	case strings.Contains(msg, "No session with given id") || strings.Contains(msg, "Session with given id not found"):
		hint = "\nThat sessionId no longer exists (the tab closed, or the target was replaced by a navigation). " +
			"`ls <session> " + h.kind + "` shows the live targets; re-attach with `open <session> " + h.kind + " --target <targetId>`."
	case strings.Contains(msg, "Method not found") || strings.Contains(msg, "' was not found"):
		hint = "\nCheck the method against the browser's own signature: `get <peer> protocol <Domain>`."
	}
	body := msg
	if hint != "" {
		body += "\n" + hint
	}
	return envelope.Slot{Code: code, Headers: map[string]string{
		"Path": stmt.Path, "Error-Code": errorCode, "Peer-Error": fmt.Sprint(doc.Error.Code),
	}, Body: body}, true
}

// fetchOverSession reads a plain HTTP document through the session's dialer, so the
// request reaches the same network the socket does.
//
// IT IS NOT A SECOND RULE. Reading /json/protocol from this process while the socket
// travels the SSH link would make `get <session> cdp protocol` work for a local browser and
// fail for the remote one the whole tool exists for.
func fetchOverSession(ctx context.Context, url string, dial jrpc.DialFunc) ([]byte, error) {
	if dial == nil {
		return nil, fmt.Errorf("no dialer for this peer")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: &http.Transport{DialContext: dial}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	// Bounded: a protocol document is a few hundred KB, and an unbounded read of a
	// misconfigured endpoint would be the daemon's memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// httpBaseOf derives the http endpoint a ws:// socket belongs to. DevTools publishes
// both on the same port, so the derivation is exact rather than a guess.
func httpBaseOf(socketURL string) string {
	u, err := url.Parse(socketURL)
	if err != nil || u.Host == "" {
		return ""
	}
	switch u.Scheme {
	case "http", "https":
		return u.Scheme + "://" + u.Host
	case "ws":
		return "http://" + u.Host
	case "wss":
		return "https://" + u.Host
	}
	return ""
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// frameBelongsTo reports whether a frame on the socket was scoped to one session.
//
// IN FLATTEN MODE THE sessionId IS PART OF THE FRAME, which is exactly why one
// socket can carry several pages: the tag is what tells them apart. A frame that
// carries no tag came from the browser process itself, not from any page, so it
// belongs to none of them — dropping it is right when the caller asked for one page.
//
// A cheap prefix test first: a frame that does not mention sessionId at all (an
// answer to a browser-level call, most events from the root session) never needs to
// be parsed, and those are the common case.
func frameBelongsTo(data, sessionID string) bool {
	if !strings.Contains(data, `"sessionId"`) {
		return false
	}
	var doc struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		return false
	}
	return doc.SessionID == sessionID
}

// receiptBody describes an oversized frame and says how to read it.
//
// IT SHOWS THE HEAD, because most oversized answers are recognizable from their first
// line or two and making the caller page just to find out what they are holding is a
// round trip for nothing. The head is cut from the same bytes the pager serves, so it
// is exactly the start of what `--offset 0` returns.
func receiptBody(n int64, body string, logBytes int, limit int, stmt directive.Statement, h *jrpcHandle) string {
	const preview = 512
	head := body
	if len(head) > preview {
		head = head[:preview]
		// Do not end the preview on a broken UTF-8 sequence: a multibyte character cut
		// in half reads as mojibake and makes the whole reply look corrupt.
		for len(head) > 0 && !utf8.ValidString(head) {
			head = head[:len(head)-1]
		}
	}
	addr := stmt.Target + " " + h.kind + ":" + h.index
	var b strings.Builder
	// TWO SIZES, AND THE SENTENCE NAMES THE ONE IT MEANS. `body` is the text the caller
	// would have been handed; logBytes is the same frame as the log stores it, which is
	// the coordinate system --offset counts in. They differ only where the peer escaped
	// non-ASCII, and saying so is what makes the two numbers in the headers add up.
	// NAME THE WINDOW, NOT A MYSTERY. "too big to inline" used to be the whole
	// explanation, and the number behind it was a constant in this file — so a caller
	// that had asked for 32 KB read the same sentence as one that had asked for 4 KB,
	// and could not tell that the limit was its own (issue #204). The size named here
	// is the one that stopped it.
	b.WriteString(fmt.Sprintf("frame %d is %s of text; this reply was asked for at most %s, so it is a receipt instead.\n",
		n, humanBytes(int64(len(body))), humanBytes(int64(limit))))
	b.WriteString("Ask for a bigger window to get it whole (--bytes N), or page it from the log.\n")
	if logBytes != len(body) {
		b.WriteString("It is " + humanBytes(int64(logBytes)) + " in the log, and --offset counts those bytes.\n")
	}
	b.WriteString("The frame is in the log; page it a slice at a time:\n\n")
	b.WriteString("  read " + addr + " --at " + fmt.Sprint(n) + " --offset 0\n")
	b.WriteString("  read " + addr + " --at " + fmt.Sprint(n) + " --offset " + fmt.Sprint(preview) + "\n\n")
	b.WriteString("Pages are rendered as text: JSON string escapes are decoded, so a title the peer\n")
	b.WriteString("sent as \\u817e\\u8baf\\u4e91 reads as 腾讯云. Add --raw for the bytes as they\n")
	b.WriteString("arrived, which is what to compare when a reply looks wrong.\n\n")
	b.WriteString("\nfirst " + fmt.Sprint(len(head)) + " bytes:\n" + head)
	return b.String()
}

// pageFrame answers `read <peer> --at <n> [--offset <b>]`: one slice of one frame.
//
// THIS IS THE OTHER AXIS OF THE CURSOR. `--since` pages forward in TIME (what happened
// after n); `--at` pages inside ONE payload (the n-th frame, from byte b). Both are
// reads of the same log, so neither consumes it.
func (s *Server) pageFrame(stmt directive.Statement, h *jrpcHandle, n int64, offset, limit int) envelope.Slot {
	latest := h.ch.Latest()
	rec, ok := h.ch.Frame(n)
	if !ok {
		return envelope.Slot{Code: 404, Headers: map[string]string{
			"Path": stmt.Path, "Error-Code": "not_found", "Latest": fmt.Sprint(latest),
		}, Body: fmt.Sprintf("frame %d is not in the log (the newest frame is %d).", n, latest)}
	}
	body := []byte(rec.Data)
	if offset < 0 || offset > len(body) {
		return badRequest(stmt, fmt.Sprintf("--offset %d is outside frame %d, which is %d bytes", offset, n, len(body)))
	}
	if limit <= 0 {
		limit = inlinePageBytes
	}
	end := offset + limit
	if end > len(body) {
		end = len(body)
	}
	slice := string(body[offset:end])
	// A slice boundary can cut a character in half; back off to a valid boundary so
	// the page reads as text rather than as replacement characters.
	for len(slice) > 0 && !utf8.ValidString(slice) {
		end--
		slice = string(body[offset:end])
	}
	// A WINDOW MUST NOT END INSIDE A \uXXXX ESCAPE EITHER. Next-Offset is a byte
	// count this code chooses, and a count that lands between the \u and its hex
	// digits hands the next page the leftovers: the seam reads "17e讯云" instead of
	// "腾讯云". Same reasoning as the UTF-8 boundary above — the page seam is this
	// function's decision, so it is this function's job to land it on a boundary.
	//
	// i > 0 AND NOT i >= 0, because an escape starting at the window's FIRST byte
	// cannot be backed off: the page would come back empty with Next-Offset equal to
	// the offset just asked for, and a caller walking Next-Offset would loop forever
	// making no progress. The caller chose that offset; a short page that advances is
	// better than a page that does not.
	if i := encoding.TrailingEscapeStart(slice); i > 0 {
		end -= len(slice) - i
		slice = slice[:i]
	}
	// A PAGE GETS THE SAME TREATMENT AS AN INLINE REPLY. frameReply already runs
	// every frame through encoding.UnescapeJSON before answering with it — the peer
	// escapes non-ASCII (Chrome sends a Chinese title as \u817e\u8baf\u4e91) and a
	// reader wants the characters. Paging used to skip that step, which made a big
	// answer strictly worse than a small one: over the inline limit the only way to
	// read a frame is --at, and --at was the one door that served the escapes.
	//
	// --raw KEEPS THE BYTES AS THEY ARRIVED. It used to be accepted on this path and
	// then never read, which is worse than refusing it: a caller asked for the peer's
	// own bytes, was handed a re-encoding, and had no way to tell.
	//
	// THE OFFSETS STAY BYTE OFFSETS OF THE STORED FRAME, not of the rendering, so
	// Next-Offset keeps working page after page.
	raw := boolFlag(stmt, "raw")
	if !raw {
		slice = encoding.UnescapeJSON(slice)
	}
	// BOTH SIZES, BOTH ABOUT THE WHOLE FRAME. Bytes is the frame as the log stores it,
	// which is what Offset and Next-Offset count; Decoded-Bytes is the same frame as
	// text, which is what a caller is reading. Equal where the peer escaped nothing.
	headers := map[string]string{
		"Path":          stmt.Path,
		"Frame":         fmt.Sprint(n),
		"Bytes":         fmt.Sprint(len(body)),
		"Decoded-Bytes": fmt.Sprint(len(encoding.UnescapeJSON(rec.Data))),
		"Offset":        fmt.Sprintf("%d-%d", offset, end),
	}
	// SAY WHICH ONE THE CALLER IS LOOKING AT. The two renderings differ only for
	// non-ASCII text, so a reader that did not ask has no way to notice the bytes were
	// unescaped — and a reader that DID ask needs to know it got them. Served is not
	// named "Decoded" because the sizes above are named after decoding too, and one
	// header meaning "this page's rendering" while another means "the frame's text
	// length" is the kind of near-collision that costs a reader a second look.
	if raw {
		headers["Served"] = "raw"
	} else {
		headers["Served"] = "decoded"
	}
	if end < len(body) {
		headers["Next-Offset"] = fmt.Sprint(end)
	}
	return envelope.Slot{Code: 200, Headers: headers, Body: slice}
}

// inlinePageBytes is the size of one page when the caller did not ask for a size.
// It is the same number the receipt previews, so "first 4 KB" and "one page" agree.
const inlinePageBytes = 4096
