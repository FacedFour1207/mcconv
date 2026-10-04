package convert

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"strings"
	"sync/atomic"
	"unicode"

	"mcconv/internal/bedrock"
	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// Entities: this converts the parts both editions share: identity, position, rotation, motion, names, tags, health, age,
// equipment, simple variants and container contents. Item frames are entities
// in Java but blocks in Bedrock and are converted between the two.

type bedrockEntityDef struct {
	Spawn  []string `json:"spawn"`
	Born   []string `json:"born"`
	Health float64  `json:"health"`
}

// bedrockEntityDefs holds the spawn component groups of Bedrock entities,
// read from the installed game (see setup); it may be empty.
var bedrockEntityDefs = map[string]bedrockEntityDef{}

var javaToBedrockEntityIDs = map[string]string{
	"zombified_piglin":  "zombie_pigman",
	"evoker":            "evocation_illager",
	"villager":          "villager_v2",
	"zombie_villager":   "zombie_villager_v2",
	"tropical_fish":     "tropicalfish",
	"end_crystal":       "ender_crystal",
	"experience_orb":    "xp_orb",
	"snow_golem":        "snow_golem",
	"mooshroom":         "mooshroom",
	"firework_rocket":   "fireworks_rocket",
	"eye_of_ender":      "eye_of_ender_signal",
	"trident":           "thrown_trident",
	"experience_bottle": "xp_bottle",
}

var bedrockToJavaEntityIDs = func() map[string]string {
	m := map[string]string{"villager": "villager", "zombie_villager": "zombie_villager"}
	for j, b := range javaToBedrockEntityIDs {
		m[b] = j
	}
	return m
}()

// entities that exist in Bedrock without a behaviour definition
var bedrockBuiltinEntities = map[string]bool{
	"item": true, "painting": true, "xp_orb": true, "falling_block": true, "leash_knot": true,
	"boat": true, "chest_boat": true,
}

// transient entities that are not worth converting
var skipEntities = map[string]bool{
	"arrow": true, "spectral_arrow": true, "trident": true, "snowball": true, "egg": true,
	"ender_pearl": true, "potion": true, "splash_potion": true, "lingering_potion": true,
	"fireball": true, "small_fireball": true, "dragon_fireball": true, "wither_skull": true,
	"shulker_bullet": true, "llama_spit": true, "fishing_bobber": true, "firework_rocket": true,
	"eye_of_ender": true, "experience_bottle": true, "area_effect_cloud": true, "lightning_bolt": true,
	"marker": true, "block_display": true, "item_display": true, "text_display": true, "interaction": true,
	"wind_charge": true, "breeze_wind_charge": true, "evoker_fangs": true, "player": true,
	"fishing_hook": true, "xp_bottle": true, "thrown_trident": true, "fireworks_rocket": true,
	"eye_of_ender_signal": true, "wither_skull_dangerous": true, "tripod_camera": true, "agent": true, "npc": true,
}

var boatWoods = []string{"oak", "spruce", "birch", "jungle", "acacia", "dark_oak", "mangrove", "bamboo", "cherry", "pale_oak"}

// entityConverter converts entities between editions.
type entityConverter struct {
	// digests maps Bedrock chunk key prefixes to actor ids (preloaded);
	// nil means look them up in the database per chunk.
	digests map[string][]byte
	data    *translate.Tables
	bc      *blockConverter
	items   *itemConverter
	count   atomic.Int64
	uid     atomic.Uint32
}

func newEntityConverter(data *translate.Tables, items *itemConverter, bc *blockConverter) *entityConverter {
	return &entityConverter{data: data, items: items, bc: bc}
}

// knownBedrock reports whether Bedrock has an entity identifier.
func (ec *entityConverter) knownBedrock(id string) bool {
	_, ok := bedrockEntityDefs["minecraft:"+id]
	return ok || ec.data.IsBedrockEntity(id)
}

// Actor ids are allocated in session -1; the written level.dat starts the
// game's own counter elsewhere so ids never collide.
const (
	actorSession     = -1
	actorSessionBits = uint64(0xFFFFFFFF) // actorSession as uint32
)

