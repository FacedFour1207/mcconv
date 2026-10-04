package convert

import (
	"math"
	"strings"

	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// beConverter converts block entities. Data both editions store (items,
// names, loot tables, sign text, banner patterns, ...) is carried over;
// data the target edition keeps in the block state instead is taken from
// the converted block (and the other way round).
type beConverter struct {
	t         *translate.Tables
	items     *itemConverter
	toBedrock bool
}

// Java block entity id -> Bedrock block entity id.
var javaToBedrockBE = map[string]string{
	"chest": "Chest", "trapped_chest": "Chest", "ender_chest": "EnderChest", "furnace": "Furnace",
	"blast_furnace": "BlastFurnace", "smoker": "Smoker", "hopper": "Hopper", "dispenser": "Dispenser",
	"dropper": "Dropper", "brewing_stand": "BrewingStand", "beacon": "Beacon", "mob_spawner": "MobSpawner",
	"sign": "Sign", "hanging_sign": "HangingSign", "banner": "Banner", "bed": "Bed", "skull": "Skull",
	"jukebox": "Jukebox", "lectern": "Lectern", "campfire": "Campfire", "barrel": "Barrel",
	"shulker_box": "ShulkerBox", "enchanting_table": "EnchantTable", "end_portal": "EndPortal",
	"end_gateway": "EndGateway", "command_block": "CommandBlock", "comparator": "Comparator",
	"daylight_detector": "DaylightDetector", "conduit": "Conduit", "bell": "Bell", "beehive": "Beehive",
	"piston": "PistonArm", "structure_block": "StructureBlock", "jigsaw": "JigsawBlock",
	"sculk_sensor": "SculkSensor", "calibrated_sculk_sensor": "CalibratedSculkSensor",
	"sculk_catalyst": "SculkCatalyst", "sculk_shrieker": "SculkShrieker",
	"chiseled_bookshelf": "ChiseledBookshelf", "brushable_block": "BrushableBlock",
	"decorated_pot": "DecoratedPot", "crafter": "Crafter", "trial_spawner": "TrialSpawner",
	"vault": "Vault", "creaking_heart": "CreakingHeart", "shelf": "Shelf",
}

var bedrockToJavaBE = func() map[string]string {
	m := map[string]string{}
	for j, b := range javaToBedrockBE {
		if _, dup := m[b]; !dup || j == "chest" {
			m[b] = j
		}
	}
	return m
}()

// extra banner pattern codes (newer than the pattern table source)
var extraBannerPatterns = map[string]string{"globe": "glb", "piglin": "pig", "flow": "flw", "guster": "gus"}

var skullTypes = map[string]int8{"skeleton": 0, "wither_skeleton": 1, "zombie": 2, "player": 3, "creeper": 4, "dragon": 5, "piglin": 6}

// sign text colours (Bedrock stores ARGB, Java the dye name)
var dyeRGB = []int32{0xF9FFFE, 0xF9801D, 0xC74EBD, 0x3AB3DA, 0xFED83D, 0x80C71F, 0xF38BAA, 0x474F52,
	0x9D9D97, 0x169C9C, 0x8932B8, 0x3C44AA, 0x835432, 0x5E7C16, 0xB02E26, 0x000000}

// brewing stand slot order differs: Java 0-2 bottles, 3 ingredient, 4 fuel;
// Bedrock 0 ingredient, 1-3 bottles, 4 fuel.
var brewJavaToBedrock = map[int64]int64{0: 1, 1: 2, 2: 3, 3: 0, 4: 4}

// beacon effects: Java id <-> Bedrock effect number
var beaconEffects = map[string]int32{"speed": 1, "haste": 3, "strength": 5, "jump_boost": 8, "regeneration": 10, "resistance": 11}

func (c *beConverter) convert(inBlock, outBlock *translate.Block, be *translate.BlockEntity, pos world.Pos) *translate.BlockEntity {
	if be == nil {
		return nil
	}
	if c.toBedrock {
		return c.toBedrockBE(inBlock, be, pos)
	}
	return c.toJavaBE(outBlock, be)
}

func (c *beConverter) toBedrockBE(java *translate.Block, be *translate.BlockEntity, pos world.Pos) *translate.BlockEntity {
	id := strings.ToLower(be.BaseName)
	bid, ok := javaToBedrockBE[id]
	if !ok {
		return nil
	}
	s := be.NBT
	o := nbt.Compound{"isMovable": int8(1)}
	c.common(s, o)
	n := strip(java.Name())
	switch id {
	case "chest", "trapped_chest":
		typ, _ := java.Props["type"].(string)
		facing, _ := java.Props["facing"].(string)
		if off, ok := dirOffset[clockwise[facing]]; ok && (typ == "left" || typ == "right") {
			if typ == "right" {
				off = [2]int{-off[0], -off[1]}
			}
			o["pairx"] = pos.X + int32(off[0])
			o["pairz"] = pos.Z + int32(off[1])
			o["pairlead"] = b8(typ == "left")
		}
		o["Findable"] = int8(0)
	case "bed":
		ci, _ := colourOf(n)
		if ci < 0 {
			ci = 14
		}
		o["color"] = int8(ci)
	case "banner":
		ci, _ := colourOf(n)
		if ci < 0 {
			ci = 0
		}
		o["Base"] = int32(15 - ci)
		o["Type"] = int32(0)
		o["Patterns"] = c.bannerPatternsToBedrock(s)
	case "shulker_box":
		facing, _ := java.Props["facing"].(string)
		f := int8(1)
		for i, v := range shulkerFacing {
			if v == facing {
				f = int8(i)
			}
		}
		o["facing"] = f
	case "skull":
		rot := float32(0)
		if r, ok := java.Props["rotation"].(string); ok {
			var v int
			for _, ch := range r {
				v = v*10 + int(ch-'0')
			}
			rot = float32(v) * 22.5
		}
		o["Rotation"] = rot
		kind := strings.TrimSuffix(strings.TrimSuffix(strings.Replace(n, "_wall", "", 1), "_skull"), "_head")
		o["SkullType"] = skullTypes[kind]
		o["MouthMoving"] = int8(0)
		o["MouthTickCount"] = int32(0)
		o["DoingAnimation"] = int8(0)
	case "sign", "hanging_sign":
		c.signToBedrock(s, o)
	case "furnace", "blast_furnace", "smoker":
		o["BurnTime"] = int16(firstNumber(s, "lit_time_remaining", "BurnTime"))
		o["CookTime"] = int16(firstNumber(s, "cooking_time_spent", "CookTime"))
		o["BurnDuration"] = int16(firstNumber(s, "lit_total_time", "BurnDuration"))
	case "brewing_stand":
		if l, ok := o["Items"].(*nbt.List); ok {
			remapSlots(l, brewJavaToBedrock)
		}
		fuel, _ := s.Number("Fuel")
		o["FuelAmount"] = int16(fuel)
		o["FuelTotal"] = int16(20)
		bt, _ := s.Number("BrewTime")
		o["CookTime"] = int16(bt)
	case "beacon":
		o["primary"] = beaconEffects[strip(str(s["primary_effect"]))]
		o["secondary"] = beaconEffects[strip(str(s["secondary_effect"]))]
	case "mob_spawner":
		copyNumbers(s, o, spawnerFields, func(v int64) any { return int16(v) })
		if id := spawnerEntity(s); id != "" {
			if b, ok := javaToBedrockEntityIDs[strip(id)]; ok {
				id = "minecraft:" + b
			}
			o["EntityIdentifier"] = id
		}
	case "hopper":
		copyNumbers(s, o, []string{"TransferCooldown"}, func(v int64) any { return int32(v) })
	case "barrel":
		o["Findable"] = int8(0)
	case "jukebox":
		if it, ok := s.Compound("RecordItem"); ok {
			if conv := c.items.convert(it); conv != nil {
				o["RecordItem"] = conv
			}
		}
	case "lectern":
		if it, ok := s.Compound("Book"); ok {
			if conv := c.items.convert(it); conv != nil {
				o["book"] = conv
				o["hasBook"] = int8(1)
				page, _ := s.Number("Page")
				o["page"] = int32(page)
			}
		}
	case "campfire":
		if l, ok := s.List("Items"); ok {
			times, _ := s["CookingTimes"].([]int32)
			for _, it := range c.items.convertList(l).Items {
				ic := it.(nbt.Compound)
				slot, _ := ic.Number("Slot")
				delete(ic, "Slot")
				if slot >= 0 && slot < 4 {
					o["Item"+itoa(int(slot)+1)] = ic
					if int(slot) < len(times) {
						o["ItemTime"+itoa(int(slot)+1)] = times[slot]
					}
				}
			}
			delete(o, "Items")
		}
	case "command_block":
		for _, k := range []string{"Command", "auto", "powered", "TrackOutput", "SuccessCount", "conditionMet", "LastOutput"} {
			if v, ok := s[k]; ok {
				o[k] = v
			}
		}
	case "end_gateway":
		if p, ok := s["exit_portal"].([]int32); ok && len(p) == 3 {
			o["ExitPortal"] = &nbt.List{Type: nbt.TagInt, Items: []any{p[0], p[1], p[2]}}
		}
		age, _ := s.Number("Age")
		o["Age"] = int32(age)
	case "decorated_pot":
		if l, ok := s.List("sherds"); ok {
			o["sherds"] = l
		}
		if it, ok := s.Compound("item"); ok {
			if conv := c.items.convert(it); conv != nil {
				o["item"] = conv
			}
		}
	}
	return &translate.BlockEntity{BaseName: bid, NBT: o}
}

func (c *beConverter) toJavaBE(java *translate.Block, be *translate.BlockEntity) *translate.BlockEntity {
	id, ok := bedrockToJavaBE[be.BaseName]
	if !ok {
		return nil
	}
	n := strip(java.Name())
	switch {
	case id == "chest" && n == "trapped_chest":
		id = "trapped_chest"
	case id == "chest" && n != "chest" && !strings.HasSuffix(n, "copper_chest"):
		return nil // block and block entity do not match
	}
	s := be.NBT
	o := nbt.Compound{}
	c.common(s, o)
	switch id {
	case "banner":
		o["patterns"] = c.bannerPatternsToJava(s)
	case "sign", "hanging_sign":
		c.signToJava(s, o)
	case "skull":
		// rotation lives in the block state (see skullRule)
	case "furnace", "blast_furnace", "smoker":
		o["lit_time_remaining"] = int32(firstNumber(s, "BurnTime"))
		o["cooking_time_spent"] = int32(firstNumber(s, "CookTime"))
		o["lit_total_time"] = int32(firstNumber(s, "BurnDuration"))
		o["cooking_total_time"] = int32(200)
	case "brewing_stand":
		if l, ok := o["Items"].(*nbt.List); ok {
			inv := map[int64]int64{}
			for k, v := range brewJavaToBedrock {
				inv[v] = k
			}
			remapSlots(l, inv)
		}
		fuel, _ := s.Number("FuelAmount")
		o["Fuel"] = int8(fuel)
		bt, _ := s.Number("CookTime")
		o["BrewTime"] = int16(bt)
	case "beacon":
		for _, k := range []string{"primary", "secondary"} {
			v, _ := s.Number(k)
			for name, num := range beaconEffects {
				if int64(num) == v {
					o[k+"_effect"] = "minecraft:" + name
				}
			}
		}
	case "mob_spawner":
		copyNumbers(s, o, spawnerFields, func(v int64) any { return int16(v) })
		id, _ := s.String("EntityIdentifier")
		o["SpawnPotentials"] = &nbt.List{Type: nbt.TagCompound}
		if id != "" {
			if j, ok := bedrockToJavaEntityIDs[strip(id)]; ok {
				id = "minecraft:" + j
			}
			o["SpawnData"] = nbt.Compound{"entity": nbt.Compound{"id": id}}
		}
	case "hopper":
		copyNumbers(s, o, []string{"TransferCooldown"}, func(v int64) any { return int32(v) })
	case "jukebox":
		if it, ok := s.Compound("RecordItem"); ok {
			if conv := c.items.convert(it); conv != nil {
				delete(conv, "Slot")
				o["RecordItem"] = conv
			}
		}
	case "lectern":
		if it, ok := s.Compound("book"); ok {
			if conv := c.items.convert(it); conv != nil {
				delete(conv, "Slot")
				o["Book"] = conv
				page, _ := s.Number("page")
				o["Page"] = int32(page)
			}
		}
	case "campfire":
		l := &nbt.List{Type: nbt.TagCompound}
		times := make([]int32, 4)
		totals := []int32{600, 600, 600, 600}
		for i := 1; i <= 4; i++ {
			if it, ok := s.Compound("Item" + itoa(i)); ok {
				if conv := c.items.convert(it); conv != nil {
					conv["Slot"] = int8(i - 1)
					l.Items = append(l.Items, conv)
				}
			}
			t, _ := s.Number("ItemTime" + itoa(i))
			times[i-1] = int32(t)
		}
		o["Items"] = l
		o["CookingTimes"] = times
		o["CookingTotalTimes"] = totals
	case "command_block":
		for _, k := range []string{"Command", "auto", "powered", "TrackOutput", "SuccessCount", "conditionMet", "LastOutput"} {
			if v, ok := s[k]; ok {
				o[k] = v
			}
		}
	case "end_gateway":
		if l, ok := s.List("ExitPortal"); ok && len(l.Items) == 3 {
			p := make([]int32, 3)
			for i, v := range l.Items {
				n, _ := nbt.AsInt(v)
				p[i] = int32(n)
			}
			o["exit_portal"] = p
		}
		age, _ := s.Number("Age")
		o["Age"] = age
	case "decorated_pot":
		if l, ok := s.List("sherds"); ok {
			o["sherds"] = l
		}
		if it, ok := s.Compound("item"); ok {
			if conv := c.items.convert(it); conv != nil {
				delete(conv, "Slot")
				o["item"] = conv
			}
		}
	}
	return &translate.BlockEntity{Namespace: "minecraft", BaseName: id, NBT: o}
}

// common converts the fields shared by most block entities: items, custom
// names and loot tables.
func (c *beConverter) common(s, o nbt.Compound) {
	if l, ok := s.List("Items"); ok {
		o["Items"] = c.items.convertList(l)
	}
	if lt, ok := s.String("LootTable"); ok && lt != "" {
		if c.toBedrock {
			o["LootTable"] = lootJavaToBedrock(lt)
			if seed, ok := s.Number("LootTableSeed"); ok {
				o["LootTableSeed"] = int32(seed)
			}
		} else {
			o["LootTable"] = lootBedrockToJava(lt)
			if seed, ok := s.Number("LootTableSeed"); ok {
				o["LootTableSeed"] = seed
			}
		}
	}
	if cn, ok := s["CustomName"]; ok {
		if c.toBedrock {
			if txt := translate.JavaTextToSection(cn); txt != "" {
				o["CustomName"] = txt
			}
		} else if str, ok := cn.(string); ok && str != "" {
			o["CustomName"] = translate.SectionToJavaText(str)
		}
	}
}

func (c *beConverter) bannerPatternsToBedrock(s nbt.Compound) *nbt.List {
	out := &nbt.List{Type: nbt.TagCompound}
	if l, ok := s.List("patterns"); ok { // 1.20.5+
		for _, it := range l.Items {
			p, _ := it.(nbt.Compound)
			name := str(p["pattern"])
			code, ok := c.t.BannerPatternToBedrock(name)
			if !ok {
				code, ok = extraBannerPatterns[strip(name)]
			}
			if !ok {
				continue
			}
			ci := colourIndex(str(p["color"]))
			if ci < 0 {
				ci = 0
			}
			out.Items = append(out.Items, nbt.Compound{"Pattern": code, "Color": int32(15 - ci)})
		}
	} else if l, ok := s.List("Patterns"); ok { // older Java: Bedrock codes already
		for _, it := range l.Items {
			p, _ := it.(nbt.Compound)
			code, _ := p.String("Pattern")
			col, _ := p.Number("Color")
			out.Items = append(out.Items, nbt.Compound{"Pattern": code, "Color": int32(15 - col)})
		}
	}
	return out
}

func (c *beConverter) bannerPatternsToJava(s nbt.Compound) *nbt.List {
	out := &nbt.List{Type: nbt.TagCompound}
	l, _ := s.List("Patterns")
	if l == nil {
		return out
	}
	for _, it := range l.Items {
		p, _ := it.(nbt.Compound)
		code, _ := p.String("Pattern")
		name, ok := c.t.BannerPatternToJava(code)
		if !ok {
			for n, cd := range extraBannerPatterns {
				if cd == code {
					name, ok = "minecraft:"+n, true
				}
			}
		}
		if !ok {
			continue
		}
		col, _ := p.Number("Color")
		if col < 0 || col > 15 {
			col = 15
		}
		out.Items = append(out.Items, nbt.Compound{"pattern": name, "color": colours[15-col]})
	}
	return out
}

func (c *beConverter) signToBedrock(s, o nbt.Compound) {
	side := func(key, legacyPrefix string) nbt.Compound {
		var lines []any
		colour, glow := "black", false
		if t, ok := s.Compound(key); ok {
			if l, ok := t.List("messages"); ok {
				lines = l.Items
			}
			if col, ok := t.String("color"); ok {
				colour = col
			}
			g, _ := t.Number("has_glowing_text")
			glow = g != 0
		} else if legacyPrefix != "" { // before 1.20
			for i := 1; i <= 4; i++ {
				lines = append(lines, s["Text"+itoa(i)])
			}
			if col, ok := s.String("Color"); ok {
				colour = col
			}
			g, _ := s.Number("GlowingText")
			glow = g != 0
		}
		parts := make([]string, 0, 4)
		for _, l := range lines {
			parts = append(parts, translate.JavaTextToSection(l))
		}
		argb := int32(-16777216) // opaque black
		if ci := colourIndex(colour); ci >= 0 && colour != "black" {
			argb = dyeRGB[ci] | -16777216
		}
		return nbt.Compound{
			"Text": strings.TrimRight(strings.Join(parts, "\n"), "\n"), "SignTextColor": argb,
			"IgnoreLighting": b8(glow), "HideGlowOutline": int8(0), "PersistFormatting": int8(1), "TextOwner": "",
		}
	}
	o["FrontText"] = side("front_text", "Text")
	o["BackText"] = side("back_text", "")
	w, _ := s.Number("is_waxed")
	o["IsWaxed"] = b8(w != 0)
}

func (c *beConverter) signToJava(s, o nbt.Compound) {
	side := func(key string) nbt.Compound {
		t, _ := s.Compound(key)
		if t == nil && key == "FrontText" { // before Bedrock 1.19.80
			t = nbt.Compound{"Text": s["Text"], "SignTextColor": s["SignTextColor"], "IgnoreLighting": s["IgnoreLighting"]}
		}
		text, _ := t.String("Text")
		lines := strings.Split(text, "\n")
		msgs := &nbt.List{Type: nbt.TagString}
		compound := false
		for i := 0; i < 4; i++ {
			v := any("")
			if i < len(lines) {
				v = translate.SectionToJavaText(lines[i])
			}
			if _, ok := v.(nbt.Compound); ok {
				compound = true
			}
			msgs.Items = append(msgs.Items, v)
		}
		if compound {
			msgs.Type = nbt.TagCompound
			for i, v := range msgs.Items {
				if str, ok := v.(string); ok {
					msgs.Items[i] = nbt.Compound{"text": str}
				}
			}
		}
		colour := "black"
		if argb, ok := t.Number("SignTextColor"); ok {
			colour = nearestDye(int32(argb) & 0xFFFFFF)
		}
		glow, _ := t.Number("IgnoreLighting")
		return nbt.Compound{"messages": msgs, "color": colour, "has_glowing_text": b8(glow != 0)}
	}
	o["front_text"] = side("FrontText")
	o["back_text"] = side("BackText")
	w, _ := s.Number("IsWaxed")
	o["is_waxed"] = b8(w != 0)
}

func nearestDye(rgb int32) string {
	best, bestD := "black", math.MaxInt64
	for i, c := range dyeRGB {
		dr, dg, db := int(rgb>>16&0xFF)-int(c>>16&0xFF), int(rgb>>8&0xFF)-int(c>>8&0xFF), int(rgb&0xFF)-int(c&0xFF)
		if d := dr*dr + dg*dg + db*db; d < bestD {
			best, bestD = colours[i], d
		}
	}
	return best
}

// spawner timing fields (the same names in both editions)
var spawnerFields = []string{"Delay", "MinSpawnDelay", "MaxSpawnDelay", "SpawnCount", "MaxNearbyEntities", "RequiredPlayerRange", "SpawnRange"}

func copyNumbers(s, o nbt.Compound, keys []string, conv func(int64) any) {
	for _, k := range keys {
		if v, ok := s.Number(k); ok {
			o[k] = conv(v)
		}
	}
}

func spawnerEntity(s nbt.Compound) string {
	if sd, ok := s.Compound("SpawnData"); ok {
		if ent, ok := sd.Compound("entity"); ok {
			id, _ := ent.String("id")
			return id
		}
		id, _ := sd.String("id")
		return id
	}
	return ""
}

func firstNumber(s nbt.Compound, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := s.Number(k); ok {
			return v
		}
	}
	return 0
}

func str(v any) string { s, _ := v.(string); return s }

func lootJavaToBedrock(s string) string {
	ns, path, ok := strings.Cut(s, ":")
	if !ok {
		path = ns
	}
	return "loot_tables/" + path + ".json"
}

func lootBedrockToJava(s string) string {
	s = strings.TrimPrefix(s, "loot_tables/")
	s = strings.TrimSuffix(s, ".json")
	return "minecraft:" + s
}

func remapSlots(l *nbt.List, m map[int64]int64) {
	for _, it := range l.Items {
		c, ok := it.(nbt.Compound)
		if !ok {
			continue
		}
		slot, _ := c.Number("Slot")
		if n, ok := m[slot]; ok {
			c["Slot"] = int8(n)
		}
	}
}
