package webui

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

/* The keychain panel repaints its rows on every state change, including while a
 * request is in flight. That render must derive `disabled` from the busy flag
 * alone: a rule that reads the element's own state latches, and one failed
 * request would leave every button dead until the page reloads.
 *
 * Mutation check: writing `el.disabled = el.disabled || busy` anywhere in the
 * module fails this test.
 */
func TestConfigStorageBusyNeverLatchesDisabled(t *testing.T) {
	body := readAssetLF(t, "static/js/config-storage.js")

	if strings.Contains(body, "el.disabled = el.disabled") ||
		strings.Contains(body, "button.disabled = button.disabled") {
		t.Fatal("config-storage.js derives disabled from the element's own state; " +
			"a failed request will leave the panel permanently disabled")
	}
	if !strings.Contains(body, "_configStorageBusy = busy") {
		t.Fatal("config-storage.js no longer records the busy flag")
	}
	if !strings.Contains(body, "button.disabled = _configStorageBusy") {
		t.Fatal("config-storage.js no longer resets a row action's disabled state from the busy flag")
	}
	/* The controls a failed request has to be retried from: the keychain list and
	 * the key form are what the busy pass repaints. They are addressed by id, so the
	 * markup and the repaint have to agree on those ids or a failed request leaves
	 * the panel looking dead. */
	for _, id := range []string{
		"config-storage-key-list",
		"modal-key-form",
		"key-form-primary",
	} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Fatalf("panel markup %q is gone; the busy pass repaints it by id", id)
		}
	}
}

/* The panel must not reintroduce the vocabulary of the model it replaced. A user
 * who reads "locked" looks for a lock to release; there is none, only keys that
 * are or are not loaded.
 *
 * Mutation check: adding any of these words back to the panel's copy or markup
 * fails the assertion.
 */
func TestKeychainPanelHasNoLockVocabulary(t *testing.T) {
	body := readAssetLF(t, "static/js/config-storage.js")
	// Only the panel's user-visible strings matter; the module also answers server
	// codes, and those names are the wire contract, not copy.
	for _, banned := range []string{
		"上锁", "解锁", "绑定本机",
		"data-i18n=\"storage.lock", "data-i18n=\"storage.unlock",
	} {
		if strings.Contains(body, banned) {
			t.Fatalf("config-storage.js still says %q", banned)
		}
	}
	cat := loadCatalogs(t)
	for _, key := range []string{
		"storage.lock", "storage.unlock", "storage.locked", "storage.unlockBind",
		"storage.password", "storage.changePassword", "storage.createKey",
		"storage.modeMachine", "storage.modePassword",
	} {
		for lang, entries := range cat {
			if value, ok := entries[key]; ok {
				t.Fatalf("%s still carries the removed key %q (%q)", lang, key, value)
			}
		}
	}
	for lang, entries := range cat {
		for key, value := range entries {
			if !strings.HasPrefix(key, "storage.") {
				continue
			}
			for _, banned := range []string{"密码", "上锁", "解锁", "绑定本机"} {
				if strings.Contains(value, banned) {
					t.Fatalf("%s/%s says %q: %q", lang, key, banned, value)
				}
			}
		}
	}
}

/* Every class the panel's JS puts on an element has to have a rule somewhere.
 *
 * This is not pedantry: rewriting config-storage.css for the new panel silently
 * dropped the whole `.conn-storage-mark` block — the host card's storage lock —
 * while dialogs.js kept emitting the class. Nothing failed. The lock fell back to
 * a native <button>, which the browser paints black-on-grey, so on a dark theme it
 * looked like an unthemed glyph: "the lock's colour does not follow cyberpunk or
 * matrix". It was not a theme gap, it was a stylesheet that had lost the rule, and
 * only a browser would have shown it.
 *
 * The check is deliberately coarse — "some rule mentions this class" — because the
 * failure mode is a class with NO rule at all, not a rule in the wrong file.
 *
 * Mutation check: delete the `.conn-storage-mark` block and this fails by name.
 */
