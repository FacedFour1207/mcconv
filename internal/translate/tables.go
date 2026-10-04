// Package translate maps blocks, items and biomes between Java Edition and
// Bedrock Edition using data tables (see tools/gendata for their sources).
package translate

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"mcconv/internal/nbt"
)

//go:embed data/tables.json.gz
var builtinTables []byte

// BuiltinTables returns the embedded table file.
func BuiltinTables() []byte { return builtinTables }

// TableFormat is the version of the table file layout.
const TableFormat = 3

// TableFile is the serialised form of the tables (data/tables.json.gz).
type TableFile struct {
	Format              int    `json:"format"`
	JavaVersion         string `json:"java_version"`
	JavaDataVersion     int    `json:"java_data_version"`
	BedrockVersion      string `json:"bedrock_version"`
	BedrockBlockVersion int32  `json:"bedrock_block_version"`
	// Java blocks in state id order.
	JavaBlocks []JavaBlockDef `json:"java_blocks"`
	// Bedrock block of every Java state: name index, states index (SNBT).
	BedrockNames  []string `json:"bedrock_names"`
	BedrockStates []string `json:"bedrock_states"`
	J2B           []int32  `json:"j2b"`
	// JavaBiomes maps Java biome ids to Bedrock numeric ids; BedrockBiomes
	// maps Bedrock biome names to their ids.
	JavaBiomes    map[string]int `json:"java_biomes"`
	BedrockBiomes map[string]int `json:"bedrock_biomes"`
	// Items maps Java item ids to Bedrock items.
	Items map[string]ItemMap `json:"items"`
	// BannerPatterns maps pattern names to Bedrock pattern codes.
	BannerPatterns map[string]string `json:"banner_patterns"`
	// BedrockEntities lists the Bedrock entity identifiers.
	BedrockEntities []string          `json:"bedrock_entities"`
	UpgradeSchemas  []json.RawMessage `json:"upgrade_schemas"`
	Sources         []string          `json:"sources"`
}

// JavaBlockDef is a Java block: its properties (in the game's order) and
// the offset of its default state.
type JavaBlockDef struct {
	Name    string     `json:"n"`
	Props   []JavaProp `json:"p,omitempty"`
	Default int        `json:"d"`
}

// JavaProp is a block property and its allowed values.
type JavaProp struct {
	Name   string   `json:"n"`
	Values []string `json:"v"`
}

// ItemMap is the Bedrock form of a Java item.
type ItemMap struct {
	ID    string `json:"id"`
	Data  int    `json:"data,omitempty"`
	Block bool   `json:"block,omitempty"`
}

// Tables answers translation queries. It is safe for concurrent use.
type Tables struct {
	JavaVersion         string
	JavaDataVersion     int
	BedrockVersion      string
	BedrockBlockVersion int32
	Source              string
	Sources             []string

	blocks  []javaBlock
	byName  map[string]*javaBlock
	stateOf []int32 // state id -> index in blocks

	javaState  []atomic.Pointer[Block] // state id -> Java block (shared, built on demand)
	toBedrock  []*Block                // state id -> Bedrock block without water
	waterBlock *Block

	b2j       map[string]int32       // Bedrock key -> Java state id
	byBedrock map[string][]candidate // Bedrock name -> its states (fuzzy matching)
	fuzzy     sync.Map               // Bedrock key -> int32 (-1 = none)

	javaBiomeToID   map[string]int
	bedrockBiomeTo  map[int]string // id -> Java biome
	bedrockBiomeIDs map[string]int // Bedrock name -> id
	bedrockBiomeNm  map[int]string // id -> Bedrock name

	items       map[string]ItemMap
	itemsBack   map[string]string // "bedrock_id/data" -> Java item
	patterns    map[string]string // Java pattern name -> Bedrock code
	patternBack map[string]string
	entities    map[string]bool

	up *upgrader
}

type javaBlock struct {
	JavaBlockDef
	index      int
	first      int32
	count      int32
	strides    []int32
	waterlog   int // index of the waterlogged property or -1
	defaultVal []string
}

type candidate struct {
	states map[string]any
	java   int32
}

var (
	loadMu   sync.Mutex
	loaded   *Tables
	dataFile string
)

// SetDataFile makes Load use a table file (from update-data) instead of the
// built-in one. Call it before the first Load.
func SetDataFile(path string) { dataFile = path }

