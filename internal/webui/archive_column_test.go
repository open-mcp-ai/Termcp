package webui

import (
	"regexp"
	"strings"
	"testing"
)

// TestArchiveRendersRowsNotCards pins the archive's shape.
//
// The archive used to render the same card as the running plate. That is the
// wrong anatomy for it: a card spends a terminal glyph, a status lamp, a lock, a
// rename control and a copy control on a session nobody can type into any more.
// What an archived session needs is enough to recognise it (the name), enough to
// judge it (when it ended) and a way to act on it (a tick, and the row itself
// opens the transcript read-only).
//
// The assertion is on the renderer's own source because this is a DOM-shape
// claim: the row must be built from the row classes, and it must not grow the
// card's anatomy back.
func TestArchiveRendersRowsNotCards(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")
	fn := between(t, js, "function renderArchiveRows(", "\n}\n")
	if fn == "" {
		t.Fatal("renderArchiveRows is gone: the archive has lost its own renderer")
	}
	for _, want := range []string{
		`'archive-row'`,
		`class="archive-row-name"`,
		`class="archive-row-age"`,
		/* The row is the thing the batch component ticks, and it is keyed by the
		   session's ID. A name is a label: two archived sessions can share one (a
		   re-dialled profile produces `ck-main` twice), so a name-keyed set ticks
		   both when the user pointed at one. */
		`data-sid`,
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("an archive row should carry %s", want)
		}
	}
	// The card's furniture stays out. Each of these would be a state readout for
	// a session whose only remaining state is "over".
	for _, unwanted := range []string{
		"sess-terminal-ic",
		"sess-status-ic",
		"sess-lock",
		"sess-x",
	} {
		if strings.Contains(fn, unwanted) {
			t.Errorf("archive rows must not grow the card's %q back", unwanted)
		}
	}
}

// TestArchiveStartsFolded is the whole point of the column: these sessions are
// over, nothing in them is urgent, and the work they would take pixels from is
// the extensions beside them. The rail's count is what makes opening it an
// informed choice instead of a guess, so the default is folded at EVERY width —
// not just on the narrow ones, which is what it used to be before the column
// became the archive.
func TestArchiveStartsFolded(t *testing.T) {
	js := readAssetLF(t, "static/js/archive-column.js")
	fn := between(t, js, "function archiveDefaultCollapsed()", "\n}\n")
	if fn == "" {
		t.Fatal("archiveDefaultCollapsed is gone: the fold has lost its default")
	}
	if !strings.Contains(fn, "return true") {
		t.Errorf("the archive must start folded at every width; got %q", fn)
	}
	if strings.Contains(fn, "innerWidth") {
		t.Errorf("the fold default must not depend on the viewport; got %q", fn)
	}
	// Picking a state by hand has to win over the default and survive the next
	// load, or the column would re-fold under a user who just opened it.
	for _, want := range []string{"localStorage.setItem", "localStorage.getItem"} {
		if !strings.Contains(js, want) {
			t.Errorf("the fold choice must be remembered; %s is missing", want)
		}
	}
}

