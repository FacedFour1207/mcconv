package java

import (
	"testing"

	"mcconv/internal/nbt"
	"mcconv/internal/world"
)

// chunk263 builds a chunk with the block palette forms of Java 26.3
// (DataVersion 5006+) and the older {Name, Properties} form.
func chunk263() nbt.Compound {
	strList := &nbt.List{Type: nbt.TagString, Items: []any{"minecraft:stone"}}
	mixed := &nbt.List{Type: nbt.TagCompound, Items: []any{
		nbt.Compound{"": "minecraft:dirt"},
		nbt.Compound{"id": "minecraft:oak_log", "properties": nbt.Compound{"axis": "x"}},
	}}
	old := &nbt.List{Type: nbt.TagCompound, Items: []any{
		nbt.Compound{"Name": "minecraft:birch_log", "Properties": nbt.Compound{"axis": "z"}},
	}}
	// 4 bits per entry, every entry 1 (oak_log)
	data := make([]int64, 256)
	for i := range data {
		data[i] = 0x1111111111111111
	}
	sec := func(y int8, palette *nbt.List, d []int64) nbt.Compound {
		bs := nbt.Compound{"palette": palette}
		if d != nil {
			bs["data"] = d
		}
		return nbt.Compound{"Y": y, "block_states": bs}
	}
	return nbt.Compound{
		"DataVersion": int32(5023),
		"Status":      "minecraft:full",
		"sections": &nbt.List{Type: nbt.TagCompound, Items: []any{
			sec(0, strList, nil),
			sec(1, mixed, data),
			sec(2, old, nil),
		}},
	}
}

func checkPalette(t *testing.T, ch *world.Chunk) {
	t.Helper()
	want := []struct {
		y    int
		name string
		prop string
	}{{0, "minecraft:stone", ""}, {16, "minecraft:oak_log", "x"}, {32, "minecraft:birch_log", "z"}}
	for _, w := range want {
		i := ch.PaletteIndex(3, w.y, 5)
		if i < 0 {
			t.Fatalf("y=%d: no block", w.y)
		}
		b := ch.Palette[i]
		if b.Name() != w.name {
			t.Errorf("y=%d: got %s, want %s", w.y, b.Name(), w.name)
		}
		if w.prop != "" && b.Props["axis"] != w.prop {
			t.Errorf("y=%d: axis=%v, want %s", w.y, b.Props["axis"], w.prop)
		}
	}
}

func TestPalette263Fast(t *testing.T) {
	c := &Codec{}
	raw := nbt.Encode("", chunk263(), nbt.BigEndian)
	ch, _, err := c.NewFastDecoder().Decode(raw, Bounds(world.Overworld))
	if err != nil {
		t.Fatal(err)
	}
	checkPalette(t, ch)
}

func TestPalette263(t *testing.T) {
	c := &Codec{}
	ch, _, err := c.DecodeChunk(chunk263(), Bounds(world.Overworld))
	if err != nil {
		t.Fatal(err)
	}
	checkPalette(t, ch)
}

func TestBlockStateFields(t *testing.T) {
	for _, tc := range []struct {
		in   any
		name string
		n    int
	}{
		{"minecraft:sand", "minecraft:sand", 0},
		{nbt.Compound{"": "minecraft:sand"}, "minecraft:sand", 0},
		{nbt.Compound{"id": "minecraft:oak_log", "properties": nbt.Compound{"axis": "y"}}, "minecraft:oak_log", 1},
		{nbt.Compound{"Name": "minecraft:oak_log", "Properties": nbt.Compound{"axis": "y"}}, "minecraft:oak_log", 1},
	} {
		name, pp := BlockStateFields(tc.in)
		if name != tc.name || len(pp) != tc.n {
			t.Errorf("%v: got %s %v", tc.in, name, pp)
		}
	}
}
