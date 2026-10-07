package locator

import (
	"strings"
	"testing"
)

// TestLooksLikeAgreesWithParse pins the property callers actually rely on:
// LooksLike decides whether a value is handed to Parse at all, so a value it
// claims must never be silently misrouted by Parse.
//
// The dangerous case is a session address classified as an entry, because the
// entry kind is what gets sent to the ssh_config profile store. A parse error is
// safe — the caller reports it — but answering "entry" for something written with
// a '#' would send a session id looking for a connection profile.
func TestLooksLikeAgreesWithParse(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		kind Kind
		sid  string
		idx  int
	}{
		{"termcp://#abc123", KindSession, "abc123", 0},
		{"termcp://#abc123:2", KindShell, "abc123", 2},
		{"#abc123", KindSession, "abc123", 0},
		{"#abc123:2", KindShell, "abc123", 2},
		{"#session-abc123", KindSession, "abc123", 0},
		{"termcp://#session-abc123", KindSession, "abc123", 0},
		{"#session-abc123:2", KindShell, "abc123", 2},
		// The scheme form with no '#' is a PROFILE name, not a session: entry
		// locators are the one shape whose whole meaning is "this is a profile".
		{"termcp://mac", KindEntry, "", 0},
		{"termcp://session-abc123", KindEntry, "", 0},
		{"internal", KindEntry, "", 0},
		{"session-foo", KindEntry, "", 0},
	} {
		p, err := Parse(tc.raw)
		if err != nil {
			t.Errorf("Parse(%q) failed: %v", tc.raw, err)
			continue
		}
		if p.Kind != tc.kind || p.SessionID != tc.sid || p.Index != tc.idx {
			t.Errorf("Parse(%q) = kind=%v sid=%q idx=%d, want kind=%v sid=%q idx=%d",
				tc.raw, p.Kind, p.SessionID, p.Index, tc.kind, tc.sid, tc.idx)
		}
	}

	// LooksLike claims exactly the two locator syntaxes. A bare name is left
	// alone so it can be a raw id or a profile name: they share one namespace, and
	// claiming it would route a legitimately-named profile through the parser.
	for _, raw := range []string{"termcp://mac", "termcp://#abc", "#abc", "#abc:2", "termcp://session-x"} {
		if !LooksLike(raw) {
			t.Errorf("LooksLike(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{"abc123def", "mac", "internal", "my-profile", "session-foo"} {
		if LooksLike(raw) {
			t.Errorf("LooksLike(%q) = true for a bare name", raw)
		}
	}
}

func TestParseResourceURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantOK  bool
		wantErr string // substring
		kind    Kind
		entry   string
		sid     string
		index   int
	}{
		// Entry locators
		{"entry bare", "mac", true, "", KindEntry, "mac", "", 0},
		{"entry scheme", "termcp://mac", true, "", KindEntry, "mac", "", 0},
		{"entry internal", "termcp://internal", true, "", KindEntry, "internal", "", 0},
		{"entry trailing slash", "termcp://mac/", true, "", KindEntry, "mac", "", 0},

		// Session locators (short form preferred)
		{"session short", "#abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session scheme short", "termcp://#abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session scheme short slash", "termcp://#/abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session session-prefix", "#session-abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session scheme session-prefix", "termcp://#session-abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session long form", "termcp://mac#abc123def", true, "", KindSession, "", "abc123def", 0},
		{"session long form session-prefix", "termcp://mac#session-abc123def", true, "", KindSession, "", "abc123def", 0},

		// Shell locators
		{"shell short index", "#abc123def:2", true, "", KindShell, "", "abc123def", 2},
		{"shell short index 1", "#abc123def:1", true, "", KindShell, "", "abc123def", 1},
		{"shell scheme short index", "termcp://#abc123def:3", true, "", KindShell, "", "abc123def", 3},
		{"shell long form index", "termcp://mac#abc123def:2", true, "", KindShell, "", "abc123def", 2},
		{"shell scheme short slash index", "termcp://#/abc123def:2", true, "", KindShell, "", "abc123def", 2},
		{"session prefix with index", "#session-abc123def:2", true, "", KindShell, "", "abc123def", 2},
		{"long form session prefix with index", "termcp://mac#session-abc123def:2", true, "", KindShell, "", "abc123def", 2},

		// A bare "session-<id>" has no '#' and no scheme; it is an entry name, not a
		// session, because profile names share the namespace (see LooksLike).
		{"bare session prefix is an entry", "session-abc123def", true, "", KindEntry, "session-abc123def", "", 0},

		// Errors
		{"empty", "", false, "empty", 0, "", "", 0},
		{"only hash", "#", false, "missing session id", 0, "", "", 0},
		{"only scheme", "termcp://", false, "malformed", 0, "", "", 0},
		{"notif uri", "termcp://shells/xyz", false, "notification broadcast URI", 0, "", "", 0},
		{"bad index zero", "#abc:0", false, "invalid shell index", 0, "", "", 0},
		{"bad index neg", "#abc:-1", false, "invalid shell index", 0, "", "", 0},
		{"bad index non-num", "#abc:x", false, "invalid shell index", 0, "", "", 0},
		{"empty index", "#abc123def:", false, "invalid shell index", 0, "", "", 0},
		{"long form bad index", "termcp://mac#abc:0", false, "invalid shell index", 0, "", "", 0},
		{"entry with colon no hash", "termcp://mac:2", false, "malformed", 0, "", "", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Parse(tc.raw)
			if !tc.wantOK {
				if err == nil {
					t.Fatalf("expected error (contains %q), got nil: %+v", tc.wantErr, p)
				}
				if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want contains %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.Kind != tc.kind {
				t.Errorf("kind = %v, want %v", p.Kind, tc.kind)
			}
			if p.Entry != tc.entry {
				t.Errorf("entry = %q, want %q", p.Entry, tc.entry)
			}
			if p.SessionID != tc.sid {
				t.Errorf("sid = %q, want %q", p.SessionID, tc.sid)
			}
			if p.Index != tc.index {
				t.Errorf("index = %d, want %d", p.Index, tc.index)
			}
		})
	}
}