// TestArchiveToolbarIsAFlatKeyRow pins the archive's verbs as FLAT keys.
//
// This column deliberately does NOT wear the hosts column's mode-switched batch
// row, and the difference is the point rather than an inconsistency. The hosts
// switch exists to separate "click connects" from "click ticks", because
// connecting is the thing you do there most. Nothing in the archive connects: its
// only direct action is opening a transcript read-only, so the tick target is an
// explicit checkbox on each row and a row click always opens. With no mode to
// enter there is nothing for a switch to switch, and a key whose only job is to
// make other things clickable would be chrome in front of a list that is already
// clickable.
//
// What the two columns DO share is the vocabulary: the three verbs wear the
// extension plate's .ext-key chips, so the flat bars on this page are one control
// rather than two dialects of it.
func TestArchiveToolbarIsAFlatKeyRow(t *testing.T) {
	index := readAssetLF(t, "index.html")
	body := between(t, index, `<div class="nethub-body extpanel-body" id="extpanel-body">`, `</aside>`)
	if body == "" {
		t.Fatal("the archive body is gone")
	}
	for _, want := range []string{
		`class="archive-keys" role="toolbar"`,
		/* All three keys are ICON-ONLY and permanently present. They were labelled
		   text keys that started `hidden` and were revealed by a tick, which put the
		   toolbar away exactly when a reader was deciding what to do with the
		   ledger. Icons keep three keys inside 260px, and `disabled` is what says
		   "not yet" — see the stylesheet's dimmed state. */
		`class="ext-key ext-key-icon" id="archive-sel-invert"`,
		`class="ext-key ext-key-icon" id="archive-batch-export"`,
		`class="ext-key ext-key-icon ext-key-danger" id="archive-batch-delete"`,
		`class="archive-tick-count" id="archive-toggle-count"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the archive key row is missing %s", want)
		}
	}
	/* The invert key LEADS the row and stays there. An earlier revision nested it
	   inside the column's trigger to save the 30px; the toolbar is permanent now, so
	   leading it is where a list-wide verb belongs, and it is also the position that
	   never moves when the selection changes (nothing is ever inserted before it). */
	if i, j := strings.Index(body, `id="archive-sel-invert"`), strings.Index(body, `id="archive-batch-export"`); i < 0 || j < 0 || i > j {
		t.Error("the invert key must lead the toolbar, before the verbs")
	}
	/* Nothing may start hidden: a key that is absent cannot be told apart from one
	   that was never built, and revealing one reflows the row under the pointer. */
	for _, id := range []string{"archive-batch-export", "archive-batch-delete", "archive-toggle-count"} {
		seg := between(t, body, `id="`+id+`"`, ">")
		if strings.Contains(seg, "hidden") {
			t.Errorf("%s must start DISABLED, not hidden", id)
		}
	}
	// A selection-mode switch must NOT come back here: it would be the hosts
	// column's control with nothing to do.
	for _, dead := range []string{
		`id="archive-select-toggle"`, `id="archive-open-split"`, `id="archive-batch-open"`,
		`id="archive-open-caret"`, `id="archive-open-menu"`, `id="archive-menu-export"`, `id="archive-menu-delete"`,
	} {
		if strings.Contains(body, dead) {
			t.Errorf("the archive has grown %s; its verbs are flat keys, not a switch and a split menu", dead)
		}
	}
	// The bespoke row must not come back either: it was a second dialect of the
	// same control, and the reason the two columns stopped matching.
	css := readAppCSS(t)
	for _, stale := range []string{".archive-tools {", ".archive-sel-all {", ".archive-del {"} {
		if strings.Contains(css, stale) {
			t.Errorf("%s is a bespoke archive key; reuse the shared chip classes", stale)
		}
	}

	js := readAssetLF(t, "static/js/sessions.js")
	batch := between(t, js, "var _archiveBatch", "}) : null;")
	if batch == "" {
		t.Fatal("the archive's batch bar is gone: its keys would drive nothing")
	}
	for _, want := range []string{
		`alwaysOn: true`,
		`toggleId: null`,
		`invertId: 'archive-sel-invert'`,
		`exportId: 'archive-batch-export'`,
		`deleteId: 'archive-batch-delete'`,
		`gridId: 'archive-grid'`,
		`cardSelector: '.archive-row'`,
		`nameAttr: 'data-sid'`,
		`onExport: function (names)`,
		`onDelete: function (names)`,
	} {
		if !strings.Contains(batch, want) {
			t.Errorf("the archive's batch bar must carry %s", want)
		}
	}
	// onOpen is gone with the split key: a row click opens, directly.
	if strings.Contains(batch, "onOpen:") {
		t.Error("the archive has no open verb on its key row; a row click opens")
	}
	// The three actions have implementations, not just hooks.
	for _, fn := range []string{
		"function exportArchivedTranscripts(",
		"function deleteArchivedSessions(",
		"function archivedSessionsFor(",
	} {
		if !strings.Contains(js, fn) {
			t.Errorf("%s is gone: the batch bar would drive a key wired to nothing", fn)
		}
	}
}

// TestArchiveRowAgeHasMoreThanOneUnit pins that the age column says something
// useful across the range an archive actually covers.
//
// A single "N days ago" would read "0 days ago" for everything younger than a
// day, which is most of what an archive holds right after a busy session; a
// timestamp alone would cost the name its width at 260px. The thresholds are
// asserted because a wrong unit boundary is silent — the row still renders, it
// just lies about when.
func TestArchiveRowAgeHasMoreThanOneUnit(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")
	fn := between(t, js, "function archiveAge(", "\n}\n")
	if fn == "" {
		t.Fatal("archiveAge is gone: archive rows would carry no age")
	}
	for _, want := range []string{
		"archive.age.now",
		"archive.age.min",
		"archive.age.hour",
		"archive.age.day",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("archiveAge should be able to say %q", want)
		}
	}
	// Server timestamps arrive in seconds on one path and milliseconds on
	// another; the magnitude is the only thing that tells them apart, and getting
	// it wrong would print every row as "just now".
	if !strings.Contains(fn, "1e12") {
		t.Error("archiveAge must normalise second- and millisecond- timestamps")
	}
}

// TestTheFoldedArchiveShowsNothingToClip pins the collapsed state.
//
// The archive had no collapsed rule for its body at all. Folding narrowed the
// column to a 56px spine while the body kept its own width, so the rows stayed laid
// out and were simply CLIPPED by the frame: a folded column showed a slice of ledger
// down its length. That reads as a rendering fault rather than as a collapse,
// because nothing on screen says the content is intentionally put away. The hosts
// column never had the bug, and this asserts the archive matches it.
func TestTheFoldedArchiveShowsNothingToClip(t *testing.T) {
	css := readAppCSS(t)
	rule := between(t, css, ".workspace-frame.ext-collapsed .extpanel-body {", "}")
	if rule == "" {
		t.Fatal("the archive body has no collapsed rule: it will keep rendering under the spine and be clipped")
	}
	for _, want := range []string{"max-height: 0", "opacity: 0", "visibility: hidden"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the folded archive body needs %s, or it stays laid out and gets cut off", want)
		}
	}
}

// TestTheCountIsPaintedBeforeTheSwitchIsLookedUp pins the order, not the value.
//
// The component paints `countId` before its early return now. That early return is
// `if (!cfg.toggleId) return` — every alwaysOn caller passes no toggle — so the
// count used to be unreachable for exactly the two columns that show one. This
// pins the shape rather than the symptom: the count assignment has to come first.
func TestTheCountIsPaintedBeforeTheSwitchIsLookedUp(t *testing.T) {
	js := readAssetLF(t, "static/js/batch-bar.js")
	fn := between(t, js, "function paintToggle(n) {", "\n  }\n")
	if fn == "" {
		t.Fatal("paintToggle is gone")
	}
	countAt := strings.Index(fn, "countEl")
	toggleAt := strings.Index(fn, "var key = el(cfg.toggleId)")
	if countAt < 0 {
		t.Fatal("paintToggle no longer paints the count")
	}
	if toggleAt < 0 {
		t.Fatal("paintToggle no longer paints the switch")
	}
	if countAt > toggleAt {
		t.Error("the count is painted AFTER the `toggleId` lookup: every alwaysOn caller returns early there and its count freezes at 0")
	}
}

// TestTheArchiveExportsAFolderPerSession pins the shape of the archive file.
//
// The export concatenated every transcript into one .txt, which threw away the only
// structure the archive has: a session is a container and the shells are what ran
// inside it. A single blob made a multi-shell archive unreadable — you could not tell
// where one channel stopped and the next started, and there was no way to open just
// the one you cared about.
//
// So the unit is a ZIP, and inside it one FOLDER per session with one FILE per shell.
func TestTheArchiveExportsAFolderPerSession(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")
	fn := between(t, js, "function exportArchivedTranscripts(", "\n}\n")
	if fn == "" {
		t.Fatal("exportArchivedTranscripts is gone")
	}
	// A folder per session, named from the session, holding a record and its shells.
	for _, want := range []string{
		"/session.txt",           // the session's own record
		"/shell-",                // one file per channel
		"/shells",                // the list that says how many there are
		"fetchAllShellOutput(",   // where a shell's complete bytes come from
		"fetchAllSessionLines(",  // the fallback for records with no live shells
		"zipStore(",              // packed, not concatenated
		"safeZipSegment(",        // names are user-controlled
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("the export is missing %s: a session must be a folder with its shells in it", want)
		}
	}
	// The old single-file path must be gone.
	if strings.Contains(fn, "termcp-archive.txt") || strings.Contains(fn, "'text/plain") {
		t.Error("the export still writes one concatenated text file")
	}
	if !strings.Contains(fn, ".zip'") {
		t.Error("the download must be a .zip")
	}
	// output-range answers a JSON envelope, not raw text: it is the terminal-replay
	// route and carries start/end/total beside the bytes. Writing the response text
	// straight into a .txt puts escape sequences inside an object literal.
	if strings.Contains(fn, "r.text()") {
		t.Error("the shell body is read as text; output-range answers JSON and the envelope would land in the file")
	}
	// The frame's `d` field is unpacked by fetchAllShellOutput now that the read is
	// paged; the caller only receives the joined bytes.
	reader := between(t, js, "function fetchAllShellOutput(", "\n}\n")
	if !strings.Contains(reader, "j.d") {
		t.Error("the shell bytes must be taken out of the frame's `d` field")
	}
}

// TestZipWritingIsRealZip pins the container format.
//
// Written against the spec rather than with a library, because this page ships no
// bundler and a dependency would mean a build step for one button. That makes the
// format something this repo has to get right itself: the local header before each
// payload, the central directory after them, CRC-32 per member, and the UTF-8 flag so
// a non-ASCII session name extracts to its real name.
func TestZipWritingIsRealZip(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")
	fn := between(t, js, "function zipStore(", "\n}\n")
	if fn == "" {
		t.Fatal("zipStore is gone")
	}
	for _, want := range []string{
		"0x04034b50", // local file header
		"0x02014b50", // central directory header
		"0x06054b50", // end of central directory
		"0x0800",     // flags: UTF-8 names
		"0xEDB88320", // the CRC-32 polynomial
		"0xFFFFFFFF", // and its pre/post conditioning
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("zipStore is missing %s: the archive would not open", want)
		}
	}
	// STORE, not DEFLATE: shipping deflate is the dependency this avoids.
	if !strings.Contains(fn, "setUint16(8, 0, true)") {
		t.Error("members must be STORE (method 0); DEFLATE would mean shipping an implementation of it")
	}
	// The local header must record the CRC and both sizes, or the archive tests as
	// corrupt even though it lists.
	for _, want := range []string{"setUint32(14, crc, true)", "setUint32(18, data.length, true)", "setUint32(22, data.length, true)"} {
		if !strings.Contains(fn, want) {
			t.Errorf("the local header is missing %s", want)
		}
	}
}

// TestZipSegmentNamesCannotTraverse pins the one security property of the names.
//
// Session names are user-supplied. A ZIP entry whose name walks upwards is a problem
// for whoever extracts it, and this file is going to be extracted by exactly that
// person. The rewrite must also keep the entry ADDRESSABLE — silently dropping it
// would make the export lie about what it contains.
func TestZipSegmentNamesCannotTraverse(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")
	fn := between(t, js, "function safeZipSegment(", "\n}\n")
	if fn == "" {
		t.Fatal("safeZipSegment is gone")
	}
	if !strings.Contains(fn, "\\/:*?") {
		t.Error("path separators must be replaced, or a name creates folders nobody asked for")
	}
	if !strings.Contains(fn, "replace(/^\\.+/, '_')") {
		t.Error("leading dots must be neutralised, or `../../etc/passwd` escapes the archive")
	}
	if !strings.Contains(fn, "return s || '_'") {
		t.Error("an empty result must fall back to something addressable rather than produce an unnamed entry")
	}
}

// TestTheArchiveSelectsByIdNotByName pins the key the selection is kept under.
//
// This was keyed by the DISPLAYED NAME and it mis-selected. A session name is not
// unique: dial a profile twice and the archive holds `ck-main` twice, because the
// name comes from the profile rather than from the run. With names as the key,
// ticking one row ticked both — visibly wrong, and it got worse underneath: the
// id->record map used to resolve a selection was also built by name, so the second
// of a duplicated pair silently overwrote the first, and a delete or an export then
// acted on a record the user had never pointed at.
//
// The row already carries its id. That is what a selection refers to.
func TestTheArchiveSelectsByIdNotByName(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")
	batch := between(t, js, "var _archiveBatch", "}) : null;")
	if batch == "" {
		t.Fatal("the archive's batch bar is gone")
	}
	if !strings.Contains(batch, `nameAttr: 'data-sid'`) {
		t.Error("the selection must be keyed by session id: a name is neither unique nor stable")
	}
	if strings.Contains(batch, `data-sess-name`) {
		t.Error("the selection is still keyed by the displayed name; duplicate names tick together")
	}
	// The resolver must map by id too, or the key is right and the lookup is wrong.
	fn := between(t, js, "function archivedSessionsFor(", "\n}\n")
	if fn == "" {
		t.Fatal("archivedSessionsFor is gone")
	}
	if strings.Contains(fn, "sessionCardName") {
		t.Error("the resolver still indexes records by name; the last duplicate wins and the first is unreachable")
	}
	if !strings.Contains(fn, "byId[s.id]") {
		t.Error("the resolver must index records by id")
	}
	// And the row must carry the id it is keyed by.
	rows := between(t, js, "function renderArchiveRows(", "\n}\n")
	if !strings.Contains(rows, "'data-sid'") {
		t.Error("the row must carry data-sid, which is what the component reads")
	}
}

// TestArchiveRowsKeepTheSelectionContract pins the one behavioural promise the
// row shape shares with the card shapes: ticking edits the SHARED component's
// set, so the count, the invert key and the split menu all agree about what is
// selected no matter which column drew the row.
//
// The row used to keep its own Set. That worked until the column needed an
// invert key and a menu, at which point the second selection mechanism became a
// second source of truth.
func TestArchiveRowsKeepTheSelectionContract(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")
	fn := between(t, js, "function renderArchiveRows(", "\n}\n")
	if fn == "" {
		t.Fatal("renderArchiveRows is gone")
	}
	// The tick goes through the component. There is no mode to check first: this
	// column runs alwaysOn, because the row carries two separate targets — the
	// checkbox ticks, the row opens — so a click never has to be reinterpreted.
	for _, want := range []string{
		"_archiveBatch.toggleSelection(sid, row)",
		"_archiveBatch.syncCards(false)",
		"archive-row-tick",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("an archive row's tick must go through the shared component; %s is missing", want)
		}
	}
	// And a row click must NOT tick: the checkbox is the only tick target.
	if strings.Contains(fn, "_archiveBatch.isOn()") {
		t.Error("the row must not branch on a selection mode: this column has none, and the checkbox is the tick")
	}
	if !strings.Contains(fn, "e.target.closest('.archive-row-tick')") {
		t.Error("a row click must ignore the checkbox and open the transcript")
	}
	// A row rebuilt on every frame must have its tick re-applied, or a selection
	// would appear to clear itself whenever any session died.
	if !strings.Contains(fn, "syncCards") {
		t.Error("a rebuilt row must have its selection re-applied from the component")
	}
}

// TestTheArchiveInvertKeyInvertsRatherThanTogglingAll pins the verb.
//
// The key was select-all/clear-all: press it and the whole list lit up or went
// dark. That is one boolean dressed as a list operation, and it cannot express
// "these four, not those two" — which is the only reason to pick a subset by hand
// in the first place. Per-row inversion keeps a partial selection meaningful: flip
// the list and the four you ticked become the two you did not.
func TestTheArchiveInvertKeyInvertsRatherThanTogglesAll(t *testing.T) {
	js := readAssetLF(t, "static/js/batch-bar.js")
	/* The handler is now a named closure so the same work can serve both the click
	   and the keyboard: the key may be nested inside the column's trigger, where a
	   <button> would be invalid HTML, so Enter/Space have to be wired by hand. */
	start := strings.Index(js, "var doInvert = function (e) {")
	if start < 0 {
		t.Fatal("the invert key's handler is gone")
	}
	handler := js[start : start+strings.Index(js[start:], "\n      };")+8]
	if !strings.Contains(js, "invert.addEventListener('click', doInvert)") {
		t.Error("the invert handler must still be wired to a click")
	}
	/* NO keydown handler, and that is correct: every caller passes a real <button>,
	   and the browser turns Enter and Space into a click for those. Wiring keydown as
	   well would make one press invert TWICE — once directly, once through the
	   synthesized click. (It was needed briefly, while the archive nested the key
	   inside the column's trigger as a <span role="button">.) */
	if strings.Contains(js, "invert.addEventListener('keydown'") {
		t.Error("invert must not wire keydown: the key is a real <button>, so Enter would double-fire")
	}

	// Per name, flipping that name — not "if all selected, clear; else select all".
	if !strings.Contains(handler, "forEach") || !strings.Contains(handler, "state.ids.has(name)") {
		t.Errorf("the invert key must flip each row independently:\n%s", handler)
	}
	if strings.Contains(handler, "allSelected") {
		t.Error("an all-or-nothing branch is the select-all toggle this replaced")
	}

	// The face still names what it would do, and it needs a word for the partial case.
	if !strings.Contains(js, "t('section.batch.invert')") {
		t.Error("a partial selection needs its own label: neither 'select all' nor 'clear' is true of it")
	}
}

// TestTheArchiveRowCarriesItsRecord pins what one row says.
//
// A row used to be a name and an age, which is the least a row can be. Everything
// the record holds — the profile it dialled, the command, how long it lived — was
// thrown away, so two sessions of the same name were indistinguishable and the only
// way to learn why one died was to open both transcripts. Those are the facts an
// archive is opened FOR.
func TestTheArchiveRowCarriesItsRecord(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")
	fn := between(t, js, "function renderArchiveRows(", "\n}\n")
	if fn == "" {
		t.Fatal("renderArchiveRows is gone")
	}
	for _, want := range []string{
		`archive-row-chip`,  // the profile it dialled
		`archive-row-cmd`,   // the command it ran
		`archive-row-facts`, // the outcome/time column
		`archive-row-lived`, // how long it lasted
		`archive-row-del`,   // a per-row erase key
		`s.ssh_config`,      // the profile comes from the record
		`archiveDuration(`,  // and the span is computed, not guessed
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("the archive row is missing %s: the record holds it and the row does not say it", want)
		}
	}
	// The exit code is on the SHELL, not the session: reading s.exit_code yields
	// null for every row, which is a silent blank column rather than an error.
	if !strings.Contains(fn, "s.shells") {
		t.Error("the outcome must come from the session's shell; the session's own exit_code is always null")
	}
	if !strings.Contains(js, "function archiveDuration(") {
		t.Error("archiveDuration is gone: the row would have no lifespan")
	}
}

// TestTheArchiveDeleteKeyDeletesAndDoesNotOpen pins the guard the row needs.
//
// The erase key sits INSIDE the row, and the row's own target opens a transcript.
// Without an early return the click both deletes and opens, which on this page
// means a read-only window appears for a session that no longer exists.
func TestTheArchiveDeleteKeyDeletesAndDoesNotOpen(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")
	fn := between(t, js, "function renderArchiveRows(", "\n}\n")
	if fn == "" {
		t.Fatal("renderArchiveRows is gone")
	}
	if !strings.Contains(fn, "closest('.archive-row-del')") {
		t.Error("a click on the erase key must not fall through to the row's open target")
	}
	// And it goes through the one destructive path the page has, with the record it
	// was built from: deleteArchivedSessions deletes by id, not by label.
	if !strings.Contains(fn, "deleteArchivedSessions([s])") {
		t.Error("the row must delete through the shared path, passing the record (it deletes by id)")
	}
}

// TestArchiveAgeReadsAFieldTheServerActuallySends pins the field name.
//
// The API reports created_at / updated_at / exit_code; there is no `ended_at`.
// Reading a field the server never sends does not fail — it yields undefined,
// archiveAge returns "", and every row renders with a blank age column, which
// looks like a styling problem rather than a wrong field. That silence is why
// this is asserted rather than left to the rendering test.
func TestArchiveAgeReadsAFieldTheServerActuallySends(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")
	fn := between(t, js, "function renderArchiveRows(", "\n}\n")
	if fn == "" {
		t.Fatal("renderArchiveRows is gone")
	}
	if !strings.Contains(fn, "s.updated_at") {
		t.Error("an archive row's age must come from updated_at, which the API sends")
	}
	for _, phantom := range []string{"s.ended_at", "s.dead_at", "s.started_at"} {
		if strings.Contains(fn, phantom) {
			t.Errorf("%q is not a field the sessions API returns; the age would be blank", phantom)
		}
	}
}

// TestArchiveRailCountIsPaintedSynchronously pins that the folded rail's number
// is refreshed by the render itself.
//
// It used to rely only on a MutationObserver over the grid. An observer fires on
// a later task, so right after a session died the rail still read the previous
// count — a visible lag on the one number the folded column exists to show.
func TestArchiveRailCountIsPaintedSynchronously(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")
	fn := between(t, js, "function renderArchiveRows(", "\n}\n")
	if !strings.Contains(fn, "paintArchiveRail()") {
		t.Error("renderArchiveRows must repaint the rail's count, not wait for the observer")
	}
	// archive-column.js loads after sessions.js, so the first paint can run before
	// the declaration exists; the call has to be guarded or the page throws.
	if !strings.Contains(fn, "typeof paintArchiveRail") {
		t.Error("the rail repaint must be guarded: its declaration loads later in the page")
	}
}

// TestArchiveWearsTheHostsColumnsFrame pins that the archive is built from the
// hosts column's components rather than a parallel set of its own.
//
// It started as its own frame — .extpanel-btn, .extpanel-rail, .extpanel-body —
// with its own glyph size, its own collapsed height and its own rail rules. That
// is not merely redundant: the page teaches one shape for "a column you put
// away", and a second one that is ALMOST the same reads as a mistake. Worse, the
// copy had already drifted (58px collapsed trigger against 46), which is the
// failure mode a copy always has.
func TestArchiveWearsTheHostsColumnsFrame(t *testing.T) {
	index := readAssetLF(t, "index.html")
	panel := between(t, index, `<aside class="nethub extpanel" id="extpanel">`, "</aside>")
	if panel == "" {
		t.Fatal("the archive column is gone")
	}
	for _, want := range []string{
		`class="nethub-btn" id="extpanel-toggle"`,
		`class="nethub-label" data-i18n="sec.archive"`,
		`class="nethub-caret"`,
		`class="nethub-rail" id="extpanel-rail"`,
		`class="nethub-body extpanel-body" id="extpanel-body"`,
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("the archive must reuse the hosts column's %s", want)
		}
	}
	// The bespoke names must not come back: a rule for them would be a second
	// definition of the same column, which is how the two drifted before.
	css := readAppCSS(t)
	for _, stale := range []string{
		".extpanel-btn {", ".extpanel-rail {", ".extpanel-glyph {",
		".extpanel-label {", ".extpanel-caret {", ".extpanel-rail-lamp {",
	} {
		if strings.Contains(css, stale) {
			t.Errorf("%s is a second, parallel definition of the hosts column; reuse .nethub*", stale)
		}
	}
	// And the class that makes the reuse real on the element itself.
	if !strings.Contains(panel, "nethub extpanel") {
		t.Error("the archive <aside> must carry both the hosts class and its own")
	}
}

// TestFoldingTheExtensionPlateHidesItsBody pins that folding actually collapses.
//
// The class was being applied and nothing moved: base.css's
// `.section-body.collapsed { display: none }` was losing to this plate's own
// `.workspace-exts .section-body { display: flex }`, which is more specific. The
// class was on the element, the aria-expanded flipped, the choice persisted —
// and the plate stayed exactly as tall as before. A test on the class list alone
// would have passed, so this asserts the override exists.
func TestFoldingTheExtensionPlateHidesItsBody(t *testing.T) {
	css := readAppCSS(t)
	if !strings.Contains(css, ".workspace-exts.is-folded .section-body { display: none; }") {
		t.Error("folding the extension plate must hide its body at matching specificity; " +
			"the shared .collapsed rule alone loses to this plate's own display")
	}
	// The keys go with the body: they act on a list that is no longer on screen.
	if !strings.Contains(css, ".workspace-exts.is-folded .ext-head-actions { display: none; }") {
		t.Error("a folded plate must not leave its batch keys offering actions on a hidden list")
	}
	// The state class goes on the PLATE, not the body, because the keys live on
	// the header bar — outside the body the fold hides.
	js := readAssetLF(t, "static/js/conn-form.js")
	if !strings.Contains(js, "foldTarget: 'ext-plate'") {
		t.Error("the fold's state class must land on the plate that contains both the bar and the body")
	}
}

// TestExtensionBatchKeysRideTheHeaderBar pins where they sit.
//
// They were a toolbar row under the title, which on an 832px plate became a band
// of chrome between a heading and the thing it headed. They act on the cards
// below, so they belong on the bar that names that list.
func TestExtensionBatchKeysRideTheHeaderBar(t *testing.T) {
	index := readAssetLF(t, "index.html")
	head := between(t, index, `<div class="ext-head">`, `</div>
          </div>`)
	if head == "" {
		t.Fatal("the plate's header bar is gone")
	}
	for _, want := range []string{
		/* The bar is the outer box and the trigger is a bare key inside it: a
		   control cannot nest in a <button>, which is why the borders moved up one
		   level so the keys could live INSIDE the bar rather than beside it. The
		   keys themselves are flat (`.ext-key`), not the hosts column's batch row. */
		`class="nethub-btn ext-head-bar" role="group"`,
		`class="ext-fold-btn" id="sec-exts"`,
		`class="ext-head-actions" id="ext-head-actions"`,
		`class="ext-key ext-key-icon" id="ext-sel-invert"`,
		`class="nethub-open-split ext-add-split" id="ext-add-split"`,
	} {
		if !strings.Contains(head, want) {
			t.Errorf("the header bar must carry %s", want)
		}
	}
	// The keys sit in their own wrapper so a click on one never reaches the fold
	// trigger sitting beside it — otherwise ticking a batch switch would collapse
	// the plate out from under the cards it just selected.
	if !strings.Contains(index, `class="ext-head-actions" id="ext-head-actions" onclick="event.stopPropagation()"`) {
		t.Error("the header's key cluster must stop clicks from reaching the fold trigger")
	}
	/* One copy of each key. The plate's keys used to be rendered in the body as a
	   toolbar AND carried on the bar for a while; two copies of an id means the
	   component drives whichever it happens to find. */
	for _, id := range []string{`id="ext-sel-invert"`, `id="ext-add"`, `id="ext-add-caret"`, `id="ext-menu-import"`} {
		if n := strings.Count(index, id); n != 1 {
			t.Errorf("%s appears %d times; it must appear once", id, n)
		}
	}

	// The selecting class follows the keys: it is what reveals them, and they are
	// no longer inside the body the class used to be applied to.
	ext := readAssetLF(t, "static/js/extensions.js")
	if !strings.Contains(ext, "selectingHostId: 'ext-plate'") {
		t.Error("the selecting class must be applied to the plate, since its keys live on the bar")
	}
}

// TestTheArchiveHeaderMirrorsTheHostsHeader pins the two triggers as a PAIR.
//
// The hosts column sits on the left edge of the page and the archive on the
// right, facing each other. Copying the hosts header verbatim put the archive's
// caret hard against the screen's right edge — the corner where a disclosure
// control reads as a window button, and where a thumb reaching in from the edge
// hits it by accident. The mirror is the fix: the caret moves INBOARD (first in
// the row), and the glyph and label take the edge position, so the two headers
// are reflections of each other.
func TestTheArchiveHeaderMirrorsTheHostsHeader(t *testing.T) {
	index := readAssetLF(t, "index.html")
	hosts := between(t, index, `id="open-host-drawer"`, "</button>")
	/* The trigger is a div with role="button" — a real <button> would swallow the
	   presses of the invert key nested inside it — so the segment ends at the div's
	   close, not at the inner key's. */
	archive := between(t, index, `id="extpanel-toggle"`, "</div>")

	posOf := func(seg, cls string) int { return strings.Index(seg, `class="`+cls+`"`) }

	if posOf(hosts, "nethub-caret") < posOf(hosts, "nethub-label") {
		t.Fatal("the hosts header is no longer glyph-then-label-then-caret; the mirror below assumes that order")
	}
	// The archive must be the reverse: the caret comes FIRST, before the glyph.
	// The segment runs from the trigger's own tag to its closing, and the nested
	// invert key inside it does not carry either class, so the positions remain
	// comparable.
	if posOf(archive, "nethub-caret") < 0 || posOf(archive, "nethub-glyph") < 0 {
		t.Fatal("the archive trigger lost its caret or its glyph")
	}
	if posOf(archive, "nethub-caret") > posOf(archive, "nethub-glyph") {
		t.Error("the archive's caret must lead the row, inboard of the glyph: " +
			"it sits on the right-hand column, so last place is the screen edge")
	}
	if posOf(archive, "nethub-label") < posOf(archive, "nethub-glyph") {
		t.Error("the archive's glyph and label must stay together, against the edge")
	}

	// And the CSS must actually place them there. The desktop header is an explicit
	// ORDER LADDER rather than a single negative order, because four items have to
	// be sequenced, not two: caret, then the count, then the glyph, then the label.
	// Pinning the ladder as a set is what keeps a later edit from putting the badge
	// back on the outside — a `-1` on two of them leaves the tie to DOM order, which
	// is exactly how the badge ended up past the label.
	css := readAssetLF(t, "static/css/workspace.css")
	/* The auto margin sits on the CARET — the first item in the ladder — and that is
	   what makes the header one right-aligned block instead of two fragments. It was
	   on the badge, which pinned the caret to the left end of a 260px button while the
	   badge, glyph and label were pushed to the right: the folding control and the
	   name it folds sat at opposite ends of the same bar. Pushing from the first item
	   also keeps the group's spacing stable when the count appears, because the badge
	   then grows inside the pushed block instead of moving it. */
	/* The caret leads and everything else is pushed RIGHT, which is the shape asked
	   for: `[▾] ......... 2 🗄 Archive`. The auto margin therefore sits on the first
	   item of the right-hand run, not on the caret — with it on the caret the caret
	   and the label travelled together and the LEFT edge went empty instead. */
	for _, want := range []string{
		".extpanel .nethub-caret { order: 1; margin-left: 0; }",
		".extpanel .archive-count-badge { margin-left: auto; order: 2; }",
		".extpanel .nethub-glyph { order: 3; }",
		".extpanel .nethub-label { order: 4; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the archive header's order ladder is missing %q; the mirror is only in the markup without it", want)
		}
	}
}

// TestTheNarrowArchiveTriggerShrinksToItsKey keeps the mobile header sane.
//
// Below the breakpoint the archive body is a fixed drawer, so the column is only
// its trigger key in a wrapped header row. Left as the desktop grid's third
// track (`.workspace-frame > .extpanel { width: 100% }`) it claimed the whole
// 390px and pushed the hosts key onto a line of its own. The narrow rule has to
// match that selector's specificity, not just the class, or it never wins.
func TestTheNarrowArchiveTriggerShrinksToItsKey(t *testing.T) {
	css := readAssetLF(t, "static/css/workspace.css")
	narrow := between(t, css, "@media (max-width: 799px) {", "/* Extremely narrow")
	if narrow == "" {
		t.Fatal("the narrow media block is gone")
	}
	if !strings.Contains(narrow, ".workspace-frame > .extpanel { width: auto; flex: 0 0 auto; }") {
		t.Error("the narrow layout must un-stretch the archive column at the SAME specificity as the desktop rule")
	}
	// The mirror is a desktop fact (two columns facing each other). Narrow, both
	// triggers are glyph-and-label keys side by side, so the mirror is undone.
	if !strings.Contains(narrow, ".extpanel .nethub-caret { order: 0; }") {
		t.Error("the narrow layout must undo the mirrored order: there is no facing column to mirror")
	}
}

// TestTheNarrowArchiveOpensAsADrawer pins the two classes the archive body needs.
//
// Below the breakpoint the archive body becomes a fixed drawer sliding in from the
// right. The rules that do that select `.extpanel-body`; the ceiling and the
// column layout select `.nethub-body`. The element shipped with ONLY the second,
// so on a phone the drawer rules never matched: the body stayed a static,
// 2px-wide column inside the header row, and pressing the trigger opened nothing
// visible while locking the page scroll behind a scrim over empty space.
//
// Two classes is not redundancy here — one is the column, the other is the
// drawer, and the narrow layout is the one place they differ.
func TestTheNarrowArchiveOpensAsADrawer(t *testing.T) {
	index := readAssetLF(t, "index.html")
	if !strings.Contains(index, `class="nethub-body extpanel-body" id="extpanel-body"`) {
		t.Fatal("the archive body must carry BOTH classes: .nethub-body for the column, .extpanel-body for the narrow drawer")
	}
	css := readAssetLF(t, "static/css/workspace.css")
	narrow := between(t, css, "@media (max-width: 799px) {", "/* Extremely narrow")
	if !strings.Contains(narrow, ".extpanel-body {") {
		t.Error("the narrow block must turn .extpanel-body into a fixed drawer")
	}
	if !strings.Contains(narrow, ".extpanel-body.drawer-open {") {
		t.Error("the narrow block must reveal the drawer when it is open")
	}
	// The pane's width must come from the drawer rule, and nothing may outrank it.
	// An earlier `width: 0` (three classes deep, to un-stretch the column) did:
	// the archive then opened onto its own 25px of padding.
	if strings.Contains(css, ".extpanel > .nethub-body { width:") {
		t.Error("no width may be set on the archive body: at three classes it outranks the drawer's own width")
	}
	if !strings.Contains(narrow, "width: min(86vw, 360px)") {
		t.Error("the drawer needs its own width; there is no other rule that gives it one")
	}
}

// TestTheDockStaysStillWhenSelectionStarts pins the bar's height against the
// per-tab difference it straddles.
//
// A tab is 24px while it shows its close key and 22px once select mode hides that
// key. The strip used to size itself to its tallest child, so entering the mode
// dropped the whole bar — and every key on it — by a pixel, and leaving it raised
// them again. The strip's height is therefore pinned, which is a stylesheet fact
// and is checked here because the markup cannot express it.
func TestTheDockStaysStillWhenSelectionStarts(t *testing.T) {
	css := readAssetLF(t, "static/css/workspace.css")
	rule := between(t, css, ".sessdock-tabs {", "}")
	if rule == "" {
		t.Fatal("the dock's tab strip rule is gone")
	}
	/* The pin is the point, not the number: a content-sized strip flinches when a
	   tab's own controls change. The value has to be the height a TAB occupies —
	   3+3 padding, 1+1 border, a 16px glyph — because a strip shorter than its only
	   child clips it. 24px was measured against the glyph and forgot the chrome, so
	   every tab hung 2px past the strip's top and bottom. */
	if !strings.Contains(rule, "height: 26px") {
		t.Error("the tab strip must pin its height to what a tab occupies, or it either flinches or clips its tabs")
	}
	if strings.Contains(rule, "height: 24px") {
		t.Error("24px is shorter than a tab (26px), so the strip has to clip it")
	}
}

// The archive header must be right-aligned in BOTH count states.
//
// The header's right-hand run is "badge, glyph, label" and the push comes from
// `margin-left: auto` on its first element. That element is the count badge, and
// the badge is `hidden` (display: none) whenever the archive is empty — which is
// the default state, because the column opens folded and usually stays that way.
// An auto margin on a box that is not laid out moves nothing, so with only that
// rule the caret stayed at the left edge and the glyph and label sat right beside
// it, leaving ~173px of empty plate at the right end of a 260px header. Measured
// in a real browser: glyph at 1192..1214, button right edge at 1420.
//
// The fix hands the push to the glyph in exactly that case, so one of the two
// rules is always live. This test pins both halves plus the markup order the
// sibling selector depends on.
func TestTheArchiveHeaderStaysRightAlignedWhenEmpty(t *testing.T) {
	index := readAssetLF(t, "index.html")
	trigger := between(t, index, `id="extpanel-toggle"`, "</div>")
	posOf := func(tag string) int { return strings.Index(trigger, tag) }

	/* The sibling selector below is only expressible because the badge comes
	   BEFORE the glyph in the markup. If DOM order changes, the fallback rule
	   silently stops matching and the bug returns with no test failure — unless
	   this asserts the order the CSS assumes. */
	badgePos := posOf(`id="extpanel-count"`)
	glyphPos := posOf(`class="nethub-glyph"`)
	if badgePos < 0 || glyphPos < 0 {
		t.Fatal("the archive trigger lost its count badge or its glyph")
	}
	if badgePos > glyphPos {
		t.Error("the count badge must precede the glyph in the markup: the empty-state auto margin is a sibling selector")
	}

	css := readAssetLF(t, "static/css/workspace.css")
	for _, want := range []string{
		// Count present: the badge carries the push and the run ends at the edge.
		".extpanel .archive-count-badge { margin-left: auto; order: 2; }",
		// Count absent (badge hidden): the glyph takes it over.
		".extpanel .archive-count-badge[hidden] + .nethub-glyph { margin-left: auto; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the archive header needs %q, or the empty archive is left-aligned", want)
		}
	}

	// Folded, the trigger is a 56px column with the glyph stacked over the count.
	// A leftover auto margin there pushes the glyph off the column's centre line.
	folded := between(t, css, ".workspace-frame.ext-collapsed .extpanel .nethub-glyph {", "}")
	if !strings.Contains(folded, "margin: 0") {
		t.Errorf("the folded trigger must clear the auto margin; got %q", strings.TrimSpace(folded))
	}
}

// Collapsing one column must not put the other one away.
//
// The archive deliberately wears the hosts column's frame — same .nethub-btn,
// .nethub-label, .nethub-caret, .nethub-body, .nethub-rail — because the page
// teaches ONE shape for "a column you put away". The shared classes are the
// reuse; the COLLAPSE STATE is not shared. The hosts rules were written against
// the bare class (`.workspace-frame.nethub-collapsed .nethub-btn`), so they
// matched both columns: collapsing the hosts column hid the archive's label and
// caret and dropped its body to zero height, while the archive's own state and
// its aria-expanded still said open. Measured in a real browser before the fix:
// archive label display:none, archive body visibility:hidden height:2px.
//
// Each collapse rule must therefore be scoped to the column it belongs to, and
// the two scopes must not overlap. This walks every stateful selector that
// mentions a shared .nethub-* part and asserts its scope.
func TestCollapsingOneColumnLeavesTheOtherAlone(t *testing.T) {
	css := readAssetLF(t, "static/css/workspace.css")

	// The hosts column's collapse rules: all of them scoped under #nethub.
	for _, sel := range []string{
		".workspace-frame.nethub-collapsed #nethub .nethub-btn",
		".workspace-frame.nethub-collapsed #nethub .nethub-label",
		".workspace-frame.nethub-collapsed #nethub .nethub-caret",
		".workspace-frame.nethub-collapsed #nethub .nethub-body",
		".workspace-frame.nethub-collapsed #nethub .nethub-rail",
	} {
		if !strings.Contains(css, sel+" ") && !strings.Contains(css, sel+",") && !strings.Contains(css, sel+" {") {
			t.Errorf("the hosts collapse rule must be scoped as %q, or it also collapses the archive", sel)
		}
	}

	// The reverse leak: an UNscoped `nethub-collapsed` rule reaching a shared part
	// is the exact shape of the bug, so no such selector may exist at all.
	unscoped := regexp.MustCompile(`\.workspace-frame\.nethub-collapsed\s+\.(nethub-btn|nethub-label|nethub-caret|nethub-body|nethub-rail|nethub-glyph)\b`)
	if m := unscoped.FindString(css); m != "" {
		t.Errorf("%q matches BOTH columns: scope it to #nethub (hosts) or .extpanel (archive)", m)
	}

	// The archive's collapse rules are scoped by the ext-collapsed state and the
	// .extpanel column, and must not reach into #nethub either.
	leakBack := regexp.MustCompile(`\.workspace-frame\.ext-collapsed\s+#nethub\b`)
	if m := leakBack.FindString(css); m != "" {
		t.Errorf("%q lets the archive's fold collapse the hosts column", m)
	}

	// And the archive's own body still has its collapsed rule — scoping the hosts
	// rules must not have removed the archive's, which is the other half of the
	// same pair.
	body := between(t, css, ".workspace-frame.ext-collapsed .extpanel-body {", "}")
	for _, want := range []string{"max-height: 0", "visibility: hidden"} {
		if !strings.Contains(body, want) {
			t.Errorf("the archive body must still collapse with %q; got %q", want, strings.TrimSpace(body))
		}
	}
}

// The ticked count LEADS the right-hand run of the archive toolbar.
//
// It used to be the last item in the row, so the number sat in the far corner
// after the trash key: `[✓] ... [⤓] [🗑] 3`. A readout in the corner has no
// visible relation to the two keys it explains, and reads as a page total rather
// than as "three rows are ticked, so these two are live". Measured before the fix
// at 1440px: count at 1399..1409, export at 1317..1352, delete at 1358..1393.
//
// Leading the run makes the row say the sentence in the order the reader needs:
//
//     [✓] ......... 3 [⤓] [🗑]
//
// which also puts the destructive key in the corner where it belongs, furthest
// from the invert check it must never be confused with.
func TestTheArchiveCountLeadsTheRightHandRun(t *testing.T) {
	index := readAssetLF(t, "index.html")
	toolbar := between(t, index, `class="archive-keys" role="toolbar"`, "</div>")
	if toolbar == "" {
		t.Fatal("the archive toolbar is gone")
	}

	countPos := strings.Index(toolbar, `id="archive-toggle-count"`)
	exportPos := strings.Index(toolbar, `id="archive-batch-export"`)
	deletePos := strings.Index(toolbar, `id="archive-batch-delete"`)
	invertPos := strings.Index(toolbar, `id="archive-sel-invert"`)
	if countPos < 0 || exportPos < 0 || deletePos < 0 || invertPos < 0 {
		t.Fatal("the archive toolbar lost one of its controls")
	}
	// Invert first (a list-wide verb), then the count, then the two selection verbs.
	if !(invertPos < countPos && countPos < exportPos && exportPos < deletePos) {
		t.Error("the order must be invert, count, export, delete: the count leads the verbs it explains, " +
			"and the destructive key takes the corner")
	}

	css := readAssetLF(t, "static/css/workspace.css")
	/* The gap that pushes the run to the right sits on the COUNT (the first item
	   of the run), and falls back to export while the count is hidden. Without the
	   fallback, an empty selection has no auto margin at all — the count is
	   display:none — and the two verbs slide over to the invert key. */
	for _, want := range []string{
		"#archive-toggle-count { margin-left: auto; }",
		"#archive-toggle-count[hidden] + #archive-batch-export { margin-left: auto; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the toolbar's right-hand run needs %q", want)
		}
	}
	// And the old spelling — the push on export unconditionally — must not return:
	// it would put the count back in the corner.
	if regexp.MustCompile(`(?m)^\s*#archive-batch-export\s*\{\s*margin-left:\s*auto`).MatchString(css) {
		t.Error("export must not carry the push unconditionally: that leaves the count in the far corner again")
	}
}

