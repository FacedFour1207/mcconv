package convert

import (
	"math"
	"strconv"
	"strings"

	"mcconv/internal/nbt"
	"mcconv/internal/translate"
)

// Bedrock keeps some information in block entities or leaves it to the
// neighbouring blocks where Java Edition has block state properties (bed
// colours, potted plants, note block pitch, ...). The rules below restore
// those properties when converting to Java; bedrockTemplate does the
// opposite for blocks that have no block entity in Java Edition.

// rule computes the Java block at one position. It returns nil to keep the
// position independent result.
type rule func(r *ruleIn) *translate.Block

type ruleIn struct {
	bc      *blockConverter
	in      *translate.Block // Bedrock block
	be      nbt.Compound     // its block entity (nil if none)
	base    *translate.Block // position independent Java block
	x, y, z int
	at      func(dx, dy, dz int) *translate.Block // neighbouring Bedrock block
	locDep  bool                                  // result depends on x, y, z

	rec       []probe
	recBlocks [maxProbes]*translate.Block
	overflow  bool
}

func (r *ruleIn) rename(name string) *translate.Block {
	if b, ok := r.bc.t.JavaRename(r.base, name); ok {
		return b
	}
	return nil
}

func (r *ruleIn) with(prop, val string) *translate.Block {
	return r.bc.t.WithJavaProp(r.base, prop, val)
}

var colours = []string{"white", "orange", "magenta", "light_blue", "yellow", "lime", "pink", "gray",
	"light_gray", "cyan", "purple", "blue", "brown", "green", "red", "black"}

func colourIndex(name string) int {
	for i, c := range colours {
		if c == name {
			return i
		}
	}
	return -1
}

// colourOf splits a Java block id into its colour and the rest
// ("light_gray_bed" -> light_gray, "bed").
func colourOf(name string) (int, string) {
	n := strip(name)
	best := -1
	for i, c := range colours {
		if strings.HasPrefix(n, c+"_") && (best < 0 || len(c) > len(colours[best])) {
			best = i
		}
	}
	if best < 0 {
		return -1, n
	}
	return best, strings.TrimPrefix(n, colours[best]+"_")
}

var shulkerFacing = []string{"down", "up", "north", "south", "west", "east"}

func javaRules(out *translate.Block) rule {
	n := strip(out.Name())
	switch {
	case strings.HasSuffix(n, "_bed"):
		return bedRule
	case strings.HasSuffix(n, "_banner"):
		return bannerRule
	case strings.HasSuffix(n, "shulker_box"):
		return shulkerRule
	case n == "flower_pot":
		return flowerPotRule
	case n == "note_block":
		return noteBlockRule
	case (strings.HasSuffix(n, "_skull") || strings.HasSuffix(n, "_head")) && !strings.Contains(n, "_wall_"):
		return skullRule
	case n == "chest" || n == "trapped_chest" || strings.HasSuffix(n, "copper_chest"):
		return chestRule
	case n == "jukebox":
		return func(r *ruleIn) *translate.Block {
			_, has := r.be["RecordItem"]
			return r.with("has_record", strconv.FormatBool(has))
		}
	case n == "lectern":
		return func(r *ruleIn) *translate.Block {
			has, _ := r.be.Byte("hasBook")
			_, book := r.be["book"]
			return r.with("has_book", strconv.FormatBool(has != 0 || book))
		}
	case n == "campfire" || n == "soul_campfire":
		return func(r *ruleIn) *translate.Block {
			return r.with("signal_fire", strconv.FormatBool(r.at(0, -1, 0).Name() == "minecraft:hay_block"))
		}
	}
	return nil
}

func bedRule(r *ruleIn) *translate.Block {
	c, ok := r.be.Byte("color")
	if !ok || c < 0 || int(c) >= len(colours) {
		return r.rename("minecraft:red_bed") // the default bed colour
	}
	return r.rename("minecraft:" + colours[c] + "_bed")
}