func TestEveryPanelClassHasARule(t *testing.T) {
	stylesheet := ""
	names, err := fs.Glob(Assets(), "static/css/*.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		stylesheet += readAssetLF(t, name)
	}
	classAttr := regexp.MustCompile(`class(?:Name)?\s*[=+]*\s*['"]([^'"]+)['"]`)
	for _, module := range []string{"static/js/config-storage.js", "static/js/dialogs.js"} {
		js := readAssetLF(t, module)
		for _, m := range classAttr.FindAllStringSubmatch(js, -1) {
			for _, class := range strings.Fields(m[1]) {
				/* A template fragment or a built-up name is not a class yet. */
				if strings.ContainsAny(class, "${}+") {
					continue
				}
				if !strings.Contains(stylesheet, "."+class) {
					t.Errorf("%s emits class %q, but no stylesheet defines it", module, class)
				}
			}
		}
	}
}

/* A checkbox or a radio inline with its word is not a form label, and base.css
 * says every `.modal-body label` is `display: block` with a bottom margin — right
 * for a caption above a field, wrong for a control that sits BESIDE its text. That
 * rule is (0,1,1) and a bare class is (0,1,0), so the panel's own rule lost: the
 * label became a block, its flex line dissolved, and the box and the word fell
 * apart — a centred dot over a name, a box jammed against "invert".
 *
 * This bit twice, so it is checked for every label-shaped control at once.
 *
 * Mutation check: shorten either selector to its bare class and this fails, which
 * is exactly what a browser did while the standalone mock — with no `.modal-body`
 * ancestor — looked fine.
 */
func TestLabelsInsideTheDialogOutrankTheFormLabelRule(t *testing.T) {
	css := readAssetLF(t, "static/css/config-storage.css")
	/* The keychain's own tick used to be a label here and moved to the title bar,
	   which is outside `.modal-body` — so the picker is what is left to check. */
	class := "cs-pick-row"
	scoped := ".modal-body ." + class + " {"
	start := strings.Index(css, scoped)
	if start < 0 {
		t.Fatalf("%s is not scoped to .modal-body; base.css's `.modal-body label` beats it", class)
	}
	end := strings.Index(css[start:], "}")
	rule := css[start : start+end]
	if !strings.Contains(rule, "display: flex") && !strings.Contains(rule, "display: inline-flex") {
		t.Errorf("%s does not state its own display; the caption rule comes back", class)
	}
	/* The rule must also take back what the caption styling imposed: a bottom
	   margin sized for a label above a field, and its smaller font. */
	if !strings.Contains(rule, "margin:") {
		t.Errorf("%s keeps the caption's own margins; the row is not a caption", class)
	}
	if !strings.Contains(css, ".modal-body .cs-pick-row > input[type=\"radio\"]") {
		t.Error("a radio inside the key picker needs its width taken back from `.modal-body input`")
	}
}

/* The keychain's actions and its tick live in the title bar: the row they used to
 * sit in was a header between the title and the list that title names, and with the
 * tick beside the name it applies to there is no second row to keep.
 *
 * Order matters twice here. The tick goes between the title and the count, and the
 * actions go last — after the count, immediately before the close button — because
 * "on the right of the row" is what made them look like they belonged to the list
 * rather than to the panel.
 */