func (ec *entityConverter) nextActorKey() (key []byte, uniqueID int64) {
	n := ec.uid.Add(1)
	key = make([]byte, 8)
	binary.BigEndian.PutUint32(key, uint32(-actorSession))
	binary.BigEndian.PutUint32(key[4:], n)
	return key, int64(actorSessionBits<<32 | uint64(n))
}

func floatList(vals ...float64) *nbt.List {
	l := &nbt.List{Type: nbt.TagFloat}
	for _, v := range vals {
		l.Items = append(l.Items, float32(v))
	}
	return l
}

func doubleList(vals ...float64) *nbt.List {
	l := &nbt.List{Type: nbt.TagDouble}
	for _, v := range vals {
		l.Items = append(l.Items, v)
	}
	return l
}

func readVec(c nbt.Compound, key string, n int) ([]float64, bool) {
	l, ok := c.List(key)
	if !ok || len(l.Items) < n {
		return nil, false
	}
	out := make([]float64, n)
	for i := range n {
		f, ok := nbt.AsFloat(l.Items[i])
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		out[i] = f
	}
	return out, true
}

var emptyBedrockItem = func() nbt.Compound {
	return nbt.Compound{"Count": int8(0), "Damage": int16(0), "Name": "", "WasPickedUp": int8(0)}
}

func (ec *entityConverter) bedrockItemOrEmpty(v any) nbt.Compound {
	if c, ok := v.(nbt.Compound); ok {
		if out := ec.items.convert(c); out != nil {
			delete(out, "Slot")
			return out
		}
	}
	return emptyBedrockItem()
}

// javaToBedrock converts a chunk's Java entities. Item frames are placed into
// the (already translated) Bedrock chunk; other entities are added to rec.
func (ec *entityConverter) javaToBedrock(list *nbt.List, out *world.Chunk, dim world.Dimension, put func(key, value []byte)) {
	var digp []byte
	for _, it := range list.Items {
		e, ok := it.(nbt.Compound)
		if !ok {
			continue
		}
		for _, actor := range ec.javaEntity(e, out) {
			key, uid := ec.nextActorKey()
			actor["UniqueID"] = uid
			actor["internalComponents"] = nbt.Compound{"EntityStorageKeyComponent": nbt.Compound{"StorageKey": string(key)}}
			put(append([]byte("actorprefix"), key...), nbt.Encode("", actor, nbt.LittleEndian))
			digp = append(digp, key...)
			ec.count.Add(1)
		}
	}
	if len(digp) > 0 {
		put(append([]byte("digp"), bedrock.ChunkKey(out.X, out.Z, dim)...), digp)
	}
}

