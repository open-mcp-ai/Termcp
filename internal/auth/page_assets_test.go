package auth

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func testPageAssets() fstest.MapFS {
	files := fstest.MapFS{}
	for name, content := range map[string]string{
		"static/theme-boot.js": "/* theme boot */", "static/js/theme.js": "/* theme engine */",
		"static/js/app.js": "private script", "static/css/theme-boot.css": "/* boot CSS */",
		"static/css/app.css": "/* theme imports */", "fonts/pixel.ttf": "test font",
		"themes/default-light/theme.css":                   ":root{color-scheme:light}",
		"themes/custom theme/theme.css":                    ":root{color-scheme:dark}",
		"themes/custom theme/theme.json":                   `{"terminal":{}}`,
		"themes/custom theme/strings.json":                 `{}`,
		"themes/custom theme/assets/static/css/tokens.css": ":root{}",
		"themes/custom theme/palette.css":                  ":root{}",
		"themes/custom theme/assets/art/background.webp":   "test image",
		"themes/custom theme/assets/fonts/pixel.ttf":       "test font",
		"themes/custom theme/assets/icons/icon.svg":        "<svg/>",
		"themes/custom theme/assets/static/js/app.js":      "private script",
		"themes/custom theme/notes.json":                   "private data",
		"themes/incomplete/readme.txt":                     "no stylesheet", "themes/.hidden/theme.css": "hidden",
		"themes/readme.txt": "not a theme", "api/accounts.json": "private data",
	} {
		files[name] = &fstest.MapFile{Data: []byte(content)}
	}
	files["static/css/directory.css/child.css"] = &fstest.MapFile{Data: []byte("/* not a file */")}
	return files
}

