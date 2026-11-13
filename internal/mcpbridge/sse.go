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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
	if b.sessionID != "" {
		b.sessMu.Unlock()
		return
	}
	b.sessionID = sessionID
	streamCtx, cancel := context.WithCancel(b.ctx)
	b.streamCancel = cancel
	b.sessMu.Unlock()
	go b.readServerStream(streamCtx, sessionID)
}

func (b *bridge) readServerStream(ctx context.Context, sessionID string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.URL, nil)
	if err != nil {
		b.fail(nil, fmt.Sprintf("GET %s: %v", b.cfg.URL, err))
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(sessionHeader, sessionID)
	b.authorize(req)
	resp, err := b.client.Do(req)
	if err != nil {
		if b.ctx.Err() != nil || ctx.Err() != nil {
			return // the bridge is shutting down, not failing
		}
		if recoveryErr := b.recoverSession(sessionID); recoveryErr != nil {
			b.fail(nil, fmt.Sprintf("GET %s: %v; reinitialization failed: %v", b.cfg.URL, err, recoveryErr))
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			if recoveryErr := b.recoverSession(sessionID); recoveryErr != nil {
				b.fail(nil, fmt.Sprintf("GET %s: the server answered %s; reinitialization failed: %v", b.cfg.URL, resp.Status, recoveryErr))
			}
			return
		}
		b.fail(nil, fmt.Sprintf("GET %s: the server answered %s", b.cfg.URL, resp.Status))
		return
	}
	err = b.relaySSE(resp.Body)
	if b.ctx.Err() != nil || ctx.Err() != nil {
		return // shutdown unblocked the stream; not a failure
	}
	if err != nil {
		if recoveryErr := b.recoverSession(sessionID); recoveryErr != nil {
			b.fail(nil, fmt.Sprintf("%v; reinitialization failed: %v", err, recoveryErr))
		}
		return
	}
	if recoveryErr := b.recoverSession(sessionID); recoveryErr != nil {
		b.fail(nil, fmt.Sprintf("GET %s: the server closed the event stream; reinitialization failed: %v", b.cfg.URL, recoveryErr))
	}
}

// runSSE serves the SSE transport: one GET event stream carries everything.
// Its first `endpoint` event names where messages are POSTed; from then on
// stdin lines are relayed to that URL, and every `message` event on the
// stream — replies and server-initiated messages alike — goes to stdout.
func (b *bridge) runSSE(readDone chan error) {
	connected := false
	reconnects := 0
	for {
		if reconnects > 0 {
			timer := time.NewTimer(time.Duration(reconnects) * 150 * time.Millisecond)
			select {
			case <-timer.C:
			case <-b.ctx.Done():
				timer.Stop()
				return
			}
		}
		streamCtx, cancelStream := context.WithCancel(b.ctx)
		b.setSSEStreamCancel(cancelStream)
		req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, b.cfg.URL, nil)
		if err != nil {
			cancelStream()
			b.fail(nil, fmt.Sprintf("GET %s: %v", b.cfg.URL, err))
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		b.authorize(req)
		resp, err := b.client.Do(req)
		if err != nil {
			cancelStream()
			b.clearSSEStreamCancel()
			if b.ctx.Err() != nil {
				return
			}
			if !connected || reconnects >= recoveryAttempts {
				b.fail(nil, fmt.Sprintf("GET %s: %v", b.cfg.URL, err))
				return
			}
			b.beginSSERecovery()
			reconnects++
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			cancelStream()
			b.clearSSEStreamCancel()
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || !connected || reconnects >= recoveryAttempts {
				b.fail(nil, fmt.Sprintf("GET %s: the server answered %s", b.cfg.URL, resp.Status))
				return
			}
			b.beginSSERecovery()
			reconnects++
			continue
		}

		endpointSeen := false
		recoveryReplies := make(chan []byte, 1)
		recovered := make(chan struct{}, 1)
		recoveryID := ""
		err = readEvents(resp.Body, func(event string, data []byte) {
			switch event {
			case "endpoint":
				if endpointSeen {
					return
				}
				endpointSeen = true
				connected = true
				target := resolveEndpoint(b.cfg.URL, strings.TrimSpace(string(data)))
				if b.sseIsRecovering() {
					recoveryID = fmt.Sprintf("%q", fmt.Sprintf("termcp-sse-recovery-%d", reconnects))
					b.setSSEMessageURL(target)
					go b.completeSSERecovery(target, recoveryID, recoveryReplies, recovered)
				} else {
					b.startSSEReader(target, readDone)
				}
			case "", "message":
				if recoveryID != "" && string(requestID(data)) == recoveryID {
					select {
					case recoveryReplies <- append([]byte(nil), data...):
					default:
					}
					return
				}
				b.writeOut(data) // before replySaw: the reply must be out before EOF may finish
				b.replySaw(data)
			}
		})
		resp.Body.Close()
		cancelStream()
		b.clearSSEStreamCancel()
		if b.ctx.Err() != nil {
			return
		}
		select {
		case <-recovered:
			reconnects = 0
		default:
		}
		b.beginSSERecovery()
		if !endpointSeen || reconnects >= recoveryAttempts {
			if err != nil {
				b.fail(nil, err.Error())
			} else {
				b.fail(nil, fmt.Sprintf("GET %s: the server closed the event stream", b.cfg.URL))
			}
			return
		}
		reconnects++
	}
}

