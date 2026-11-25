package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/open-mcp-ai/termcp/mcp"
)

// The human-facing documentation surface (#355): one read-only tool, one read-only
// resource namespace, and ONE copy of the text (guide_text.go).
//
// WHY THIS EXISTS AT ALL. The `term` surface documents itself for a CALLER — `help`
// lists every directive and its flags — and that is the wrong material to read to the
// person who owns the machine: they need to know that a host comes before a connect,
// that an empty `ls ext` is normal, and which step is theirs rather than the agent's.
// Without it every assistant improvised the same walkthrough in different words, and a
// wrong guess had nothing to correct it.
//
// WHY IT IS A SEPARATE TOOL RATHER THAN A `term` ARGUMENT. `help` stays the machine
// view of the command language; this is the human view of the same world. Merging them
// means either burying the walkthrough as one more `help` page — where the caller that
// needs it never looks, because it is already reading a directive table — or
// lengthening the command space's own summary for a reader who is not configuring
// anything. Two audiences, two entry points, one material.
//
// WHY RESOURCES, AND ONLY THESE. #327's "not doing" list still holds for arbitrary
// resources and for `resources/subscribe`: there is no state here to push to a client,
// so subscribing would be a promise nothing can keep. What #355 adds is narrower — this
// static material, addressable at a URI, so an MCP client can offer it in its own UI
// without spending a tool call. Only termcp://guide/* is served; any other URI is
// refused with the list, because the honest answer to "GET /etc/passwd" is not an empty
// document.

const (
	guideToolName = "guide"
	// guideURIPrefix is the only resource namespace this server publishes.
	guideURIPrefix = "termcp://guide/"
	// guideMIME is honest rather than aspirational: the pages are written to be read
	// as they are — in a terminal, a chat message or a file — with no markup to
	// render and no link to resolve.
	guideMIME = "text/plain"
	// guideBudgetBytes is the ceiling for ONE page (#355: "about 4 KB"). The first-run
	// page is pasted into a conversation when a person asks "so what now?", and a
	// manual is not an answer.
	guideBudgetBytes = 4096
)

// registerGuide publishes the guide tool and its resources. It runs from New()
// because the material is static: unlike the docs prompt (docs.go) it needs no origin
// and no base URL, so a client gets the same pages in an in-process test as in a
// daemon.
func registerGuide(mcpServer *mcp.Server, s *Server) {
	mcpServer.AddTool(guideTool(), withLogging(guideToolName, s.handleGuide, s))
	// Registered through the protocol layer's escape hatch: mcp/ models tools and
	// prompts, and resources are served by the surface that has them. BOTH methods are
	// registered together — a listing whose URIs cannot be read is worse than no
	// listing, because a client would build a menu of dead entries.
	mcpServer.OnMethod("resources/list", s.handleResourcesList)
	mcpServer.OnMethod("resources/read", s.handleResourcesRead)
}

// guideTool is the descriptor. The topic is optional and enumerated: a model that
// reads the schema learns the page names without a call, which is the whole point of
// publishing them.
func guideTool() mcp.Tool {
	readOnly := true
	return mcp.NewTool(guideToolName,
		mcp.WithDescription("Onboarding material for a PERSON: what to configure next, why each step exists, and which steps are theirs rather than yours. No argument returns the first-run page (under 4 KB); topic returns one subject. `help` inside term is the machine view of the command language; this is the human one. Call it when someone asks how to start or what to do next, and read it to them."),
		mcp.WithString("topic",
			mcp.Description("One subject. Omit for the first-run page, which is the same material as termcp://guide/start."),
			mcp.Enum(guideTopicIDs()...),
		),
		mcp.WithToolAnnotation(mcp.ToolAnnotation{Title: "Guide for a person", ReadOnlyHint: &readOnly}),
	)
}

// handleGuide answers one tools/call.
//
// An unknown topic is a TOOL error carrying the list, not a protocol error: the call
// reached the tool, the tool read the argument and the caller needs the vocabulary
// back, which is information rather than a failure of framing.
func (s *Server) handleGuide(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	topic := ""
	if args := request.GetArguments(); args != nil {
		if v, ok := args["topic"]; ok && v != nil {
			text, isString := v.(string)
			if !isString {
				return mcp.NewToolResultError("topic must be a string; the topics are: " + guideTopicList()), nil
			}
			topic = strings.TrimSpace(text)
		}
	}
	page, ok := guidePageText(topic)
	if !ok {
		return mcp.NewToolResultError("unknown topic " + quoteOrDash(topic) +
			". The topics are: " + guideTopicList() +
			". Omit topic for the first-run page (the same material as " + guideURIPrefix + guideStartID + ")."), nil
	}
	noteGuideBudget(ctx, topic, page)
	return mcp.NewToolResultText(page), nil
}

