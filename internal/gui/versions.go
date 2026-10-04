package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"mcconv/internal/translate"
	"mcconv/internal/update"
)

// Version lists for the settings: Bedrock releases from Bedrock-OSS/BDS-Versions,
// Java releases (with their DataVersion) from PrismarineJS/minecraft-data,
// plus whatever is installed. Lists are cached in the data directory so the
// settings also work offline.

const (
	bedrockVersionsURL = "https://raw.githubusercontent.com/Bedrock-OSS/BDS-Versions/main/versions.json"
	javaVersionsURL    = "https://raw.githubusercontent.com/PrismarineJS/minecraft-data/master/data/pc/common/protocolVersions.json"
)

// JavaOption is a selectable Java Edition version.
type JavaOption struct {
	Name        string `json:"name"`
	DataVersion int    `json:"dataVersion"`
	Installed   bool   `json:"installed"`
	OK          bool   `json:"ok"` // not older than the block tables
}

// BedrockOption is a selectable Bedrock Edition version.
type BedrockOption struct {
	Version   string `json:"version"`
	Installed bool   `json:"installed"`
	OK        bool   `json:"ok"`
}

type versionCache struct {
	Java    []JavaOption `json:"java"`
	Bedrock []string     `json:"bedrock"`
}

// versionLists builds the lists and emits them as a "versions" event.
func (a *App) versionLists() {
	t := a.tables()
	cachePath := filepath.Join(a.dataDir, "version_lists.json")
	var cache versionCache
	if b, err := os.ReadFile(cachePath); err == nil {
		json.Unmarshal(b, &cache)
	}
	online := false
	client := &http.Client{Timeout: 8 * time.Second}
	if j, err := fetchJavaVersions(client); err == nil && len(j) > 0 {
		cache.Java, online = j, true
	}
	if b, err := fetchBedrockVersions(client); err == nil && len(b) > 0 {
		cache.Bedrock, online = b, true
	}
	if online {
		if data, err := json.Marshal(cache); err == nil {
			os.MkdirAll(a.dataDir, 0o755)
			os.WriteFile(cachePath, data, 0o644)
		}
	}

	// Java: known releases + installed versions + the tables' version
	java := map[string]JavaOption{}
	for _, o := range cache.Java {
		java[o.Name] = o
	}
	for _, inst := range update.InstalledJava("") {
		java[inst.Name] = JavaOption{Name: inst.Name, DataVersion: inst.WorldVersion, Installed: true}
	}
	if t != nil {
		if _, ok := java[t.JavaVersion]; !ok {
			java[t.JavaVersion] = JavaOption{Name: t.JavaVersion, DataVersion: t.JavaDataVersion}
		}
	}
	var jl []JavaOption
	for _, o := range java {
		o.OK = t == nil || o.DataVersion >= t.JavaDataVersion
		jl = append(jl, o)
	}
	sort.Slice(jl, func(i, j int) bool { return jl[i].DataVersion > jl[j].DataVersion })

	// Bedrock: known releases + the installed version
	bedrock := map[string]BedrockOption{}
	for _, v := range cache.Bedrock {
		bedrock[v] = BedrockOption{Version: v}
	}
	if be, err := update.FindBedrock(""); err == nil {
		v := joinVersion(be.Version)
		bedrock[v] = BedrockOption{Version: v, Installed: true}
	}
	cur := a.versions().BedrockVersion
	if _, ok := bedrock[cur]; !ok && cur != "" {
		bedrock[cur] = BedrockOption{Version: cur}
	}
	var bl []BedrockOption
	for _, o := range bedrock {
		o.OK = t == nil || !olderThan(o.Version, t.BedrockVersion)
		bl = append(bl, o)
	}
	sort.Slice(bl, func(i, j int) bool { return olderThan(bl[j].Version, bl[i].Version) })

	ev := map[string]any{"t": "versions", "java": jl, "bedrock": bl, "online": online}
	if t != nil {
		ev["tablesJava"], ev["tablesBedrock"] = t.JavaVersion, t.BedrockVersion
	}
	a.emit(ev)
}

// tables returns the tables conversions will use (data directory or built-in).
func (a *App) tables() *translate.Tables {
	src := translate.BuiltinTables()
	if b, err := os.ReadFile(filepath.Join(a.dataDir, "tables.json.gz")); err == nil {
		src = b
	}
	t, err := translate.Parse(src)
	if err != nil {
		return nil
	}
	return t
}

func fetchJavaVersions(c *http.Client) ([]JavaOption, error) {
	resp, err := c.Get(javaVersionsURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	var list []struct {
		Name        string `json:"minecraftVersion"`
		DataVersion int    `json:"dataVersion"`
		Type        string `json:"releaseType"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	var out []JavaOption
	for _, v := range list {
		if v.Type == "release" && v.DataVersion > 0 {
			out = append(out, JavaOption{Name: v.Name, DataVersion: v.DataVersion})
		}
	}
	return out, nil
}

func fetchBedrockVersions(c *http.Client) ([]string, error) {
	resp, err := c.Get(bedrockVersionsURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	var d struct {
		Windows struct {
			Stable   string   `json:"stable"`
			Versions []string `json:"versions"`
		} `json:"windows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	return append(d.Windows.Versions, d.Windows.Stable), nil
}

func joinVersion(v []int32) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(int(n))
	}
	return strings.Join(parts, ".")
}

// olderThan compares dotted versions numerically (missing parts are 0).
func olderThan(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}
