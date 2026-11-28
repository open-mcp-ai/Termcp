package webui

import (
	"io/fs"
	"regexp"
	"testing"
)

var themeImportRe = regexp.MustCompile(`@import url\("([^"]+)"\)`)

/* A theme is a palette, not a page. Its root theme.css imports the shared
 * stylesheet manifest, and the server resolves that URL inside the theme's own
 * directory first — so the shared manifest's relative imports pick up the theme's
 * tokens.css and theme.css while every structural chunk (base, workspace,
 * approval, config-storage, …) still comes from the shared copy.
 *
 * A theme that ships its own copy of the manifest breaks exactly that: it is a
 * second list of chunks to keep in step, and it drifts by omission. Cyberpunk had
 * one, it predated config-storage.css, and the encryption lock rendered as an
 * unstyled button in that theme alone. Matrix and the two default themes own no
 * manifest; this keeps it that way, and also catches a copy that was edited
 * instead of deleted.
 *
 * Mutation check: writing a themes/cyberpunk/assets/static/css/app.css that omits
 * one import fails this test.
 */
func TestThemeManifestsAreTheSharedOne(t *testing.T) {
	shared := readAssetLF(t, "static/css/app.css")
	if n := len(themeImportRe.FindAllStringSubmatch(shared, -1)); n < 10 {
		t.Fatalf("shared app.css imports only %d modules; the manifest is not what this test assumes", n)
	}

	/* Assets() resolves a theme path to the theme's own file when it has one and
	   to the shared file otherwise, so this glob lists every theme that *could*
	   serve a manifest. Identity is therefore a byte comparison, not the file's
	   presence: the inherited copy and the shared copy are the same bytes. */
	forks, err := fs.Glob(Assets(), "themes/*/assets/static/css/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range forks {
		if body := readAssetLF(t, path); body != shared {
			t.Errorf("%s is a fork of the stylesheet manifest; delete it so the theme inherits "+
				"static/css/app.css, and keep theme-specific rules in the theme's own tokens.css/theme.css", path)
		}
	}

	/* Every theme must still enter through the shared manifest. */
	themes, err := fs.Glob(Assets(), "themes/*/theme.css")
	if err != nil || len(themes) == 0 {
		t.Fatalf("no themes found: %v", err)
	}
	for _, path := range themes {
		if body := readAssetLF(t, path); !themeImportRe.MatchString(body) {
			t.Errorf("%s does not import the shared manifest", path)
		}
	}
}
