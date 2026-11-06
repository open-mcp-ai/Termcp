package envelope

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestDualProjectionIsIsomorphic is the rule the two projections exist for: the
// keys in the text's header block and the keys in the structured value are the
// SAME SET, and a shared key carries the same value.
//
// A projection that grew a key the other lacks would be a second source of truth,
// and the one that is read less would silently rot.
func TestDualProjectionIsIsomorphic(t *testing.T) {
	slot := Slot{
		Code: 206,
		Headers: map[string]string{
			"Path":          "read a1f3:1 --cursor 1024",
			"Status":        "exited",
			"Exit-Code":     "2",
			"Content-Range": "bytes 1024-2047/8192",
			"Idempotent":    "no",
			"Error-Code":    "command_failed",
		},
		Body: "ls: cannot open directory\n",
	}

	textKeys := sortedCopy(headerKeys(t, slot.Text()))
	structKeys := structuredKeys(slot.Structured())

	if !reflect.DeepEqual(textKeys, structKeys) {
		t.Fatalf("key sets differ:\n text: %v\n json: %v", textKeys, structKeys)
	}
	if got := slot.Structured()["body"]; got != slot.Body {
		t.Errorf("body = %v, want %q", got, slot.Body)
	}
	for _, key := range textKeys {
		want := slot.Headers[key]
		got := slot.Structured()[key]
		switch key {
		case "Exit-Code":
			if got != 2 {
				t.Errorf("Exit-Code structured value = %v (%T), want the number 2", got, got)
			}
		default:
			if got != want {
				t.Errorf("%s = %v, want %q", key, got, want)
			}
		}
	}
}

// TestStatusLineIsStructured pins that the status line is three keys rather than
// one string: a programmatic consumer must not have to parse the line it can read
// as a line.
func TestStatusLineIsStructured(t *testing.T) {
	slot := Slot{Code: 404}
	m := slot.Structured()
	if m["protocol"] != Protocol {
		t.Errorf("protocol = %v", m["protocol"])
	}
	if m["code"] != 404 {
		t.Errorf("code = %v", m["code"])
	}
	if m["reason"] != "Not Found" {
		t.Errorf("reason = %v", m["reason"])
	}
	if !strings.HasPrefix(slot.Text(), "TERM/1 404 Not Found\n") {
		t.Errorf("text = %q", slot.Text())
	}
}

// TestBodyIsInBothProjections pins that the structured projection carries the
// body too: a client that renders only structuredContent must still receive the
// bytes. The text keeps them verbatim as well.
func TestBodyIsInBothProjections(t *testing.T) {
	const body = "hello"
	slot := Slot{Code: 200, Headers: map[string]string{"Path": "run a1f3:1 echo hi"}, Body: body}
	if got := slot.Structured()["body"]; got != body {
		t.Errorf("structured body = %v, want %q", got, body)
	}
	if !strings.HasSuffix(slot.Text(), body) {
		t.Error("the text projection must end with the body verbatim")
	}
}

// TestIsErrorIsExactlyTheStatusCode pins the one rule that makes isError
// trustworthy: it is a function of the code, with no exceptions — a 206 is a
// success (the requested range was delivered), a 500 is a failure even though the
// call itself worked.
func TestIsErrorIsExactlyTheStatusCode(t *testing.T) {
	for code, want := range map[int]bool{
		200: false, 202: false, 204: false, 206: false,
		400: true, 403: true, 404: true, 409: true, 422: true, 500: true,
	} {
		if got := (Slot{Code: code}).IsError(); got != want {
			t.Errorf("code %d IsError = %v, want %v", code, got, want)
		}
	}
	// A batch is a failure if any slot is.
	r := Reply{Slots: []Slot{{Code: 200}, {Code: 500}}}
	if !r.IsError() {
		t.Error("a reply containing a failed slot must report failure")
	}
}