func TestTheKeychainActionsLiveInTheTitleBar(t *testing.T) {
	panel := readAssetLF(t, "static/js/config-storage.js")
	bar := strings.Index(panel, `class="modal-h cs-title-bar"`)
	if bar < 0 {
		t.Fatal("the keychain title bar has no class of its own; its rules cannot be scoped")
	}
	close := strings.Index(panel, `id="config-storage-close"`)
	tick := strings.Index(panel, `id="config-storage-key-select-all"`)
	tools := strings.Index(panel, `class="cs-list-tools"`)
	head := strings.Index(panel, `class="cs-list-head"`)
	if close < 0 || tick < 0 || tools < 0 || head < 0 {
		t.Fatal("the title bar, the tick, the tools or the list header is missing")
	}
	if !(bar < tick && tick < tools && tools < close) {
		t.Error("the title bar must read: title, tick, count, actions, close")
	}
	if head < tools {
		t.Error("the actions are still in the list header; they belong to the title bar")
	}
	/* And the tick is no longer a label with a word beside it: base.css's caption
	   rule is what a label in the dialog body gets, and the word was the second half
	   of the control. */
	if strings.Contains(panel, "cs-select-all") {
		t.Error("the tick is still built as a labelled control")
	}
}

/* An empty keychain had no way to add a key: `renderConfigStorage` folded the whole
 * header row away when the list was empty, and that row is where "Add key" lived.
 * The panel was a title, a status line and nothing to press — which is the state a
 * fresh install starts in, so the first thing a new user saw was a dead panel.
 *
 * The fix was not a second set of buttons in the empty state: the actions moved up
 * to the title bar, which no key count can fold. That is what this pins — one row
 * folds by count (the filter's), and it must not be the row with the entrances.
 */
func TestAnEmptyKeychainStillOffersTheActions(t *testing.T) {
	panel := readAssetLF(t, "static/js/config-storage.js")
	/* The only row whose visibility follows the key count. */
	if !strings.Contains(panel, "head.style.display = keys.length > 4 && !damaged ? '' : 'none'") {
		t.Error("the filter's row is no longer the one that folds by key count; this test watches it")
	}
	for _, folded := range []string{"cs-list-tools').style.display", "cs-title-bar').style.display"} {
		if strings.Contains(panel, folded) {
			t.Errorf("%s folds the title bar or its actions by key count; "+
				"an empty keychain would have no way to add one", folded)
		}
	}
	/* And the empty state does not repeat them: one dialog, one pair of buttons. */
	start := strings.Index(panel, "if (!_configStorageKeys.length) {")
	end := strings.Index(panel[start:], "list.appendChild(empty);")
	if start < 0 || end < 0 {
		t.Fatal("the empty keychain branch is gone; this test watches it")
	}
	branch := panel[start : start+end]
	if strings.Contains(branch, "openKeyCreate") || strings.Contains(branch, "openKeyImport") {
		t.Error("the empty state repeats the title bar's entrances")
	}
}

/* A checkbox is an input, and every dialog rule that styles "an input" meant a
 * TEXT field: full width, a padded well, and `color: var(--text-primary)`. The
 * tick is drawn in that colour — the shared definition paints it `var(--bg-canvas)`
 * so the box looks like a hole punched in the accent fill — and the dialog's rule
 * outranked it, so the tick was near-black on the light theme and near-white on
 * the dark one: the reverse of the row it sits in, in every theme at once.
 *
 * Measured before the fix: default-light tick rgb(41,46,54) on fill rgb(68,112,178),
 * default-dark tick rgb(228,230,235) on fill rgb(130,168,224). After it, each tick
 * equals its theme canvas (rgb(243,244,246) / rgb(36,38,43)).
 *
 * The condition belongs in the selector, so this checks every file that carries a
 * copy of those rules rather than the one that was reported.
 *
 * Mutation check: drop either `:not()` from base.css and this names base.css.
 */