// javaEntity returns the Bedrock actors for one Java entity (passengers included).
func (ec *entityConverter) javaEntity(e nbt.Compound, out *world.Chunk) []nbt.Compound {
	id, _ := e.String("id")
	id = strip(id)
	if skipEntities[id] {
		return nil
	}
	pos, ok := readVec(e, "Pos", 3)
	if !ok {
		return nil
	}
	if id == "item_frame" || id == "glow_item_frame" {
		ec.placeFrame(e, id == "glow_item_frame", out)
		return nil
	}
	beID := id
	variant := int32(-1)
	chestItems := false
	switch {
	case strings.HasSuffix(id, "_chest_boat") || strings.HasSuffix(id, "_chest_raft"):
		variant = int32(indexOf(boatWoods, strings.TrimSuffix(strings.TrimSuffix(id, "_chest_boat"), "_chest_raft")))
		beID = "chest_boat"
		chestItems = true
	case strings.HasSuffix(id, "_boat") || strings.HasSuffix(id, "_raft"):
		variant = int32(indexOf(boatWoods, strings.TrimSuffix(strings.TrimSuffix(id, "_boat"), "_raft")))
		beID = "boat"
	case id == "chest_minecart" || id == "hopper_minecart":
		chestItems = true
	default:
		if b, ok := javaToBedrockEntityIDs[id]; ok {
			beID = b
		}
	}
	def := bedrockEntityDefs["minecraft:"+beID]
	if !ec.knownBedrock(beID) && !bedrockBuiltinEntities[beID] {
		return nil
	}
	rot, _ := readVec(e, "Rotation", 2)
	if rot == nil {
		rot = []float64{0, 0}
	}
	motion, _ := readVec(e, "Motion", 3)
	if motion == nil {
		motion = []float64{0, 0, 0}
	}
	a := nbt.Compound{
		"identifier": "minecraft:" + beID,
		"Pos":        floatList(pos...),
		"Rotation":   floatList(rot...),
		"Motion":     floatList(motion...),
	}
	baby := false
	if age, ok := e.Number("Age"); ok && age < 0 && id != "item" {
		baby = true
		a["IsBaby"] = int8(1)
	}
	if b, _ := e.Byte("IsBaby"); b != 0 {
		baby = true
		a["IsBaby"] = int8(1)
	}
	defs := &nbt.List{Type: nbt.TagString}
	defs.Items = append(defs.Items, "+minecraft:"+beID)
	groups := def.Spawn
	if baby && len(def.Born) > 0 {
		groups = def.Born
	}
	horseColour, horseMarks := "", ""
	if id == "horse" {
		v, _ := e.Number("Variant")
		colours := []string{"white", "creamy", "chestnut", "brown", "black", "gray", "darkbrown"}
		marks := []string{"none", "white_details", "white_fields", "white_dots", "black_dots"}
		if c := int(v & 0xFF); c < len(colours) {
			horseColour = "minecraft:base_" + colours[c]
		}
		if m := int(v>>8) & 0xFF; m < len(marks) {
			horseMarks = "minecraft:markings_" + marks[m]
		}
	}
	for _, g := range groups {
		if horseColour != "" && strings.HasPrefix(g, "minecraft:base_") {
			g = horseColour
		}
		if horseMarks != "" && strings.HasPrefix(g, "minecraft:markings_") {
			g = horseMarks
		}
		defs.Items = append(defs.Items, "+"+g)
	}
	a["definitions"] = defs
	if variant >= 0 {
		a["Variant"] = variant
	}
	for _, k := range []string{"OnGround", "Invulnerable", "CustomNameVisible", "Silent", "NoGravity"} {
		if v, ok := e.Byte(k); ok {
			a[k] = v
		}
	}
	if v, ok := e.Number("Fire"); ok {
		a["Fire"] = int16(v)
	}
	if v, ok := e.Number("Air"); ok {
		a["Air"] = int16(v)
	}
	if v, ok := nbt.AsFloat(e["fall_distance"]); ok {
		a["FallDistance"] = float32(v)
	} else if v, ok := nbt.AsFloat(e["FallDistance"]); ok {
		a["FallDistance"] = float32(v)
	}
	if cn, ok := e["CustomName"]; ok {
		if s := textToSection(cn); s != "" {
			a["CustomName"] = s
		}
	}
	if t, ok := e.List("Tags"); ok && len(t.Items) > 0 {
		a["Tags"] = nbt.Clone(t)
	}
	persistent := false
	if v, _ := e.Byte("PersistenceRequired"); v != 0 {
		persistent = true
	}
	if _, named := a["CustomName"]; named || bedrockEntityHasNoDespawn(beID) {
		persistent = true
	}
	a["Persistent"] = b8(persistent)
	if h, ok := nbt.AsFloat(e["Health"]); ok && def.Health > 0 {
		a["Attributes"] = nbt.NewList(nbt.Compound{
			"Name": "minecraft:health", "Base": float32(def.Health), "Max": float32(def.Health), "Current": float32(h),
		})
	}
	// equipment (1.21.5+ "equipment" compound or legacy lists)
	var head, chest, legs, feet, main, off any
	if eq, ok := e.Compound("equipment"); ok {
		head, chest, legs, feet, main, off = eq["head"], eq["chest"], eq["legs"], eq["feet"], eq["mainhand"], eq["offhand"]
	} else {
		if l, ok := e.List("ArmorItems"); ok && len(l.Items) == 4 {
			feet, legs, chest, head = l.Items[0], l.Items[1], l.Items[2], l.Items[3]
		}
		if l, ok := e.List("HandItems"); ok && len(l.Items) == 2 {
			main, off = l.Items[0], l.Items[1]
		}
	}
	if head != nil || chest != nil || legs != nil || feet != nil {
		a["Armor"] = nbt.NewList(ec.bedrockItemOrEmpty(head), ec.bedrockItemOrEmpty(chest), ec.bedrockItemOrEmpty(legs), ec.bedrockItemOrEmpty(feet))
	}
	if main != nil {
		a["Mainhand"] = nbt.NewList(ec.bedrockItemOrEmpty(main))
	}
	if off != nil {
		a["Offhand"] = nbt.NewList(ec.bedrockItemOrEmpty(off))
	}
	switch id {
	case "item":
		item, ok := e.Compound("Item")
		if !ok {
			return nil
		}
		conv := ec.items.convert(item)
		if conv == nil {
			return nil
		}
		delete(conv, "Slot")
		a["Item"] = conv
		if v, ok := e.Number("Age"); ok {
			a["Age"] = int16(v)
		}
		delete(a, "IsBaby")
	case "sheep":
		if c, ok := e.Number("Color"); ok {
			a["Color"] = int8(c)
		}
		if s, ok := e.Byte("Sheared"); ok {
			a["Sheared"] = s
		}
	case "horse":
		if v, ok := e.Number("Variant"); ok {
			a["Variant"] = int32(v & 0xFF)
			a["MarkVariant"] = int32((v >> 8) & 0xFF)
		}
	case "painting":
		v, _ := e.String("variant")
		a["Motif"] = pascalCase(strip(v))
		if f, ok := e.Number("facing"); ok {
			a["Direction"] = int8(f)
		}
	case "falling_block":
		if bs, ok := e.Compound("BlockState"); ok {
			name, _ := bs.String("Name")
			props := map[string]any{}
			if pp, ok := bs.Compound("Properties"); ok {
				for k, v := range pp {
					props[k] = v
				}
			}
			if b := ec.bc.lookup(translate.NewBlock(name, props)); b != nil && b.out != nil {
				a["FallingBlock"] = bedrockBlockNBT(b.out)
			}
		}
	}
	if chestItems {
		if l, ok := e.List("Items"); ok {
			a["ChestItems"] = ec.items.convertList(l)
		}
		if lt, ok := e.String("LootTable"); ok && lt != "" {
			a["LootTable"] = lootJavaToBedrock(lt)
			if seed, ok := e.Number("LootTableSeed"); ok {
				a["LootTableSeed"] = int32(seed)
			}
		}
	}
	out1 := []nbt.Compound{a}
	if pl, ok := e.List("Passengers"); ok {
		for _, p := range pl.Items {
			if pc, ok := p.(nbt.Compound); ok {
				out1 = append(out1, ec.javaEntity(pc, out)...)
			}
		}
	}
	return out1
}

