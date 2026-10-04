// Package java reads and writes Java Edition (Anvil, 1.18+) worlds.
package java

import (
	"fmt"
	"math/bits"
	"sort"
	"strings"
	"sync"

	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// Codec converts between Java chunk NBT and world.Chunk.
type Codec struct {
	// DataVersion written to chunks.
	DataVersion int32

	intern sync.Map // raw palette key -> *translate.Block (shared between chunks)
}

var air = translate.NewBlock("minecraft:air", nil)

// DecodeChunk parses chunk NBT (1.18+ format). It returns the chunk status.
func (c *Codec) DecodeChunk(root nbt.Compound, bounds world.Bounds) (*world.Chunk, string, error) {
	x, _ := root.Number("xPos")
	z, _ := root.Number("zPos")
	status, _ := root.String("Status")
	ch := world.NewChunk(int32(x), int32(z), bounds)
	ch.Palette = []*translate.Block{air}
	keyIndex := map[*translate.Block]uint32{air: 0}
	ch.BiomePalette = nil
	biomeIndex := map[string]uint16{}

	sections, _ := root.List("sections")
	if sections == nil {
		sections, _ = root.List("Sections")
	}
	if sections == nil {
		return ch, status, nil
	}
	for _, it := range sections.Items {
		sec, ok := it.(nbt.Compound)
		if !ok {
			continue
		}
		sy, _ := sec.Number("Y")
		si := int(sy) - bounds.MinSection
		if si < 0 || si >= bounds.Height() {
			continue
		}
		if bs, ok := sec.Compound("block_states"); ok {
			pal, _ := bs.List("palette")
			if pal.Len() == 0 {
				continue
			}
			local := make([]uint32, len(pal.Items))
			for i, pe := range pal.Items {
				b := c.paletteBlock(pe)
				idx, ok := keyIndex[b]
				if !ok {
					idx = uint32(len(ch.Palette))
					keyIndex[b] = idx
					ch.Palette = append(ch.Palette, b)
				}
				local[i] = idx
			}
			data, _ := bs["data"].([]int64)
			if len(local) == 1 || len(data) == 0 {
				if local[0] != 0 {
					s := world.NewSection()
					for i := range s.Blocks {
						s.Blocks[i] = local[0]
					}
					ch.Sections[si] = s
				}
			} else {
				s := world.NewSection()
				bitsPer := max(4, bits.Len(uint(len(local)-1)))
				unpackIndices(data, bitsPer, s.Blocks[:], local)
				ch.Sections[si] = s
			}
		}
		if bm, ok := sec.Compound("biomes"); ok {
			pal, _ := bm.List("palette")
			if pal.Len() == 0 {
				continue
			}
			local := make([]uint16, len(pal.Items))
			for i, pe := range pal.Items {
				name, _ := pe.(string)
				idx, ok := biomeIndex[name]
				if !ok {
					idx = uint16(len(ch.BiomePalette))
					biomeIndex[name] = idx
					ch.BiomePalette = append(ch.BiomePalette, name)
				}
				local[i] = idx
			}
			bsec := &world.BiomeSection{}
			data, _ := bm["data"].([]int64)
			if len(local) == 1 || len(data) == 0 {
				for i := range bsec {
					bsec[i] = local[0]
				}
			} else {
				bitsPer := bits.Len(uint(len(local) - 1))
				var tmp [64]uint32
				l32 := make([]uint32, len(local))
				for i, v := range local {
					l32[i] = uint32(v)
				}
				unpackIndices(data, bitsPer, tmp[:], l32)
				for i, v := range tmp {
					bsec[i] = uint16(v)
				}
			}
			ch.Biomes[si] = bsec
		}
	}
	if bes, ok := root.List("block_entities"); ok {
		c.addBlockEntities(ch, bes)
	}
	return ch, status, nil
}

// addBlockEntities stores block entities with id/x/y/z removed from their NBT.
func (c *Codec) addBlockEntities(ch *world.Chunk, bes *nbt.List) {
	for _, it := range bes.Items {
		be, ok := it.(nbt.Compound)
		if !ok {
			continue
		}
		id, _ := be.String("id")
		bx, ok1 := be.Number("x")
		by, ok2 := be.Number("y")
		bz, ok3 := be.Number("z")
		if id == "" || !ok1 || !ok2 || !ok3 {
			continue
		}
		be = be.Clone()
		delete(be, "id")
		delete(be, "x")
		delete(be, "y")
		delete(be, "z")
		ns, name, ok := strings.Cut(id, ":")
		if !ok {
			ns, name = "minecraft", id
		}
		ch.BlockEntities[world.Pos{X: int32(bx), Y: int32(by), Z: int32(bz)}] = &translate.BlockEntity{Namespace: ns, BaseName: name, NBT: be}
	}
}

var errNotCompound = fmt.Errorf("java: chunk root is not a compound")

// paletteBlock converts a palette compound into a block. Equal states return
// the same (shared, read-only) block.
func (c *Codec) paletteBlock(pe any) *translate.Block {
	pc, _ := pe.(nbt.Compound)
	name, _ := pc.String("Name")
	pp, _ := pc.Compound("Properties")
	var kb strings.Builder
	kb.WriteString(name)
	if len(pp) > 0 {
		keys := make([]string, 0, len(pp))
		for k := range pp {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			kb.WriteByte('|')
			kb.WriteString(k)
			kb.WriteByte('=')
			if s, ok := pp[k].(string); ok {
				kb.WriteString(s)
			} else {
				kb.WriteString(nbt.ToSNBT(pp[k]))
			}
		}
	}
	rawKey := kb.String()
	if v, ok := c.intern.Load(rawKey); ok {
		return v.(*translate.Block)
	}
	b := c.newPaletteBlock(name, pp)
	if name == "minecraft:air" && len(pp) == 0 {
		b = air
	}
	v, _ := c.intern.LoadOrStore(rawKey, b)
	return v.(*translate.Block)
}

func (c *Codec) newPaletteBlock(name string, pp nbt.Compound) *translate.Block {
	if name == "" {
		name = "minecraft:air"
	}
	props := map[string]any{}
	if pp != nil {
		for k, v := range pp {
			if s, ok := v.(string); ok {
				props[k] = s
			} else {
				props[k] = nbt.ToSNBT(v)
			}
		}
	}
	return translate.NewBlock(name, props)
}

// unpackIndices decodes the 1.16+ packed long array (entries do not span longs).
func unpackIndices(data []int64, bitsPer int, out []uint32, palette []uint32) {
	perLong := 64 / bitsPer
	mask := uint64(1)<<bitsPer - 1
	n := len(out)
	maxIdx := uint64(len(palette) - 1)
	i := 0
	for _, l := range data {
		v := uint64(l)
		for j := 0; j < perLong && i < n; j++ {
			idx := v & mask
			if idx > maxIdx {
				idx = 0
			}
			out[i] = palette[idx]
			v >>= bitsPer
			i++
		}
		if i >= n {
			break
		}
	}
	for ; i < n; i++ { // truncated data
		out[i] = palette[0]
	}
}

func packIndices(idx []uint32, bitsPer int) []int64 {
	perLong := 64 / bitsPer
	out := make([]int64, (len(idx)+perLong-1)/perLong)
	for i, v := range idx {
		out[i/perLong] |= int64(uint64(v) << (uint(i%perLong) * uint(bitsPer)))
	}
	return out
}

// paletteNBT converts a block into a Java palette compound.
func (c *Codec) paletteNBT(b *translate.Block) nbt.Compound {
	props := nbt.Compound{}
	for k, v := range b.Props {
		switch t := v.(type) {
		case string:
			props[k] = t
		case int8:
			if t != 0 {
				props[k] = "true"
			} else {
				props[k] = "false"
			}
		default:
			props[k] = strings.Trim(nbt.ToSNBT(v), "\"")
		}
	}
	out := nbt.Compound{"Name": b.Name()}
	if len(props) > 0 {
		out["Properties"] = props
	}
	return out
}

// EncodeChunk builds chunk NBT from a chunk whose palette holds Java blocks.
func (c *Codec) EncodeChunk(ch *world.Chunk) nbt.Compound {
	palNBT := make([]nbt.Compound, len(ch.Palette))
	// the palette may contain the same block more than once; canon maps each
	// entry to its first occurrence so section palettes have no duplicates
	canon := make([]uint32, len(ch.Palette))
	first := map[*translate.Block]uint32{}
	for i, b := range ch.Palette {
		if j, ok := first[b]; ok {
			canon[i] = j
			continue
		}
		first[b] = uint32(i)
		canon[i] = uint32(i)
		palNBT[i] = c.paletteNBT(b)
	}
	biomePal := ch.BiomePalette
	if len(biomePal) == 0 {
		biomePal = []string{"minecraft:plains"}
	}
	sections := &nbt.List{Type: nbt.TagCompound}
	var localMap = make([]int32, len(ch.Palette))
	for i := range localMap {
		localMap[i] = -1
	}
	idx := make([]uint32, 4096)
	for si := 0; si < ch.Bounds.Height(); si++ {
		sy := ch.Bounds.MinSection + si
		sec := nbt.Compound{"Y": int8(sy)}
		s := ch.Sections[si]
		// block states
		var pal []int
		if s == nil {
			pal = []int{0}
			sec["block_states"] = nbt.Compound{"palette": nbt.NewList(palNBT[0].Clone())}
		} else {
			for i := range localMap {
				localMap[i] = -1
			}
			for i, v := range s.Blocks {
				v = canon[v]
				l := localMap[v]
				if l < 0 {
					l = int32(len(pal))
					localMap[v] = l
					pal = append(pal, int(v))
				}
				idx[i] = uint32(l)
			}
			pl := &nbt.List{Type: nbt.TagCompound, Items: make([]any, len(pal))}
			for i, g := range pal {
				pl.Items[i] = palNBT[g].Clone()
			}
			bsTag := nbt.Compound{"palette": pl}
			if len(pal) > 1 {
				bitsPer := max(4, bits.Len(uint(len(pal)-1)))
				bsTag["data"] = packIndices(idx, bitsPer)
			}
			sec["block_states"] = bsTag
		}
		// biomes
		var bsec *world.BiomeSection
		if si < len(ch.Biomes) {
			bsec = ch.Biomes[si]
		}
		if bsec == nil {
			// inherit the nearest defined section below/above for continuity
			for d := 1; d < len(ch.Biomes) && bsec == nil; d++ {
				if si-d >= 0 && ch.Biomes[si-d] != nil {
					bsec = ch.Biomes[si-d]
				} else if si+d < len(ch.Biomes) && ch.Biomes[si+d] != nil {
					bsec = ch.Biomes[si+d]
				}
			}
		}
		if bsec == nil {
			sec["biomes"] = nbt.Compound{"palette": nbt.NewList(biomePal[0])}
		} else {
			var bl []uint16
			lm := map[uint16]uint32{}
			var bidx [64]uint32
			for i, v := range bsec {
				l, ok := lm[v]
				if !ok {
					l = uint32(len(bl))
					lm[v] = l
					bl = append(bl, v)
				}
				bidx[i] = l
			}
			pl := &nbt.List{Type: nbt.TagString}
			for _, v := range bl {
				name := "minecraft:plains"
				if int(v) < len(biomePal) {
					name = biomePal[v]
				}
				pl.Items = append(pl.Items, name)
			}
			bt := nbt.Compound{"palette": pl}
			if len(bl) > 1 {
				bt["data"] = packIndices(bidx[:], bits.Len(uint(len(bl)-1)))
			}
			sec["biomes"] = bt
		}
		sections.Items = append(sections.Items, sec)
	}
	postProcessing := &nbt.List{Type: nbt.TagList}
	if pp, _ := ch.Misc["postProcess"].(bool); pp {
		postProcessing = c.postProcessing(ch)
	}
	bes := &nbt.List{Type: nbt.TagCompound}
	for pos, be := range ch.BlockEntities {
		t := be.NBT.Clone()
		if t == nil {
			t = nbt.Compound{}
		}
		t["id"] = be.Namespace + ":" + be.BaseName
		t["x"], t["y"], t["z"] = pos.X, pos.Y, pos.Z
		if _, ok := t["keepPacked"]; !ok {
			t["keepPacked"] = int8(0)
		}
		bes.Items = append(bes.Items, t)
	}
	return nbt.Compound{
		"DataVersion":    c.DataVersion,
		"xPos":           ch.X,
		"zPos":           ch.Z,
		"yPos":           int32(ch.Bounds.MinSection),
		"Status":         "minecraft:full",
		"LastUpdate":     int64(0),
		"InhabitedTime":  int64(0),
		"isLightOn":      int8(0),
		"sections":       sections,
		"block_entities": bes,
		"block_ticks":    &nbt.List{},
		"fluid_ticks":    &nbt.List{},
		"PostProcessing": postProcessing,
		"structures":     nbt.Compound{"References": nbt.Compound{}, "starts": nbt.Compound{}},
		"Heightmaps":     nbt.Compound{},
	}
}

// ErrSkip marks chunks that are intentionally not converted.
var ErrSkip = fmt.Errorf("skip")

// NeedsShapeUpdate reports whether a Java block's state depends on its
// neighbours in a way Bedrock does not store (fence/wall/pane connections,
// stair shapes, leaf distance, redstone wire, ...).
func NeedsShapeUpdate(name string) bool {
	n := strings.TrimPrefix(name, "minecraft:")
	switch {
	case strings.HasSuffix(n, "_fence"), strings.HasSuffix(n, "_fence_gate"),
		strings.HasSuffix(n, "_pane"), strings.HasSuffix(n, "_bars"), n == "iron_bars",
		strings.HasSuffix(n, "_stairs"),
		n == "redstone_wire", n == "tripwire", n == "chorus_plant":
		return true
	case strings.HasSuffix(n, "_wall"):
		return !strings.Contains(n, "_wall_") // excludes *_wall_sign etc. (they end differently anyway)
	}
	return false
}

// postProcessing lists positions the game re-evaluates against their
// neighbours when the chunk is loaded (ProtoChunk.packOffsetCoordinates).
func (c *Codec) postProcessing(ch *world.Chunk) *nbt.List {
	need := make([]bool, len(ch.Palette))
	for i, b := range ch.Palette {
		need[i] = NeedsShapeUpdate(b.Name())
	}
	out := &nbt.List{Type: nbt.TagList}
	for _, sec := range ch.Sections {
		l := &nbt.List{Type: nbt.TagShort}
		if sec != nil {
			for p, v := range sec.Blocks {
				if need[v] {
					x, y, z := p&15, p>>8, (p>>4)&15
					l.Items = append(l.Items, int16(x|y<<4|z<<8))
				}
			}
		}
		out.Items = append(out.Items, l)
	}
	return out
}