func TestPageAssetsArePublicWithoutOpeningApplicationRoutes(t *testing.T) {
	m := &Managed{}
	m.SetPageAssets(testPageAssets())
	nextCalls := 0
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		w.WriteHeader(204)
	}))
	for _, name := range []string{
		"static/theme-boot.js", "static/js/theme.js", "static/css/theme-boot.css", "static/css/app.css",
		"fonts/pixel.ttf", "themes/", "themes/default-light/theme.css", "themes/custom%20theme/theme.css",
		"themes/custom%20theme/theme.json", "themes/custom%20theme/strings.json",
		"themes/custom%20theme/assets/static/css/tokens.css", "themes/custom%20theme/assets/fonts/pixel.ttf",
		"themes/custom%20theme/assets/icons/icon.svg",
		"themes/custom%20theme/palette.css", "themes/custom%20theme/assets/art/background.webp",
	} {
		for _, method := range []string{"GET", "HEAD"} {
			w := managedRequest(t, h, method, pageAssetsPrefix+name, "", "", "")
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("%s %s: status=%d headers=%v", method, name, w.Code, w.Header())
			}
			if method == "HEAD" && w.Body.Len() != 0 {
				t.Fatalf("HEAD %s returned a body", name)
			}
			if len(w.Result().Cookies()) != 0 {
				t.Fatalf("public asset %s issued a cookie", name)
			}
		}
	}
	listing := managedRequest(t, h, "GET", pageAssetsPrefix+"themes/", "", "", "").Body.String()
	if !strings.Contains(listing, `href="custom%20theme/"`) || strings.Contains(listing, "incomplete") || strings.Contains(listing, ".hidden") || strings.Contains(listing, "readme") {
		t.Fatalf("unexpected theme discovery: %s", listing)
	}
	for _, name := range []string{
		"", "themes", "themes/default-light/", "themes/custom%20theme/assets/", "static/css/", "static/js/app.js",
		"static/css/directory.css", "themes/custom%20theme/notes.json", "themes/custom%20theme/assets/static/js/app.js",
		"themes/.hidden/theme.css", "api/accounts.json", "static/css/missing.css",
		"../groups", "themes/../../groups", "themes/%2e%2e/groups", "themes/bad%5cname/theme.css",
	} {
		if w := managedRequest(t, h, "GET", pageAssetsPrefix+name, "", "", ""); w.Code != 404 {
			t.Errorf("asset %s: %d, want 404", name, w.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		if w := managedRequest(t, h, method, pageAssetsPrefix+"static/js/theme.js", "", "", ""); w.Code != 405 {
			t.Errorf("write %s: %d, want 405", method, w.Code)
		}
	}
	for _, target := range []string{"/auth/groups", "/auth/principals", "/auth/tokens", "/api/sessions", "/api/term"} {
		if w := managedRequest(t, h, "GET", target, "", "", ""); w.Code != 401 {
			t.Errorf("anonymous %s: %d, want 401", target, w.Code)
		}
	}
	if nextCalls != 0 {
		t.Fatalf("public assets reached application handler %d times", nextCalls)
	}
}

func TestManagedPagesShareNonceProtectedThemeShell(t *testing.T) {
	m, token, _ := configuredManaged(t)
	m.SetPageAssets(testPageAssets())
	h := m.Wrap(http.NotFoundHandler())
	for _, target := range []string{"/setup", "/login", "/auth", "/permissions"} {
		w := managedRequest(t, h, "GET", target, "", token, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d", target, w.Code)
		}
		csp := w.Header().Get("Content-Security-Policy")
		for _, directive := range []string{"default-src 'none'", "style-src 'self' 'unsafe-inline'", "font-src 'self'", "base-uri 'none'", "frame-ancestors 'none'", "form-action 'self'"} {
			if !strings.Contains(csp, directive) {
				t.Errorf("%s: CSP lacks %s", target, directive)
			}
		}
		nonce := regexp.MustCompile(`script-src 'nonce-([^']+)'`).FindStringSubmatch(csp)
		if len(nonce) != 2 {
			t.Fatal("missing script nonce")
		}
		body := w.Body.String()
		for _, tag := range regexp.MustCompile(`<script\b[^>]*>`).FindAllString(body, -1) {
			if !strings.Contains(tag, `nonce="`+nonce[1]+`"`) {
				t.Errorf("%s: script without matching nonce: %s", target, tag)
			}
		}
		for _, expected := range []string{`data-theme-base="auth/ui/"`, `id="termcp-theme"`, `src="auth/ui/static/theme-boot.js"`, `src="auth/ui/static/js/theme.js"`, `id="theme-switch"`} {
			if !strings.Contains(body, expected) {
				t.Errorf("%s lacks %s", target, expected)
			}
		}
	}
}

func TestLoginPageExposesOnlyLoginAndDecryptionPasswords(t *testing.T) {
	for _, forbidden := range []string{`id="username"`, `automatic-unlock`, `previous-config`, `vault-automatic`} {
		if strings.Contains(loginHTML, forbidden) {
			t.Errorf("login page retains obsolete credential field %q", forbidden)
		}
	}
	for _, want := range []string{`id="password"`, `id="config-password"`, `id="vault-password"`, `id="decryption-confirmation"`} {
		if !strings.Contains(loginHTML, want) {
			t.Errorf("login page is missing %q", want)
		}
	}
}

func TestManagedAuthPageUsesLoginSecurityLanguage(t *testing.T) {
	for _, forbidden := range []string{
		`"account": "Account"`,
		`"account": "账号"`,
		"Checking your account…",
		"正在检查账号…",
	} {
		if strings.Contains(loginHTML, forbidden) {
			t.Errorf("login page retains account terminology %q", forbidden)
		}
	}
	for _, want := range []string{`"account": "Login & security"`, `"account": "登录与安全"`} {
		if !strings.Contains(loginHTML, want) {
			t.Errorf("login page lacks %q", want)
		}
	}
}

func TestManagedPagesKeepFallbackWhenThemesAreUnavailable(t *testing.T) {
	for _, assets := range []fs.FS{nil, fstest.MapFS{"api.md": &fstest.MapFile{Data: []byte("docs only")}}} {
		m := &Managed{}
		m.SetPageAssets(assets)
		h := m.Wrap(http.NotFoundHandler())
		w := managedRequest(t, h, "GET", "/login", "", "", "")
		if w.Code != 200 || strings.Contains(w.Body.String(), `id="termcp-theme"`) || !strings.Contains(w.Body.String(), "--bg-canvas:") {
			t.Fatalf("unusable page fallback: %d", w.Code)
		}
		if w := managedRequest(t, h, "GET", pageAssetsPrefix+"themes/", "", "", ""); w.Code != 404 {
			t.Fatalf("unavailable assets: %d", w.Code)
		}
	}
}

func TestPermissionPageUsesStableAccessPathLabels(t *testing.T) {
	for _, want := range []string{"function permissionAccessLabel(pathName)", "dsl:'accessDSL'", "webui:'accessWebUI'"} {
		if !strings.Contains(permissionsHTML, want) {
			t.Errorf("permissions page lacks %q", want)
		}
	}
	if strings.Contains(permissionsHTML, "t('access'+") {
		t.Fatal("permissions page constructs access labels from camelizing internal path names")
	}
}

func TestPermissionPageKeepsPreviewPresetsAndIndependentScrollPanes(t *testing.T) {
	for _, want := range []string{
		"previewPresetList", "previewPresetHost", "previewPresetConnect",
		"previewPresetOutput", "previewPresetRun", "previewPresetReadFile",
		"previewPresetWriteFile", "previewPresetForwards",
		".auth-permissions #policy-pane .action-panel,.auth-permissions #policy-pane .scope-panel{min-height:0;overflow-y:auto",
	} {
		if !strings.Contains(permissionsHTML, want) {
			t.Errorf("permissions page lacks %q", want)
		}
	}
}

func TestPermissionPageHasOneContextualBackLinkAndMobileColumnReset(t *testing.T) {
	for _, gone := range []string{`href="/permissions" aria-current="page"`, `<a href="/auth" data-text="account"`} {
		if strings.Contains(permissionsHTML, gone) {
			t.Errorf("permissions page retains duplicate global navigation %q", gone)
		}
	}
	for _, want := range []string{
		`class="back-link"`,
		`.auth-permissions .workspace{display:flex;flex-direction:column;grid-template-columns:1fr`,
		`.auth-permissions .workspace>aside{`,
		`width:100%;max-width:100%;min-width:0`,
	} {
		if !strings.Contains(permissionsHTML, want) {
			t.Errorf("permissions responsive shell lacks %q", want)
		}
	}
}

func TestPermissionPageHasMobileGroupAccordionAndDraftControls(t *testing.T) {
	for _, want := range []string{
		`id="permission-groups-mobile-toggle"`,
		`id="permission-mobile-subjects"`,
		`batchToggle.id='permission-batch-toggle'`,
		`data-batch-effect`,
		`discardButton.id='discard-policy'`,
		`permissionCreate('button','',t('discardChanges'))`,
		`['deny','filterDeny']`,
		`function permissionLayoutDetail()`,
		`id="permission-subject-home"`,
		`function permissionLayoutSubjectSection()`,
		`function permissionRenderSubjects()`,
		`max-height:none!important`,
		`.auth-permissions .permission-groups-mobile-toggle{display:flex`,
		`@media(max-width:840px)`,
	} {
		if !strings.Contains(permissionsHTML, want) {
			t.Errorf("permissions mobile shell lacks %q", want)
		}
	}
}

func TestPermissionPageExplainsMCPAccessSwitches(t *testing.T) {
	for _, want := range []string{
		`<span data-text="instanceAccess">MCP 访问</span>`,
		`aria-describedby="mcp-access-help"`,
		`id="mcp-access-help" class="visually-hidden"`,
		`id="mcp-access-help-trigger"`,
		`id="mcp-access-tooltip"`,
		`mcpAccessHelp:'管理员 MCP 允许管理员凭据访问 MCP`,
		`.mcp-help-icon:hover~.mcp-help-tooltip`,
	} {
		if !strings.Contains(permissionsHTML, want) {
			t.Errorf("permissions page lacks MCP access explanation %q", want)
		}
	}
	if strings.Contains(permissionsHTML, `class="mcp-help-copy"`) {
		t.Error("expanded MCP access panel repeats the tooltip copy")
	}
	if strings.Contains(permissionsHTML, `summary:hover .mcp-help-tooltip`) {
		t.Error("MCP tooltip is still triggered by the whole summary hover")
	}
}

func TestPermissionPageProvidesCompleteAgentManagement(t *testing.T) {
	for _, want := range []string{
		`data-text="mobileSubjects">Agent 管理`,
		`id="permission-agent-search"`,
		`id="permission-agent-filter"`,
		`id="permission-agent-create-form"`,
		`id="delete-agent-dialog"`,
		`function permissionPreviewAgentDelete(principal)`,
		`/delete-preview`,
		`{method:'DELETE',body:JSON.stringify({revision:principalDeletePlan.revision,plan_revision:principalDeletePlan.plan_revision})}`,
		`.workspace>aside .permission-mobile-subjects-body{flex:1 1 auto;min-height:0;overflow-y:auto`,
		`.permission-mobile-subject-actions{display:grid;grid-template-columns:repeat(2`,
		`scopeHead.after(summary)`,
		`effectiveAccess:'总体预览'`,
		`unassignedHint:'未绑定有效权限组，凭据当前不可用。'`,
		`tokenHint:'每个客户端使用独立 Token，可单独撤销。'`,
		`applyGroupChange:'修改'`,
	} {
		if !strings.Contains(permissionsHTML, want) {
			t.Errorf("permissions page lacks Agent management contract %q", want)
		}
	}
	if strings.Contains(permissionsHTML, "主体") {
		t.Error("permissions page retains user-facing 主体 terminology")
	}
	if strings.Contains(permissionsHTML, `addSpecial('admin'`) {
		t.Error("permissions page retains the single-user Admin management entry")
	}
	if !strings.Contains(permissionsHTML, `leftUnassigned-rightUnassigned`) {
		t.Error("permissions page does not place unassigned Agents first")
	}
	if !strings.Contains(permissionsHTML, `subjectsOpen:false`) || strings.Contains(permissionsHTML, `permissionUI.subjectsOpen=true`) {
		t.Error("Agent management must start collapsed")
	}
}

func TestPermissionCreatePanelCanCloseFromButtonKeyboardAndToggle(t *testing.T) {
	for _, want := range []string{
		`id="close-new-agent"`,
		`function closeNewAgentForm()`,
		`event.key==='Escape'`,
		`wrap.hidden=false`,
		`if(!wrap.hidden){closeNewAgentForm();return;}`,
	} {
		if !strings.Contains(permissionsHTML, want) {
			t.Errorf("permissions create panel lacks %q", want)
		}
	}
}

func TestPageAssetsRejectConditionalStaleThemeResponse(t *testing.T) {
	m := &Managed{}
	m.SetPageAssets(testPageAssets())
	r := httptest.NewRequest("GET", pageAssetsPrefix+"themes/default-light/theme.css", nil)
	r.Header.Set("If-Modified-Since", "Wed, 01 Jan 2098 00:00:00 GMT")
	w := httptest.NewRecorder()
	m.Wrap(http.NotFoundHandler()).ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "color-scheme:light") {
		t.Fatalf("theme did not refresh: %d %s", w.Code, w.Body.String())
	}
}

type directoryAfterStatFS struct{ fs.FS }

func (f directoryAfterStatFS) Stat(name string) (fs.FileInfo, error) {
	if name == "static/css/swapped.css" {
		return fs.Stat(fstest.MapFS{name: &fstest.MapFile{Data: []byte("original stylesheet")}}, name)
	}
	return fs.Stat(f.FS, name)
}

func TestPageAssetsNeverListDirectoryReplacedAfterStat(t *testing.T) {
	assets := testPageAssets()
	assets["static/css/swapped.css/private-name.txt"] = &fstest.MapFile{Data: []byte("private")}
	m := &Managed{}
	m.SetPageAssets(directoryAfterStatFS{assets})
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), "GET", pageAssetsPrefix+"static/css/swapped.css", "", "", "")
	if w.Code != 404 || strings.Contains(w.Body.String(), "private-name") {
		t.Fatalf("directory reached file server listing: %d %s", w.Code, w.Body.String())
	}
}
