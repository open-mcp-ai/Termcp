package webui

import (
	"io/fs"
	"path/filepath"
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

/* The panel must not reintroduce the vocabulary of the model it replaced. "上锁"
 * meant the old model's keychain-level lock, which does not exist any more: there
 * are keys that are or are not loaded.
 *
 * "解锁" is not on the list, and the reason is the card. Storage mode is a padlock
 * the person clicks — 已加锁 / 未加锁, 解锁为明文 — and that padlock is a different
 * control from the second layer (钥匙未载入 / 未激活 / 已激活). Banning the word
 * banned the padlock's own verb; what the old model did with it (bind this machine,
 * hold a password) is what the list is for.
 *
 * Mutation check: adding "上锁" back to the panel's copy or markup fails the
 * assertion.
 */
func TestKeychainPanelHasNoLockVocabulary(t *testing.T) {
	body := readAssetLF(t, "static/js/config-storage.js")
	// Only the panel's user-visible strings matter; the module also answers server
	// codes, and those names are the wire contract, not copy.
	for _, banned := range []string{
		"上锁", "绑定本机",
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
			for _, banned := range []string{"密码", "上锁", "绑定本机"} {
				if strings.Contains(value, banned) {
					t.Fatalf("%s/%s says %q: %q", lang, key, banned, value)
				}
			}
		}
	}
}

/* A wrong passphrase is said where the question was asked.
 *
 * Three paths ask for one. The prompt for loading a key now hands the request to the
 * dialog, so a typo comes back under the field and the box stays open — a second
 * press and a retyped word was the cost of getting it wrong once. Activation used to
 * hide its dialog BEFORE posting, and the failure was written into the keychain
 * panel's error line, which is not open when the action starts from a host card: the
 * dialog vanished, nothing was activated, and the page said nothing. The panel's line
 * stays the first choice and a toast is the fallback, because a message shouted into a
 * closed panel is the same as no message.
 *
 * Mutation check: drop the `#secret-err` paragraph, or the `hideModal` back in front of
 * the activate POST, or the toast in configStorageReportFailure — each fails by name.
 */
func TestAWrongPassphraseIsSaidWhereTheQuestionWasAsked(t *testing.T) {
	panel := readAssetLF(t, "static/js/config-storage.js")

	prompt := panel[strings.Index(panel, "function configStoragePromptSecret"):]
	if i := strings.Index(prompt, "function configStoragePickKey"); i > 0 {
		prompt = prompt[:i]
	}
	// the error line lives in the dialog's markup, the rest in the function that fills it
	if !strings.Contains(panel, `id="secret-err"`) {
		t.Error("the prompt has no line to say that the passphrase was wrong")
	}
	for _, want := range []string{
		"opts.submit(value).then(function () { close(value); }, function (err) { fail(err); });",
		"errEl.textContent = configStorageFailure(err);",
		"errEl.style.display = 'block';",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt cannot say that the passphrase was wrong: %q is gone", want)
		}
	}

	submit := panel[strings.Index(panel, "function configStorageSubmitActivate"):]
	if i := strings.Index(submit, "function configStorageOpenActivate"); i > 0 {
		submit = submit[:i]
	}
	if head := submit[:strings.Index(submit, "configStoragePostBatch")]; strings.Contains(head, "hideModal('modal-activate')") {
		t.Error("the activate dialog closes before the answer: a wrong passphrase leaves nothing on screen")
	}
	if !strings.Contains(submit, "activateError(") {
		t.Error("the activate dialog has no way to show why it refused")
	}

	report := panel[strings.Index(panel, "function configStorageReportFailure"):]
	if i := strings.Index(report, "function configStorageError"); i > 0 {
		report = report[:i]
	}
	if !strings.Contains(report, "panel.classList.contains('hidden')") || !strings.Contains(report, "showCopyToast(message)") {
		t.Error("a failure from a card writes only into the closed panel's error line")
	}
	if !strings.Contains(panel, "configStorageReportFailure(err);") {
		t.Error("the batch path no longer routes failures through the panel-or-toast choice")
	}
}

/* A hint never pulls itself up.
 *
 * `.conn-field-hint` carried `margin: -8px 0 8px`, which only looked right in one
 * position: directly under an input, whose own bottom margin is 12px, so the caption
 * landed 4px below it. In every other position the -8px subtracted from whatever
 * container the hint was in — a dialog whose first line was a hint had its own top
 * padding cancelled, and "the padding is missing" was reported twice for that reason.
 * The rule is now 0 above and 8px below, and this test is what keeps a negative pull
 * off any class whose job is to caption something.
 *
 * Mutation check: restore `margin: -8px 0 8px` on `.conn-field-hint` and this fails
 * by name.
 */