// guideResource is one entry of resources/list.
//
// The shape is declared here because mcp/ deliberately does not model resources (see
// its package comment): this is the adapter layer, and the fields carry the spec's own
// names so a client needs no translation.
type guideResource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// guideResourceContents is one entry of a resources/read answer.
type guideResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text"`
}

// codeResourceNotFound is the specification's error code for a URI this server does
// not serve. mcp/ names only the JSON-RPC core codes, and adding one there would be a
// change to a package another line is editing; the number is the protocol's, not this
// file's invention.
const codeResourceNotFound = -32002

// handleResourcesList answers resources/list.
//
// The params carry only an optional cursor, and there are six pages, so there is no
// paging: the whole list is the answer, and a client that sends a cursor gets the same
// list without a nextCursor rather than an error.
func (s *Server) handleResourcesList(context.Context, json.RawMessage) (any, *mcp.RPCError) {
	res := make([]guideResource, 0, len(guideTopics)+1)
	res = append(res, guideResource{
		URI:         guideURIPrefix + guideStartID,
		Name:        guideToolName + ":" + guideStartID,
		Description: "First-run guide: what to configure next, and which steps are the person's.",
		MimeType:    guideMIME,
	})
	for _, t := range guideTopics {
		res = append(res, guideResource{
			URI:         guideURIPrefix + t.ID,
			Name:        guideToolName + ":" + t.ID,
			Description: t.Summary,
			MimeType:    guideMIME,
		})
	}
	return struct {
		Resources []guideResource `json:"resources"`
	}{Resources: res}, nil
}

// handleResourcesRead answers resources/read: the same bytes the `guide` tool returns
// for that topic, which is what makes the two entrances one material rather than two
// copies of it.
func (s *Server) handleResourcesRead(ctx context.Context, params json.RawMessage) (any, *mcp.RPCError) {
	var p struct {
		URI string `json:"uri"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &mcp.RPCError{Code: mcp.CodeInvalidParams, Message: "invalid resources/read params: " + err.Error()}
		}
	}
	uri := strings.TrimSpace(p.URI)
	if uri == "" {
		return nil, &mcp.RPCError{Code: mcp.CodeInvalidParams,
			Message: "resources/read needs a uri; this server publishes " + guideURIList()}
	}
	topic, served := strings.CutPrefix(uri, guideURIPrefix)
	page, ok := guidePageText(topic)
	if !served || !ok {
		return nil, &mcp.RPCError{Code: codeResourceNotFound,
			Message: "unknown resource " + uri + "; this server publishes only " + guideURIList()}
	}
	noteGuideBudget(ctx, topic, page)
	return struct {
		Contents []guideResourceContents `json:"contents"`
	}{Contents: []guideResourceContents{{
		URI:      guideURIPrefix + topicOrStart(topic),
		MimeType: guideMIME,
		Text:     page,
	}}}, nil
}

// topicOrStart canonicalises an empty or "start" id, so the answer's URI is the one a
// client can ask for again.
func topicOrStart(topic string) string {
	if t := strings.TrimSpace(topic); t != "" && t != guideStartID {
		return t
	}
	return guideStartID
}

// guideURIList names every served URI, in listing order.
func guideURIList() string {
	uris := make([]string, 0, len(guideTopics)+1)
	uris = append(uris, guideURIPrefix+guideStartID)
	for _, t := range guideTopics {
		uris = append(uris, guideURIPrefix+t.ID)
	}
	return strings.Join(uris, ", ")
}

// noteGuideBudget reports a page that grew past the budget.
//
// IT WARNS INSTEAD OF TRUNCATING: a walkthrough cut mid-sentence is worse than a long
// one, and the person reading it cannot tell that a step is missing. The defect is a
// build-time mistake (someone added a paragraph), so it is reported where the operator
// will see it rather than silently shipped.
func noteGuideBudget(ctx context.Context, topic, page string) {
	if n := len(page); n > guideBudgetBytes {
		slog.WarnContext(ctx, "guide page over budget",
			"topic", topicOrStart(topic), "bytes", n, "budget", guideBudgetBytes)
	}
}
