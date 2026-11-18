package translate

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Older Java releases: converted worlds can be written for an older game
// than the tables (down to 1.21.1). Blocks, items, biomes and entities the
// older game does not have are replaced with similar ones (see
// tools/genversions for the data).

//go:embed data/java_versions.json.gz
var builtinJavaVersions []byte

// JavaVersionFormat is the version of the java_versions file layout.
const JavaVersionFormat = 1

// JavaVersionFile is the serialised form of data/java_versions.json.gz.
type JavaVersionFile struct {
	Format   int              `json:"format"`
	Releases []JavaRelease    `json:"releases"`
	Sets     []JavaVersionSet `json:"sets"`
}

// JavaRelease is a released Java version and its DataVersion.
type JavaRelease struct {
	Name        string `json:"name"`
	DataVersion int    `json:"data_version"`
}

// JavaVersionSet is the content of one release.
type JavaVersionSet struct {
	Name        string         `json:"name"`
	DataVersion int            `json:"data_version"`
	Blocks      []JavaBlockDef `json:"blocks"`
	Items       []string       `json:"items"`
	Biomes      []string       `json:"biomes"`
	Entities    []string       `json:"entities"`
}

// MinJavaDataVersion is the oldest Java version worlds can be written for (1.21.1).
const MinJavaDataVersion = 3955

var (
	javaVersionsOnce sync.Once
	javaVersions     *JavaVersionFile
	javaVersionsErr  error
)

func loadJavaVersions() (*JavaVersionFile, error) {
	javaVersionsOnce.Do(func() {
		zr, err := gzip.NewReader(bytes.NewReader(builtinJavaVersions))
		if err != nil {
			javaVersionsErr = err
			return
		}
		var vf JavaVersionFile
		if err := json.NewDecoder(zr).Decode(&vf); err != nil {
			javaVersionsErr = err
			return
		}
		if vf.Format != JavaVersionFormat {
			javaVersionsErr = fmt.Errorf("java_versions: unsupported format %d", vf.Format)
			return
		}
		javaVersions = &vf
	})
	return javaVersions, javaVersionsErr
}

// JavaReleases lists the Java releases worlds can be written for, oldest
// first, up to the tables' version.
func (t *Tables) JavaReleases() []JavaRelease {
	vf, err := loadJavaVersions()
	var out []JavaRelease
	if err == nil {
		for _, r := range vf.Releases {
			if r.DataVersion < t.JavaDataVersion {
				out = append(out, r)
			}
		}
	}
	return append(out, JavaRelease{Name: t.JavaVersion, DataVersion: t.JavaDataVersion})
}

// JavaReleaseByName finds a release by its version name ("1.21.1").
func (t *Tables) JavaReleaseByName(name string) (JavaRelease, bool) {
	for _, r := range t.JavaReleases() {
		if r.Name == name {
			return r, true
		}
	}
	return JavaRelease{}, false
}

// JavaTarget converts the tables' Java blocks, items, biomes and entities to
// an older release. A nil *JavaTarget (the tables' own version) changes
// nothing. It is safe for concurrent use.
type JavaTarget struct {
	DataVersion int
	Set         string // name of the content set used

	t        *Tables
	blocks   map[string]*javaBlock
	items    map[string]bool
	biomes   map[string]bool
	entities map[string]bool

	cache    sync.Map // *Block -> *Block
	interns  sync.Map // key -> *Block
	replaced sync.Map // block id -> replacement id
}