func bedrockEntityHasNoDespawn(id string) bool {
	switch id {
	case "villager_v2", "iron_golem", "snow_golem", "armor_stand", "boat", "chest_boat", "minecart",
		"chest_minecart", "hopper_minecart", "tnt_minecart", "command_block_minecart", "painting", "allay":
		return true
	}
	return false
}

func b8(v bool) int8 {
	if v {
		return 1
	}
	return 0
}

func pascalCase(s string) string {
	var sb strings.Builder
	up := true
	for _, r := range s {
		if r == '_' {
			up = true
			continue
		}
		if up {
			sb.WriteRune(unicode.ToUpper(r))
			up = false
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func snakeCase(s string) string {
	var sb strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				sb.WriteByte('_')
			}
			sb.WriteRune(unicode.ToLower(r))
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// placeFrame turns a Java item frame entity into a Bedrock frame block.
func (ec *entityConverter) placeFrame(e nbt.Compound, glow bool, out *world.Chunk) {
	var bx, by, bz int32
	if bp, ok := e["block_pos"].([]int32); ok && len(bp) == 3 {
		bx, by, bz = bp[0], bp[1], bp[2]
	} else {
		x, ok1 := e.Number("TileX")
		y, ok2 := e.Number("TileY")
		z, ok3 := e.Number("TileZ")
		if !ok1 || !ok2 || !ok3 {
			return
		}
		bx, by, bz = int32(x), int32(y), int32(z)
	}
	if bx>>4 != out.X || bz>>4 != out.Z {
		return
	}
	pi := out.PaletteIndex(int(bx&15), int(by), int(bz&15))
	if pi < 0 {
		return
	}
	cur := out.Palette[pi]
	if n := cur.Name(); n != "minecraft:air" && n != "minecraft:water" && n != "minecraft:cave_air" {
		return
	}
	facing, _ := e.Number("Facing")
	item, _ := e.Compound("Item")
	conv := ec.items.convert(item)
	mapBit := int8(0)
	if conv != nil {
		delete(conv, "Slot")
		if n, _ := conv.String("Name"); n == "minecraft:filled_map" {
			mapBit = 1
		}
	}
	name, beName := "minecraft:frame", "ItemFrame"
	if glow {
		name, beName = "minecraft:glow_frame", "GlowItemFrame"
	}
	b := translate.NewBlock(name, map[string]any{
		"facing_direction":     int32(facing),
		"item_frame_map_bit":   mapBit,
		"item_frame_photo_bit": int8(0),
	})
	if cur.Name() == "minecraft:water" {
		b.Extra = []*translate.Block{cur}
	}
	setBlock(out, int(bx&15), int(by), int(bz&15), b)
	nb := nbt.Compound{"isMovable": int8(1)}
	if conv != nil {
		nb["Item"] = conv
		rot, _ := e.Number("ItemRotation")
		nb["ItemRotation"] = float32(rot * 45)
		drop := float32(1)
		if d, ok := nbt.AsFloat(e["ItemDropChance"]); ok {
			drop = float32(d)
		}
		nb["ItemDropChance"] = drop
	}
	out.BlockEntities[world.Pos{X: bx, Y: by, Z: bz}] = &translate.BlockEntity{BaseName: beName, NBT: nb}
	ec.count.Add(1)
}

// setBlock sets a block in a chunk, extending the palette.
func setBlock(ch *world.Chunk, lx, y, lz int, b *translate.Block) {
	si := (y >> 4) - ch.Bounds.MinSection
	if si < 0 || si >= len(ch.Sections) {
		return
	}
	key := b.Key()
	idx := -1
	for i, p := range ch.Palette {
		if p.Key() == key {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = len(ch.Palette)
		ch.Palette = append(ch.Palette, b)
	}
	if ch.Sections[si] == nil {
		ch.Sections[si] = &world.Section{}
	}
	ch.Sections[si].Blocks[(y&15)<<8|lz<<4|lx] = uint32(idx)
}

// bedrockToJava returns the Java entities of a Bedrock chunk. in is the decoded
// Bedrock chunk and out the translated Java chunk; frames are moved from
// blocks to entities.
func (ec *entityConverter) bedrockToJava(src *bedrock.World, rc *bedrock.RawChunk, in, out *world.Chunk) *nbt.List {
	list := &nbt.List{Type: nbt.TagCompound}
	var actors []nbt.Compound
	if raw, ok := rc.Data[bedrock.TagEntity]; ok {
		actors, _ = nbt.DecodeAll(raw, nbt.LittleEndian)
	}
	var digp []byte
	var ok bool
	if ec.digests != nil {
		digp, ok = ec.digests[string(bedrock.ChunkKey(rc.X, rc.Z, rc.Dim))]
	} else {
		digp, ok, _ = src.DB.Get([]byte("digp" + string(bedrock.ChunkKey(rc.X, rc.Z, rc.Dim))))
	}
	if ok {
		for i := 0; i+8 <= len(digp); i += 8 {
			v, ok, _ := src.DB.Get(append([]byte("actorprefix"), digp[i:i+8]...))
			if !ok {
				continue
			}
			if c, _, err := nbt.DecodeCompound(v, nbt.LittleEndian); err == nil {
				actors = append(actors, c)
			}
		}
	}
	for _, a := range actors {
		if e := ec.bedrockEntity(a); e != nil {
			list.Items = append(list.Items, e)
			ec.count.Add(1)
		}
	}
	// frames: blocks in Bedrock, entities in Java
	for pos, be := range in.BlockEntities {
		if be.BaseName != "ItemFrame" && be.BaseName != "GlowItemFrame" {
			continue
		}
		pi := in.PaletteIndex(int(pos.X&15), int(pos.Y), int(pos.Z&15))
		if pi < 0 {
			continue
		}
		blk := in.Palette[pi]
		if blk.Name() != "minecraft:frame" && blk.Name() != "minecraft:glow_frame" {
			continue
		}
		facing, _ := nbt.AsInt(blk.Props["facing_direction"])
		id := "minecraft:item_frame"
		if blk.Name() == "minecraft:glow_frame" {
			id = "minecraft:glow_item_frame"
		}
		dx, dy, dz := facingVec(int(facing))
		e := nbt.Compound{
			"id":        id,
			"Pos":       doubleList(float64(pos.X)+0.5-float64(dx)*0.46875, float64(pos.Y)+0.5-float64(dy)*0.46875, float64(pos.Z)+0.5-float64(dz)*0.46875),
			"Motion":    doubleList(0, 0, 0),
			"Rotation":  floatList(0, 0),
			"block_pos": []int32{pos.X, pos.Y, pos.Z},
			"Facing":    int8(facing),
			"UUID":      uuidFromSeed(int64(pos.X)<<40 ^ int64(pos.Y)<<20 ^ int64(pos.Z)),
		}
		if item, ok := be.NBT.Compound("Item"); ok {
			if conv := ec.items.convert(item); conv != nil {
				delete(conv, "Slot")
				e["Item"] = conv
				rot, _ := nbt.AsFloat(be.NBT["ItemRotation"])
				e["ItemRotation"] = int8(int(math.Round(rot/45)) & 7)
				drop := float32(1)
				if d, ok := nbt.AsFloat(be.NBT["ItemDropChance"]); ok {
					drop = float32(d)
				}
				e["ItemDropChance"] = drop
			}
		}
		list.Items = append(list.Items, e)
		ec.count.Add(1)
	}
	return list
}

func facingVec(f int) (int, int, int) {
	switch f {
	case 0:
		return 0, -1, 0
	case 1:
		return 0, 1, 0
	case 2:
		return 0, 0, -1
	case 3:
		return 0, 0, 1
	case 4:
		return -1, 0, 0
	case 5:
		return 1, 0, 0
	}
	return 0, 0, 1
}

func uuidFromSeed(seed int64) []int32 {
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(seed))
	h.Write(b[:])
	lo := h.Sum64()
	h.Write([]byte("mcconv"))
	hi := h.Sum64()
	hi = hi&^0xF000 | 0x4000 // version 4
	lo = lo&^(3<<62) | 2<<62 // variant
	return []int32{int32(hi >> 32), int32(hi), int32(lo >> 32), int32(lo)}
}

func (ec *entityConverter) javaItemOrEmpty(v any) nbt.Compound {
	if c, ok := v.(nbt.Compound); ok {
		if out := ec.items.convert(c); out != nil {
			delete(out, "Slot")
			return out
		}
	}
	return nil
}

func (ec *entityConverter) bedrockEntity(a nbt.Compound) nbt.Compound {
	ident, _ := a.String("identifier")
	ident = strip(ident)
	if i := strings.IndexByte(ident, '<'); i >= 0 {
		ident = ident[:i]
	}
	if skipEntities[ident] || ident == "" {
		return nil
	}
	pos, ok := readVec(a, "Pos", 3)
	if !ok {
		return nil
	}
	id := ident
	if j, ok := bedrockToJavaEntityIDs[ident]; ok {
		id = j
	}
	variant, _ := a.Number("Variant")
	switch ident {
	case "boat", "chest_boat":
		wood := "oak"
		if variant >= 0 && int(variant) < len(boatWoods) {
			wood = boatWoods[variant]
		}
		kind := "_boat"
		if wood == "bamboo" {
			kind = "_raft"
		}
		if ident == "chest_boat" {
			kind = "_chest" + kind
		}
		id = wood + kind
	}
	if ident != "item" && ident != "painting" && ident != "falling_block" && ident != "xp_orb" &&
		ident != "boat" && ident != "chest_boat" && ident != "leash_knot" {
		if !ec.knownBedrock(ident) {
			return nil
		}
	}
	rot, _ := readVec(a, "Rotation", 2)
	if rot == nil {
		rot = []float64{0, 0}
	}
	motion, _ := readVec(a, "Motion", 3)
	if motion == nil {
		motion = []float64{0, 0, 0}
	}
	uid, _ := a.Number("UniqueID")
	e := nbt.Compound{
		"id":       "minecraft:" + id,
		"Pos":      doubleList(pos...),
		"Rotation": floatList(rot...),
		"Motion":   doubleList(motion...),
		"UUID":     uuidFromSeed(uid),
	}
	for _, k := range []string{"OnGround", "Invulnerable", "CustomNameVisible", "Silent", "NoGravity"} {
		if v, ok := a.Byte(k); ok {
			e[k] = v
		}
	}
	if v, ok := a.Number("Fire"); ok {
		e["Fire"] = int16(v)
	}
	if v, ok := a.Number("Air"); ok {
		e["Air"] = int16(v)
	}
	if v, ok := nbt.AsFloat(a["FallDistance"]); ok {
		e["fall_distance"] = v
	}
	if s, ok := a.String("CustomName"); ok && s != "" {
		e["CustomName"] = translate.SectionToJavaText(s)
	}
	if t, ok := a.List("Tags"); ok && len(t.Items) > 0 {
		e["Tags"] = nbt.Clone(t)
	}
	if p, _ := a.Byte("Persistent"); p != 0 {
		e["PersistenceRequired"] = int8(1)
	}
	if b, _ := a.Byte("IsBaby"); b != 0 {
		e["Age"] = int32(-24000)
	}
	if attrs, ok := a.List("Attributes"); ok {
		for _, it := range attrs.Items {
			at, _ := it.(nbt.Compound)
			if n, _ := at.String("Name"); n == "minecraft:health" {
				if cur, ok := nbt.AsFloat(at["Current"]); ok {
					e["Health"] = float32(cur)
				}
			}
		}
	}
	eq := nbt.Compound{}
	if l, ok := a.List("Armor"); ok && len(l.Items) == 4 {
		for i, slot := range []string{"head", "chest", "legs", "feet"} {
			if it := ec.javaItemOrEmpty(l.Items[i]); it != nil {
				eq[slot] = it
			}
		}
	}
	if l, ok := a.List("Mainhand"); ok && len(l.Items) > 0 {
		if it := ec.javaItemOrEmpty(l.Items[0]); it != nil {
			eq["mainhand"] = it
		}
	}
	if l, ok := a.List("Offhand"); ok && len(l.Items) > 0 {
		if it := ec.javaItemOrEmpty(l.Items[0]); it != nil {
			eq["offhand"] = it
		}
	}
	if len(eq) > 0 {
		e["equipment"] = eq
	}
	switch ident {
	case "item":
		item, ok := a.Compound("Item")
		if !ok {
			return nil
		}
		conv := ec.items.convert(item)
		if conv == nil {
			return nil
		}
		delete(conv, "Slot")
		e["Item"] = conv
		if v, ok := a.Number("Age"); ok {
			e["Age"] = int16(v)
		}
		e["PickupDelay"] = int16(0)
	case "sheep":
		if c, ok := a.Number("Color"); ok {
			e["Color"] = int8(c)
		}
		if s, ok := a.Byte("Sheared"); ok {
			e["Sheared"] = s
		}
	case "horse":
		mark, _ := a.Number("MarkVariant")
		e["Variant"] = int32(variant&0xFF | mark<<8)
	case "painting":
		motif, _ := a.String("Motif")
		dir, _ := a.Number("Direction")
		e["variant"] = "minecraft:" + snakeCase(motif)
		e["facing"] = int8(dir)
		w, h := paintingSize(snakeCase(motif))
		e["block_pos"] = paintingBlockPos(pos, int(dir), w, h)
	case "falling_block":
		if fb, ok := a.Compound("FallingBlock"); ok {
			name, _ := fb.String("name")
			props := map[string]any{}
			if st, ok := fb.Compound("states"); ok {
				for k, v := range st {
					props[k] = v
				}
			}
			fbb := translate.NewBlock(name, props)
			if v, ok := fb.Number("version"); ok {
				fbb.Version = int32(v)
			}
			if b := ec.bc.lookup(fbb); b != nil && b.out != nil {
				pp := nbt.Compound{}
				for k, v := range b.out.Props {
					if s, ok := v.(string); ok {
						pp[k] = s
					}
				}
				bs := nbt.Compound{"Name": b.out.Name()}
				if len(pp) > 0 {
					bs["Properties"] = pp
				}
				e["BlockState"] = bs
			}
		}
	}
	if l, ok := a.List("ChestItems"); ok && (ident == "chest_minecart" || ident == "hopper_minecart" || ident == "chest_boat") {
		items := ec.items.convertList(l)
		e["Items"] = items
		if lt, ok := a.String("LootTable"); ok && lt != "" {
			e["LootTable"] = lootBedrockToJava(lt)
			if seed, ok := a.Number("LootTableSeed"); ok {
				e["LootTableSeed"] = seed
			}
		}
	}
	return e
}

// painting sizes (width, height) in blocks
var paintingSizes = map[string][2]int{
	"kebab": {1, 1}, "aztec": {1, 1}, "alban": {1, 1}, "aztec2": {1, 1}, "bomb": {1, 1}, "plant": {1, 1},
	"wasteland": {1, 1}, "meditative": {1, 1}, "pool": {2, 1}, "courbet": {2, 1}, "sea": {2, 1},
	"sunset": {2, 1}, "creebet": {2, 1}, "wanderer": {1, 2}, "graham": {1, 2}, "prairie_ride": {1, 2},
	"match": {2, 2}, "bust": {2, 2}, "stage": {2, 2}, "void": {2, 2}, "skull_and_roses": {2, 2},
	"wither": {2, 2}, "baroque": {2, 2}, "humble": {2, 2}, "earth": {2, 2}, "fire": {2, 2},
	"water": {2, 2}, "wind": {2, 2}, "fighters": {4, 2}, "changing": {4, 2}, "finding": {4, 2},
	"lowmist": {4, 2}, "passage": {4, 2}, "skeleton": {4, 3}, "donkey_kong": {4, 3}, "pointer": {4, 4},
	"pigscene": {4, 4}, "burning_skull": {4, 4}, "orb": {4, 4}, "unpacked": {4, 4}, "backyard": {3, 4},
	"pond": {3, 4}, "bouquet": {3, 3}, "cavebird": {3, 3}, "cotan": {3, 3}, "endboss": {3, 3},
	"fern": {3, 3}, "owlemons": {3, 3}, "sunflowers": {3, 3}, "tides": {3, 3}, "dennis": {3, 3},
}

func paintingSize(name string) (int, int) {
	if s, ok := paintingSizes[name]; ok {
		return s[0], s[1]
	}
	return 1, 1
}

// paintingBlockPos inverts Java's painting placement: given the entity centre,
// facing (2D data value) and size, return the anchor block position.
func paintingBlockPos(center []float64, facing, w, h int) []int32 {
	// facing 2D: 0 south(+z), 1 west(-x), 2 north(-z), 3 east(+x)
	fx, fz := [4]float64{0, -1, 0, 1}[facing&3], [4]float64{1, 0, -1, 0}[facing&3]
	// counter-clockwise direction of the facing
	cx, cz := fz, -fx
	x := center[0] + fx*0.46875
	y := center[1]
	z := center[2] + fz*0.46875
	if w%2 == 0 {
		x -= cx * 0.5
		z -= cz * 0.5
	}
	if h%2 == 0 {
		y -= 0.5
	}
	return []int32{int32(math.Floor(x)), int32(math.Floor(y)), int32(math.Floor(z))}
}
