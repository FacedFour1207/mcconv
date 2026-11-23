package gui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Japanese block names for the replacement suggestions. They are read from
// the Java Edition language file the launcher has downloaded
// (.minecraft/assets), never bundled. Without it only ids are shown.

var jaNames struct {
	once sync.Once
	m    map[string]string // "block.minecraft.stone" -> "石"
}

func javaLangJA() map[string]string {
	jaNames.once.Do(func() {
		assets := filepath.Join(os.Getenv("APPDATA"), ".minecraft", "assets")
		idx, _ := filepath.Glob(filepath.Join(assets, "indexes", "*.json"))
		// newest index first
		sort.Slice(idx, func(i, j int) bool { return modTime(idx[i]) > modTime(idx[j]) })
		for _, p := range idx {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var ix struct {
				Objects map[string]struct {
					Hash string `json:"hash"`
				} `json:"objects"`
			}
			if json.Unmarshal(b, &ix) != nil {
				continue
			}
			o, ok := ix.Objects["minecraft/lang/ja_jp.json"]
			if !ok || len(o.Hash) < 2 {
				continue
			}
			lb, err := os.ReadFile(filepath.Join(assets, "objects", o.Hash[:2], o.Hash))
			if err != nil {
				continue
			}
			var m map[string]string
			if json.Unmarshal(lb, &m) == nil && len(m) > 0 {
				jaNames.m = m
				return
			}
		}
	})
	return jaNames.m
}

func modTime(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

// bedrockOnlyJA names Bedrock blocks that have no Java counterpart.
var bedrockOnlyJA = map[string]string{
	"invisible_bedrock": "透明な岩盤",
	"border_block":      "境界ブロック",
	"allow":             "許可ブロック",
	"deny":              "拒否ブロック",
}

// BlockInfo is a block id with its Japanese name ("" if unknown).
type BlockInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

func javaName(lang map[string]string, id string) string {
	ns, name, ok := strings.Cut(id, ":")
	if !ok {
		ns, name = "minecraft", id
	}
	return lang["block."+ns+"."+name]
}
