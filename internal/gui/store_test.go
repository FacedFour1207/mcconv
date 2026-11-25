package gui

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUIStore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mcconv", "settings.json")
	s := openUIStore(p)
	if got := s.inject("var m = " + storePlaceholder + ";"); got != "var m = {};" {
		t.Fatalf("empty store: %q", got)
	}
	if err := s.Set("mcconv.prefs", `{"eula":true,"x":"</script>"}`); err != nil {
		t.Fatal(err)
	}
	got := openUIStore(p).inject("var m = " + storePlaceholder + ";")
	if !strings.Contains(got, `\"eula\":true`) || strings.Contains(got, "</script>") {
		t.Fatalf("reloaded store: %q", got)
	}
}
