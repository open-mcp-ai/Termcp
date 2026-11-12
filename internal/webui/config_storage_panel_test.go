package webui

import (
	"strings"
	"testing"
)

/* The encryption panel's busy pass disables the form while a password request is
 * in flight, then renders again once it settles. That second render is the only
 * thing that re-enables the fields, so it must derive `disabled` from the busy
 * flag alone.
 *
 * It once read `el.disabled = el.disabled || busy`. The first submit latched
 * every input and button in the form to disabled forever: a rejected password
 * (six characters) could not be corrected, Cancel was dead with the submit key,
 * and only the modal's X let the user out. A flag that reads itself has no way
 * back, so this guards the shape itself.
 *
 * Mutation check: restoring `el.disabled = el.disabled` anywhere in the module
 * fails this test.
 */
func TestConfigStorageBusyNeverLatchesDisabled(t *testing.T) {
	body := readAssetLF(t, "static/js/config-storage.js")

	if strings.Contains(body, "el.disabled = el.disabled") {
		t.Fatal("config-storage.js derives disabled from the element's own state; " +
			"a failed password request will leave the form permanently disabled")
	}
	if !strings.Contains(body, "el.disabled = _configStorageBusy") {
		t.Fatal("config-storage.js no longer resets the form's disabled state from the busy flag")
	}
	/* The fields a rejected password has to be corrected in, and the keys that
	 * get the user out: all four are inside the forms the busy pass repaints. */
	for _, id := range []string{
		"config-storage-setup",
		"config-storage-unlock",
		"config-storage-change",
	} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Fatalf("panel form %q is gone; the busy pass repaints forms by id", id)
		}
	}
}