func bannerRule(r *ruleIn) *translate.Block {
	base, _ := r.be.Number("Base")
	if base < 0 || base > 15 {
		base = 15
	}
	_, rest := colourOf(r.base.Name())
	return r.rename("minecraft:" + colours[15-base] + "_" + rest)
}

func shulkerRule(r *ruleIn) *translate.Block {
	f, ok := r.be.Byte("facing")
	if !ok || f < 0 || int(f) >= len(shulkerFacing) {
		f = 1
	}
	return r.with("facing", shulkerFacing[f])
}

// pottedPlants maps Java plant ids to their potted block where the name is
// not simply "potted_" + plant.
var pottedPlants = map[string]string{
	"minecraft:azalea":           "minecraft:potted_azalea_bush",
	"minecraft:flowering_azalea": "minecraft:potted_flowering_azalea_bush",
}

func flowerPotRule(r *ruleIn) *translate.Block {
	pb, ok := r.be.Compound("PlantBlock")
	if !ok {
		return nil
	}
	name, _ := pb.String("name")
	states, _ := pb.Compound("states")
	plant := translate.NewBlock(name, map[string]any(states))
	if v, ok := pb.Number("version"); ok {
		plant.Version = int32(v)
	}
	jp, ok := r.bc.t.BedrockToJava(plant)
	if !ok {
		return nil
	}
	potted, ok := pottedPlants[jp.Name()]
	if !ok {
		potted = "minecraft:potted_" + strip(jp.Name())
	}
	if b, ok := r.bc.t.JavaDefault(potted); ok {
		return b
	}
	return nil
}

func noteBlockRule(r *ruleIn) *translate.Block {
	b := r.base
	if n, ok := r.be.Byte("note"); ok && n >= 0 && n <= 24 {
		b = r.bc.t.WithJavaProp(b, "note", strconv.Itoa(int(n)))
	}
	inst := noteInstrument(r)
	return r.bc.t.WithJavaProp(b, "instrument", inst)
}

// noteInstrument derives the instrument like Java Edition does: from a mob
// head on top, otherwise from the block below.
func noteInstrument(r *ruleIn) string {
	above := strip(r.at(0, 1, 0).Name())
	heads := map[string]string{"zombie_head": "zombie", "skeleton_skull": "skeleton", "creeper_head": "creeper",
		"dragon_head": "dragon", "wither_skeleton_skull": "wither_skeleton", "piglin_head": "piglin", "player_head": "custom_head"}
	if inst, ok := heads[above]; ok {
		return inst
	}
	below := strip(r.at(0, -1, 0).Name())
	switch {
	case below == "gold_block":
		return "bell"
	case below == "clay":
		return "flute"
	case below == "packed_ice":
		return "chime"
	case strings.HasSuffix(below, "_wool"):
		return "guitar"
	case below == "bone_block":
		return "xylophone"
	case below == "iron_block":
		return "iron_xylophone"
	case below == "soul_sand":
		return "cow_bell"
	case below == "pumpkin":
		return "didgeridoo"
	case below == "emerald_block":
		return "bit"
	case below == "hay_block":
		return "banjo"
	case below == "glowstone":
		return "pling"
	case below == "sand" || below == "red_sand" || below == "gravel" || strings.HasSuffix(below, "concrete_powder") || strings.HasPrefix(below, "suspicious_"):
		return "snare"
	case strings.Contains(below, "glass") || below == "sea_lantern" || below == "beacon":
		return "hat"
	case isWoodish(below):
		return "bass"
	case isStoneish(below):
		return "basedrum"
	}
	return "harp"
}

func isWoodish(n string) bool {
	for _, s := range []string{"planks", "_log", "_wood", "_stem", "_hyphae", "bookshelf", "crafting_table", "chest", "note_block", "jukebox", "_fence", "_sign", "barrel", "loom", "cartography_table", "fletching_table", "smithing_table", "lectern", "composter", "beehive", "bee_nest", "bamboo", "daylight_detector", "_trapdoor", "_door", "_pressure_plate"} {
		if strings.Contains(n, s) && !strings.Contains(n, "iron") && !strings.Contains(n, "copper") {
			return true
		}
	}
	return false
}

