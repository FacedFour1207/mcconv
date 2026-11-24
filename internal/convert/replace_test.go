package convert

import (
	"testing"

	"mcconv/internal/config"
	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

func TestParseReplace(t *testing.T) {
	m, err := ParseReplace(" barrier = invisible_bedrock , minecraft:stone=Minecraft:DIRT,")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["minecraft:barrier"] != "minecraft:invisible_bedrock" || m["minecraft:stone"] != "minecraft:dirt" {
		t.Fatalf("got %v", m)
	}
	for _, bad := range []string{"barrier", "barrier=", "=stone", "a=b,a=c", "minecraft:=stone"} {
		if _, err := ParseReplace(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if m, err := ParseReplace(""); err != nil || len(m) != 0 {
		t.Fatalf("empty: %v %v", m, err)
	}
}

func TestCheckReplace(t *testing.T) {
	data, err := translate.Load()
	if err != nil {
		t.Fatal(err)
	}
	ok := []struct {
		toBedrock bool
		from, to  string
	}{
		{true, "minecraft:barrier", "minecraft:invisible_bedrock"}, // Bedrock only block
		{true, "minecraft:barrier", "minecraft:bedrock"},
		{true, "minecraft:oak_stairs", "minecraft:stone_brick_stairs"}, // Java id, converted
		{true, "minecraft:grass", "minecraft:dirt"},                    // renamed Java block
		{false, "minecraft:invisible_bedrock", "minecraft:barrier"},
		{false, "minecraft:border_block", "minecraft:barrier"},
		{false, "minecraft:stone", "minecraft:oak_planks"}, // Bedrock id with a Java form
	}
	for _, c := range ok {
		if err := CheckReplace(data, c.toBedrock, map[string]string{c.from: c.to}); err != nil {
			t.Errorf("%v %s=%s: %v", c.toBedrock, c.from, c.to, err)
		}
	}
	bad := []struct {
		toBedrock bool
		from, to  string
	}{
		{true, "minecraft:invisible_bedrock", "minecraft:stone"}, // not a Java block
		{true, "minecraft:barrier", "minecraft:no_such_block"},
		{false, "minecraft:no_such_block", "minecraft:stone"},
		{false, "minecraft:barrier", "minecraft:invisible_bedrock"}, // no Java form
	}
	for _, c := range bad {
		if err := CheckReplace(data, c.toBedrock, map[string]string{c.from: c.to}); err == nil {
			t.Errorf("%v %s=%s: no error", c.toBedrock, c.from, c.to)
		}
	}
}

func TestReplaceToBedrock(t *testing.T) {
	data, err := translate.Load()
	if err != nil {
		t.Fatal(err)
	}
	bc := newBlockConverter(data, true, map[string]string{
		"minecraft:barrier":     "minecraft:invisible_bedrock",
		"minecraft:oak_stairs":  "minecraft:stone_brick_stairs",
		"minecraft:chest":       "minecraft:stone",
		"minecraft:short_grass": "minecraft:dirt",
	}, nil)

	e := bc.lookup(translate.NewBlock("minecraft:barrier", map[string]any{"waterlogged": "true"}))
	if e.out.Key() != "minecraft:invisible_bedrock" || e.replaced != "minecraft:barrier" {
		t.Fatalf("barrier: got %s (%q)", e.out.Key(), e.replaced)
	}

	// properties the target has are kept (Java id: its Bedrock form)
	stairs := map[string]any{"facing": "east", "half": "top", "shape": "straight", "waterlogged": "true"}
	e = bc.lookup(translate.NewBlock("minecraft:oak_stairs", stairs))
	want, _ := data.JavaToBedrock(translate.NewBlock("minecraft:stone_brick_stairs", stairs))
	if e.out.Key() != want.Key() || len(e.out.Extra) != 1 {
		t.Fatalf("stairs: got %s, want %s", e.out.Key(), want.Key())
	}

	// old Java ids match their current name
	if e = bc.lookup(translate.NewBlock("minecraft:grass", nil)); e.out.Name() != "minecraft:dirt" || e.replaced != "minecraft:short_grass" {
		t.Fatalf("grass: got %s", e.out.Key())
	}
	if m := replacements(data, Options{Config: testConfig(), Replace: map[string]string{"grass": "dirt"}}, true); m["minecraft:short_grass"] != "minecraft:dirt" {
		t.Fatalf("replacements: %v", m)
	}

	// a chunk: replaced blocks are counted, their block entities dropped
	bc.be = &beConverter{t: data, items: &itemConverter{t: data, toBedrock: true}, toBedrock: true}
	b := world.Bounds{MinSection: -4, MaxSection: 20}
	in := world.NewChunk(0, 0, b)
	in.Palette = []*translate.Block{
		translate.NewBlock("minecraft:air", nil),
		translate.NewBlock("minecraft:barrier", nil),
		translate.NewBlock("minecraft:chest", map[string]any{"facing": "north", "type": "single", "waterlogged": "false"}),
		translate.NewBlock("minecraft:barrel", map[string]any{"facing": "up", "open": "false"}),
	}
	sec := world.NewSection()
	for i := 0; i < 10; i++ {
		sec.Blocks[i] = 1
	}
	sec.Blocks[100] = 2
	sec.Blocks[101] = 3
	in.Sections[4] = sec // y 0..15
	chestPos := world.Pos{X: 100 & 15, Y: 100 >> 8, Z: (100 >> 4) & 15}
	barrelPos := world.Pos{X: 101 & 15, Y: 101 >> 8, Z: (101 >> 4) & 15}
	items := &nbt.List{Type: nbt.TagCompound}
	in.BlockEntities[chestPos] = &translate.BlockEntity{Namespace: "minecraft", BaseName: "chest", NBT: nbt.Compound{"Items": items}}
	in.BlockEntities[barrelPos] = &translate.BlockEntity{Namespace: "minecraft", BaseName: "barrel", NBT: nbt.Compound{"Items": items}}
	out := bc.convertChunk(in, b, nil, nil)
	if _, ok := out.BlockEntities[chestPos]; ok {
		t.Fatal("the replaced chest kept its block entity")
	}
	if _, ok := out.BlockEntities[barrelPos]; !ok {
		t.Fatal("the barrel lost its block entity")
	}
	if got := out.Palette[out.Sections[4].Blocks[100]].Name(); got != "minecraft:stone" {
		t.Fatalf("chest: got %s", got)
	}
	count := func(name string) int64 {
		var n int64
		if c, ok := bc.replaced.Load(name); ok {
			n = c.(interface{ Load() int64 }).Load()
		}
		return n
	}
	if n := count("minecraft:barrier"); n != 10 {
		t.Fatalf("barrier count %d, want 10", n)
	}
	if n := count("minecraft:chest"); n != 1 {
		t.Fatalf("chest count %d, want 1", n)
	}
}

func TestReplaceToJava(t *testing.T) {
	data, err := translate.Load()
	if err != nil {
		t.Fatal(err)
	}
	bc := newBlockConverter(data, false, map[string]string{
		"minecraft:invisible_bedrock": "minecraft:barrier",
		"minecraft:oak_stairs":        "minecraft:stone_brick_stairs",
		"minecraft:stone":             "minecraft:oak_planks", // a Bedrock id
	}, nil)

	e := bc.lookup(translate.NewBlock("minecraft:invisible_bedrock", nil))
	if e.out.Name() != "minecraft:barrier" || e.replaced == "" {
		t.Fatalf("invisible_bedrock: got %s", e.out.Key())
	}

	// a waterlogged upside-down stair keeps its direction and water
	src := translate.NewBlock("minecraft:oak_stairs", map[string]any{"upside_down_bit": int8(1), "weirdo_direction": int32(2)})
	src.Extra = []*translate.Block{translate.NewBlock("minecraft:water", map[string]any{"liquid_depth": int32(0)})}
	plain, ok := data.BedrockToJava(src)
	if !ok {
		t.Fatal("oak_stairs has no Java form")
	}
	e = bc.lookup(src)
	if e.out.Name() != "minecraft:stone_brick_stairs" {
		t.Fatalf("stairs: got %s", e.out.Key())
	}
	for _, p := range []string{"facing", "half", "waterlogged"} {
		if e.out.Props[p] != plain.Props[p] {
			t.Errorf("stairs %s: got %v, want %v", p, e.out.Props[p], plain.Props[p])
		}
	}
	if e.out.Props["waterlogged"] != "true" {
		t.Errorf("stairs lost their water: %s", e.out.Key())
	}

	if e = bc.lookup(translate.NewBlock("minecraft:stone", nil)); e.out.Name() != "minecraft:oak_planks" {
		t.Fatalf("stone: got %s", e.out.Key())
	}
}

func TestParseReplaceCustom(t *testing.T) {
	m, err := ParseReplace(`oak_sapling = Addon:Custom_Block[ stage = 1 , open=true, label="a b" ], stone=addon:plain, addon:old=dirt`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"minecraft:oak_sapling": `addon:custom_block[stage=1,open=true,label="a b"]`,
		"minecraft:stone":       "addon:plain",
		"addon:old":             "minecraft:dirt",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s: got %q, want %q", k, m[k], v)
		}
	}
	for _, bad := range []string{
		"stone=dirt[snowy=true]", // states only for add-on / mod blocks
		"addon:a[x=1]=stone",     // not on the source block
		"stone=addon:a[x]",       // no value
		"stone=addon:a[x=1,x=2]", // twice
		"stone=addon:a[x=1",      // not closed
		`stone=addon:a[x=a"b]`,   // stray quote
	} {
		if _, err := ParseReplace(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestReplaceCustom(t *testing.T) {
	data, err := translate.Load()
	if err != nil {
		t.Fatal(err)
	}
	// unknown add-on / mod ids are accepted on both sides
	for _, tb := range []bool{true, false} {
		if err := CheckReplace(data, tb, map[string]string{"minecraft:stone": "addon:custom_block", "addon:other": "minecraft:dirt"}); err != nil {
			t.Errorf("toBedrock=%v: %v", tb, err)
		}
		if err := CheckReplace(data, tb, map[string]string{"minecraft:stone": "minecraft:no_such_block"}); err == nil {
			t.Errorf("toBedrock=%v: unknown vanilla id accepted", tb)
		}
	}

	// to Bedrock: typed states, the current block state version
	bc := newBlockConverter(data, true, map[string]string{
		"minecraft:oak_sapling": `addon:custom_block[stage=1,open=true,label="2"]`,
		"minecraft:stone":       "addon:plain",
		"addon:old":             "minecraft:dirt",
	}, nil)
	e := bc.lookup(translate.NewBlock("minecraft:oak_sapling", map[string]any{"stage": "0"}))
	p := e.out.Props
	if e.out.Name() != "addon:custom_block" || p["stage"] != int32(1) || p["open"] != int8(1) || p["label"] != "2" || e.out.Version != data.BedrockBlockVersion {
		t.Fatalf("sapling: got %s %#v v%d", e.out.Name(), p, e.out.Version)
	}
	if e = bc.lookup(translate.NewBlock("minecraft:stone", nil)); e.out.Key() != "addon:plain" || e.replaced == "" {
		t.Fatalf("stone: got %s", e.out.Key())
	}
	if e = bc.lookup(translate.NewBlock("addon:old", map[string]any{"x": "1"})); e.out.Name() != "minecraft:dirt" {
		t.Fatalf("mod block: got %s", e.out.Key())
	}

	// to Java: string states; older target releases keep mod blocks
	bc = newBlockConverter(data, false, map[string]string{"minecraft:stone": "addon:custom_block[stage=1,open=true]", "addon:old": "minecraft:oak_planks"}, nil)
	e = bc.lookup(translate.NewBlock("minecraft:stone", nil))
	if e.out.Name() != "addon:custom_block" || e.out.Props["stage"] != "1" || e.out.Props["open"] != "true" {
		t.Fatalf("java: got %s %#v", e.out.Name(), e.out.Props)
	}
	if e2 := bc.lookup(translate.NewBlock("addon:old", nil)); e2.out.Name() != "minecraft:oak_planks" {
		t.Fatalf("add-on block: got %s", e2.out.Key())
	}
	jt, err := data.JavaTarget(translate.MinJavaDataVersion)
	if err != nil || jt == nil {
		t.Fatalf("java target: %v", err)
	}
	if got := jt.Block(e.out); got != e.out {
		t.Fatalf("older Java target changed the mod block to %s", got.Key())
	}
}

func testConfig() *config.Config { return &config.Config{Versions: config.DefaultVersions()} }
