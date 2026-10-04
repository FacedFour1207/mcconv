// Package update refreshes the data directory from the locally installed
// games so new Minecraft releases can be handled without rebuilding.
package update

import (
	"archive/zip"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"mcconv/internal/config"
	"mcconv/internal/translate"
)

// JavaInstall is a detected Java Edition client jar.
type JavaInstall struct {
	Name         string
	WorldVersion int
	Jar          string
}

// InstalledJava returns the stable releases in a .minecraft/versions folder
// (one per version name).
func InstalledJava(minecraftDir string) []JavaInstall {
	if minecraftDir == "" {
		minecraftDir = filepath.Join(os.Getenv("APPDATA"), ".minecraft")
	}
	jars, _ := filepath.Glob(filepath.Join(minecraftDir, "versions", "*", "*.jar"))
	seen := map[string]bool{}
	var out []JavaInstall
	for _, jar := range jars {
		v, err := readJarVersion(jar)
		if err != nil || !v.Stable || v.WorldVersion == 0 || seen[v.Name] {
			continue
		}
		seen[v.Name] = true
		out = append(out, JavaInstall{Name: v.Name, WorldVersion: v.WorldVersion, Jar: jar})
	}
	return out
}

// FindJava returns the newest stable release in a .minecraft/versions folder.
func FindJava(minecraftDir string) (*JavaInstall, error) {
	var best *JavaInstall
	for _, j := range InstalledJava(minecraftDir) {
		if best == nil || j.WorldVersion > best.WorldVersion {
			best = &j
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no Java Edition release found")
	}
	return best, nil
}

type jarVersion struct {
	Name         string `json:"name"`
	WorldVersion int    `json:"world_version"`
	Stable       bool   `json:"stable"`
}

func readJarVersion(jar string) (*jarVersion, error) {
	zr, err := zip.OpenReader(jar)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "version.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		var v jarVersion
		if err := json.NewDecoder(rc).Decode(&v); err != nil {
			return nil, err
		}
		return &v, nil
	}
	return nil, fmt.Errorf("%s has no version.json", jar)
}

// BedrockInstall is a detected Bedrock Edition installation.
type BedrockInstall struct {
	Version []int32 // major, minor, patch, revision
	Content string  // folder containing data/behavior_packs
}

// FindBedrock looks for the GDK (XboxGames) installation of Bedrock.
func FindBedrock(dir string) (*BedrockInstall, error) {
	var candidates []string
	if dir != "" {
		candidates = append(candidates, dir, filepath.Join(dir, "Content"))
	} else {
		for d := 'C'; d <= 'Z'; d++ {
			candidates = append(candidates, string(d)+`:\XboxGames\Minecraft for Windows\Content`)
		}
	}
	for _, c := range candidates {
		manifest, err := os.ReadFile(filepath.Join(c, "appxmanifest.xml"))
		if err != nil {
			continue
		}
		v, err := parseAppxVersion(manifest)
		if err != nil {
			return nil, err
		}
		return &BedrockInstall{Version: v, Content: c}, nil
	}
	return nil, fmt.Errorf("Bedrock Edition installation not found (use -bedrock-dir)")
}

var identityVersion = regexp.MustCompile(`<Identity[^>]*\sVersion="(\d+)\.(\d+)\.(\d+)\.\d+"`)