// JavaTarget returns the converter for a Java DataVersion: nil when the
// tables already are that version (or newer), an error below 1.21.1.
func (t *Tables) JavaTarget(dataVersion int) (*JavaTarget, error) {
	if dataVersion <= 0 || dataVersion >= t.JavaDataVersion {
		return nil, nil
	}
	if dataVersion < MinJavaDataVersion {
		return nil, fmt.Errorf("Java DataVersion %d is older than 1.21.1 (%d), which is not supported", dataVersion, MinJavaDataVersion)
	}
	vf, err := loadJavaVersions()
	if err != nil {
		return nil, err
	}
	var set *JavaVersionSet
	for i := range vf.Sets {
		if vf.Sets[i].DataVersion <= dataVersion {
			set = &vf.Sets[i]
		}
	}
	if set == nil {
		return nil, fmt.Errorf("no block data for Java DataVersion %d", dataVersion)
	}
	jt := &JavaTarget{DataVersion: dataVersion, Set: set.Name, t: t,
		blocks: map[string]*javaBlock{}, items: map[string]bool{}, biomes: map[string]bool{}, entities: map[string]bool{}}
	for i, d := range set.Blocks {
		jb := &javaBlock{JavaBlockDef: d, index: i, waterlog: -1}
		jb.strides = make([]int32, len(d.Props))
		n := int32(1)
		for p := len(d.Props) - 1; p >= 0; p-- {
			jb.strides[p] = n
			n *= int32(len(d.Props[p].Values))
		}
		jb.count = n
		jb.defaultVal = jb.values(int32(d.Default))
		jt.blocks[d.Name] = jb
	}
	for _, s := range set.Items {
		jt.items[s] = true
	}
	for _, s := range set.Biomes {
		jt.biomes[s] = true
	}
	for _, s := range set.Entities {
		jt.entities[s] = true
	}
	return jt, nil
}

// HasBlock reports whether the target release has a block id.
func (jt *JavaTarget) HasBlock(name string) bool { return jt == nil || jt.blocks[name] != nil }

// Block converts a Java block state of the tables' version to the target
// release (shared result instances).
func (jt *JavaTarget) Block(b *Block) *Block {
	if jt == nil {
		return b
	}
	if v, ok := jt.cache.Load(b); ok {
		return v.(*Block)
	}
	out := jt.convertBlock(b)
	if v, ok := jt.interns.LoadOrStore(out.Key(), out); ok {
		out = v.(*Block)
	}
	if out.Name() != b.Name() {
		jt.replaced.Store(b.Name(), out.Name())
	}
	jt.cache.Store(b, out)
	return out
}

// Replaced lists the blocks that were replaced so far ("from -> to"), sorted.
func (jt *JavaTarget) Replaced() []string {
	if jt == nil {
		return nil
	}
	var out []string
	jt.replaced.Range(func(k, v any) bool {
		out = append(out, k.(string)+" -> "+v.(string))
		return true
	})
	sort.Strings(out)
	return out
}

func (jt *JavaTarget) convertBlock(b *Block) *Block {
	name, props := b.Name(), b.Props
	for range 4 {
		if jt.blocks[name] != nil {
			break
		}
		name = jt.blockFallback(name, props)
	}
	jb := jt.blocks[name]
	if jb == nil || name == "minecraft:air" {
		if s, _ := props["waterlogged"].(string); s == "true" {
			return NewBlock("minecraft:water", map[string]any{"level": "0"})
		}
		return NewBlock("minecraft:air", nil)
	}
	out := make(map[string]any, len(jb.Props))
	for i, p := range jb.Props {
		v, _ := props[p.Name].(string)
		if indexOf(p.Values, v) < 0 {
			v = jb.defaultVal[i]
		}
		out[p.Name] = v
	}
	return NewBlock(name, out)
}

// Biome converts a biome id to one the target release has.
func (jt *JavaTarget) Biome(name string) string {
	if jt == nil || jt.biomes[name] {
		return name
	}
	if f, ok := biomeFallbacks[name]; ok && jt.biomes[f] {
		return f
	}
	return "minecraft:plains"
}

// Item converts an item id ("minecraft:...") to one the target release
// has; ok is false if there is none.
func (jt *JavaTarget) Item(id string) (string, bool) {
	if jt == nil || jt.items[id] {
		return id, true
	}
	for range 4 {
		f, ok := itemFallbacks[id]
		if !ok && strings.HasSuffix(id, "_map") {
			// 26.3 explorer maps have their own ids; older releases use a
			// filled map, which has no map data here: an empty map
			f, ok = "minecraft:map", true
		}
		if !ok {
			// block items: the block's replacement
			if jt.t.HasJavaBlock(id) {
				f = jt.blockFallback(id, nil)
				ok = f != "minecraft:air"
			}
		}
		if !ok {
			return "", false
		}
		if jt.items[f] {
			return f, true
		}
		id = f
	}
	return "", false
}

