package webui

import (
	"strings"
	"testing"
)

func TestProductWordmarkUsesCapitalT(t *testing.T) {
	for _, tc := range []struct {
		path string
		want []string
	}{
		{
			path: "index.html",
			want: []string{"<title>Termcp</title>", "<h1>Termcp"},
		},
		{
			path: "api.html",
			want: []string{"<title>Termcp · API / MCP</title>", "<h1>Termcp · API / MCP</h1>"},
		},
	} {
		html := readAssetLF(t, tc.path)
		for _, want := range tc.want {
			if !strings.Contains(html, want) {
				t.Errorf("%s lacks capitalized product wordmark %q", tc.path, want)
			}
		}
		for _, gone := range []string{"<title>termcp", "<h1>termcp"} {
			if strings.Contains(html, gone) {
				t.Errorf("%s retains lowercase product wordmark %q", tc.path, gone)
			}
		}
	}
}