// parseAppxVersion turns "1.26.5203.0" into 1.26.52.3.
func parseAppxVersion(manifest []byte) ([]int32, error) {
	m := identityVersion.FindSubmatch(manifest)
	if m == nil {
		return nil, fmt.Errorf("appxmanifest.xml: version not found")
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	build, _ := strconv.Atoi(string(m[3]))
	return []int32{int32(major), int32(minor), int32(build / 100), int32(build % 100)}, nil
}

// readArchive parses Bedrock's .brarchive container.
func readArchive(path string) (map[string][]byte, error) {
	d, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(d) < 16 {
		return nil, fmt.Errorf("%s: too short", path)
	}
	n := int(binary.LittleEndian.Uint32(d[8:]))
	off := 16
	type entry struct {
		name string
		o, l int
	}
	var entries []entry
	for i := 0; i < n; i++ {
		if off+256 > len(d) {
			return nil, fmt.Errorf("%s: truncated index", path)
		}
		ln := int(d[off])
		name := string(d[off+1 : off+1+ln])
		o := int(binary.LittleEndian.Uint32(d[off+248:]))
		l := int(binary.LittleEndian.Uint32(d[off+252:]))
		entries = append(entries, entry{name, o, l})
		off += 256
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if off+e.o+e.l > len(d) {
			continue
		}
		out[e.name] = d[off+e.o : off+e.o+e.l]
	}
	return out, nil
}

// EntityDef mirrors convert's embedded bedrock_entities.json entries.
type EntityDef struct {
	Spawn  []string `json:"spawn"`
	Born   []string `json:"born"`
	Health float64  `json:"health,omitempty"`
}

func packVersion(name string) []int {
	if name == "vanilla" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(strings.TrimPrefix(name, "vanilla_"), ".") {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}

func lessInts(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// ExportBedrockEntities reads the vanilla behaviour packs (layered by version)
// and extracts the component groups each entity gets when spawned or born.
func ExportBedrockEntities(content string) (map[string]EntityDef, error) {
	packsDir := filepath.Join(content, "data", "behavior_packs")
	entries, err := os.ReadDir(packsDir)
	if err != nil {
		return nil, err
	}
	var packs []string
	for _, e := range entries {
		if e.Name() == "vanilla" || strings.HasPrefix(e.Name(), "vanilla_1.") {
			packs = append(packs, e.Name())
		}
	}
	sort.Slice(packs, func(i, j int) bool { return lessInts(packVersion(packs[i]), packVersion(packs[j])) })
	files := map[string][]byte{}
	for _, p := range packs {
		arc, err := readArchive(filepath.Join(packsDir, p, "__brarchive", "entities.brarchive"))
		if err != nil {
			continue
		}
		for k, v := range arc {
			files[k] = v
		}
	}
	out := map[string]EntityDef{}
	for _, raw := range files {
		var doc struct {
			Entity struct {
				Description struct {
					Identifier string `json:"identifier"`
				} `json:"description"`
				Components map[string]any `json:"components"`
				Events     map[string]any `json:"events"`
			} `json:"minecraft:entity"`
		}
		if json.Unmarshal(raw, &doc) != nil || doc.Entity.Description.Identifier == "" {
			continue
		}
		ev := doc.Entity.Events
		def := EntityDef{
			Spawn: collectGroups(ev, ev["minecraft:entity_spawned"], 0),
			Born:  collectGroups(ev, ev["minecraft:entity_born"], 0),
		}
		if h, ok := doc.Entity.Components["minecraft:health"].(map[string]any); ok {
			mx := h["max"]
			if mx == nil {
				mx = h["value"]
			}
			if r, ok := mx.(map[string]any); ok {
				mx = r["range_max"]
				if mx == nil {
					mx = r["range_min"]
				}
			}
			if f, ok := mx.(float64); ok {
				def.Health = f
			}
		}
		out[doc.Entity.Description.Identifier] = def
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no entity definitions found in %s", packsDir)
	}
	return out, nil
}

// collectGroups follows an event (add / randomize / sequence / trigger) and
// returns the component groups it adds.
func collectGroups(events map[string]any, ev any, depth int) []string {
	m, ok := ev.(map[string]any)
	if !ok || depth > 8 {
		return []string{}
	}
	var groups []string
	if add, ok := m["add"].(map[string]any); ok {
		for _, g := range asList(add["component_groups"]) {
			if s, ok := g.(string); ok {
				groups = append(groups, s)
			}
		}
	}
	if opts := asList(m["randomize"]); len(opts) > 0 {
		best, bestW := opts[0], -1.0
		for _, o := range opts {
			w := 1.0
			if om, ok := o.(map[string]any); ok {
				if f, ok := om["weight"].(float64); ok {
					w = f
				}
			}
			if w > bestW {
				best, bestW = o, w
			}
		}
		groups = append(groups, collectGroups(events, best, depth+1)...)
	}
	filteredTaken := false
	for _, step := range asList(m["sequence"]) {
		if sm, ok := step.(map[string]any); ok {
			if _, hasFilter := sm["filters"]; hasFilter {
				if filteredTaken {
					continue
				}
				filteredTaken = true
			}
		}
		groups = append(groups, collectGroups(events, step, depth+1)...)
	}
	if t, ok := m["trigger"]; ok {
		name, _ := t.(string)
		if tm, ok := t.(map[string]any); ok {
			name, _ = tm["event"].(string)
		}
		if e, ok := events[name]; ok {
			groups = append(groups, collectGroups(events, e, depth+1)...)
		}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, g := range groups {
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// Options for Run.
type Options struct {
	DataDir      string
	MinecraftDir string // Java .minecraft folder
	BedrockDir   string // Bedrock Content folder
	// Offline skips downloading new translation tables.
	Offline bool
	Log     io.Writer
}

// Run refreshes versions.json, bedrock_entities.json and tables.json.gz.
func Run(o Options) error {
	logf := func(f string, a ...any) { fmt.Fprintf(o.Log, f+"\n", a...) }
	cfg, err := config.Load(o.DataDir)
	if err != nil {
		return err
	}
	v := cfg.Versions
	// block tables first: they decide which Java version can be written
	tablesJava, tablesDV := "", 0
	if !o.Offline {
		if r, err := UpdateTables(o.DataDir, o.MinecraftDir, logf); err != nil {
			logf("Block tables: not updated: %v", err)
		} else {
			logf("Block tables: Java %s / Bedrock %s -> %s", r.JavaVersion, r.BedrockVersion, r.Path)
			tablesJava, tablesDV = r.JavaVersion, r.JavaDataVersion
		}
	}
	if tablesDV == 0 {
		src := translate.BuiltinTables()
		if b, err := os.ReadFile(filepath.Join(o.DataDir, "tables.json.gz")); err == nil {
			src = b
		}
		if t, err := translate.Parse(src); err == nil {
			tablesJava, tablesDV = t.JavaVersion, t.JavaDataVersion
		}
	}
	// Java worlds are written for the tables' version (newer games upgrade
	// them), or for the installed game if that is older
	if j, err := FindJava(o.MinecraftDir); err != nil {
		logf("Java:    %v", err)
		if tablesDV > 0 {
			v.JavaName, v.JavaDataVersion = tablesJava, tablesDV
		}
	} else {
		logf("Java:    newest installed %s (DataVersion %d)", j.Name, j.WorldVersion)
		v.JavaName, v.JavaDataVersion = j.Name, j.WorldVersion
		if tablesDV > 0 && tablesDV < j.WorldVersion {
			v.JavaName, v.JavaDataVersion = tablesJava, tablesDV
		}
	}
	logf("Java worlds will be written as %s (DataVersion %d)", v.JavaName, v.JavaDataVersion)
	be, err := FindBedrock(o.BedrockDir)
	if err != nil {
		logf("Bedrock: %v (kept %v)", err, v.BedrockVersion)
	} else {
		v.BedrockVersion = be.Version
		logf("Bedrock: %d.%d.%d.%d from %s", be.Version[0], be.Version[1], be.Version[2], be.Version[3], be.Content)
		defs, err := ExportBedrockEntities(be.Content)
		if err != nil {
			logf("Bedrock entities: %v", err)
		} else if err := config.WriteJSON(filepath.Join(o.DataDir, "bedrock_entities.json"), defs); err != nil {
			return err
		} else {
			logf("Bedrock entities: %d definitions", len(defs))
		}
	}
	if err := config.WriteJSON(filepath.Join(o.DataDir, "versions.json"), v); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(o.DataDir, "overrides.json")); err != nil {
		empty := config.Overrides{
			Blocks:   config.Mapping{JavaToBedrock: map[string]string{}, BedrockToJava: map[string]string{}},
			Items:    config.Mapping{JavaToBedrock: map[string]string{}, BedrockToJava: map[string]string{}},
			Entities: config.Mapping{JavaToBedrock: map[string]string{}, BedrockToJava: map[string]string{}},
		}
		if err := config.WriteJSON(filepath.Join(o.DataDir, "overrides.json"), empty); err != nil {
			return err
		}
	}
	logf("data written to %s", o.DataDir)
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
