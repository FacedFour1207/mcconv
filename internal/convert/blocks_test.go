package convert

import (
	"testing"

	"mcconv/internal/translate"
)

func TestUnknownAndOverride(t *testing.T) {
	data, err := translate.Load()
	if err != nil {
		t.Fatal(err)
	}
	bc := newBlockConverter(data, true, map[string]string{"minecraft:sulfur": "minecraft:yellow_concrete"}, nil)
	// a block newer than the tables is copied by name without properties
	e := bc.lookup(translate.NewBlock("minecraft:future_test_block", map[string]any{"foo": "bar"}))
	if e.out.Key() != "minecraft:future_test_block" || e.rule != nil {
		t.Fatalf("unknown block: got %s", e.out.Key())
	}
	if got := bc.unknownBlocks(); len(got) != 1 || got[0] != "minecraft:future_test_block (1)" {
		t.Fatalf("unknown report: %v", got)
	}
	// overrides win
	e = bc.lookup(translate.NewBlock("minecraft:sulfur", nil))
	if e.out.Name() != "minecraft:yellow_concrete" {
		t.Fatalf("override: got %s", e.out.Key())
	}
	// known blocks are unaffected; waterlogged blocks get a water layer
	e = bc.lookup(translate.NewBlock("minecraft:stone", nil))
	if e.out.Key() != "minecraft:stone" {
		t.Fatalf("stone: got %s", e.out.Key())
	}
	e = bc.lookup(translate.NewBlock("minecraft:oak_slab", map[string]any{"type": "bottom", "waterlogged": "true"}))
	if len(e.out.Extra) != 1 || e.out.Extra[0].Name() != "minecraft:water" {
		t.Fatalf("waterlogged slab: got %s", e.out.Key())
	}
	// potted plants get their Bedrock block entity
	e = bc.lookup(translate.NewBlock("minecraft:potted_poppy", nil))
	if e.out.Name() != "minecraft:flower_pot" || e.tpl == nil {
		t.Fatalf("potted poppy: got %s tpl=%v", e.out.Key(), e.tpl)
	}
}
