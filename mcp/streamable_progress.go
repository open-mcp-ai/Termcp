package mcp

import (
	"context"
	"encoding/json"
	"net/http"
)

// THE OTHER HALF OF PROGRESS: a POST that can stream before it answers.
//
// notifications/progress has no route out of this transport on its own. A POST is
// one request and one JSON answer, so a server that wants to say "still working" has
// nowhere to put the sentence — the answer is the last thing that happens, and by
// then the information is worthless. The specification's answer is that the RESPONSE
// may be text/event-stream: the progress frames ride down it, and the result is the
// final frame on the same stream.
//
// SO THE DECISION IS THE CLIENT'S, made by sending `_meta.progressToken`. A caller that
// asks to watch gets a stream and may stop reading at any moment (the answer arrives
// anyway, and the frames it missed are the ones it chose not to read); a caller that
// does not ask gets exactly the JSON answer it got before, byte for byte. Nothing
// changes for a client that never mentions progress.
//
// WRITES HAPPEN ON ONE GOROUTINE. The handler runs in its own goroutine and hands
// frames to a channel; this one drains it. A progress frame and the final answer
// therefore cannot interleave mid-line, which is the corruption an http.ResponseWriter
// shared between goroutines produces.

// progressStreamQueue is how many frames may be waiting before a send is dropped.
//
// DROPPING, NOT BLOCKING: progress is advisory, and a client that stopped reading must
// not be able to stall the tool it is watching. Sixteen frames is several seconds of
// ordinary reporting, so a client that is keeping up never loses one.
const progressStreamQueue = 16

// progressTokenOf reports the progress token of a tools/call frame, if it has one.
//
// It reads the RAW frame rather than the parsed request because the decision has to be
// made before dispatch: the answer's content type is chosen here, and the handler that
// would have parsed the token runs later.
func progressTokenOf(body []byte) (json.RawMessage, bool) {
	var probe struct {
		Method string `json:"method"`
		Params struct {
			Meta struct {
				Token json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, false
	}
	if probe.Method != "tools/call" {
		return nil, false
	}
	tok := probe.Params.Meta.Token
	if len(tok) == 0 || string(tok) == "null" {
		return nil, false
	}
	return tok, true
}

// serveWithProgress runs one tools/call as a stream: progress frames while it works,
// the ordinary answer last.
func (s *StreamableHTTPServer) serveWithProgress(ctx context.Context, w http.ResponseWriter, sess *streamClient, body []byte, token json.RawMessage) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// A writer that cannot flush cannot stream. Answering with an error would
		// refuse a call the tool could have run, so the call runs and the reply is the
		// plain JSON one — progress is the optional half, not the request.
		s.servePlain(ctx, w, sess, body)
		return
	}
	frames := make(chan []byte, progressStreamQueue)
	sink := &progressSink{
		token: token,
		send: func(f ProgressFrame) {
			frame, err := json.Marshal(Notification{JSONRPC: "2.0", Method: "notifications/progress", Params: f})
			if err != nil {
				return
			}
			select {
			case frames <- frame:
			default:
				// The client is not keeping up. A dropped progress frame is a fact the
				// client can live with; a blocked tool is not.
			}
		},
	}
	pctx := context.WithValue(ctx, progressKey{}, sink)

	go func() {
		answer := s.srv.Handle(WithSessionContext(pctx, sess), body, sess)
		if answer != nil {
			// THE RESULT GOES THROUGH THE SAME QUEUE, so it cannot overtake a progress
			// frame written just before the tool returned. It is blocking: the answer is
			// not optional, and a full queue here means the client has to read to get
			// its result rather than losing it.
			frames <- answer
		}
		close(frames)
	}()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Set(versionHeader, ProtocolVersion)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for frame := range frames {
		if err := writeSSEFrame(w, flusher, "message", frame); err != nil {
			// The client went away mid-call. The handler's ctx is this request's ctx,
			// so it has already been cancelled; reading the rest would only spin.
			return
		}
	}
}

// servePlain is the JSON answer this transport has always given.
func (s *StreamableHTTPServer) servePlain(ctx context.Context, w http.ResponseWriter, sess *streamClient, body []byte) {
	answer := s.srv.Handle(WithSessionContext(ctx, sess), body, sess)
	if answer == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(versionHeader, ProtocolVersion)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(answer)
}
