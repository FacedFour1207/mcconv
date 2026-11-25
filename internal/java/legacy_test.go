package java

import (
	"testing"

	"mcconv/internal/nbt"
	"mcconv/internal/world"
)

// Chunks of old worlds upgraded by the game (--forceUpgrade): entities of
// 1.16 and older stay in the chunk ("entities"), and pre-1.18 chunks are
// "empty" with the real status in below_zero_retrogen.
func TestUpgradedLegacyChunk(t *testing.T) {
	root := chunk263()
	root["Status"] = "minecraft:empty"
	root["below_zero_retrogen"] = nbt.Compound{"target_status": "minecraft:spawn"}
	root["entities"] = &nbt.List{Type: nbt.TagCompound, Items: []any{
		nbt.Compound{"id": "minecraft:cow", "Pos": &nbt.List{Type: nbt.TagDouble, Items: []any{1.5, 64.0, 2.5}}},
	}}
	raw := nbt.Encode("", root, nbt.BigEndian)
	ch, res, err := (&Codec{}).NewFastDecoder().Decode(raw, Bounds(world.Overworld))
	if err != nil {
		t.Fatal(err)
	}
	checkPalette(t, ch)
	if res.Status != "minecraft:empty" || res.RetrogenStatus != "minecraft:spawn" {
		t.Fatalf("status %q, retrogen %q", res.Status, res.RetrogenStatus)
	}
	if res.Entities == nil || len(res.Entities.Items) != 1 {
		t.Fatalf("entities: %v", res.Entities)
	}
}
