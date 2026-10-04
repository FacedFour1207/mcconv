// Package gen builds the translation table file (translate.TableFile) from
// its freely licensed sources; see tools/gendata.
package gen

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"mcconv/internal/nbt"
	"mcconv/internal/translate"
)

// Options name the inputs.
type Options struct {
	Report          string // Java data generator reports/blocks.json
	JavaVersion     string
	JavaDataVersion int
	GeyserDir       string // GeyserMC/mappings: blocks.nbt, items.json, biomes.json, README.md
	PmmpDir         string // pmmp/BedrockData: biome_id_map.json, banner_patterns.json
	UpgradeDir      string // BedrockBlockUpgradeSchema/nbt_upgrade_schema
}

type reportBlock struct {
	Properties map[string][]string `json:"properties"`
	States     []struct {
		ID         int               `json:"id"`
		Properties map[string]string `json:"properties"`
		Default    bool              `json:"default"`
	} `json:"states"`
}

// Build returns the gzip compressed table file.
func Build(o Options) ([]byte, error) {
	tf := &translate.TableFile{Format: translate.TableFormat, JavaVersion: o.JavaVersion, JavaDataVersion: o.JavaDataVersion}

	// --- Java block states (ordered by state id) ---
	raw, err := os.ReadFile(o.Report)
	if err != nil {
		return nil, err
	}
	var report map[string]reportBlock
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, fmt.Errorf("%s: %w", o.Report, err)
	}
	// the report's property order is lost by the map decoding above; read it
	// again in document order
	propOrder, err := reportPropertyOrder(raw)
	if err != nil {
		return nil, err
	}
	type blk struct {
		name  string
		first int
		def   reportBlock
	}
	var blocks []blk
	total := 0
	for name, b := range report {
		first := b.States[0].ID
		for _, s := range b.States {
			first = min(first, s.ID)
		}
		blocks = append(blocks, blk{name, first, b})
		total += len(b.States)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].first < blocks[j].first })
	next := 0
	for _, b := range blocks {
		if b.first != next {
			return nil, fmt.Errorf("java states are not contiguous at %s (%d, want %d)", b.name, b.first, next)
		}
		def := translate.JavaBlockDef{Name: b.name}
		for _, p := range propertyOrder(b.def, propOrder[b.name]) {
			def.Props = append(def.Props, translate.JavaProp{Name: p, Values: b.def.Properties[p]})
		}
		// state ids enumerate the properties in order, the last one fastest
		for _, s := range b.def.States {
			id, stride := 0, 1
			for i := len(def.Props) - 1; i >= 0; i-- {
				p := def.Props[i]
				vi := indexOf(p.Values, s.Properties[p.Name])
				if vi < 0 {
					return nil, fmt.Errorf("%s: unknown value %q of %s", b.name, s.Properties[p.Name], p.Name)
				}
				id += vi * stride
				stride *= len(p.Values)
			}
			if b.first+id != s.ID {
				return nil, fmt.Errorf("%s: state %v has id %d, expected %d", b.name, s.Properties, s.ID, b.first+id)
			}
			if s.Default {
				def.Default = id
			}
		}
		tf.JavaBlocks = append(tf.JavaBlocks, def)
		next += len(b.def.States)
	}

	// --- Java -> Bedrock blocks (GeyserMC, indexed by Java state id) ---
	gz, err := os.ReadFile(filepath.Join(o.GeyserDir, "blocks.nbt"))
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	nbtData, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	_, root, _, err := nbt.Decode(nbtData, nbt.BigEndian)
	if err != nil {
		return nil, fmt.Errorf("blocks.nbt: %w", err)
	}
	rc, _ := root.(nbt.Compound)
	mappings, ok := rc.List("bedrock_mappings")
	if !ok {
		return nil, fmt.Errorf("blocks.nbt: no bedrock_mappings")
	}
	if len(mappings.Items) != total {
		return nil, fmt.Errorf("GeyserMC mappings have %d Java states but the %s report has %d: the versions do not match", len(mappings.Items), o.JavaVersion, total)
	}
	nameIdx, stateIdx := map[string]int{}, map[string]int{}
	intern := func(m map[string]int, list *[]string, s string) int32 {
		if i, ok := m[s]; ok {
			return int32(i)
		}
		m[s] = len(*list)
		*list = append(*list, s)
		return int32(m[s])
	}
	id := 0
	for _, b := range tf.JavaBlocks {
		n := 1
		for _, p := range b.Props {
			n *= len(p.Values)
		}
		for range n {
			e, _ := mappings.Items[id].(nbt.Compound)
			name, ok := e.String("bedrock_identifier")
			if !ok {
				name = b.Name
			} else if !strings.Contains(name, ":") {
				name = "minecraft:" + name
			}
			st, _ := e.Compound("state")
			if st == nil {
				st = nbt.Compound{}
			}
			tf.J2B = append(tf.J2B, intern(nameIdx, &tf.BedrockNames, name), intern(stateIdx, &tf.BedrockStates, nbt.ToSNBT(st)))
			id++
		}
	}
	if readme, err := os.ReadFile(filepath.Join(o.GeyserDir, "README.md")); err == nil {
		if m := regexp.MustCompile(`Java Edition ([0-9]+(?:\.[0-9]+)*) and Minecraft: Bedrock Edition ([0-9]+(?:\.[0-9]+)*)`).FindSubmatch(readme); m != nil {
			if o.JavaVersion != "" && string(m[1]) != o.JavaVersion {
				return nil, fmt.Errorf("GeyserMC mappings are for Java %s, the report is %s", m[1], o.JavaVersion)
			}
			tf.BedrockVersion = string(m[2])
		}
	}

	// --- items and biomes ---
	var items map[string]struct {
		ID    string `json:"bedrock_identifier"`
		Data  int    `json:"bedrock_data"`
		Block bool   `json:"is_block"`
	}
	if err := readJSON(filepath.Join(o.GeyserDir, "items.json"), &items); err != nil {
		return nil, err
	}
	tf.Items = map[string]translate.ItemMap{}
	for j, it := range items {
		tf.Items[j] = translate.ItemMap{ID: it.ID, Data: it.Data, Block: it.Block}
	}
	var biomes map[string]struct {
		ID int `json:"bedrock_id"`
	}
	if err := readJSON(filepath.Join(o.GeyserDir, "biomes.json"), &biomes); err != nil {
		return nil, err
	}
	tf.JavaBiomes = map[string]int{}
	for j, b := range biomes {
		tf.JavaBiomes[j] = b.ID
	}
	if err := readJSON(filepath.Join(o.PmmpDir, "biome_id_map.json"), &tf.BedrockBiomes); err != nil {
		return nil, err
	}
	var patterns map[string]struct {
		ID string `json:"id"`
	}
	if err := readJSON(filepath.Join(o.PmmpDir, "banner_patterns.json"), &patterns); err != nil {
		return nil, err
	}
	tf.BannerPatterns = map[string]string{}
	for n, p := range patterns {
		tf.BannerPatterns[n] = p.ID
	}

	var entities map[string]int
	if err := readJSON(filepath.Join(o.PmmpDir, "entity_id_map.json"), &entities); err != nil {
		return nil, err
	}
	for id := range entities {
		tf.BedrockEntities = append(tf.BedrockEntities, id)
	}
	sort.Strings(tf.BedrockEntities)

	// --- Bedrock block state upgrade schemas ---
	files, err := filepath.Glob(filepath.Join(o.UpgradeDir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, b); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		tf.UpgradeSchemas = append(tf.UpgradeSchemas, json.RawMessage(buf.Bytes()))
	}
	if len(tf.UpgradeSchemas) > 0 {
		var last struct{ MaxVersionMajor, MaxVersionMinor, MaxVersionPatch, MaxVersionRevision int32 }
		json.Unmarshal(tf.UpgradeSchemas[len(tf.UpgradeSchemas)-1], &last)
		tf.BedrockBlockVersion = last.MaxVersionMajor<<24 | last.MaxVersionMinor<<16 | last.MaxVersionPatch<<8 | last.MaxVersionRevision
	}
	tf.Sources = []string{
		"Java block states: Minecraft data generator report (" + o.JavaVersion + ")",
		"Block, item and biome mappings: GeyserMC/mappings (MIT License, https://github.com/GeyserMC/mappings)",
		"Bedrock biome ids, banner patterns, entity ids: pmmp/BedrockData (CC0, https://github.com/pmmp/BedrockData)",
		"Block state upgrade schemas: BedrockBlockUpgradeSchema (CC0, https://github.com/opencollab-incubator/BedrockBlockUpgradeSchema)",
	}

	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err := json.NewEncoder(zw).Encode(tf); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// propertyOrder sorts the properties by how fast they vary in the state