// The archive is a STRIP of rows — one column, top to bottom, always.
//
// `#archive-grid` carries `conn-grid`, whose `flex-wrap: wrap` is correct for the
// card grids it was written for: cards flow along a row and wrap DOWN. The archive
// flips the axis (`flex-direction: column`), so wrapping means the overflow goes
// ACROSS — into a second column. The container is height-capped by the panel's
// max-height chain, so the moment the ledger outgrew that cap it split into two
// side-by-side columns, the second one painting past the panel's right edge.
//
// Measured at 1440x900 with 20 rows and no explicit wrap: `columnCount: 2`, the
// columns at left 1167 and left 1348 while the panel ended at 1419.
//
// A ledger has ONE reading order. A second column is a second one.
func TestTheArchiveIsOneColumnOfRows(t *testing.T) {
	css := readAssetLF(t, "static/css/workspace.css")
	grid := between(t, css, "\n#archive-grid {", "}")
	if grid == "" {
		t.Fatal("the archive grid lost its rule")
	}
	if !strings.Contains(grid, "flex-direction: column") {
		t.Errorf("the archive grid must be a column of rows; got %q", strings.TrimSpace(grid))
	}
	/* nowrap is the fix, not a stylistic choice: without it the inherited
	   `flex-wrap: wrap` turns any overflow into a second column. */
	if !strings.Contains(grid, "flex-wrap: nowrap") {
		t.Error("the archive grid must declare flex-wrap: nowrap, or a long ledger wraps into a SECOND column " +
			"instead of scrolling — the second column paints past the panel's right edge")
	}
}

