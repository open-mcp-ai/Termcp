package term

import "testing"

// TestAnEmbedderCanDeclareAVerbatimPayload pins #297 from the outside in.
//
// `Op.PayloadSpan` was exported while its type was not, so the only value an embedder
// could put there was the zero one: declaring a payload that is a protocol frame — the
// thing this build's own `call` does — was impossible from outside the module, and
// `go doc` rendered the field as `PayloadSpan spanMode`, which says nothing about what
// to write. The capability existed and could not be asked for.
//
// A frame payload is where the difference is visible: under shell rules a reference
// beside a `{` is "a variable inside a shell line" and is refused, while under
// SpanVerbatim the value is spliced as the caller's own bytes — no shell quotes, which
// is what makes it JSON rather than a JSON-looking string.
func TestAnEmbedderCanDeclareAVerbatimPayload(t *testing.T) {
	g := NewGrammar([]Op{{
		Name: "frame", Kind: KindPayload, Target: true,
		PayloadSpan: SpanVerbatim,
		Usage:       "frame <peer> $1",
	}})
	f, err := New(g).Parse(`frame peer {"v":$1}`, Subst{Pos: []string{"2+3"}})
	if err != nil {
		t.Fatalf("a frame payload could not be declared from outside:\n%v", err)
	}
	if got, want := f.Statements[0].Payload, `{"v":2+3}`; got != want {
		t.Errorf("payload = %q, want %q (the value spliced as bytes, not as a shell word)", got, want)
	}
}

// TestTheDefaultPayloadSpanIsUnchanged is the compatibility half: an Op that says
// nothing keeps the shell-line reading, so nothing written before #297 moves.
func TestTheDefaultPayloadSpanIsUnchanged(t *testing.T) {
	g := NewGrammar([]Op{{
		Name: "say", Kind: KindPayload, Target: true,
		Usage: "say <peer> $1",
	}})
	op, ok := g.Lookup("say")
	if !ok {
		t.Fatal("say is not in the grammar")
	}
	if op.PayloadSpan != SpanCommandLine {
		t.Errorf("an undeclared PayloadSpan = %v, want SpanCommandLine", op.PayloadSpan)
	}
	// The shape that tells a shell line apart: a reference that IS the payload is
	// spliced, one beside other text is refused.
	if _, err := New(g).Parse(`say peer $1`, Subst{Pos: []string{"hi"}}); err != nil {
		t.Fatalf("a whole-payload reference was refused: %v", err)
	}
	if _, err := New(g).Parse(`say peer x-$1`, Subst{Pos: []string{"hi"}}); err == nil {
		t.Error("a reference mixed with other shell text was accepted under the default mode")
	}
}
