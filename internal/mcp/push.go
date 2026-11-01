package mcp

import (
	"context"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Sampling passthrough: a sub-MCP borrows the host Agent's model.
//
// This is what `sampling/createMessage` is FOR. The protocol's own description is
// "the server asks the client's LLM to generate" — the direction is upstream, from
// the thing being driven to the thing driving it. A sub-MCP started on a shell
// channel (`pipe mac --argv npx -y …`, or an extension that runs a program) has
// no model of its own, so it asks its host; Termcp sits between them and forwards.
//
//	sub-MCP  ──sampling/createMessage──▶  Termcp  ──▶  Agent (MCP client)
//	sub-MCP  ◀────────result────────────  Termcp  ◀──  Agent
//
// Termcp adds no policy and inspects no prompt: it has no opinion about what the
// sub-MCP wants generated, and inventing one would make it a participant in a
// conversation it was only asked to carry.
//
// WHAT THIS REPLACED, and why the difference matters. This function used to be
// SendSamplingNotification, which issued a createMessage on a timer to WAKE the
// agent up. That is the wrong direction twice over: the protocol asks the client
// to GENERATE, not to be nudged, and a wake-up that costs a model call is a strange
// price for a notification. A notification channel is the right tool for a
// notification, and Termcp does not have one the client is obliged to listen on —
// which is why `watch` is gone rather than repaired.

// ForwardSampling proxies one createMessage request to the Agent that is driving
// this server, and returns its answer unchanged.
//
// target carries the client session because dispatch happens off a goroutine that
// has no request context to recover it from: the caller that received the sub-MCP's
// frame captures the session and hands it here.
//
// An error means one of exactly two things, and both are the caller's to report:
// the captured target is not a sampling-capable session (the Agent never declared
// the capability), or the Agent answered with an error.
func (s *Server) ForwardSampling(ctx context.Context, target any, req mcpgo.CreateMessageRequest) (*mcpgo.CreateMessageResult, error) {
	sess, ok := target.(mcpserver.SessionWithSampling)
	if !ok || sess == nil {
		return nil, fmt.Errorf("this Agent did not declare sampling capability, so a sub-MCP's createMessage cannot be forwarded")
	}
	return sess.RequestSampling(ctx, req)
}

// SetUINotifier wires the Web UI notification delivery end (notify_user tool).
func (s *Server) SetUINotifier(fn func(level, title, message, sessionID string, durationSec int) int) {
	s.uiNotify = fn
}