func TestNoHintPullsItselfUp(t *testing.T) {
	files, err := fs.Glob(Assets(), "static/css/*.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		name := filepath.Base(path)
		css := readAssetLF(t, "static/css/"+name)
		for _, block := range strings.Split(css, "}") {
			open := strings.Index(block, "{")
			if open < 0 {
				continue
			}
			// Only the selector line itself: a rule may carry a comment above it.
			lines := strings.Split(block[:open], "\n")
			selector := strings.TrimSpace(lines[len(lines)-1])
			if !strings.Contains(strings.ToLower(selector), "hint") {
				continue
			}
			for _, decl := range strings.Split(block[open+1:], ";") {
				decl = strings.TrimSpace(decl)
				if !strings.HasPrefix(decl, "margin") {
					continue
				}
				// The VALUE only: the hyphen inside `margin-top` is not a negative.
				value := decl
				if colon := strings.Index(value, ":"); colon >= 0 {
					value = value[colon+1:]
				}
				if strings.Contains(value, "-") {
					t.Errorf("%s: %q on %q pulls the hint into the block above it", name, decl, selector)
				}
			}
		}
	}
}

/* A press on one card is not a selection.
 *
 * The padlock changes one host's storage form, so its dialog names that host: "the
 * selected host" is a sentence about a set the person never made, and the person
 * pressed 加锁/解锁 — the dialog answering 加密/还原 was a second vocabulary for the
 * same action, on the surface that shows the first one.
 *
 * Mutation check: drop the options argument at the call site and the first half fails;
 * drop `(options && options.title) ||` in either dialog and the second half does.
 */