// Load returns the tables (parsed once).
func Load() (*Tables, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	if loaded != nil {
		return loaded, nil
	}
	src, name := builtinTables, "built-in"
	if dataFile != "" {
		b, err := os.ReadFile(dataFile)
		if err != nil {
			return nil, fmt.Errorf("translate: %w", err)
		}
		src, name = b, dataFile
	}
	t, err := Parse(src)
	if err != nil {
		return nil, fmt.Errorf("translate: %s: %w", name, err)
	}
	t.Source = name
	loaded = t
	return t, nil
}

// Parse decodes a gzip compressed table file.
func Parse(src []byte) (*Tables, error) {
	zr, err := gzip.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	var tf TableFile
	if err := json.NewDecoder(zr).Decode(&tf); err != nil {
		return nil, err
	}
	if tf.Format != TableFormat {
		return nil, fmt.Errorf("unsupported table format %d (want %d)", tf.Format, TableFormat)
	}
	t := &Tables{
		JavaVersion: tf.JavaVersion, JavaDataVersion: tf.JavaDataVersion,
		BedrockVersion: tf.BedrockVersion, BedrockBlockVersion: tf.BedrockBlockVersion,
		Sources: tf.Sources, byName: map[string]*javaBlock{},
	}
	var first int32
	for i, d := range tf.JavaBlocks {
		jb := javaBlock{JavaBlockDef: d, index: i, first: first, waterlog: -1}
		jb.strides = make([]int32, len(d.Props))
		n := int32(1)
		for p := len(d.Props) - 1; p >= 0; p-- {
			jb.strides[p] = n
			n *= int32(len(d.Props[p].Values))
			if d.Props[p].Name == "waterlogged" {
				jb.waterlog = p
			}
		}
		jb.count = n
		jb.defaultVal = jb.values(int32(d.Default))
		t.blocks = append(t.blocks, jb)
		first += n
	}
	t.stateOf = make([]int32, first)
	for i := range t.blocks {
		jb := &t.blocks[i]
		t.byName[jb.Name] = jb
		for s := range jb.count {
			t.stateOf[jb.first+s] = int32(i)
		}
	}
	if len(tf.J2B) != 2*int(first) {
		return nil, fmt.Errorf("j2b has %d entries for %d Java states", len(tf.J2B)/2, first)
	}
	// Bedrock blocks (shared between Java states)
	states := make([]map[string]any, len(tf.BedrockStates))
	for i, s := range tf.BedrockStates {
		v, err := nbt.ParseSNBT(s)
		if err != nil {
			return nil, fmt.Errorf("bedrock state %q: %w", s, err)
		}
		c, _ := v.(nbt.Compound)
		states[i] = map[string]any(c)
	}
	shared := map[[2]int32]*Block{}
	t.javaState = make([]atomic.Pointer[Block], first)
	t.toBedrock = make([]*Block, first)
	for id := range first {
		k := [2]int32{tf.J2B[2*id], tf.J2B[2*id+1]}
		b := shared[k]
		if b == nil {
			b = NewBlock(tf.BedrockNames[k[0]], states[k[1]])
			b.Version = tf.BedrockBlockVersion
			shared[k] = b
		}
		t.toBedrock[id] = b
	}
	t.waterBlock = NewBlock("minecraft:water", map[string]any{"liquid_depth": int32(0)})
	t.waterBlock.Version = tf.BedrockBlockVersion
	t.buildReverse()

	// biomes
	t.javaBiomeToID = tf.JavaBiomes
	t.bedrockBiomeIDs = map[string]int{}
	t.bedrockBiomeNm = map[int]string{}
	for n, id := range tf.BedrockBiomes {
		n = strings.TrimPrefix(n, "minecraft:")
		t.bedrockBiomeIDs[n] = id
		t.bedrockBiomeNm[id] = n
	}
	t.bedrockBiomeTo = map[int]string{}
	javaNames := make([]string, 0, len(tf.JavaBiomes))
	for j := range tf.JavaBiomes {
		javaNames = append(javaNames, j)
	}
	sort.Strings(javaNames)
	for _, j := range javaNames { // prefer the Java biome named like the Bedrock one
		id := tf.JavaBiomes[j]
		if old, ok := t.bedrockBiomeTo[id]; !ok || strings.TrimPrefix(j, "minecraft:") == t.bedrockBiomeNm[id] && strings.TrimPrefix(old, "minecraft:") != t.bedrockBiomeNm[id] {
			t.bedrockBiomeTo[id] = j
		}
	}
	for bedrockName, javaName := range legacyBedrockBiomes {
		if id, ok := t.bedrockBiomeIDs[bedrockName]; ok {
			if _, done := t.bedrockBiomeTo[id]; !done {
				t.bedrockBiomeTo[id] = javaName
			}
		}
	}

	// items
	t.items = tf.Items
	t.itemsBack = map[string]string{}
	javaItems := make([]string, 0, len(tf.Items))
	for j := range tf.Items {
		javaItems = append(javaItems, j)
	}
	sort.Strings(javaItems)
	for _, j := range javaItems {
		it := tf.Items[j]
		k := itemKey(it.ID, it.Data)
		// prefer the Java item named like the Bedrock one
		if old, ok := t.itemsBack[k]; !ok || j == it.ID && old != it.ID {
			t.itemsBack[k] = j
		}
	}
	t.patterns = map[string]string{}
	t.patternBack = map[string]string{}
	for n, code := range tf.BannerPatterns {
		t.patterns[n] = code
		t.patternBack[code] = n
	}
	t.entities = map[string]bool{}
	for _, e := range tf.BedrockEntities {
		t.entities[e] = true
	}
	up, err := newUpgrader(tf.UpgradeSchemas)
	if err != nil {
		return nil, err
	}
	t.up = up
	return t, nil
}