// The archive column is a PLAIN LENGTH, and that is what keeps its collapse smooth.
//
// An intrinsically-sized track (`fit-content(...)`, `max-content`) is not
// interpolable: CSS drops a transition between an intrinsic keyword and a length.
// Measured at 1440x900, sampling every frame across the .24s window:
//
//   px -> px             15 distinct widths   (56 -> 67 -> 89 -> ... -> 260)
//   fit-content -> px    2  distinct widths   (260, then 56: one frame, a snap)
//
// The archive opens and closes by class toggle, so BOTH states have to be plain
// lengths or the animation is silently lost — which is exactly how "the collapse
// stopped animating and just snaps open" gets reported.
//
// The "one vertical strip" constraint lives in the grid rule
// (`flex-wrap: nowrap`), NOT in the track width: an intrinsic track was reaching for
// the content and paid for it with the animation.
func TestTheArchiveColumnKeepsItsCollapseAnimation(t *testing.T) {
	css := readAssetLF(t, "static/css/workspace.css")

	frame := between(t, css, ".workspace-frame {", "}")
	if frame == "" {
		t.Fatal("the workspace frame lost its rule")
	}
	/* Both ends of the transition must be plain lengths. A keyword here is the whole
	   bug: the transition is dropped and the column snaps. */
	for _, banned := range []string{"fit-content", "max-content", "min-content", "auto"} {
		if strings.Contains(frame, "--ext-w: "+banned) {
			t.Errorf("--ext-w must be a plain length: %q cannot be interpolated to the collapsed "+
				"56px, so the collapse loses its transition and snaps in one frame", banned)
		}
	}
	if !strings.Contains(frame, "--ext-w: 260px") {
		t.Errorf("the expanded archive track must be a plain 260px; got %q", strings.TrimSpace(frame))
	}
	// The transition that the value has to be compatible with.
	if !strings.Contains(frame, "transition: grid-template-columns") {
		t.Error("the frame must still transition grid-template-columns, or nothing animates at all")
	}

	// The collapsed end is the other half of the pair, and it must be a length too.
	if !strings.Contains(css, ".workspace-frame.ext-collapsed { --ext-w: 56px; }") {
		t.Error("the collapsed archive is a 56px rail; that width must stay a plain length")
	}

	// The item must still be allowed to shrink: min-width: auto (a grid item's
	// default) is its min-content width, which can hold the track open past its value.
	panel := between(t, css, ".workspace-frame > .extpanel {", "}")
	if panel == "" {
		t.Fatal("the archive column lost its placement rule")
	}
	if !strings.Contains(panel, "min-width: 0") {
		t.Error("the archive column needs min-width: 0, so its content cannot outvote the track width")
	}
	// And `width: 100%` on the item would make an explicit track size moot.
	if strings.Contains(panel, "width: 100%") {
		t.Error("the archive column must not be width: 100%: as a grid item it stretches to its track anyway")
	}
}