func (b *bridge) startSSEReader(target string, readDone chan error) {
	b.sessMu.Lock()
	if b.readerStarted {
		b.sessMu.Unlock()
		return
	}
	b.messageURL = target
	b.readerStarted = true
	b.sessMu.Unlock()
	go func() { readDone <- b.readStdin() }()
}

func (b *bridge) setSSEMessageURL(target string) {
	b.sessMu.Lock()
	b.messageURL = target
	b.sessMu.Unlock()
}

func (b *bridge) sseIsRecovering() bool {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	return b.sseRecovering
}

func (b *bridge) beginSSERecovery() {
	b.sessMu.Lock()
	defer b.sessMu.Unlock()
	if !b.readerStarted {
		return
	}
	if !b.sseRecovering {
		b.sseRecovering = true
		b.sseRecoveryDone = make(chan struct{})
	}
	if b.sseStreamCancel != nil {
		b.sseStreamCancel()
		b.sseStreamCancel = nil
	}
}

func (b *bridge) waitSSERecovery(ctx context.Context) error {
	b.sessMu.Lock()
	recovering := b.sseRecovering
	done := b.sseRecoveryDone
	b.sessMu.Unlock()
	if !recovering {
		return nil
	}
	if done == nil {
		return errors.New("SSE recovery has no completion signal")
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *bridge) setSSEStreamCancel(cancel context.CancelFunc) {
	b.sessMu.Lock()
	b.sseStreamCancel = cancel
	b.sessMu.Unlock()
}

func (b *bridge) clearSSEStreamCancel() {
	b.sessMu.Lock()
	b.sseStreamCancel = nil
	b.sessMu.Unlock()
}

func (b *bridge) completeSSERecovery(target, recoveryID string, replies chan []byte, recovered chan struct{}) {
	initialize, initialized, ok := b.recoveryMessages()
	if !ok {
		b.fail(nil, "the bridge did not observe an initialize request to replay into a new SSE session")
		return
	}
	var envelope map[string]any
	if err := json.Unmarshal(initialize, &envelope); err != nil {
		b.fail(nil, fmt.Sprintf("cannot prepare SSE reinitialization: %v", err))
		return
	}
	envelope["id"] = strings.Trim(recoveryID, `"`)
	payload, err := json.Marshal(envelope)
	if err != nil {
		b.fail(nil, fmt.Sprintf("cannot prepare SSE reinitialization: %v", err))
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, recoveryTimeout)
	resp, err := b.postSSE(ctx, target, payload)
	if err != nil {
		cancel()
		b.fail(nil, fmt.Sprintf("POST %s during SSE reinitialization: %v", target, err))
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		cancel()
		b.fail(nil, fmt.Sprintf("POST %s during SSE reinitialization answered %s", target, resp.Status))
		return
	}
	cancel()

	select {
	case <-replies:
	case <-time.After(recoveryTimeout):
		b.fail(nil, "SSE reinitialization did not receive an initialize reply")
		return
	case <-b.ctx.Done():
		return
	}

	ctx, cancel = context.WithTimeout(b.ctx, recoveryTimeout)
	notify, err := b.postSSE(ctx, target, initialized)
	if err != nil {
		cancel()
		b.fail(nil, fmt.Sprintf("POST initialized notification during SSE reinitialization: %v", err))
		return
	}
	_, _ = io.Copy(io.Discard, notify.Body)
	notify.Body.Close()
	cancel()
	if notify.StatusCode != http.StatusOK && notify.StatusCode != http.StatusAccepted {
		b.fail(nil, fmt.Sprintf("initialized notification during SSE reinitialization answered %s", notify.Status))
		return
	}
	b.sessMu.Lock()
	b.messageURL = target
	b.sseRecovering = false
	if b.sseRecoveryDone != nil {
		close(b.sseRecoveryDone)
		b.sseRecoveryDone = nil
	}
	b.sessMu.Unlock()
	select {
	case recovered <- struct{}{}:
	default:
	}
}

func (b *bridge) postSSE(ctx context.Context, target string, msg []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	b.authorize(req)
	return b.client.Do(req)
}
