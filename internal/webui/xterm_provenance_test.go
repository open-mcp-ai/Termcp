package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vendored xterm.js bundle is one minified line of third-party code, and
// that single line is what stops an audit of this repository dead: 488KB of
// `!function(e,t){...}` looks the same whether it is upstream build output or
// something we wrote to hide in. Nothing we can write *inside* the file helps —
// annotating it would destroy the one property that makes it trustworthy, which
// is that it is byte-for-byte what npm publishes.
//
// So the provenance lives outside the bytes and this test keeps the two
// together: the same two hashes appear in docs/xterm.md, in THIRD_PARTY_NOTICES.md
// and in third_party/xterm/README.md, and the assertion is made against what the
// server actually serves — the bytes that ship — so an upgrade that forgets one
// copy fails here instead of silently misdescribing the software.
//
// It also pins the arrangement, which is the part a tidy-up would remove:
//
//   - third_party/xterm/ holds the two runtime files AND the two that explain
//     them (README.md provenance, NOTICE licence);
//   - embed.go embeds the files BY NAME, so only the two ship;
//   - the served paths are unchanged (static/xterm/…), because index.html
//     references them and a 404 there is a blank page with no Go error.
func TestVendoredXtermProvenance(t *testing.T) {
	if !Embedded() {
		// The pure-API build (`-tags no_webui`) embeds only the agent documents,
		// so there is no bundle here to hash.
		t.Skip("no_webui build: the browser UI and its vendored xterm are not in this binary")
	}
	const (
		wantJS  = "14903579ff54664cd72f8e8699e6961a6272c21863ec1c3b118cdc8af5d4a972"
		wantCSS = "854a7c0fb70e8b1a083c16797ab827299fb18744f5ad34f227b48337e33293c6"
	)
	for _, c := range []struct{ served, want string }{
		{"static/xterm/xterm.js", wantJS},
		{"static/xterm/xterm.css", wantCSS},
	} {
		b, err := fs.ReadFile(Assets(), c.served)
		if err != nil {
			t.Fatalf("the served assets lost %s: %v (index.html references it by that exact path)", c.served, err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != c.want {
			t.Errorf("the served %s is not the vendored upstream build:\n  got  %s\n  want %s\n"+
				"If this was an intentional upgrade, update the hash here, in docs/xterm.md, in "+
				"THIRD_PARTY_NOTICES.md and in third_party/xterm/README.md — all four describe one artifact",
				c.served, got, c.want)
		}
	}

	// The files that describe the artifact must NOT ship. This is the whole
	// reason embed.go names two files instead of embedding its directory, and it
	// is what makes a plain README.md / NOTICE possible next to the bytes.
	for _, name := range []string{"static/xterm/NOTICE", "static/xterm/README.md"} {
		if _, err := fs.Stat(Assets(), name); err == nil {
			t.Errorf("%s is served/embedded; the licence and the provenance belong in the repository only — "+
				"they ship through the release assets and the container image, not inside the binary", name)
		}
	}

	// Every human-readable copy of the provenance has to say the same thing, or
	// the cheapest one to read becomes the one that lies. The licence carries no
	// hashes — it is the licence — and embed.go pins the embed list instead.
	hashFiles := map[string]string{
		"../../docs/xterm.md":               "the upgrade procedure",
		"../../THIRD_PARTY_NOTICES.md":      "the repository-level attribution",
		"../../third_party/xterm/README.md": "the sidecar beside the vendored files",
	}
	for path, what := range hashFiles {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s (%s) is missing: %v", path, what, err)
		}
		for _, want := range []string{wantJS, wantCSS} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s (%s) does not record the vendored hash %s; there must be one hash, in several places, not several hashes",
					path, what, want[:12])
			}
		}
	}

	// embed.go must name the two runtime files, and the notice must be the MIT
	// text: a licence that says nothing is not a licence.
	embed, err := os.ReadFile("../../third_party/xterm/embed.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(embed), "//go:embed xterm.js xterm.css") {
		t.Error("third_party/xterm/embed.go no longer embeds exactly xterm.js and xterm.css by name; " +
			"embedding the directory would put NOTICE and README.md back into the binary")
	}
	notice, err := os.ReadFile("../../third_party/xterm/NOTICE")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"The xterm.js authors",
		"Permission is hereby granted, free of charge",
		`THE SOFTWARE IS PROVIDED "AS IS"`,
	} {
		if !strings.Contains(string(notice), want) {
			t.Errorf("third_party/xterm/NOTICE does not contain %q", want)
		}
	}

	// GitHub is where the bundle is read by eye, and `.gitattributes` is the only
	// thing that stops linguist counting it as hand-written termcp source.
	attrs, err := os.ReadFile(filepath.Join("..", "..", ".gitattributes"))
	if err == nil {
		for _, want := range []string{
			"third_party/xterm/xterm.js   linguist-vendored linguist-generated=true",
			"third_party/xterm/xterm.css  linguist-vendored linguist-generated=true",
		} {
			if !strings.Contains(string(attrs), want) {
				t.Errorf(".gitattributes no longer marks %q; without it the 488KB upstream bundle reads as this project's own single-line JavaScript", strings.Fields(want)[0])
			}
		}
	}
}