// The export must be COMPLETE — every byte the shell ever printed.
//
// The first version asked for `output-range?max=524288&tail=1`: one request, the
// LAST 512 KiB. That is not a truncated export only if the shell never printed more
// than 512 KiB. When it did, the folder silently held the tail of a transcript with
// nothing anywhere saying the beginning was gone — the worst kind of loss, because
// the archive looks complete and the reader has no way to notice.
//
// `output-range` is already a paging route: it answers `start`/`end`/`total` beside
// the bytes. So completeness is a loop, and this pins the loop rather than the
// request that used to be there.
func TestTheArchiveExportIsComplete(t *testing.T) {
	js := readAssetLF(t, "static/js/sessions.js")

	fn := between(t, js, "function fetchAllShellOutput(", "\n}\n")
	if fn == "" {
		t.Fatal("fetchAllShellOutput is gone: the export must page, not take one window")
	}

	// The window size is the server's hard max, and it is a CHUNK size — the loop is
	// what makes the export complete, not a bigger number.
	if !strings.Contains(fn, "512 * 1024") {
		t.Error("the per-request window should be the server's hard max (512 KiB); the loop makes it complete")
	}
	// Page from where the last window ended, using the SERVER's `end` (an absolute
	// offset), not a locally computed length: the server may clamp or re-align.
	for _, want := range []string{
		"&start=",          // paging by absolute offset
		"j.end",            // the server's end offset is the next start
		"j.total",          // and total is what says when to stop
		"start = end",      // advance from what was served
		"end >= total",     // normal exit
		"end <= start",     // no-progress guard, or a clamped window spins forever
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("fetchAllShellOutput is missing %q: paging must advance on the server's own offsets "+
				"and stop on a window that does not move", want)
		}
	}
	// The tail-only request is the bug. Check the request the code BUILDS, not the
	// file: the prose above quotes `tail=1` to explain why it is wrong, so comments
	// have to go before the pattern can mean anything.
	code := stripJSComments(js)
	if regexp.MustCompile(`output-range\?[^'"\s]*tail=1`).MatchString(code) {
		t.Error("the export must not read `tail=1`: that returns the LAST window only, so a shell that " +
			"printed more than 512 KiB exports as its tail with nothing saying the rest is missing")
	}

	/* The fallback for records whose shells the server no longer lists had its own
	   two bugs: it asked for `/lines?limit=100000`, and the route takes `from`+`count`
	   — so `limit` was ignored and the default of 50 applied, exporting a record as
	   fifty lines. `count` is capped server-side too, so completeness is again a loop. */
	lines := between(t, js, "function fetchAllSessionLines(", "\n}\n")
	if lines == "" {
		t.Fatal("fetchAllSessionLines is gone: records without live shells still need their transcript")
	}
	if regexp.MustCompile(`lines\?[^'"\s]*limit=`).MatchString(code) {
		t.Error("the lines route takes `from` + `count`; `limit` is ignored and the default of 50 applies")
	}
	for _, want := range []string{"from=", "count=", "content=1", "total_lines"} {
		if !strings.Contains(lines, want) {
			t.Errorf("fetchAllSessionLines is missing %q: the route is line-addressed and paged", want)
		}
	}
}

// stripJSComments removes /* ... */ and // ... runs so a test can assert on the code
// a file RUNS rather than on the prose it uses to explain that code. Several of the
// comments in sessions.js quote the exact request they replaced.
func stripJSComments(src string) string {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	src = regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(src, "")
	return src
}