func TestTheDialogFieldRulesExcludeCheckboxesAndRadios(t *testing.T) {
	paths := []string{"static/css/base.css", "static/css/theme.css"}
	forks, err := fs.Glob(Assets(), "themes/*/assets/static/css/theme.css")
	if err != nil {
		t.Fatal(err)
	}
	roots, err := fs.Glob(Assets(), "themes/*/theme.css")
	if err != nil {
		t.Fatal(err)
	}
	paths = append(append(paths, forks...), roots...)
	const want = `.modal-body input:not([type="checkbox"]):not([type="radio"])`
	seen := 0
	for _, path := range paths {
		src := readAssetLF(t, path)
		for i := 0; ; {
			j := strings.Index(src[i:], ".modal-body input")
			if j < 0 {
				break
			}
			at := i + j
			i = at + len(".modal-body input")
			seen++
			if !strings.HasPrefix(src[at:], want) {
				line := 1 + strings.Count(src[:at], "\n")
				t.Errorf("%s:%d styles every input, so a checkbox or a radio gets a text field's "+
					"width, well and colour; write %s", path, line, want)
			}
		}
	}
	/* A test that passes because it scanned nothing would hide the next copy. */
	if seen < 6 {
		t.Fatalf("found %d `.modal-body input` selectors; the rules moved and this test no longer "+
			"watches them", seen)
	}
}

/* Sharing has two directions, and the receiving one used to have no way in:
 * share.js built the import half of the dialog, but the only element ever bound
 * to shareOpen was the batch menu's SEND button. A recipient could paste a share
 * text nowhere, and /api/share/import was reachable by curl alone.
 *
 * This pins the wiring rather than the markup alone: the KEY DETAIL carries the
 * receive button — the address is the key's, so the receiving direction belongs
 * where that address is shown — AND that button has to open the dialog in import
 * mode. An id that exists but is never bound is exactly the bug that slipped
 * through.
 */
