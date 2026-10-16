package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// servedPages returns every HTML page this plugin hands to the panel.
func servedPages() map[string]string {
	return map[string]string{
		"login": renderLoginPage(),
		"quota": renderQuotaPage(),
	}
}

var (
	scriptRe = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	// Element lookups, literal ids only: $('tab-' + k) is dynamic and skipped.
	idRe = regexp.MustCompile(`(?:getElementById|\\?\$)\(['"]([^'"]+)['"]\)`)
)

func pageScript(t *testing.T, name, html string) string {
	t.Helper()
	m := scriptRe.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("%s: no inline <script> found", name)
	}
	if strings.TrimSpace(m[1]) == "" {
		t.Fatalf("%s: inline <script> is empty", name)
	}
	return m[1]
}

// TestPagesScriptParses compiles each page's inline script. A syntax error here
// is invisible to Go and silently kills the whole page: the browser drops the
// script, no handler is ever attached, and the panel looks like it ignored the
// click. This is how the quota page shipped broken once.
func TestPagesScriptParses(t *testing.T) {
	for name, html := range servedPages() {
		if _, err := goja.Compile(name+".js", pageScript(t, name, html), false); err != nil {
			t.Fatalf("%s page script does not parse: %v", name, err)
		}
	}
}

// TestPagesScriptElementsExist keeps the script and the markup it drives in
// sync: every element the script looks up must actually be in the page.
func TestPagesScriptElementsExist(t *testing.T) {
	for name, html := range servedPages() {
		src := pageScript(t, name, html)
		ids := idRe.FindAllStringSubmatch(src, -1)
		if len(ids) == 0 {
			t.Fatalf("%s: found no element lookups, the id regexp is stale", name)
		}
		for _, m := range ids {
			if !strings.Contains(html, `id="`+m[1]+`"`) {
				t.Errorf("%s: script looks up #%s but the page never defines it", name, m[1])
			}
		}
	}
}
