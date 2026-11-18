// Command genversions builds internal/translate/data/java_versions.json.gz:
// the blocks, items, biomes and entities of older Java releases, used to
// write worlds for an older game than the translation tables (for example
// Java 1.21.1). Its input is PrismarineJS/minecraft-data (MIT):
//
//	git clone --depth 1 https://github.com/PrismarineJS/minecraft-data
//	go run ./tools/genversions -mcdata minecraft-data/data
//
// Releases minecraft-data does not cover yet can be added from the game's
// own data generator (java -DbundlerMainClass=net.minecraft.data.Main -jar
// server.jar --reports) and its client jar (for the biome list):
//
//	go run ./tools/genversions -base internal/translate/data/java_versions.json.gz \
//	    -add 26.2:4903 -reports generated/reports -jar 26.2.jar
package main

import (
	"archive/zip"
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
	"mcconv/internal/translate/gen"
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
	base := flag.String("base", "", "start from this java_versions.json.gz instead of -mcdata")
	add := flag.String("add", "", "add a set from the data generator: name:dataVersion")
	reports := flag.String("reports", "", "data generator reports directory (with -add)")
	clientJar := flag.String("jar", "", "client jar of that version, for its biomes (with -add)")
	flag.Parse()
	var err error
	if *base != "" {
		err = runAdd(*base, *add, *reports, *clientJar, *out)
	} else {
		err = run(*dir, *out)
	}
	if err != nil {
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
	return write(vf, out)
}

func write(vf translate.JavaVersionFile, out string) error {
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

// runAdd adds (or replaces) one set built from the game's data generator.
func runAdd(base, spec, reports, clientJar, out string) error {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 || reports == "" || clientJar == "" {
		return fmt.Errorf("-add wants name:dataVersion, with -reports and -jar")
	}
	var dv int
	if _, err := fmt.Sscan(parts[1], &dv); err != nil {
		return fmt.Errorf("data version %q: %v", parts[1], err)
	}
	f, err := os.Open(base)
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return err
	}
	var vf translate.JavaVersionFile
	err = json.NewDecoder(zr).Decode(&vf)
	f.Close()
	if err != nil {
		return err
	}
	set := translate.JavaVersionSet{Name: parts[0], DataVersion: dv}
	if set.Blocks, err = gen.JavaBlocks(filepath.Join(reports, "blocks.json")); err != nil {
		return err
	}
	var reg map[string]struct {
		Entries map[string]any `json:"entries"`
	}
	if err := readJSON(filepath.Join(reports, "registries.json"), &reg); err != nil {
		return err
	}
	for kind, dst := range map[string]*[]string{"minecraft:item": &set.Items, "minecraft:entity_type": &set.Entities} {
		for name := range reg[kind].Entries {
			*dst = append(*dst, name)
		}
		if len(*dst) == 0 {
			return fmt.Errorf("registries.json has no %s entries", kind)
		}
		sort.Strings(*dst)
	}
	// biomes are data driven: the vanilla data pack in the client jar
	jar, err := zip.OpenReader(clientJar)
	if err != nil {
		return err
	}
	const biomeDir = "data/minecraft/worldgen/biome/"
	for _, e := range jar.File {
		rest, ok := strings.CutPrefix(e.Name, biomeDir)
		if ok && strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/") {
			set.Biomes = append(set.Biomes, "minecraft:"+strings.TrimSuffix(rest, ".json"))
		}
	}
	jar.Close()
	if len(set.Biomes) == 0 {
		return fmt.Errorf("%s has no biomes", clientJar)
	}
	sort.Strings(set.Biomes)
	sets := vf.Sets[:0]
	for _, s := range vf.Sets {
		if s.Name != set.Name {
			sets = append(sets, s)
		}
	}
	vf.Sets = append(sets, set)
	sort.Slice(vf.Sets, func(i, j int) bool { return vf.Sets[i].DataVersion < vf.Sets[j].DataVersion })
	fmt.Printf("%s: %d blocks, %d items, %d entities, %d biomes\n", set.Name, len(set.Blocks), len(set.Items), len(set.Entities), len(set.Biomes))
	return write(vf, out)
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
