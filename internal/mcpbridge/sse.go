// Package mcpbridge relays MCP JSON-RPC messages between a stdio client and an
// MCP HTTP endpoint: termcp's streamable-HTTP /stream (the default) or its SSE
// transport (/sse), or any other MCP server speaking either one.
//
// It is deliberately a dumb relay: every stdin line is POSTed verbatim, every
// reply is written to stdout verbatim, and no MCP semantics are interpreted —
// the client sees exactly what the server answers. stdout therefore carries
// nothing but MCP messages; diagnostics go to stderr.
package mcpbridge

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// readEvents parses one SSE stream, invoking onEvent once per event with the
// event name ("" when unnamed) and the joined data payload. ReadBytes, not
// bufio.Scanner: a single MCP message can far exceed any fixed line limit.
func readEvents(r io.Reader, onEvent func(event string, data []byte)) error {
	br := bufio.NewReader(r)
	var event string
	var data []byte
	dispatch := func() {
		if data != nil {
			onEvent(event, data)
		}
		event, data = "", nil
	}
	for {
		line, err := br.ReadBytes('\n')
		trimmed := bytes.TrimRight(line, "\r\n")
		switch {
		case len(trimmed) == 0:
			dispatch()
		case bytes.HasPrefix(trimmed, []byte("event:")):
			event = string(bytes.TrimSpace(trimmed[len("event:"):]))
		case bytes.HasPrefix(trimmed, []byte("data:")):
			payload := bytes.TrimPrefix(trimmed[len("data:"):], []byte(" "))
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, payload...)
		}
		if err == nil {
			continue
		}
		dispatch()
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("reading event stream: %w", err)
	}
}

// startServerStream opens the listening GET stream once a session id exists:
// it is the carrier for server-initiated messages (notifications, sampling
// requests). Opening it before initialize would make mcp-go mint a second
// session instead of attaching to this one.
func (b *bridge) startServerStream(sessionID string) {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	if b.sessionID != "" {
		return
	}
	b.sessionID = sessionID
	go b.readServerStream(sessionID)
}

func (b *bridge) readServerStream(sessionID string) {
	req, err := http.NewRequestWithContext(b.ctx, http.MethodGet, b.cfg.URL, nil)
	if err != nil {
		b.fail(nil, fmt.Sprintf("GET %s: %v", b.cfg.URL, err))
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(sessionHeader, sessionID)
	b.authorize(req)
	resp, err := b.client.Do(req)
	if err != nil {
		if b.ctx.Err() != nil {
			return // the bridge is shutting down, not failing
		}
		b.fail(nil, fmt.Sprintf("GET %s: %v", b.cfg.URL, err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.fail(nil, fmt.Sprintf("GET %s: the server answered %s", b.cfg.URL, resp.Status))
		return
	}
	err = b.relaySSE(resp.Body)
	if b.ctx.Err() != nil {
		return // shutdown unblocked the stream; not a failure
	}
	if err != nil {
		b.fail(nil, err.Error())
		return
	}
	b.fail(nil, fmt.Sprintf("GET %s: the server closed the event stream", b.cfg.URL))
}

// runSSE serves the SSE transport: one GET event stream carries everything.
// Its first `endpoint` event names where messages are POSTed; from then on
// stdin lines are relayed to that URL, and every `message` event on the
// stream — replies and server-initiated messages alike — goes to stdout.
func (b *bridge) runSSE(readDone chan error) {
	req, err := http.NewRequestWithContext(b.ctx, http.MethodGet, b.cfg.URL, nil)
	if err != nil {
		b.fail(nil, fmt.Sprintf("GET %s: %v", b.cfg.URL, err))
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	b.authorize(req)
	resp, err := b.client.Do(req)
	if err != nil {
		if b.ctx.Err() != nil {
			return // the bridge is shutting down, not failing
		}
		b.fail(nil, fmt.Sprintf("GET %s: %v", b.cfg.URL, err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.fail(nil, fmt.Sprintf("GET %s: the server answered %s", b.cfg.URL, resp.Status))
		return
	}
	err = readEvents(resp.Body, func(event string, data []byte) {
		switch event {
		case "endpoint":
			b.startSSEReader(data, readDone)
		case "", "message":
			b.writeOut(data) // before replySaw: the reply must be out before EOF may finish
			b.replySaw(data)
		}
	})
	if b.ctx.Err() != nil {
		return // shutdown unblocked the stream; not a failure
	}
	if err != nil {
		b.fail(nil, err.Error())
		return
	}
	b.fail(nil, fmt.Sprintf("GET %s: the server closed the event stream", b.cfg.URL))
}

// startSSEReader records the message endpoint from the stream's endpoint event
// and starts relaying stdin to it (once; later endpoint events are ignored).
func (b *bridge) startSSEReader(endpoint []byte, readDone chan error) {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	if b.messageURL != "" {
		return
	}
	b.messageURL = resolveEndpoint(b.cfg.URL, strings.TrimSpace(string(endpoint)))
	go func() { readDone <- b.readStdin() }()
}
