// Command genversions builds internal/translate/data/java_versions.json.gz:
// the blocks, items, biomes and entities of older Java releases, used to
// write worlds for an older game than the translation tables (for example
// Java 1.21.1). Its input is PrismarineJS/minecraft-data (MIT):
//
//	git clone --depth 1 https://github.com/PrismarineJS/minecraft-data
//	go run ./tools/genversions -mcdata minecraft-data/data
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mcconv/internal/translate"
)

// sets are the releases whose content is stored; a target version uses the
// newest set not newer than itself. 1.21.2 and 1.21.3 use the 1.21.1 set:
// their new blocks (the pale garden) were still an experiment.
var sets = []struct {
	name string
	dv   int
}{
	{"1.21.1", 3955}, {"1.21.4", 4189}, {"1.21.5", 4325}, {"1.21.6", 4435},
	{"1.21.9", 4554}, {"1.21.11", 4671}, {"26.1", 4786},
}

// first and last releases listed (older releases are not supported)
const minDataVersion = 3955

type mdState struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Num    int      `json:"num_values"`
	Values []string `json:"values"`
}

type mdBlock struct {
	Name     string    `json:"name"`
	Default  int       `json:"defaultState"`
	MinState int       `json:"minStateId"`
	States   []mdState `json:"states"`
}

type named struct {
	Name string `json:"name"`
}

func main() {
	dir := flag.String("mcdata", "", "minecraft-data/data directory")
	out := flag.String("o", "internal/translate/data/java_versions.json.gz", "output file")
	flag.Parse()
	if err := run(*dir, *out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func run(dir, out string) error {
	var paths struct {
		PC map[string]map[string]string `json:"pc"`
	}
	if err := readJSON(filepath.Join(dir, "dataPaths.json"), &paths); err != nil {
		return err
	}
	file := func(version, kind string) string {
		return filepath.Join(dir, paths.PC[version][kind], kind+".json")
	}
	vf := translate.JavaVersionFile{Format: translate.JavaVersionFormat}
	var proto []struct {
		Name        string `json:"minecraftVersion"`
		DataVersion int    `json:"dataVersion"`
		Release     string `json:"releaseType"`
	}
	if err := readJSON(filepath.Join(dir, "pc", "common", "protocolVersions.json"), &proto); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range proto {
		if p.Release == "release" && p.DataVersion >= minDataVersion && !seen[p.Name] {
			seen[p.Name] = true
			vf.Releases = append(vf.Releases, translate.JavaRelease{Name: p.Name, DataVersion: p.DataVersion})
		}
	}
	sort.Slice(vf.Releases, func(i, j int) bool { return vf.Releases[i].DataVersion < vf.Releases[j].DataVersion })
	for _, s := range sets {
		set := translate.JavaVersionSet{Name: s.name, DataVersion: s.dv}
		var blocks []mdBlock
		if err := readJSON(file(s.name, "blocks"), &blocks); err != nil {
			return err
		}
		for _, b := range blocks {
			d := translate.JavaBlockDef{Name: "minecraft:" + b.Name, Default: b.Default - b.MinState}
			for _, st := range b.States {
				vals := st.Values
				switch {
				case st.Type == "bool":
					vals = []string{"true", "false"}
				case len(vals) == 0:
					for i := range st.Num {
						vals = append(vals, fmt.Sprint(i))
					}
				}
				d.Props = append(d.Props, translate.JavaProp{Name: st.Name, Values: vals})
			}
			set.Blocks = append(set.Blocks, d)
		}
		for kind, dst := range map[string]*[]string{"items": &set.Items, "biomes": &set.Biomes, "entities": &set.Entities} {
			var l []named
			if err := readJSON(file(s.name, kind), &l); err != nil {
				return err
			}
			for _, e := range l {
				*dst = append(*dst, "minecraft:"+strings.TrimPrefix(e.Name, "minecraft:"))
			}
			sort.Strings(*dst)
		}
		if s.dv < 4067 { // bundles were an experiment until 1.21.2
			set.Items = remove(set.Items, "minecraft:bundle")
		}
		vf.Sets = append(vf.Sets, set)
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err := json.NewEncoder(zw).Encode(vf); err != nil {
		return err
	}
	zw.Close()
	if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes, %d releases, %d sets)\n", out, buf.Len(), len(vf.Releases), len(vf.Sets))
	return nil
}

func remove(l []string, s string) []string {
	out := l[:0]
	for _, v := range l {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