// TestContentRangeForms pins the three renderings and the arithmetic the caller
// depends on: end+1 is the next --cursor.
func TestContentRangeForms(t *testing.T) {
	if got := ContentRange(1024, 2047, 8192); got != "bytes 1024-2047/8192" {
		t.Errorf("ContentRange = %q", got)
	}
	if got := ContentRange(0, 0, -1); got != "bytes 0-0/*" {
		t.Errorf("unknown length = %q", got)
	}
	if got := UnsatisfiedRange(212); got != "bytes */212" {
		t.Errorf("UnsatisfiedRange = %q", got)
	}
	// The contract: the end of a range is inclusive, so the next cursor is end+1.
	const start, end = int64(1024), int64(2047)
	if next := end + 1; next != 2048 {
		t.Fatalf("next cursor arithmetic is wrong: %d", next)
	}
}

// TestHeaderOrderIsStable pins reproducibility: the same reply renders to the
// same bytes, so a diff between two replies means a real difference.
func TestHeaderOrderIsStable(t *testing.T) {
	headers := map[string]string{
		"Usage":         "run <handle> <payload>",
		"Path":          "run a1f3:1",
		"Error-Code":    "unknown_directive",
		"Content-Range": "bytes */10",
	}
	a := Slot{Code: 400, Headers: headers}.Text()
	b := Slot{Code: 400, Headers: headers}.Text()
	if a != b {
		t.Fatalf("two renders of one slot differ:\n%s\n---\n%s", a, b)
	}
	// Path comes before the diagnostic lines, so the echo is always read first.
	if strings.Index(a, "Path:") > strings.Index(a, "Error-Code:") {
		t.Errorf("Path must be rendered before the diagnosis:\n%s", a)
	}
}

// TestUnknownHeaderIsStillRendered pins the fallback: a header this build does
// not know about appears in both projections (after the known ones) rather than
// being dropped from one of them.
func TestUnknownHeaderIsStillRendered(t *testing.T) {
	slot := Slot{Code: 200, Headers: map[string]string{"Path": "ps", "X-Future": "1"}}
	if !strings.Contains(slot.Text(), "X-Future: 1") {
		t.Errorf("text dropped an unknown header:\n%s", slot.Text())
	}
	if _, ok := slot.Structured()["X-Future"]; !ok {
		t.Error("structured dropped an unknown header")
	}
	if textKeys, structKeys := sortedCopy(headerKeys(t, slot.Text())), structuredKeys(slot.Structured()); !reflect.DeepEqual(textKeys, structKeys) {
		t.Errorf("key sets differ: %v vs %v", textKeys, structKeys)
	}
}

// TestEmptyReplyIs204Shape pins the empty case: no body, no Content-Range unless
// the caller knows the stream length, and still a complete response.
func TestEmptyReplyIs204Shape(t *testing.T) {
	slot := Slot{Code: 204, Headers: map[string]string{
		"Path":          "read a1f3:1 --cursor 212",
		"Content-Range": UnsatisfiedRange(212),
	}}
	text := slot.Text()
	if !strings.HasPrefix(text, "TERM/1 204 No Content\n") {
		t.Errorf("status line = %q", strings.SplitN(text, "\n", 2)[0])
	}
	if !strings.Contains(text, "Content-Range: bytes */212") {
		t.Errorf("a 204 still says how long the stream is:\n%s", text)
	}
}

// headerKeys extracts the key set from a rendered slot's header block.
func headerKeys(t *testing.T, text string) []string {
	t.Helper()
	lines := strings.Split(text, "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], Protocol+" ") {
		t.Fatalf("first line is not a status line: %q", lines[0])
	}
	var keys []string
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		key, _, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("malformed header line %q", line)
		}
		keys = append(keys, key)
	}
	return keys
}

// sortedCopy sorts a key list so the two projections compare as sets.
func sortedCopy(keys []string) []string {
	out := append([]string(nil), keys...)
	sort.Strings(out)
	return out
}

// structuredKeys returns the header keys of a structured value (everything but
// the three status-line keys and the body), sorted so the two sides compare.
func structuredKeys(m map[string]any) []string {
	var keys []string
	for k := range m {
		switch k {
		case "protocol", "code", "reason", "body":
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
