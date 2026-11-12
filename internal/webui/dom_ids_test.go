package webui

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

/* A getElementById on an id nothing ever creates is a null dereference at load:
 * the assignment itself throws, and every handler registered after it silently
 * never binds. That is exactly how the encryption panel lost its unlock form:
 * a checkbox was deleted from the markup while its onchange handler stayed, and
 * the module died at that line.
 *
 * The check is deliberately about existence, not about reachability: an id may
 * be produced by a builder function (the password inputs are), so a quoted
 * occurrence anywhere in the assets counts as a definition. A typo produces no
 * occurrence at all, which is the failure this catches.
 */
func TestReferencedElementIDsExist(t *testing.T) {
	root := filepath.Join("assets")
	assets := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext != ".js" && ext != ".html" {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		assets[path] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Everything the assets define, either as markup or as a quoted id passed to
	// a builder that emits the element.
	defined := map[string]bool{}
	for _, body := range assets {
		for _, m := range regexp.MustCompile(`id="([^"]+)"`).FindAllStringSubmatch(body, -1) {
			defined[m[1]] = true
		}
		for _, m := range regexp.MustCompile(`'([a-z0-9-]+)'`).FindAllStringSubmatch(body, -1) {
			defined[m[1]] = true
		}
	}
	ref := regexp.MustCompile(`getElementById\('([^']+)'\)`)
	var missing []string
	seen := map[string]bool{}
	for path, body := range assets {
		if !strings.HasSuffix(path, ".js") {
			continue
		}
		for _, m := range ref.FindAllStringSubmatch(body, -1) {
			if defined[m[1]] {
				continue
			}
			if !seen[m[1]] {
				seen[m[1]] = true
				missing = append(missing, m[1]+" ("+path+")")
			}
		}
	}
	sort.Strings(missing)
	for _, id := range missing {
		t.Errorf("getElementById target is never created anywhere in the assets: %s", id)
	}
}
