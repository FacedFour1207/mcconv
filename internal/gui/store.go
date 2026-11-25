package gui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The page is loaded from a string, so it has no origin and WebView2 does
// not keep its localStorage. The few settings the page remembers (theme,
// conversion settings) are kept in a small JSON file instead: they are put
// into the page before it is loaded and saved through mcStoreSet.

const storePlaceholder = "/*MCCONV_STORE*/null"

type uiStore struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

func openUIStore(path string) *uiStore {
	s := &uiStore{path: path, m: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &s.m)
		if s.m == nil {
			s.m = map[string]string{}
		}
	}
	return s
}

// inject puts the saved values into the page.
func (s *uiStore) inject(html string) string {
	s.mu.Lock()
	b, err := json.Marshal(s.m) // escapes < and >, safe inside <script>
	s.mu.Unlock()
	if err != nil {
		b = []byte("{}")
	}
	return strings.Replace(html, storePlaceholder, string(b), 1)
}

// Set saves one value.
func (s *uiStore) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(key) > 64 || len(value) > 1<<20 {
		return nil
	}
	s.m[key] = value
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
