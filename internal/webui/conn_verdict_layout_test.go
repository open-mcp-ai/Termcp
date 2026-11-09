package webui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Test verdict has to land inside the dialog's visible box, in the case that
// made it invisible: a form taller than the dialog.
//
// The defect was placement, so a source-text assertion cannot see it — the
// markup looked fine and so did the CSS. The numbers that made it invisible were
// geometric: the verdict was the last child of `.modal-body`, i.e. appended
// BELOW the row holding the Test button, in a container that is already scrolled
// to its end when the user reaches that button. Measured on the real page before
// the fix, at 1280x720: a one-line success was 15/16 pixels under the fold, and a
// failure carrying the API's multi-line hint 95/96. `role="status"` made it the
// same for a screen reader.
//
// So this probe clicks the real button on the real page, at a viewport where the
// dialog must scroll, and measures where the verdict ended up.
func TestConnectionTestVerdictLandsInsideTheDialog(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("no Chrome or Edge installed; cannot measure the layout")
	}
	dir := t.TempDir()
	copyEmbeddedAssets(t, dir)
	index := filepath.Join(dir, "index.html")

	// The probe waits for the page's own functions rather than for a duration:
	// on a loaded runner the scripts land later than any fixed delay would guess.
	script := `
(function () {
  function report(obj) { document.title = 'RESULT::' + JSON.stringify(obj); }
  var deadline = Date.now() + 30000;

  (function ready() {
    if (typeof openConnModal === 'function' && typeof showConnTestResult === 'function' &&
        document.getElementById('conn-test') && document.getElementById('conn-test-result')) {
      return run();
    }
    if (Date.now() > deadline) return report({ error: 'the connection dialog never came up' });
    setTimeout(ready, 50);
  })();

  function run() {
    try {
      openConnModal(false);
      var modal = document.querySelector('#modal-conn .modal');
      var btn = document.getElementById('conn-test');
      var out = document.getElementById('conn-test-result');
      if (!modal || !btn || !out) return report({ error: 'the dialog is missing its parts' });

      /* The user gets to the button by scrolling to the end of the form, and that
         scroll position is part of the repro: the verdict arrives while the
         container is already at its bottom. */
      var scrollable = modal.scrollHeight > modal.clientHeight + 1;
      modal.scrollTop = modal.scrollHeight;

      /* A real failure text: TestConnection returns DescribeDialError's output,
         which is the transport message plus a multi-line hint — the tall case,
         and what the user needs to read. */
      window.fetch = function () {
        return Promise.resolve({ ok: true, json: function () {
          return Promise.resolve({ ok: false, error: 'dial tcp 192.0.2.10:22: i/o timeout\nHint: TCP connect timed out: no reply from the target (packets silently dropped). Typical causes: the host is down or on another network; a firewall silently DROPs this port instead of rejecting it; or fail2ban banned the termcp host after earlier failed attempts. Verify from the machine running termcp: nc -zv <host> <port>.' });
        } });
      };
      btn.click();

      var started = Date.now();
      (function waitForVerdict() {
        if (out.style.display === 'block' && out.textContent) return measure(scrollable);
        if (Date.now() - started > 20000) return report({ error: 'the verdict never appeared' });
        setTimeout(waitForVerdict, 50);
      })();
    } catch (e) {
      report({ error: String((e && e.message) || e) });
    }
  }

  function measure(scrollable) {
    var modal = document.querySelector('#modal-conn .modal');
    var out = document.getElementById('conn-test-result');
    var btn = document.getElementById('conn-test');
    var m = modal.getBoundingClientRect();
    var r = out.getBoundingClientRect();
    var b = btn.getBoundingClientRect();
    report({
      scrollable: scrollable,
      inActionsRow: !!(out.parentNode && out.parentNode.classList && out.parentNode.classList.contains('modal-actions')),
      verdictTop: r.top,
      verdictBottom: r.bottom,
      modalTop: m.top,
      modalBottom: m.bottom,
      verdictHeight: r.height,
      buttonTop: b.top,
      buttonLeft: b.left,
      verdictRight: r.right,
      text: out.textContent.slice(0, 48)
    });
  }
})();
`
	if err := os.WriteFile(filepath.Join(dir, "verdictprobe.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	page = []byte(strings.Replace(string(page), "</body>", `<script src="verdictprobe.js"></script></body>`, 1))
	if err := os.WriteFile(index, page, 0o600); err != nil {
		t.Fatal(err)
	}

	// 1280x720 is the smallest desktop the dialog is used at, and the form is
	// already taller than it there: the dialog must scroll to reach Test.
	const width, height = 1280, 720
	dom := renderedProbeDOMSized(t, chrome, index, width, height)
	probe := verdictProbeResult(t, dom)
	if probe.Error != "" {
		t.Fatalf("the probe did not get to a verdict: %s", probe.Error)
	}
	if !probe.Scrollable {
		t.Fatalf("the connection dialog no longer overflows at %dx%d, so this probe measured the one case that was never broken; shrink the viewport so the dialog still scrolls", width, height)
	}
	if !probe.InActionsRow {
		t.Errorf("the verdict is not in the action row with the Test button; as a sibling BELOW that row it is the last line of a scrolled dialog and lands under the fold")
	}
	// A pixel of slack for subpixel layout.
	if probe.VerdictTop < probe.ModalTop-1 || probe.VerdictBottom > probe.ModalBottom+1 {
		t.Errorf("the verdict is not inside the dialog's visible area: verdict %.0f..%.0f is outside %.0f..%.0f (%.0fpx below the fold, %.0fpx tall) — showing it has to scroll the dialog to it",
			probe.VerdictTop, probe.VerdictBottom, probe.ModalTop, probe.ModalBottom,
			probe.VerdictBottom-probe.ModalBottom, probe.VerdictHeight)
	}
	// And the verdict must not displace the button it answers. A flex item's base
	// size is its max-content width, so a long hint would claim the whole row and
	// wrap Test/Save onto the next line, under the text — the verdict visible, the
	// control it belongs to moved somewhere else. `flex: 1 1 0` is what keeps them
	// side by side.
	if probe.ButtonLeft < probe.VerdictRight && probe.ButtonTop >= probe.VerdictBottom-1 {
		t.Errorf("the Test button (top %.0f, left %.0f) was wrapped below the verdict (bottom %.0f, right %.0f); the verdict must share the row with the button, not push it out of the way",
			probe.ButtonTop, probe.ButtonLeft, probe.VerdictBottom, probe.VerdictRight)
	}
	t.Logf("verdict %.0f..%.0f inside dialog %.0f..%.0f (%0.fpx tall, %q); button at %.0f,%.0f",
		probe.VerdictTop, probe.VerdictBottom, probe.ModalTop, probe.ModalBottom, probe.VerdictHeight, probe.Text,
		probe.ButtonLeft, probe.ButtonTop)
}

// verdictProbe is one measurement of the probe.
type verdictProbe struct {
	Error         string  `json:"error"`
	Scrollable    bool    `json:"scrollable"`
	InActionsRow  bool    `json:"inActionsRow"`
	VerdictTop    float64 `json:"verdictTop"`
	VerdictBottom float64 `json:"verdictBottom"`
	VerdictRight  float64 `json:"verdictRight"`
	ModalTop      float64 `json:"modalTop"`
	ModalBottom   float64 `json:"modalBottom"`
	VerdictHeight float64 `json:"verdictHeight"`
	ButtonTop     float64 `json:"buttonTop"`
	ButtonLeft    float64 `json:"buttonLeft"`
	Text          string  `json:"text"`
}

func verdictProbeResult(t *testing.T, dom string) verdictProbe {
	t.Helper()
	const marker = "RESULT::"
	i := strings.Index(dom, marker)
	if i < 0 {
		t.Fatalf("the probe did not report; the page never ran it")
	}
	rest := dom[i+len(marker):]
	end := strings.Index(rest, "</title>")
	if end < 0 {
		t.Fatalf("the probe's result is not a title: %.120q", rest)
	}
	var out verdictProbe
	if err := json.Unmarshal([]byte(htmlUnescape(rest[:end])), &out); err != nil {
		t.Fatalf("decoding the probe's measurement: %v", err)
	}
	return out
}
