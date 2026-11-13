package extensions

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
)

// THE HANDLER IS THIS INSTANCE'S OWN HTTP SURFACE, WHICH MEANS THE AUTH IS ALREADY IN IT
// (issue #321). `core.Server.Handler()` returns the stack the local listener serves —
// the mux with auth.Middleware wrapped around it — so a request arriving over the tunnel
// passes exactly the checks it would have passed on loopback, and a caller that reaches a
// protected path without a token gets the same 401 challenge. Exposing the bare mux
// instead would have published every route unauthenticated, which is why this is a
// function rather than a value: the stack is only final after the middleware is applied.
//
// What the tunnel does NOT inherit is the idle watcher, which wraps later. That is
// deliberate: an idle countdown is about a human leaving a browser open, not about a
// request that arrived through a tunnel.

// Whitelist answers 404 for every path the declaration did not name.
//
// EXACT MATCH, NO WILDCARDS, NO PREFIXES. A prefix rule turns "/api" into "/api.md,
// /api/term and every route somebody adds later", which is the opposite of what a
// whitelist is for. 404 rather than 403 is also deliberate: a 403 confirms the route
// exists, and the point of not publishing a route is that the far side cannot tell.
func Whitelist(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(allowed, r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SetExposeHandler tells the reconciler what to publish when a template declares an
// expose. Nil (the default) means this instance has no HTTP surface to publish, and a
// template that asks for one is reported rather than silently left without a listener.
func (r *Reconciler) SetExposeHandler(fn func() http.Handler) {
	r.exposeHandler = fn
}

// ensureExposes publishes the exposes the template declares, skipping the ones still
// listening. It mirrors ensureForwards on purpose: the same "keep what already worked"
// rule, the same teardown with the session, the same position-is-identity.
func (r *Reconciler) ensureExposes(ext *Extension, st *extState) error {
	if len(ext.Exposes) == 0 {
		return nil
	}
	if r.forwardMgr == nil || r.exposeHandler == nil {
		return errors.New("this instance cannot expose an HTTP surface: it was started without one (embedders that build their own listener must call SetExposeHandler)")
	}
	st.mu.Lock()
	if st.sessionID == "" {
		st.mu.Unlock()
		return errors.New("no session to attach exposes to")
	}
	sessID := st.sessionID
	existing := append([]ForwardStatus(nil), st.exposes...)
	st.mu.Unlock()

	live := map[string]bool{}
	for _, fw := range r.forwardMgr.ListBySession(sessID) {
		live[fw.ForwardID] = true
	}

	sess := r.sessMgr.Get(sessID)
	if sess == nil {
		return errors.New("session for exposes is gone")
	}
	info := sess.Info()
	sshCli := sess.SSHClient()

	base := r.exposeHandler()
	built := make([]ForwardStatus, len(ext.Exposes))
	for i, ex := range ext.Exposes {
		if i < len(existing) && live[existing[i].ForwardID] {
			built[i] = existing[i]
			continue
		}
		// The whitelist is built per declaration, so two exposes of the same instance
		// with different path lists do not share one answer.
		handler := Whitelist(ex.Paths, base)
		fw, err := r.forwardMgr.CreateRemoteHTTP(sessID, info.Name, ex.RemoteHost, ex.RemotePort, handler, sshCli)
		if err != nil {
			st.mu.Lock()
			st.exposes = append([]ForwardStatus(nil), built...)
			st.mu.Unlock()
			return fmt.Errorf("http expose on %s: %w", exposeSpec(ex), err)
		}
		sess.AttachCleanup(func() { r.forwardMgr.Close(fw.ForwardID) })
		built[i] = ForwardStatus{
			DirectionKind: "http",
			ForwardID:     fw.ForwardID,
			ListenAddr:    fw.ListenAddr,
			TargetAddr:    fw.TargetAddr,
		}
	}

	st.mu.Lock()
	st.exposes = built
	st.mu.Unlock()
	return nil
}

// exposeSpec renders a declaration for an error message, the way listenSpec does for a
// forward.
func exposeSpec(x ExposeDecl) string {
	host := x.RemoteHost
	if host == "" {
		host = "loopback"
	}
	port := "ephemeral port"
	if x.RemotePort > 0 {
		port = fmt.Sprintf("%d", x.RemotePort)
	}
	return fmt.Sprintf("%s:%s (%d paths)", host, port, len(x.Paths))
}

// exposeStatuses reports what this extension published, for the state the UI and the
// agent read.
func (st *extState) exposeStatuses() []ForwardStatus {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]ForwardStatus(nil), st.exposes...)
}
