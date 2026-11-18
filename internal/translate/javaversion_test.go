package translate

import (
	"slices"
	"testing"

	"mcconv/internal/nbt"
)

func TestJavaTargetBlocks(t *testing.T) {
	tb := testTables(t)
	vf, err := loadJavaVersions()
	if err != nil {
		t.Fatal(err)
	}
	for _, set := range vf.Sets {
		jt, err := tb.JavaTarget(set.DataVersion)
		if err != nil || jt == nil {
			t.Fatalf("%s: %v", set.Name, err)
		}
		var toAir []string
		for i := range tb.blocks {
			jb := &tb.blocks[i]
			for s := range jb.count {
				in := tb.JavaBlock(jb.first + s)
				out := jt.Block(in)
				if out.Name() == "minecraft:air" && in.Name() != "minecraft:air" && !slices.Contains(toAir, in.Name()) {
					toAir = append(toAir, in.Name())
				}
				ob := jt.blocks[out.Name()]
				if ob == nil {
					t.Errorf("%s: %s -> %s (not a block of the target)", set.Name, in.Key(), out.Key())
					continue
				}
				for _, p := range ob.Props {
					if v, _ := out.Props[p.Name].(string); indexOf(p.Values, v) < 0 {
						t.Errorf("%s: %s -> %s: bad %s", set.Name, in.Key(), out.Key(), p.Name)
					}
				}
				if jt.blocks[in.Name()] != nil && in.Name() != out.Name() {
					t.Errorf("%s: %s renamed to %s although the target has it", set.Name, in.Name(), out.Name())
				}
			}
		}
		t.Logf("%s: blocks replaced with air: %v", set.Name, toAir)
	}
}

// The default state of blocks the tables and the newest set share must
// agree (checks the property order of the generated data).
func TestJavaVersionDefaults(t *testing.T) {
	tb := testTables(t)
	vf, err := loadJavaVersions()
	if err != nil {
		t.Fatal(err)
	}
	jt, _ := tb.JavaTarget(vf.Sets[len(vf.Sets)-1].DataVersion)
	n := 0
	for name, ob := range jt.blocks {
		jb := tb.byName[name]
		if jb == nil || len(jb.Props) != len(ob.Props) {
			continue
		}
		if !slices.Equal(jb.defaultVal, ob.defaultVal) {
			t.Errorf("%s: default %v, tables %v", name, ob.defaultVal, jb.defaultVal)
		}
		n++
	}
	if n < 1000 {
		t.Errorf("only %d blocks compared", n)
	}
}

func TestJavaTargetItems(t *testing.T) {
	tb := testTables(t)
	jt, err := tb.JavaTarget(MinJavaDataVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ in, out string }{
		{"minecraft:stone", "minecraft:stone"},
		{"minecraft:pale_oak_planks", "minecraft:dark_oak_planks"},
		{"minecraft:red_bundle", ""}, // bundles were an experiment in 1.21.1
		{"minecraft:iron_chain", "minecraft:chain"},
		{"minecraft:waxed_copper_lantern", "minecraft:lantern"},
		{"minecraft:iron_spear", "minecraft:iron_sword"},
		{"minecraft:music_disc_tears", ""},
	} {
		got, ok := jt.Item(c.in)
		if !ok {
			got = ""
		}
		if got != c.out {
			t.Errorf("Item(%s) = %q, want %q", c.in, got, c.out)
		}
	}
	if b := jt.Biome("minecraft:pale_garden"); b != "minecraft:dark_forest" {
		t.Errorf("pale_garden -> %s", b)
	}
	if jt.Entity("minecraft:creaking") || !jt.Entity("minecraft:zombie") {
		t.Error("entity list")
	}
	if r, ok := tb.JavaReleaseByName("1.21.1"); !ok || r.DataVersion != 3955 {
		t.Errorf("release 1.21.1: %v %v", r, ok)
	}
}

func testTables(t *testing.T) *Tables {
	t.Helper()
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func TestMixedTextList(t *testing.T) {
	// 1.21.5+ stores strings in lists of compounds as {"": "..."}
	if got := JavaTextToSection(nbt.Compound{"": "first line"}); got != "first line" {
		t.Errorf("got %q", got)
	}
	if got := JavaTextToJSON(nbt.Compound{"text": "a", "color": "red", "bold": int8(1)}); got != `{"bold":true,"color":"red","text":"a"}` {
		t.Errorf("json %s", got)
	}
	if got := JavaTextToJSON("plain"); got != `"plain"` {
		t.Errorf("json %s", got)
	}
}