func isStoneish(n string) bool {
	for _, s := range []string{"stone", "cobble", "_ore", "brick", "netherrack", "obsidian", "quartz", "sandstone", "concrete", "terracotta", "deepslate", "basalt", "tuff", "calcite", "prismarine", "purpur", "granite", "diorite", "andesite", "nylium", "bedrock", "magma", "observer", "dispenser", "dropper", "furnace", "smoker", "coral_block", "end_stone", "dripstone", "respawn_anchor", "lodestone", "enchanting_table", "ender_chest", "spawner"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

func skullRule(r *ruleIn) *translate.Block {
	rot, ok := nbt.AsFloat(r.be["Rotation"])
	if !ok {
		return nil
	}
	n := int(math.Round(rot/22.5)) & 15
	return r.with("rotation", strconv.Itoa(n))
}

// cardinal offsets and the clockwise neighbour (the other half of a "left"
// Java chest).
var dirOffset = map[string][2]int{"north": {0, -1}, "south": {0, 1}, "west": {-1, 0}, "east": {1, 0}}
var clockwise = map[string]string{"north": "east", "east": "south", "south": "west", "west": "north"}

func chestRule(r *ruleIn) *translate.Block {
	px, okx := r.be.Number("pairx")
	pz, okz := r.be.Number("pairz")
	if !okx || !okz {
		return nil
	}
	r.locDep = true
	facing, _ := r.base.Props["facing"].(string)
	cw, ok := dirOffset[clockwise[facing]]
	if !ok {
		return nil
	}
	dx, dz := int(px)-r.x, int(pz)-r.z
	switch {
	case dx == cw[0] && dz == cw[1]:
		return r.with("type", "left")
	case dx == -cw[0] && dz == -cw[1]:
		return r.with("type", "right")
	}
	return nil
}

// bedrockTemplate returns the block entity a converted Java block needs in
// Bedrock Edition where Java Edition may have none (flower pots, note
// blocks, beds).
func (bc *blockConverter) bedrockTemplate(java *translate.Block) *translate.BlockEntity {
	n := strip(java.Name())
	switch {
	case strings.HasPrefix(n, "potted_"):
		plant := "minecraft:" + strings.TrimPrefix(n, "potted_")
		for p, potted := range pottedPlants {
			if potted == java.Name() {
				plant = p
			}
		}
		jp, ok := bc.t.JavaDefault(plant)
		if !ok {
			return nil
		}
		bp, ok := bc.t.JavaToBedrock(jp)
		if !ok {
			return nil
		}
		return &translate.BlockEntity{BaseName: "FlowerPot", NBT: nbt.Compound{
			"PlantBlock": bedrockBlockNBT(bp), "isMovable": int8(1),
		}}
	case n == "flower_pot":
		return &translate.BlockEntity{BaseName: "FlowerPot", NBT: nbt.Compound{"isMovable": int8(1)}}
	case n == "spore_blossom":
		return &translate.BlockEntity{BaseName: "SporeBlossom", NBT: nbt.Compound{"isMovable": int8(1)}}
	case strings.HasSuffix(n, "_bed"):
		// Java does not always save bed block entities
		ci, _ := colourOf(n)
		if ci < 0 {
			ci = 14
		}
		return &translate.BlockEntity{BaseName: "Bed", NBT: nbt.Compound{"color": int8(ci), "isMovable": int8(1)}}
	case n == "note_block":
		ns, _ := java.Props["note"].(string)
		note, _ := strconv.Atoi(ns)
		return &translate.BlockEntity{BaseName: "Music", NBT: nbt.Compound{"note": int8(note), "isMovable": int8(1)}}
	}
	return nil
}

// bedrockBlockNBT is the {name, states, version} form of a Bedrock block
// used in block entities and items.
func bedrockBlockNBT(b *translate.Block) nbt.Compound {
	states := nbt.Compound{}
	for k, v := range b.Props {
		states[k] = v
	}
	return nbt.Compound{"name": b.Name(), "states": states, "version": b.Version}
}
