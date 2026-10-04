package gui

import (
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mcconv/internal/bedrock"
	"mcconv/internal/convert"
	"mcconv/internal/java"
)

// WorldInfo describes a world for the world list.
type WorldInfo struct {
	Path       string `json:"path"`
	Folder     string `json:"folder"`
	Name       string `json:"name"`
	Edition    string `json:"edition"` // "java" | "bedrock"
	Version    string `json:"version"`
	Mode       string `json:"mode"`
	Icon       string `json:"icon"`       // data URL or ""
	LastPlayed int64  `json:"lastPlayed"` // unix ms
}

// javaSavesDir is the default Java launcher's saves folder.
func javaSavesDir() string {
	return filepath.Join(os.Getenv("APPDATA"), ".minecraft", "saves")
}

// bedrockWorldDirs returns the minecraftWorlds folders of every Bedrock
// profile (GDK builds keep one per user; older UWP builds one per install).
func bedrockWorldDirs() []string {
	var out []string
	users, _ := filepath.Glob(filepath.Join(os.Getenv("APPDATA"), "Minecraft Bedrock", "Users", "*", "games", "com.mojang", "minecraftWorlds"))
	out = append(out, users...)
	uwp := filepath.Join(os.Getenv("LOCALAPPDATA"), "Packages", "Microsoft.MinecraftUWP_8wekyb3d8bbwe", "LocalState", "games", "com.mojang", "minecraftWorlds")
	if st, err := os.Stat(uwp); err == nil && st.IsDir() {
		out = append(out, uwp)
	}
	return out
}

// listWorlds returns the worlds found in the game folders, newest first.
func listWorlds() (javaWorlds, bedrockWorlds []WorldInfo) {
	scan := func(root string) []WorldInfo {
		var out []WorldInfo
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if w, err := inspectWorld(filepath.Join(root, e.Name())); err == nil {
				out = append(out, *w)
			}
		}
		return out
	}
	javaWorlds = scan(javaSavesDir())
	for _, d := range bedrockWorldDirs() {
		bedrockWorlds = append(bedrockWorlds, scan(d)...)
	}
	for _, l := range [][]WorldInfo{javaWorlds, bedrockWorlds} {
		sort.SliceStable(l, func(i, j int) bool { return l[i].LastPlayed > l[j].LastPlayed })
	}
	if javaWorlds == nil {
		javaWorlds = []WorldInfo{}
	}
	if bedrockWorlds == nil {
		bedrockWorlds = []WorldInfo{}
	}
	return
}

var gameModes = map[int64]string{0: "サバイバル", 1: "クリエイティブ", 2: "アドベンチャー", 3: "スペクテイター", 6: "スペクテイター"}

// inspectWorld reads the summary of a world folder.
func inspectWorld(dir string) (*WorldInfo, error) {
	edition, err := convert.Detect(dir)
	if err != nil {
		return nil, err
	}
	w := &WorldInfo{Path: dir, Folder: filepath.Base(dir), Edition: edition, Name: filepath.Base(dir)}
	if st, err := os.Stat(filepath.Join(dir, "level.dat")); err == nil {
		w.LastPlayed = st.ModTime().UnixMilli()
	}
	if edition == "java" {
		root, err := java.ReadNBTFile(filepath.Join(dir, "level.dat"))
		if err != nil {
			return nil, err
		}
		data, _ := root.Compound("Data")
		if n, ok := data.String("LevelName"); ok && n != "" {
			w.Name = n
		}
		if v, ok := data.Compound("Version"); ok {
			w.Version, _ = v.String("Name")
		}
		if t, ok := data.Number("LastPlayed"); ok && t > 0 {
			w.LastPlayed = t
		}
		gt, _ := data.Number("GameType")
		w.Mode = gameModes[gt]
		if h, ok := data.Number("hardcore"); ok && h != 0 {
			w.Mode = "ハードコア"
		}
		w.Icon = dataURL(filepath.Join(dir, "icon.png"), "image/png")
	} else {
		if b, err := os.ReadFile(filepath.Join(dir, "levelname.txt")); err == nil && strings.TrimSpace(string(b)) != "" {
			w.Name = strings.TrimSpace(string(b))
		}
		if root, err := bedrock.ReadLevelDat(filepath.Join(dir, "level.dat")); err == nil {
			if t, ok := root.Number("LastPlayed"); ok && t > 0 {
				w.LastPlayed = t * 1000
			}
			gt, _ := root.Number("GameType")
			w.Mode = gameModes[gt]
			if l, ok := root.List("lastOpenedWithVersion"); ok {
				var parts []string
				for i, v := range l.Items {
					if i >= 3 {
						break
					}
					parts = append(parts, fmt.Sprint(v))
				}
				w.Version = strings.Join(parts, ".")
			}
		}
		w.Icon = dataURL(filepath.Join(dir, "world_icon.jpeg"), "image/jpeg")
	}
	return w, nil
}

func dataURL(path, mime string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 2<<20 {
		return ""
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

// dirSize sums the file sizes below dir.
func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// Destination is a folder the converted world can be saved in.
type Destination struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Game  bool   `json:"game"` // the game's own worlds folder
}

// destinations lists the game folders for the target edition; the first
// one is the default.
func destinations(target string) []Destination {
	out := []Destination{}
	if target == "java" {
		if st, err := os.Stat(javaSavesDir()); err == nil && st.IsDir() {
			out = append(out, Destination{Path: javaSavesDir(), Label: "Java版 のワールド (.minecraft\\saves)", Game: true})
		}
		return out
	}
	type cand struct {
		d      Destination
		newest time.Time
	}
	var cs []cand
	for _, dir := range bedrockWorldDirs() {
		entries, _ := os.ReadDir(dir)
		var newest time.Time
		n := 0
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			n++
			if fi, err := e.Info(); err == nil && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		label := "統合版 のワールド"
		if strings.Contains(dir, "Packages") {
			label += " (旧 UWP 版)"
		} else {
			user := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(dir))))
			if len(user) > 6 {
				user = user[:6] + "…"
			}
			label += fmt.Sprintf(" (プロファイル %s)", user)
		}
		label += fmt.Sprintf(" — %d 個", n)
		cs = append(cs, cand{Destination{Path: dir, Label: label, Game: true}, newest})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].newest.After(cs[j].newest) })
	for _, c := range cs {
		out = append(out, c.d)
	}
	return out
}
