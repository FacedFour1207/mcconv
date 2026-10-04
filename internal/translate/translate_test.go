package translate

import (
	"sort"
	"testing"

	"mcconv/internal/nbt"
)

func TestRoundTrip(t *testing.T) {
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tables: Java %s (%d), Bedrock %s, block version %s", tb.JavaVersion, tb.JavaDataVersion, tb.BedrockVersion, VersionString(tb.BedrockBlockVersion))
	lossy := map[string]int{}
	exact := 0
	for id := range int32(len(tb.stateOf)) {
		j := tb.JavaBlock(id)
		b, ok := tb.JavaToBedrock(j)
		if !ok {
			t.Fatalf("no Bedrock block for %s", j.Key())
		}
		back, ok := tb.BedrockToJava(b)
		if !ok {
			t.Fatalf("no Java block for %s (from %s)", b.Key(), j.Key())
		}
		if back == j {
			exact++
			continue
		}
		if back.Name() != j.Name() {
			lossy[j.Name()+" -> "+back.Name()]++
			continue
		}
		for k, v := range j.Props {
			if back.Props[k] != v {
				lossy[j.Name()+"."+k]++
			}
		}
	}
	keys := make([]string, 0, len(lossy))
	for k := range lossy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("%d of %d Java states round trip exactly; lossy properties/blocks: %d", exact, len(tb.stateOf), len(keys))
	for _, k := range keys {
		t.Logf("  %s (%d)", k, lossy[k])
	}
}

func TestUpgrade(t *testing.T) {
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		states map[string]any
		want   string
	}{
		{"minecraft:wool", map[string]any{"color": "red"}, "minecraft:red_wool"},
		{"minecraft:log", map[string]any{"old_log_type": "birch", "pillar_axis": "x"}, "minecraft:birch_log[axis=\"x\"]"},
		{"minecraft:stone", map[string]any{"stone_type": "granite"}, "minecraft:granite"},
		{"minecraft:grass", map[string]any{}, "minecraft:grass_block[snowy=\"false\"]"},
		{"minecraft:oak_stairs", map[string]any{"upside_down_bit": int8(0), "weirdo_direction": int32(2)}, "minecraft:oak_stairs[facing=\"south\",half=\"bottom\",shape=\"straight\",waterlogged=\"false\"]"},
	}
	for _, c := range cases {
		b := NewBlock(c.name, c.states)
		b.Version = 17694723 // 1.13.0.3
		out, ok := tb.BedrockToJava(b)
		if !ok {
			t.Errorf("%s: no conversion", c.name)
			continue
		}
		if out.Key() != c.want {
			t.Errorf("%s %v -> %s, want %s", c.name, c.states, out.Key(), c.want)
		}
	}
}

func TestText(t *testing.T) {
	c := nbt.Compound{"text": "Hello ", "color": "red", "extra": &nbt.List{Items: []any{nbt.Compound{"text": "World", "bold": int8(1)}}}}
	s := JavaTextToSection(c)
	if s != "§cHello §r§c§lWorld" {
		t.Errorf("JavaTextToSection = %q", s)
	}
	if JavaTextToSection(`{"text":"x","color":"#55FF55"}`) != "§ax" {
		t.Errorf("hex colour")
	}
	back := SectionToJavaText("§cHello §lWorld")
	if JavaTextToSection(back) != "§cHello §r§c§lWorld" {
		t.Errorf("round trip: %v -> %q", back, JavaTextToSection(back))
	}
	if SectionToJavaText("plain") != "plain" {
		t.Error("plain text")
	}
}