func TestReceiveSharedHostHasAnEntrance(t *testing.T) {
	panel := readAssetLF(t, "static/js/config-storage.js")
	if !strings.Contains(panel, `id="key-form-receive"`) {
		t.Fatal("the key detail has no receive button; a shared host cannot be pasted in")
	}
	if !strings.Contains(panel, `document.getElementById('key-form-receive').onclick`) {
		t.Fatal("the receive button exists but nothing is bound to its click")
	}
	/* And it must not creep back into the panel toolbar: a host is received at one
	   key's address, so the entrance lives on that key. */
	if strings.Contains(panel, `id="config-storage-key-receive"`) {
		t.Fatal("receiving moved back into the keychain toolbar")
	}
	if !strings.Contains(panel, `shareOpen('import')`) {
		t.Fatal("the receive button does not open the share dialog in import mode")
	}

	/* An import adds a host to one key, so both the host list and the keychain
	   panel are stale afterwards. The first version refreshed only the host list,
	   and the panel kept reporting the host count from before the paste. */
	share := readAssetLF(t, "static/js/share.js")
	start := strings.Index(share, "function shareImport(")
	if start < 0 {
		t.Fatal("share.js no longer defines shareImport")
	}
	end := strings.Index(share[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit shareImport")
	}
	fn := share[start : start+end]
	for _, refresh := range []string{"loadConnections(", "loadConfigStorage(", "loadConfigStorageKeys("} {
		if !strings.Contains(fn, refresh) {
			t.Errorf("shareImport does not call %s: its view stays stale after a paste", refresh)
		}
	}

	/* The address is per key and lives in that key's own view, so the share dialog
	   must not grow a second copy of it: the removed block was dead markup that
	   nothing ever filled, with a copy button that had no handler. */
	for _, gone := range []string{"share-mine-row", "share-mine-value", "share-mine-copy"} {
		if strings.Contains(share, gone) {
			t.Errorf("share.js still carries %q; the address belongs to the key view", gone)
		}
	}
	if !strings.Contains(panel, `data-i18n-title="storage.keyAddressCopy"`) {
		t.Error("the key address tooltip must say 'click to copy', not 'copied'")
	}
}

/* Sending is about exactly ONE host, so the menu item follows its own rule, not
 * the batch bar's "one or more": with two ticked there is no single host to send.
 * It has to be in the batch bar's disable list as well, or an empty selection
 * leaves it looking actionable and the dialog can only answer with an error.
 */
func TestShareMenuItemFollowsTheSelection(t *testing.T) {
	dialogs := readAssetLF(t, "static/js/dialogs.js")
	start := strings.Index(dialogs, "var _nodeBatch = createBatchBar(")
	if start < 0 {
		t.Fatal("dialogs.js no longer builds the host batch bar")
	}
	block := dialogs[start:]
	if end := strings.Index(block, "cardSelector:"); end >= 0 {
		block = block[:end]
	}
	if !strings.Contains(block, `'nethub-menu-share'`) {
		t.Error("nethub-menu-share is not in the batch bar's actionIds; it stays clickable with nothing selected")
	}

	panel := readAssetLF(t, "static/js/config-storage.js")
	fnStart := strings.Index(panel, "function syncConfigStorageActions(")
	if fnStart < 0 {
		t.Fatal("config-storage.js no longer defines syncConfigStorageActions")
	}
	fn := panel[fnStart:]
	if end := strings.Index(fn, "\n}\n"); end >= 0 {
		fn = fn[:end]
	}
	if !strings.Contains(fn, "shareKey.disabled = selected.length !== 1") {
		t.Error("the share item is not disabled unless exactly one host is ticked")
	}
}

/* The detail sheet has two panels whose state must not move the dialog around:
 *
 *  - the words fold to a BLURRED grid, not to nothing. Removing the panel shrank
 *    the sheet on "hide" and grew it on "show", so the button moved out from under
 *    the pointer that was about to click it again.
 *  - the address box of a released key says why it is empty. A dashed empty box is
 *    indistinguishable from a broken one, which is exactly how it was reported.
 */
func TestTheDetailSheetHoldsItsShape(t *testing.T) {
	panel := readAssetLF(t, "static/js/config-storage.js")
	if strings.Contains(panel, "grid.style.display = shown ? '' : 'none'") {
		t.Fatal("folding the words removes the panel; the dialog resizes between states")
	}
	if !strings.Contains(panel, "grid.classList.toggle('is-folded', !shown)") {
		t.Fatal("the folded words are not marked; nothing can blur them")
	}
	if !strings.Contains(panel, "publicKey || t('storage.keyAddressNotLoaded')") {
		t.Fatal("a released key's address box is left empty; say why there is no address")
	}
	/* And the copy verb is armed by the same fact: a disabled button beside an empty
	   box is the state that says "there is nothing to copy yet". */
	if !strings.Contains(panel, "copy.disabled = !publicKey") {
		t.Fatal("the address copy button stays armed with no address")
	}
	/* The words panel's verbs are icons with a word in the tooltip, not text buttons
	   beside a heading that already names the panel. */
	for _, id := range []string{"key-form-reveal", "key-form-copy", "key-form-export"} {
		at := strings.Index(panel, `id="`+id+`"`)
		if at < 0 {
			t.Fatalf("%s is gone", id)
		}
		line := panel[strings.LastIndex(panel[:at], "<button"):strings.Index(panel[at:], ">")+at]
		if !strings.Contains(line, `class="cs-tool"`) {
			t.Errorf("%s is not an icon button", id)
		}
		if !strings.Contains(line, "data-i18n-title") || !strings.Contains(line, "aria-label") {
			t.Errorf("%s has no word for its glyph", id)
		}
	}
	/* The glyph itself, and the address's own copy verb — one everywhere. */
	if !strings.Contains(panel, "var KEY_GEAR_COPY = '<svg") ||
		!strings.Contains(panel, "var KEY_GEAR_DOWNLOAD = '<svg") ||
		!strings.Contains(panel, "var KEY_GEAR_EYE = '<svg") {
		t.Error("the icon set is gone; the buttons above draw nothing")
	}
	if !strings.Contains(panel, `id="key-form-pub-copy"`) {
		t.Error("the address has no copy button of its own")
	}
	css := readAssetLF(t, "static/css/config-storage.css")
	if !strings.Contains(css, ".cs-phrase.is-folded") || !strings.Contains(css, "filter: blur(") {
		t.Fatal("the folded words need a blur rule, or they are readable while 'hidden'")
	}
	if !strings.Contains(css, ".cs-form-head .cs-form-actions") {
		t.Fatal("a verb group on a heading row keeps the margin it needs under a panel")
	}
}

/* A name used in the markup has to exist. The icons are built as `+ KEY_GEAR_X +`
 * concatenations, and one of them was spelled `KEY_COPY` while the constant was
 * `KEY_GEAR_COPY`: the module threw a ReferenceError while building its own panel,
 * so the whole keychain was gone from the page — and `node --check` was happy,
 * because a free variable is not a syntax error. Only a browser noticed.
 *
 * Mutation check: shorten any `+ KEY_GEAR_X +` to `+ KEY_X +` and this fails by name.
 */
func TestEveryIconConstantUsedIsDefined(t *testing.T) {
	panel := readAssetLF(t, "static/js/config-storage.js")
	used := regexp.MustCompile(`\+\s*(KEY_[A-Z_]+)\s*\+`).FindAllStringSubmatch(panel, -1)
	if len(used) == 0 {
		t.Fatal("no icon constants are used; this test no longer watches anything")
	}
	for _, m := range used {
		name := m[1]
		if !strings.Contains(panel, "var "+name+" = ") {
			t.Errorf("the markup uses %s, which is never defined: the panel throws while building itself", name)
		}
	}
}

/* A glyph inside a button whose label comes from `data-i18n` on the BUTTON is a
 * glyph that is gone: the engine writes `textContent`, which is a full replacement.
 * The import button lost its icon exactly that way, with the label still correct and
 * nothing else complaining. It now carries no text at all — the address's copy button
 * beside it is the shape it copies — so its word has to live in the tooltip, which
 * `data-i18n-title` fills and `data-i18n` would not.
 *
 * Mutation check: put `data-i18n="storage.keyImportHostCiphertext"` back on the
 * button and this fails by name.
 */
func TestALabelledButtonKeepsItsGlyph(t *testing.T) {
	panel := readAssetLF(t, "static/js/config-storage.js")
	at := strings.Index(panel, `id="key-form-receive"`)
	if at < 0 {
		t.Fatal("the receiving button is gone")
	}
	start := strings.LastIndex(panel[:at], "<button")
	openTagEnd := strings.Index(panel[at:], ">") + at
	end := strings.Index(panel[at:], "</button>") + at
	openTag, button := panel[start:openTagEnd], panel[start:end]
	if !strings.Contains(button, "<svg") {
		t.Fatal("the receiving button has no glyph")
	}
	if !strings.Contains(openTag, `class="cs-tool"`) {
		t.Error("the receiving button is not drawn like the address's copy button beside it")
	}
	/* The word lives in the tooltip. `data-i18n` on the button would set
	   `textContent` and delete the glyph — which is how this button lost its icon
	   once already. */
	if strings.Contains(openTag, `data-i18n=`) {
		t.Error("the receiving button's label is translated onto the button, which deletes its glyph")
	}
	if !strings.HasPrefix(openTag, "<button type=\"button\"") ||
		!strings.Contains(openTag, "data-i18n-title=\"storage.keyImportHostCiphertext\"") ||
		!strings.Contains(openTag, "data-i18n-aria=\"storage.keyImportHostCiphertext\"") {
		t.Error("an icon-only button needs its word as a tooltip and an aria-label")
	}
}

/* A dialog's cleanup names every handler it bound. Both halves of that sentence have
 * been wrong in this file: a `cancel.onclick = null` whose variable had been deleted
 * with the button (a ReferenceError the moment the dialog opened), and a `var cancel`
 * that was declared, nulled on close, and never bound (a Cancel button that did
 * nothing). `node --check` sees neither — a free reference is not a syntax error, and
 * an unused variable is legal. The invariant is cheap to state: a name that is nulled
 * on close must be declared, and must have been given something other than null.
 */
func TestEveryHandlerNulledOnCloseIsBound(t *testing.T) {
	modules, err := fs.Glob(Assets(), "static/js/*.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range modules {
		source := readAssetLF(t, name)
		for _, match := range regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\.onclick = null`).FindAllStringSubmatch(source, -1) {
			binding := match[1]
			if !regexp.MustCompile(`\bvar ` + regexp.QuoteMeta(binding) + `\b`).MatchString(source) {
				t.Errorf("%s nulls %s.onclick but never declares it: opening that dialog throws", name, binding)
				continue
			}
			bound := regexp.MustCompile(`\b` + regexp.QuoteMeta(binding) + `\.onclick = ([^\n]*)`)
			assigned := false
			for _, assignment := range bound.FindAllStringSubmatch(source, -1) {
				/* `null;` is still null — the semicolon is not a handler. */
				if strings.Trim(assignment[1], " 	;") != "null" {
					assigned = true
					break
				}
			}
			if !assigned {
				t.Errorf("%s declares %s and nulls it on close, but never binds it: that button does nothing", name, binding)
			}
		}
	}
}

/* A card whose key is not loaded, or whose host is not activated, says so on the
 * protocol row, hard right.
 *
 * It was a paragraph in a bare <button> below that row: the browser drew it as a
 * centred, bordered box two lines tall, in the one place on the card that already
 * had room. The state is a caption — three words and a glyph — so it rides the short
 * "SSH" row and `margin-left: auto` is what puts it at the far edge.
 */
func TestTheLockedCaptionSitsOnTheProtocolRow(t *testing.T) {
	panel := readAssetLF(t, "static/js/dialogs.js")
	at := strings.Index(panel, `'<div class="node-meta">'`)
	if at < 0 {
		t.Fatal("the protocol row is gone")
	}
	end := strings.Index(panel[at:], "node-sess")
	if end < 0 {
		t.Fatal("the session line is gone")
	}
	if !strings.Contains(panel[at:at+end], "lockLine") {
		t.Error("the locked caption is not on the protocol row")
	}
	/* A key, never a second padlock: the card's own padlock is two lines above and
	   says "encrypted at rest"; what the caption answers is whether the key is here.
	   Loaded = a key, not loaded = the same key struck through. */
	helper := panel[strings.Index(panel, "function cardLockLine"):]
	helper = helper[:strings.Index(helper, "function nodeAddress")]
	if strings.Contains(helper, "SVG_LOCK") {
		t.Error("the caption draws a padlock, under the padlock the card already has")
	}
	calls := panel[strings.Index(panel, "var lockLine ="):]
	calls = calls[:strings.Index(calls, "tile.innerHTML")]
	for _, want := range []string{"SVG_KEY_SLASHED, t('storage.cardLocked')", "SVG_KEY, t('storage.cardInactive')"} {
		if !strings.Contains(calls, want) {
			t.Errorf("the caption does not use %q: the glyph no longer says which key is missing", want)
		}
	}
	glyphs := readAssetLF(t, "static/js/util.js")
	for _, want := range []string{"var SVG_KEY =", "var SVG_KEY_SLASHED ="} {
		if !strings.Contains(glyphs, want) {
			t.Errorf("%s is gone: the caption would render an empty box", want)
		}
	}
	// And the sentence it used to print longhand is the tooltip now.
	for _, want := range []string{"'key_unloaded'", "'not_activated'",
		"t('storage.cardLockedHint')", "t('storage.cardInactiveHint')"} {
		if !strings.Contains(panel, want) {
			t.Errorf("the caption no longer carries %q", want)
		}
	}

	/* The caption is a question, not a door. A card press would open the keychain
	   behind the toast, so the card's own handler has to step aside for it. */
	if !strings.Contains(panel, "e.target.closest('.node-addr.is-locked')") {
		t.Error("a click on the caption also fires the card's connect path")
	}

	css := readAssetLF(t, "static/css/workspace.css")
	rule := css[strings.Index(css, ".node-addr.is-locked {"):]
	rule = rule[:strings.Index(rule, "}")]
	for _, want := range []string{"margin: 0 0 0 auto", "border: 0", "white-space: nowrap"} {
		if !strings.Contains(rule, want) {
			t.Errorf(".node-addr.is-locked lost %q: it is a box again", want)
		}
	}
}

/* An element that carries a glyph must not carry `data-i18n`.
 *
 * The engine writes `textContent` for that attribute (i18n.js), which replaces every
 * child — so the glyph survives the first paint (the module built it) and disappears
 * on the first language or theme switch, which is the kind of bug that only shows up
 * in the language nobody testing it uses. `data-i18n-title` and `data-i18n-aria` are
 * the ones that coexist with markup. Two buttons lost their icons this way in this
 * branch: the host card's import button, and the NetHub menu's Share item.
 */
func TestAButtonWithAGlyphNeverCarriesDataI18n(t *testing.T) {
	modules, err := fs.Glob(Assets(), "static/js/*.js")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range modules {
		source := readAssetLF(t, name)
		for at := 0; ; {
			open := strings.Index(source[at:], "<button")
			if open < 0 {
				break
			}
			open += at
			close := strings.Index(source[open:], "</button>")
			if close < 0 {
				break
			}
			element := source[open : open+close]
			at = open + close
			tagEnd := strings.Index(element, ">")
			if tagEnd < 0 {
				continue
			}
			if !strings.Contains(element, "<svg") {
				continue
			}
			checked++
			if strings.Contains(element[:tagEnd], `data-i18n="`) {
				t.Errorf("%s: a button with a glyph carries data-i18n, which deletes it on a language switch:\n\t%s",
					name, strings.TrimSpace(element[:tagEnd]))
			}
		}
	}
	if checked < 5 {
		t.Fatalf("only %d buttons with glyphs were checked; the scanner stopped matching", checked)
	}
}

