package java

import (
	"hash/maphash"
	"math/bits"
	"mcconv/internal/bitpack"
	"sync"

	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// FastDecoder parses chunk NBT directly from bytes into a world.Chunk without
// building generic NBT maps for sections and palettes. One per goroutine.
type FastDecoder struct {
	c     *Codec
	sc    nbt.Scanner
	longs []uint64
	local []uint32
	pairs [][2][]byte
	key   []byte
	bkey  map[*translate.Block]uint32
	// Sections, if set, supplies section arrays (per-goroutine free list).
	Sections *world.SectionCache
}

// FastResult holds the parts of a chunk outside the block data.
type FastResult struct {
	Status        string
	DataVersion   int
	BlockEntities *nbt.List
	Entities      *nbt.List // legacy in-chunk entities
}

// NewFastDecoder returns a decoder bound to the codec.
func (c *Codec) NewFastDecoder() *FastDecoder {
	return &FastDecoder{c: c, bkey: map[*translate.Block]uint32{}}
}

// palette interning keyed by the raw "name|k=v|..." bytes; sharded so
// concurrent decoders rarely contend, and map lookups with string(bytes)
// do not allocate.
type internShard struct {
	mu sync.RWMutex
	m  map[string]*translate.Block
}

type internTable struct {
	seed   maphash.Seed
	shards [64]internShard
}

var fastIntern = func() *internTable {
	t := &internTable{seed: maphash.MakeSeed()}
	for i := range t.shards {
		t.shards[i].m = map[string]*translate.Block{}
	}
	return t
}()

// Decode parses a chunk. Sections come from the world section pool.
func (fd *FastDecoder) Decode(data []byte, bounds world.Bounds) (ch *world.Chunk, res FastResult, err error) {
	sc := &fd.sc
	sc.Reset(data)
	defer sc.Catch(&err)
	if sc.Root() != nbt.TagCompound {
		return nil, res, errNotCompound
	}
	ch = world.NewChunk(0, 0, bounds)
	ch.Palette = append(ch.Palette[:0], air)
	clear(fd.bkey)
	fd.bkey[air] = 0
	biomeIdx := map[string]uint16{}
	for {
		t, name := sc.Next()
		if t == nbt.TagEnd {
			break
		}
		switch string(name) {
		case "xPos":
			v, _ := sc.Int(t)
			ch.X = int32(v)
		case "zPos":
			v, _ := sc.Int(t)
			ch.Z = int32(v)
		case "DataVersion":
			v, _ := sc.Int(t)
			res.DataVersion = int(v)
		case "Status":
			if t == nbt.TagString {
				res.Status = string(sc.Bytes())
			} else {
				sc.Skip(t)
			}
		case "sections", "Sections":
			if t != nbt.TagList {
				sc.Skip(t)
				continue
			}
			et, n := sc.ListHeader()
			for i := 0; i < n; i++ {
				if et != nbt.TagCompound {
					sc.Skip(et)
					continue
				}
				fd.section(ch, bounds, biomeIdx)
			}
		case "block_entities", "TileEntities":
			if l, ok := sc.Value(t).(*nbt.List); ok {
				res.BlockEntities = l
			}
		case "Entities":
			if l, ok := sc.Value(t).(*nbt.List); ok {
				res.Entities = l
			}
		default:
			sc.Skip(t)
		}
	}
	if res.BlockEntities != nil {
		fd.c.addBlockEntities(ch, res.BlockEntities)
	}
	return ch, res, nil
}

func (fd *FastDecoder) section(ch *world.Chunk, bounds world.Bounds, biomeIdx map[string]uint16) {
	sc := &fd.sc
	sy := 0
	var blockLocal []uint32
	var blockData []uint64
	var biomeLocal []uint16
	var biomeData []uint64
	haveBlocks, haveBiomes := false, false
	for {
		t, name := sc.Next()
		if t == nbt.TagEnd {
			break
		}
		switch string(name) {
		case "Y":
			v, _ := sc.Int(t)
			sy = int(v)
		case "block_states":
			if t != nbt.TagCompound {
				sc.Skip(t)
				continue
			}
			blockLocal, blockData = fd.blockStates(ch)
			haveBlocks = true
		case "biomes":
			if t != nbt.TagCompound {
				sc.Skip(t)
				continue
			}
			biomeLocal, biomeData = fd.biomes(ch, biomeIdx)
			haveBiomes = true
		default:
			sc.Skip(t)
		}
	}
	si := sy - bounds.MinSection
	if si < 0 || si >= bounds.Height() {
		return
	}
	if haveBlocks && len(blockLocal) > 0 {
		if len(blockLocal) == 1 || len(blockData) == 0 {
			if blockLocal[0] != 0 {
				s := fd.Sections.Get()
				v := blockLocal[0]
				for i := range s.Blocks {
					s.Blocks[i] = v
				}
				ch.Sections[si] = s
			}
		} else {
			s := fd.Sections.Get()
			unpackLongs(blockData, max(4, bits.Len(uint(len(blockLocal)-1))), s.Blocks[:], blockLocal)
			ch.Sections[si] = s
		}
	}
	if haveBiomes && len(biomeLocal) > 0 {
		bsec := &world.BiomeSection{}
		if len(biomeLocal) == 1 || len(biomeData) == 0 {
			for i := range bsec {
				bsec[i] = biomeLocal[0]
			}
		} else {
			bitsPer := bits.Len(uint(len(biomeLocal) - 1))
			perLong := 64 / bitsPer
			mask := uint64(1)<<bitsPer - 1
			i := 0
			for _, l := range biomeData {
				for j := 0; j < perLong && i < 64; j++ {
					idx := int(l & mask)
					if idx >= len(biomeLocal) {
						idx = 0
					}
					bsec[i] = biomeLocal[idx]
					l >>= bitsPer
					i++
				}
			}
		}
		ch.Biomes[si] = bsec
	}
}

// blockStates reads a block_states compound; it returns the chunk palette
// indices of the section palette and the packed data.
func (fd *FastDecoder) blockStates(ch *world.Chunk) ([]uint32, []uint64) {
	sc := &fd.sc
	local := fd.local[:0]
	var data []uint64
	for {
		t, name := sc.Next()
		if t == nbt.TagEnd {
			break
		}
		switch string(name) {
		case "palette":
			if t != nbt.TagList {
				sc.Skip(t)
				continue
			}
			et, n := sc.ListHeader()
			for i := 0; i < n; i++ {
				if et != nbt.TagCompound {
					sc.Skip(et)
					continue
				}
				b := fd.paletteEntry()
				idx, ok := fd.bkey[b]
				if !ok {
					idx = uint32(len(ch.Palette))
					fd.bkey[b] = idx
					ch.Palette = append(ch.Palette, b)
				}
				local = append(local, idx)
			}
		case "data":
			if t == nbt.TagLongArray {
				fd.longs = sc.LongArray(fd.longs)
				data = fd.longs
			} else {
				sc.Skip(t)
			}
		default:
			sc.Skip(t)
		}
	}
	fd.local = local
	return local, data
}

func (fd *FastDecoder) biomes(ch *world.Chunk, biomeIdx map[string]uint16) ([]uint16, []uint64) {
	sc := &fd.sc
	var local []uint16
	var data []uint64
	for {
		t, name := sc.Next()
		if t == nbt.TagEnd {
			break
		}
		switch string(name) {
		case "palette":
			if t != nbt.TagList {
				sc.Skip(t)
				continue
			}
			et, n := sc.ListHeader()
			for i := 0; i < n; i++ {
				if et != nbt.TagString {
					sc.Skip(et)
					continue
				}
				b := sc.Bytes()
				idx, ok := biomeIdx[string(b)]
				if !ok {
					idx = uint16(len(ch.BiomePalette))
					name := string(b)
					biomeIdx[name] = idx
					ch.BiomePalette = append(ch.BiomePalette, name)
				}
				local = append(local, idx)
			}
		case "data":
			if t == nbt.TagLongArray {
				data = sc.LongArray(nil)
			} else {
				sc.Skip(t)
			}
		default:
			sc.Skip(t)
		}
	}
	return local, data
}

// paletteEntry reads {Name, Properties} and returns the shared block.
func (fd *FastDecoder) paletteEntry() *translate.Block {
	sc := &fd.sc
	var name []byte
	pairs := fd.pairs[:0]
	for {
		t, k := sc.Next()
		if t == nbt.TagEnd {
			break
		}
		switch {
		case t == nbt.TagString && string(k) == "Name":
			name = sc.Bytes()
		case t == nbt.TagCompound && string(k) == "Properties":
			for {
				pt, pk := sc.Next()
				if pt == nbt.TagEnd {
					break
				}
				if pt != nbt.TagString {
					sc.Skip(pt)
					continue
				}
				pairs = append(pairs, [2][]byte{pk, sc.Bytes()})
			}
		default:
			sc.Skip(t)
		}
	}
	fd.pairs = pairs
	// insertion sort: palettes have few properties
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0 && string(pairs[j][0]) < string(pairs[j-1][0]); j-- {
			pairs[j], pairs[j-1] = pairs[j-1], pairs[j]
		}
	}
	key := append(fd.key[:0], name...)
	for _, p := range pairs {
		key = append(key, '|')
		key = append(key, p[0]...)
		key = append(key, '=')
		key = append(key, p[1]...)
	}
	fd.key = key
	sh := &fastIntern.shards[maphash.Bytes(fastIntern.seed, key)&63]
	sh.mu.RLock()
	b, ok := sh.m[string(key)]
	sh.mu.RUnlock()
	if ok {
		return b
	}
	nm := string(name)
	if nm == "" {
		nm = "minecraft:air"
	}
	var pp nbt.Compound
	if len(pairs) > 0 {
		pp = nbt.Compound{}
		for _, p := range pairs {
			pp[string(p[0])] = string(p[1])
		}
	}
	b = fd.c.newPaletteBlock(nm, pp)
	if nm == "minecraft:air" && len(pairs) == 0 {
		b = air
	}
	sh.mu.Lock()
	if old, ok := sh.m[string(key)]; ok {
		b = old
	} else {
		sh.m[string(key)] = b
	}
	sh.mu.Unlock()
	return b
}

// unpackLongs decodes the 1.16+ packed long array (entries do not span longs).
func unpackLongs(data []uint64, bitsPer int, out []uint32, palette []uint32) {
	bitpack.Unpack64(data, bitsPer, out)
	maxIdx := uint32(len(palette) - 1)
	for i, v := range out {
		if v > maxIdx {
			v = 0
		}
		out[i] = palette[v]
	}
}
