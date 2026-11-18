package convert

import (
	"strings"

	"mcconv/internal/translate"
)

// DataVersions where the Java save format of something converted changed
// (from the game's data fixers).
const (
	dvBoatSplit      = 4067 // 1.21.2: one entity id per boat wood
	dvFurnaceTimes   = 4181 // 1.21.4: furnace BurnTime -> lit_time_remaining ...
	dvTextNBT        = 4290 // 1.21.5: text components as NBT instead of JSON strings
	dvEquipment      = 4301 // 1.21.5: "equipment" instead of ArmorItems/HandItems
	dvFallDistance   = 4303 // 1.21.5: fall_distance (double) instead of FallDistance
	dvTooltipDisplay = 4307 // 1.21.5: no show_in_tooltip wrappers (enchantments, dyed_color)
	dvBlockPos       = 4314 // 1.21.5: block_pos instead of TileX/TileY/TileZ
	dvNoBedEntity    = 4885 // 26.2: beds have no block entity
)

// javaFormat writes Java data for the target release. A nil *javaFormat
// (Java -> Bedrock) or one without a target writes the tables' version.
type javaFormat struct {
	target *translate.JavaTarget // nil: the tables' version
}

// older reports whether the target is older than a format change.
func (f *javaFormat) older(dv int) bool {
	return f != nil && f.target != nil && f.target.DataVersion < dv
}

// text returns a text component in the target's storage form.
func (f *javaFormat) text(v any) any {
	if f.older(dvTextNBT) {
		return translate.JavaTextToJSON(v)
	}
	return v
}

// item converts an item id ("minecraft:...") for the target; ok is false
// if the target has no such item.
func (f *javaFormat) item(id string) (string, bool) {
	if f == nil {
		return id, true
	}
	return f.target.Item(id)
}

// entity reports whether the target has an entity id ("minecraft:...").
func (f *javaFormat) entity(id string) bool { return f == nil || f.target.Entity(id) }

// block converts a block state for the target.
func (f *javaFormat) block(b *translate.Block) *translate.Block {
	if f == nil {
		return b
	}
	return f.target.Block(b)
}

// keepBlockEntity reports whether a block entity still belongs to its block
// after the block was replaced for the target (pale oak signs stay signs,
// shelves become air).
func (f *javaFormat) keepBlockEntity(id string, b *translate.Block) bool {
	if f == nil || f.target == nil {
		return true
	}
	nb := f.target.Block(b)
	if nb.Name() == b.Name() {
		return true
	}
	n := nb.Name()
	switch id {
	case "sign":
		return strings.HasSuffix(n, "_sign") && !strings.HasSuffix(n, "_hanging_sign")
	case "hanging_sign":
		return strings.HasSuffix(n, "_hanging_sign")
	case "chest":
		return n == "minecraft:chest"
	}
	return false
}