/* Two ways the connection form lied about a save:
 *
 * 1. It read the sealing key from `current.key_id` — but `/api/connections` carries
 *    no such field (see connectionSummary), so editing an encrypted host preselected
 *    nothing and the save went out with no key at all, which the server refuses.
 * 2. A failed save printed the response BODY into the dialog, so the operator read
 *    `{"code":"key_required","error":"…"}` instead of a sentence.
 */
func TestTheConnectionFormKnowsWhichKeySealsAHost(t *testing.T) {
	form := readAssetLF(t, "static/js/conn-form.js")
	if strings.Contains(form, "current.key_id") {
		t.Error("the sealing key is read from /api/connections, which does not publish it")
	}
	if !strings.Contains(form, "_configStorageState") || !strings.Contains(form, "hostState[i].key_id") {
		t.Error("the sealing key is not taken from the keychain's own payload")
	}
	if strings.Contains(form, "throw new Error(t || r.status)") {
		t.Error("a failed save dumps the raw response body into the dialog")
	}
	if !strings.Contains(form, "connFailureText(r)") {
		t.Error("a failed save does not go through the shared failure text")
	}
}

/* The create form must not inherit the previous key's host count.
 *
 * `#modal-key-form` is one reused element, so a create after a detail keeps whatever
 * the detail wrote. The line it writes is `#key-form-hosts-summary` — the guard that
 * should have caught this looked for `key-form-facts`, an id that no longer exists,
 * and `TestReferencedElementIDsExist` counted the lookup itself as proof the element
 * was there.
 */
func TestTheCreateFormClearsTheHostsLine(t *testing.T) {
	panel := readAssetLF(t, "static/js/config-storage.js")
	start := strings.Index(panel, "function openKeyCreate")
	if start < 0 {
		t.Fatal("openKeyCreate is gone")
	}
	body := panel[start:]
	body = body[:strings.Index(body, "\n}\n")]
	if strings.Contains(body, "key-form-facts") {
		t.Error("the create form hides an element that no longer exists")
	}
	if !strings.Contains(body, "key-form-hosts-summary") || !strings.Contains(body, "textContent = ''") {
		t.Error("the create form leaves the previous key's host count on screen")
	}
}