// Entity reports whether the target release has an entity id.
func (jt *JavaTarget) Entity(id string) bool { return jt == nil || jt.entities[id] }

// blockFallback names the block that replaces one the target lacks
// ("minecraft:air" when nothing fits).
func (jt *JavaTarget) blockFallback(name string, props map[string]any) string {
	base := strings.TrimPrefix(name, "minecraft:")
	mc := func(s string) string { return "minecraft:" + s }
	if f, ok := blockFallbacks[base]; ok {
		return mc(f)
	}
	switch {
	case base == "pale_moss_carpet":
		if s, _ := props["bottom"].(string); s == "false" {
			return mc("air") // the hanging side part
		}
		return mc("moss_carpet")
	case strings.Contains(base, "pale_oak"):
		return mc(strings.Replace(base, "pale_oak", "dark_oak", 1))
	case strings.HasSuffix(base, "_poplar_leaves"): // red_, orange_, yellow_ (26.3)
		return mc("birch_leaves")
	case strings.Contains(base, "poplar"):
		return mc(strings.Replace(base, "poplar", "birch", 1))
	case strings.HasSuffix(base, "_wool_stairs"), strings.HasSuffix(base, "_wool_slab"):
		return mc(base[:strings.LastIndex(base, "_wool_")] + "_wool")
	case strings.HasSuffix(base, "_concrete_stairs"), strings.HasSuffix(base, "_concrete_slab"):
		return mc(base[:strings.LastIndex(base, "_concrete_")] + "_concrete")
	case strings.HasSuffix(base, "_shelf"):
		return mc("air")
	}
	// copper variants: waxed / weathered forms of blocks newer than the target
	for _, p := range []string{"waxed_", "exposed_", "weathered_", "oxidized_"} {
		if strings.HasPrefix(base, p) {
			return mc(strings.TrimPrefix(base, p))
		}
	}
	for _, f := range []struct{ prefix, to string }{
		{"cinnabar_brick_", "red_nether_brick_"}, {"polished_cinnabar_", "red_nether_brick_"}, {"cinnabar_", "red_nether_brick_"},
		{"sulfur_brick_", "sandstone_"}, {"polished_sulfur_", "smooth_sandstone_"}, {"sulfur_", "sandstone_"},
		{"resin_brick_", "brick_"},
	} {
		if strings.HasPrefix(base, f.prefix) {
			n := f.to + strings.TrimPrefix(base, f.prefix)
			if n == "smooth_sandstone_wall" {
				n = "sandstone_wall"
			}
			return mc(n)
		}
	}
	return mc("air")
}