// ids (the report lists them in another order): slowest first.
func propertyOrder(b reportBlock, names []string) []string {
	ids := map[string]int{}
	key := func(p map[string]string) string {
		var sb strings.Builder
		for _, n := range names {
			sb.WriteString(n + "=" + p[n] + ",")
		}
		return sb.String()
	}
	base := b.States[0]
	for _, s := range b.States {
		ids[key(s.Properties)] = s.ID
		if s.ID < base.ID {
			base = s
		}
	}
	stride := map[string]int{}
	for _, n := range names {
		vals := b.Properties[n]
		i := indexOf(vals, base.Properties[n])
		next := map[string]string{}
		for k, v := range base.Properties {
			next[k] = v
		}
		next[n] = vals[(i+1)%len(vals)]
		stride[n] = ids[key(next)] - base.ID
	}
	out := append([]string(nil), names...)
	sort.SliceStable(out, func(i, j int) bool { return stride[out[i]] > stride[out[j]] })
	return out
}

// reportPropertyOrder returns each block's property names in the order of
// the report ("properties" objects keep the game's declaration order).
func reportPropertyOrder(raw []byte) (map[string][]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	out := map[string][]string{}
	if _, err := dec.Token(); err != nil { // {
		return nil, err
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name := t.(string)
		var body map[string]json.RawMessage
		if err := dec.Decode(&body); err != nil {
			return nil, err
		}
		if p, ok := body["properties"]; ok {
			keys, err := objectKeys(p)
			if err != nil {
				return nil, err
			}
			out[name] = keys
		}
	}
	return out, nil
}

func objectKeys(raw []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var keys []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, t.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func indexOf(l []string, s string) int {
	for i, v := range l {
		if v == s {
			return i
		}
	}
	return -1
}