// buildReverse picks, for every Bedrock state, the Java state it converts
// back to: the block named like the Bedrock block if there is one, then the
// state closest to the Java block's default state (properties Bedrock does
// not store, such as fence connections or redstone power, take their
// default values).
func (t *Tables) buildReverse() {
	t.b2j = map[string]int32{}
	t.byBedrock = map[string][]candidate{}
	score := func(id int32, b *Block) int {
		jb := &t.blocks[t.stateOf[id]]
		s := 0
		if jb.Name == b.Name() {
			s += 1000
		}
		vals := jb.values(id - jb.first)
		for i, v := range vals {
			if v == jb.defaultVal[i] {
				s++
			}
		}
		return s
	}
	best := map[*Block]int32{}
	for id, b := range t.toBedrock {
		cur, ok := best[b]
		if !ok || score(int32(id), b) > score(cur, b) {
			best[b] = int32(id)
		}
	}
	for b, id := range best {
		t.b2j[b.Key()] = id
		t.byBedrock[b.Name()] = append(t.byBedrock[b.Name()], candidate{states: b.Props, java: id})
	}
	for _, l := range t.byBedrock {
		sort.Slice(l, func(i, j int) bool { return l[i].java < l[j].java })
	}
}

// values returns the property values of the state at offset s.
func (jb *javaBlock) values(s int32) []string {
	out := make([]string, len(jb.Props))
	for i, p := range jb.Props {
		out[i] = p.Values[(s/jb.strides[i])%int32(len(p.Values))]
	}
	return out
}

func itemKey(id string, data int) string { return fmt.Sprintf("%s/%d", id, data) }

// legacyBedrockBiomes maps Bedrock biomes that no longer exist in Java
// Edition to the closest Java biome.
var legacyBedrockBiomes = map[string]string{
	"bamboo_jungle_hills":              "minecraft:bamboo_jungle",
	"birch_forest_hills":               "minecraft:birch_forest",
	"birch_forest_hills_mutated":       "minecraft:old_growth_birch_forest",
	"cold_taiga_hills":                 "minecraft:snowy_taiga",
	"cold_taiga_mutated":               "minecraft:snowy_taiga",
	"deep_warm_ocean":                  "minecraft:warm_ocean",
	"desert_hills":                     "minecraft:desert",
	"desert_mutated":                   "minecraft:desert",
	"extreme_hills_edge":               "minecraft:windswept_hills",
	"extreme_hills_plus_trees_mutated": "minecraft:windswept_forest",
	"forest_hills":                     "minecraft:forest",
	"ice_mountains":                    "minecraft:snowy_plains",
	"jungle_edge_mutated":              "minecraft:sparse_jungle",
	"jungle_hills":                     "minecraft:jungle",
	"jungle_mutated":                   "minecraft:jungle",
	"legacy_frozen_ocean":              "minecraft:frozen_ocean",
	"mega_taiga_hills":                 "minecraft:old_growth_pine_taiga",
	"mesa_plateau":                     "minecraft:wooded_badlands",
	"mesa_plateau_mutated":             "minecraft:badlands",
	"mesa_plateau_stone_mutated":       "minecraft:wooded_badlands",
	"mushroom_island_shore":            "minecraft:mushroom_fields",
	"redwood_taiga_hills_mutated":      "minecraft:old_growth_spruce_taiga",
	"roofed_forest_mutated":            "minecraft:dark_forest",
	"savanna_plateau_mutated":          "minecraft:windswept_savanna",
	"swampland_mutated":                "minecraft:swamp",
	"taiga_hills":                      "minecraft:taiga",
	"taiga_mutated":                    "minecraft:taiga",
}