// blockFallbacks replace blocks added after 1.21.1 (ids without namespace).
var blockFallbacks = map[string]string{
	"iron_chain":               "chain", // renamed in 1.21.9
	"chain":                    "iron_chain",
	"creaking_heart":           "dark_oak_log",
	"pale_moss_block":          "moss_block",
	"pale_hanging_moss":        "air",
	"open_eyeblossom":          "oxeye_daisy",
	"closed_eyeblossom":        "oxeye_daisy",
	"potted_open_eyeblossom":   "potted_oxeye_daisy",
	"potted_closed_eyeblossom": "potted_oxeye_daisy",
	"resin_block":              "honeycomb_block",
	"resin_bricks":             "bricks",
	"chiseled_resin_bricks":    "bricks",
	"resin_clump":              "air",
	"leaf_litter":              "air",
	"wildflowers":              "pink_petals",
	"bush":                     "fern",
	"firefly_bush":             "fern",
	"short_dry_grass":          "dead_bush",
	"tall_dry_grass":           "dead_bush",
	"cactus_flower":            "air",
	"golden_dandelion":         "dandelion",
	"potted_golden_dandelion":  "potted_dandelion",
	"dried_ghast":              "air",
	"test_block":               "air",
	"test_instance_block":      "air",
	"copper_chest":             "chest",
	"copper_golem_statue":      "air",
	"copper_bars":              "iron_bars",
	"copper_chain":             "iron_chain",
	"copper_lantern":           "lantern",
	"copper_torch":             "torch",
	"copper_wall_torch":        "wall_torch",
	"cinnabar":                 "red_nether_bricks",
	"polished_cinnabar":        "red_nether_bricks",
	"chiseled_cinnabar":        "red_nether_bricks",
	"cinnabar_bricks":          "red_nether_bricks",
	"sulfur":                   "sandstone",
	"polished_sulfur":          "smooth_sandstone",
	"chiseled_sulfur":          "chiseled_sandstone",
	"sulfur_bricks":            "cut_sandstone",
	"potent_sulfur":            "sandstone",
	"sulfur_spike":             "pointed_dripstone",
	// 26.3
	"straw_bed":      "yellow_bed",
	"red_shrub":      "fern",
	"shelf_mushroom": "air",
}

// itemFallbacks replace items (that are not block items) added after 1.21.1.
var itemFallbacks = map[string]string{
	"minecraft:pale_oak_boat":         "minecraft:dark_oak_boat",
	"minecraft:pale_oak_chest_boat":   "minecraft:dark_oak_chest_boat",
	"minecraft:blue_egg":              "minecraft:egg",
	"minecraft:brown_egg":             "minecraft:egg",
	"minecraft:resin_brick":           "minecraft:brick",
	"minecraft:copper_nugget":         "minecraft:iron_nugget",
	"minecraft:copper_axe":            "minecraft:stone_axe",
	"minecraft:copper_hoe":            "minecraft:stone_hoe",
	"minecraft:copper_pickaxe":        "minecraft:stone_pickaxe",
	"minecraft:copper_shovel":         "minecraft:stone_shovel",
	"minecraft:copper_sword":          "minecraft:stone_sword",
	"minecraft:copper_helmet":         "minecraft:chainmail_helmet",
	"minecraft:copper_chestplate":     "minecraft:chainmail_chestplate",
	"minecraft:copper_leggings":       "minecraft:chainmail_leggings",
	"minecraft:copper_boots":          "minecraft:chainmail_boots",
	"minecraft:copper_horse_armor":    "minecraft:iron_horse_armor",
	"minecraft:netherite_horse_armor": "minecraft:diamond_horse_armor",
	"minecraft:wooden_spear":          "minecraft:wooden_sword",
	"minecraft:stone_spear":           "minecraft:stone_sword",
	"minecraft:copper_spear":          "minecraft:stone_sword",
	"minecraft:iron_spear":            "minecraft:iron_sword",
	"minecraft:golden_spear":          "minecraft:golden_sword",
	"minecraft:diamond_spear":         "minecraft:diamond_sword",
	"minecraft:netherite_spear":       "minecraft:netherite_sword",
	"minecraft:sulfur_cube_bucket":    "minecraft:bucket",
	"minecraft:poplar_boat":           "minecraft:birch_boat",
	"minecraft:poplar_chest_boat":     "minecraft:birch_chest_boat",
}

func init() {
	for _, c := range []string{"white", "orange", "magenta", "light_blue", "yellow", "lime", "pink", "gray",
		"light_gray", "cyan", "purple", "blue", "brown", "green", "red", "black"} {
		itemFallbacks["minecraft:"+c+"_bundle"] = "minecraft:bundle"
		itemFallbacks["minecraft:"+c+"_cushion"] = "minecraft:" + c + "_carpet" // 26.3
	}
}

var biomeFallbacks = map[string]string{
	"minecraft:pale_garden":    "minecraft:dark_forest",
	"minecraft:sulfur_caves":   "minecraft:dripstone_caves",
	"minecraft:dappled_forest": "minecraft:birch_forest",
}