func TestThePadlockNamesTheHostItChanges(t *testing.T) {
	panel := readAssetLF(t, "static/js/config-storage.js")
	handler := panel[strings.Index(panel, "var mark = event.target.closest('.conn-storage-mark')"):]
	handler = handler[:strings.Index(handler, "new MutationObserver")]
	for _, want := range []string{"'storage.lockOneTitle'", "'storage.unlockOneTitle'"} {
		if !strings.Contains(handler, want) {
			t.Errorf("the padlock press opens a batch dialog: %s is never used", want)
		}
	}
	for _, want := range []string{
		"var titleOn = (options && options.title) || t('storage.encryptTitle');",
		"var title = (options && options.title) || t('storage.restoreTitle');",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("the dialog ignores the host it was opened for: %q", want)
		}
	}
	cat := loadCatalogs(t)
	for lang, entries := range cat {
		for _, key := range []string{"storage.lockOneTitle", "storage.unlockOneTitle"} {
			if v, ok := entries[key]; !ok || !strings.Contains(v, "{name}") {
				t.Errorf("%s/%s does not name the host (%q)", lang, key, v)
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

/* Exporting a host and sharing it were two menu items for one intention: the same
 * document leaves the machine either way, and only "who may open it" differs. The
 * merge is pinned here at the places it could silently come apart — a second exit
 * creeping back into the batch bar, and a protection the dialog offers that never
 * reaches the route implementing it.
 */
func TestExportAndShareAreOneVerb(t *testing.T) {
	dialogs := readAssetLF(t, "static/js/dialogs.js")
	start := strings.Index(dialogs, "var _nodeBatch = createBatchBar(")
	if start < 0 {
		t.Fatal("dialogs.js no longer builds the host batch bar")
	}
	block := dialogs[start:]
	if end := strings.Index(block, "cardSelector:"); end >= 0 {
		block = block[:end]
	}
	if strings.Contains(block, `'nethub-menu-share'`) {
		t.Error("the batch bar still carries a second exit for the same intention; " +
			"sharing is a protection inside the export dialog")
	}
	if !strings.Contains(dialogs, "shareOpen('export')") {
		t.Error("the export verb does not open the export/share dialog")
	}

	// The three protections the dialog offers, each with the sentence that says what
	// it costs: a choice nobody can evaluate is a coin toss with a keyboard.
	share := readAssetLF(t, "static/js/share.js")
	for _, want := range []string{
		`value="plain"`, `value="password"`, `value="key"`,
		"share.hintPlain", "share.hintPassword", "share.hintKey",
	} {
		if !strings.Contains(share, want) {
			t.Errorf("share.js is missing %q; the merge lost one of the three protections", want)
		}
	}
	/* Encryption succeeds whatever was typed, so the second field is the only place a
	   mistyped passphrase is catchable — on this side. On the other side the file is
	   simply unopenable and the only fix is to go back and export again. */
	if !strings.Contains(share, "id=\"share-pass2\"") || !strings.Contains(share, "share.passMismatch") {
		t.Error("the passphrase is not typed twice and compared; a typo produces a file nobody can open")
	}
	/* A share seals ONE profile (the name rides the envelope), so the dialog must not
	   promise more than the route can do. */
	if !strings.Contains(share, "share.selectOne") {
		t.Error("nothing refuses a multi-host share; hostshare sells one profile per blob")
	}

	// And the two file-shaped answers reach the same route, one with the password
	// header and one without. Without this the dialog would offer encryption that
	// never happens — which is exactly the gap the import dialog used to have.
	conn := readAssetLF(t, "static/js/conn-form.js")
	if !strings.Contains(conn, "'X-Termcp-Password': password") {
		t.Error("the sealed export never sends the password header")
	}
	if !strings.Contains(readAssetLF(t, "static/js/util.js"), "fetch(path, headers ? { headers: headers } : undefined)") {
		t.Error("downloadTOML drops the headers, so the password cannot reach the route")
	}

	// Receiving a sealed file is the other half of that: a download the browser cannot
	// import back is a file the UI produced and cannot consume. The field lives in
	// conn-form.js, not index.html — that page has a markup budget and this dialog's
	// logic already owns it.
	if !strings.Contains(conn, `id="conn-import-pass"`) {
		t.Error("the import dialog has no passphrase field; a sealed .enc file cannot be opened from the page")
	}
	if !strings.Contains(conn, "X-Termcp-Password") {
		t.Error("the import dialog does not send the passphrase it collected")
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
		line := panel[strings.LastIndex(panel[:at], "<button") : strings.Index(panel[at:], ">")+at]
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
	/* The third state is the readable one, and it is the same slot: a host that is
	   activated can be deactivated, and the caption is where that lives. The address
	   is replaced only while the host cannot be read — an activated host keeps its
	   address and wears the state beside it. */
	if !strings.Contains(calls, "SVG_KEY, t('storage.cardActive')") {
		t.Error("an activated host has no caption, so the card cannot offer the way back")
	}
	if !strings.Contains(calls, "var shut = !!(c.key_unloaded || c.not_activated);") {
		t.Error("the address is hidden by a state that is not shut, or by none at all")
	}
	if !strings.Contains(panel, "(shut ? '' : (addr ?") {
		t.Error("the address line no longer follows the shut states")
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

	/* The caption is a door, and the card's own handler steps aside for it so one
	   press opens one dialog. The caption's handler lives in config-storage.js and is
	   pinned by TestALockedHostOpensInOnePress. */
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

/* The entry key in the host drawer has four states, and that button is the only
 * place encryption is visible while the panel is closed. A state class the module
 * paints with no rule selecting it is a state the operator cannot see — which is
 * what `is-orphaned` was: ciphertext that outlived its key, painted, unstyled.
 *
 * Mutation check: delete either rule from config-storage.css and this fails by name.
 */
func TestTheKeychainEntryKeyShowsEveryState(t *testing.T) {
	panel := readAssetLF(t, "static/js/config-storage.js")
	css := readAssetLF(t, "static/css/config-storage.css")
	for _, state := range []string{"is-configured", "is-locked", "is-orphaned", "is-unknown"} {
		if !strings.Contains(panel, "'"+state+"'") {
			t.Errorf("config-storage.js no longer paints %q on the entry key", state)
		}
		if !strings.Contains(css, "#config-storage-open."+state) {
			t.Errorf("the entry key can be %q, but no rule selects that state", state)
		}
	}
}

/* The mask button's glyph names the action its label names: "Show addresses" while
 * masked, "Hide addresses" while visible. `aria-pressed` is true exactly when they
 * are visible, so the pressed rule must show the struck eye and the unpressed one the
 * open eye. The paint comment used to say the opposite — that the eye showed the
 * current state — so the stylesheet and the prose disagreed about which of them was
 * wrong.
 *
 * Mutation check: swap the two display values in workspace.css and this fails.
 */
func TestTheMaskGlyphNamesTheActionItsLabelNames(t *testing.T) {
	css := readAssetLF(t, "static/css/workspace.css")
	for _, want := range []string{
		".drawer-mask .eye-open { display: block; }",
		`.drawer-mask[aria-pressed="true"] .eye-closed { display: block; }`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the mask no longer offers the action its label names: missing %q", want)
		}
	}
	for _, wrong := range []string{
		".drawer-mask .eye-closed { display: block; }",
		`.drawer-mask[aria-pressed="true"] .eye-open { display: block; }`,
	} {
		if strings.Contains(css, wrong) {
			t.Errorf("the mask glyph is inverted by %q", wrong)
		}
	}

	panel := readAssetLF(t, "static/js/config-storage.js")
	if !strings.Contains(panel, "btn.setAttribute('aria-pressed', masked ? 'false' : 'true')") {
		t.Error("paintMaskToggle no longer makes pressed mean visible")
	}
}

/* A press on a host that cannot be read DOES the job, it does not describe it.
 *
 *   key not in memory, passphrase    -> ask once; that answer inserts the key AND
 *                                       activates every host it seals
 *   key not in memory, no passphrase -> insert and activate, silently
 *   key in memory, host not activated-> activate it
 *
 * The card body and the caption both go there, because those are the two places a
 * person presses. Nothing here opens the keychain panel: a key the press can insert
 * itself is a step the press takes itself, and the panel is for the keys themselves
 * (create, rename, delete), not for a host one of them happens to seal.
 *
 * Mutation checks: (1) point the caption back at the keychain panel and the first
 * assertion fails; (2) make the card body open the keychain again and the third
 * fails; (3) drop the passphrase question and the fourth fails; (4) stop asking on
 * the key row's plug button and the sixth fails.
 */
func TestALockedHostOpensInOnePress(t *testing.T) {
	panel := readAssetLF(t, "static/js/config-storage.js")

	caption := panel[strings.Index(panel, "document.getElementById('conn-grid').addEventListener('click'"):]
	next := strings.Index(caption, "var mark = event.target.closest('.conn-storage-mark')")
	if next < 0 {
		t.Fatal("the lock's click handler moved; this test would slice to the end of the file")
	}
	caption = caption[:next]
	if !strings.Contains(caption, "configStorageUnlockHost(name)") {
		t.Error("a press on the state caption no longer unlocks the host")
	}
	if strings.Contains(caption, "showCopyToast") {
		t.Error("the caption explains itself instead of acting")
	}
	/* One dialog per press: in selection mode the card is a tick and the batch bar
	   owns the actions on the ticked set. */
	if !strings.Contains(caption, "_nodeBatch.isOn()) return;") {
		t.Error("the caption ignores selection mode: one press would open a dialog and tick a card")
	}

	cards := readAssetLF(t, "static/js/dialogs.js")
	body := cards[strings.Index(cards, "inner.addEventListener('click'"):]
	body = body[:strings.Index(body, "startSessionAndOpenShell")]
	if !strings.Contains(body, "configStorageUnlockHost(c.name)") {
		t.Error("a press on the card body still goes somewhere other than the unlock")
	}
	if strings.Contains(body, "showModal('modal-config-storage')") {
		t.Error("the card body still opens the keychain, which answers a question the press did not ask")
	}

	/* The unlock itself: at most one answer from the person, and the silent path
	   really is silent. */
	unlock := panel[strings.Index(panel, "function configStorageUnlockHost"):]
	unlock = unlock[:strings.Index(unlock, "function configStorageOneHostOptions")]
	if !strings.Contains(unlock, "if (!key.passphrase) {") {
		t.Error("the unlock asks a passphrase question even when the key has none")
	}
	if !strings.Contains(unlock, "/plug', {method: 'POST'}") {
		t.Error("the silent path no longer inserts the key that is out of memory")
	}
	if !strings.Contains(unlock, "body: JSON.stringify({passphrase: passphrase})") {
		t.Error("the one-step path no longer sends the passphrase with the insert, so the hosts stay shut")
	}
	for _, want := range []string{
		"configStoragePromptSecret({",
		"configStoragePostBatch('/api/config-storage/activate'",
	} {
		if !strings.Contains(unlock, want) {
			t.Errorf("the unlock no longer reaches %q", want)
		}
	}

	/* The key row's own button asks too: loading a passphrase key without it is the
	   half-done state the one-step flow exists to remove. */
	action := panel[strings.Index(panel, "function configStorageKeyAction"):]
	action = action[:strings.Index(action, "function configStorageRunKeyAction")]
	plug := action[strings.Index(action, "case 'plug':"):]
	plug = plug[:strings.Index(plug, "case 'unplug':")]
	if !strings.Contains(plug, "key.passphrase") || !strings.Contains(plug, "configStoragePromptSecret(") {
		t.Error("the plug button loads a passphrase key without asking for its passphrase")
	}
	if !strings.Contains(plug, "configStorageActivatedToast(key.hosts)") {
		t.Error("loading a key says nothing about the hosts it just made readable")
	}

	/* One host is not a selection: the batch wording would describe a set the person
	   never made. The dialog carries the host's name and nothing else — the hint line
	   that used to sit above the fields repeated what the title and the label already
	   say, on a negative margin that ate the block above it. */
	dialog := panel[strings.Index(panel, "function configStorageOpenActivate"):]
	dialog = dialog[:strings.Index(dialog, "function configStorageSubmitActivate")]
	if !strings.Contains(dialog, "options.title ||") {
		t.Error("the activate dialog ignores the host name it was opened for")
	}
	if strings.Contains(dialog, "options.hint") || strings.Contains(dialog, "options.noPassphrase") {
		t.Error("the activate dialog still renders a hint line above the fields")
	}
}

/* Deactivating one host is reachable from the card, not only from the batch bar.
 *
 * "Activate" had a one-press door on the card; "deactivate" had only the selection
 * menu, which is the control the caption used to point at from across the page. The
 * two halves of the second layer share one slot — the protocol row's caption — so the
 * label says which state the host is in and the press moves it to the other one. A
 * second glyph in the title row (a key beside the padlock) was the first attempt, and
 * it put two answers where the eye expects one: the padlock owns storage, the caption
 * owns everything about the key.
 *
 * Mutation check: drop the `c.storage === 'encrypted'` branch and the first half
 * fails; point the branch at 'activate' and the second fails.
 */
func TestTheCardOffersTheWayBackToInactive(t *testing.T) {
	cards := readAssetLF(t, "static/js/dialogs.js")
	calls := cards[strings.Index(cards, "var lockLine ="):]
	calls = calls[:strings.Index(calls, "tile.innerHTML")]
	if !strings.Contains(calls, `cardLockLine(c.name, 'activated',`) {
		t.Fatal("an activated host's caption no longer names the state it is in")
	}
	if !strings.Contains(calls, "SVG_KEY, t('storage.cardActive')") {
		t.Error("the activated caption does not offer the way back")
	}
	/* One glyph in the title row: the padlock. A key up there was the second answer,
	   and the eye reads a row of glyphs as one row of answers. */
	if strings.Contains(cards, "conn-key-mark") {
		t.Error("the title row carries a second glyph again")
	}
	/* The caption is a button and carries its own handler, so the card body steps
	   aside for it: one press, one dialog. */
	if !strings.Contains(cards, "e.target.closest('.node-addr.is-locked')") {
		t.Error("a click on the caption also fires the card's connect path")
	}

	panel := readAssetLF(t, "static/js/config-storage.js")
	unlock := panel[strings.Index(panel, "function configStorageUnlockHost"):]
	unlock = unlock[:strings.Index(unlock, "function configStorageOneHostOptions")]
	if !strings.Contains(unlock, "configStorageConvert('deactivate', [name],") {
		t.Error("the activated caption does not deactivate")
	}
	/* And the confirm it opens names the host: "deactivate the selected host" is a
	   sentence about a set, and this press selected nothing. */
	if !strings.Contains(unlock, "storage.deactivateOneTitle") {
		t.Error("the single-host confirm still reads as the batch one")
	}
	/* The deactivate branch sits BELOW the doors a shut host needs: a host whose key
	   is out of memory has to be loaded first, and one that was never activated has
	   nothing to deactivate. */
	shut := unlock[:strings.Index(unlock, "if (key.plugged && host.activated)")]
	if !strings.Contains(shut, "if (host.orphaned)") {
		t.Error("the deactivate branch swallowed the orphaned door: a host with no key has nothing to deactivate")
	}
	/* And it comes BEFORE the doors that activate: an inactive host is a press away
	   from working, not from being shut. */
	after := unlock[strings.Index(unlock, "if (key.plugged && host.activated)"):]
	if !strings.Contains(after, "configStorageOpenActivate([name], configStorageOneHostOptions(name))") {
		t.Error("the deactivate branch swallowed the activate path")
	}
}
