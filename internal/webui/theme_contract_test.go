package webui

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestSharedThemeRoleContractCoversCurrentMenus(t *testing.T) {
	base := readAssetLF(t, "static/css/base.css")
	for _, want := range []string{
		".app-control",
		".app-menu-trigger",
		".app-menu-panel",
		".app-menu-item",
		"--ui-control-radius",
		"--ui-panel-shadow",
		"--ui-menu-item-radius",
		"var(--ui-control-radius, 7px)",
		"var(--ui-panel-radius, 10px)",
		"var(--ui-control-font-family, var(--font-sans))",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base.css is missing shared theme contract %q", want)
		}
	}

	tokens := readAssetLF(t, "static/css/tokens.css")
	for _, want := range []string{
		"--ui-control-radius:",
		"--ui-control-border-width:",
		"--ui-control-font-family:",
		"--ui-control-text-transform:",
		"--ui-panel-radius:",
		"--ui-panel-border-width:",
		"--ui-panel-shadow:",
	} {
		if !strings.Contains(tokens, want) {
			t.Errorf("tokens.css does not define fallback %q", want)
		}
	}

	for file, wants := range map[string][]string{
		"static/js/settings.js": {
			"app-control app-menu-trigger",
			"settings-panel lang-menu app-menu-panel",
			"api.classList.add('app-control')",
		},
		"static/js/approval.js": {
			"approval-btn lang-btn app-control app-menu-trigger",
			"policy-approval-panel lang-menu app-menu-panel",
		},
		"static/js/theme.js": {
			"trigger.classList.add('app-control', 'app-menu-trigger')",
			"panel.classList.add('app-menu-panel')",
			"theme-option app-menu-item",
		},
	} {
		body := readAssetLF(t, file)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not expose role contract %q", file, want)
			}
		}
	}
}

func TestSharedCSSUsesSemanticColorsOnly(t *testing.T) {
	files, err := fs.Glob(Assets(), "static/css/*.css")
	if err != nil {
		t.Fatal(err)
	}
	hexColor := regexp.MustCompile(`(?i)#[0-9a-f]{3,8}\b`)
	rgbCall := regexp.MustCompile(`(?i)rgba?\(`)
	comment := regexp.MustCompile(`(?s)/\*.*?\*/`)

	for _, path := range files {
		if path == "static/css/tokens.css" || path == "static/css/theme-boot.css" {
			continue
		}
		body := comment.ReplaceAllString(readAssetLF(t, path), "")
		if match := hexColor.FindString(body); match != "" {
			t.Errorf("%s contains literal color %q; use a theme or palette custom property", path, match)
		}
		for _, loc := range rgbCall.FindAllStringIndex(body, -1) {
			rest := strings.TrimLeft(body[loc[1]:], " \t\r\n")
			if !strings.HasPrefix(rest, "var(") {
				t.Errorf("%s contains a literal rgb/rgba color; use a theme or palette custom property", path)
			}
		}
	}
}

func TestDynamicTerminalPanelsKeepAppearanceInCSS(t *testing.T) {
	for _, file := range []string{
		"static/js/terminal-templates.js",
		"static/js/terminal-panels.js",
	} {
		body := readAssetLF(t, file)
		for _, forbidden := range []string{
			"style=",
			"onmouseover=",
			"onmouseout=",
			"onmousedown=",
			"onmouseup=",
		} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s still embeds appearance or hover state with %q", file, forbidden)
			}
		}
	}
	if panels := readAssetLF(t, "static/js/terminal-panels.js"); !strings.Contains(panels, "document.body.appendChild(fileCtxMenu)") {
		t.Error("the file context menu must leave the overflow-clipped terminal panel when opened")
	}

	panels := readAssetLF(t, "static/css/panels.css")
	for _, selector := range []string{
		".shell-tab-panel",
		".shell-panel-icon-btn",
		".shell-fw-row",
		".shell-fw-direction.is-local",
		".shell-fw-del-btn",
		".shell-file-toolbar",
		".shell-file-row",
		".shell-panel-message.is-error",
	} {
		if !strings.Contains(panels, selector) {
			t.Errorf("panels.css does not define dynamic terminal role %q", selector)
		}
	}
}
