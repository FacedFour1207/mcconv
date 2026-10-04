package java

import (
	"unsafe"

	"encoding/binary"
	"math/bits"
	"mcconv/internal/bitpack"
	"sync"

	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// Encoder writes chunk NBT bytes directly (big endian), without building an
// NBT tree. It reuses its buffers; use one per goroutine.
type Encoder struct {
	c        *Codec
	buf      []byte
	canon    []uint32
	first    map[*translate.Block]uint32
	localMap []int32
	pal      []int
	idx      [4096]uint32
	longs    []int64
	shape    []bool
	secShape []bool // per section: contains blocks needing a shape update
}

// NewEncoder returns an encoder bound to the codec.
func (c *Codec) NewEncoder() *Encoder {
	return &Encoder{c: c, first: map[*translate.Block]uint32{}}
}

// paletteCache maps a (shared) output block to its encoded palette compound
// payload: {Name, Properties} including the waterlogged property.
var paletteCache sync.Map // *translate.Block -> []byte

func (c *Codec) palettePayload(b *translate.Block) []byte {
	if v, ok := paletteCache.Load(b); ok {
		return v.([]byte)
	}
	p := nbt.AppendPayload(nil, c.paletteNBT(b), nbt.BigEndian)
	v, _ := paletteCache.LoadOrStore(b, p)
	return v.([]byte)
}

type bw struct{ b []byte }

func (w *bw) tag(t byte, name string) {
	w.b = append(w.b, t)
	w.b = binary.BigEndian.AppendUint16(w.b, uint16(len(name)))
	w.b = append(w.b, name...)
}
func (w *bw) int8(name string, v int8) { w.tag(nbt.TagByte, name); w.b = append(w.b, byte(v)) }
func (w *bw) int32(name string, v int32) {
	w.tag(nbt.TagInt, name)
	w.b = binary.BigEndian.AppendUint32(w.b, uint32(v))
}
func (w *bw) int64(name string, v int64) {
	w.tag(nbt.TagLong, name)
	w.b = binary.BigEndian.AppendUint64(w.b, uint64(v))
}
func (w *bw) str(name, v string) {
	w.tag(nbt.TagString, name)
	w.b = nbt.AppendString(w.b, v, nbt.BigEndian)
}
func (w *bw) listHeader(name string, elem byte, n int) {
	w.tag(nbt.TagList, name)
	w.b = append(w.b, elem)
	w.b = binary.BigEndian.AppendUint32(w.b, uint32(n))
}
func (w *bw) longArray(name string, v []int64) {
	w.tag(nbt.TagLongArray, name)
	w.b = binary.BigEndian.AppendUint32(w.b, uint32(len(v)))
	for _, x := range v {
		w.b = binary.BigEndian.AppendUint64(w.b, uint64(x))
	}
}
func (w *bw) end() { w.b = append(w.b, nbt.TagEnd) }

func packInto(dst []int64, idx []uint32, bitsPer int) []int64 {
	n := bitpack.Words64(len(idx), bitsPer)
	if cap(dst) < n {
		dst = make([]int64, n)
	}
	dst = dst[:n]
	bitpack.Pack64(unsafe.Slice((*uint64)(unsafe.Pointer(unsafe.SliceData(dst))), n), idx, bitsPer)
	return dst
}

// Encode returns the chunk as named-root NBT bytes (valid until the next call).
func (e *Encoder) Encode(ch *world.Chunk) []byte {
	c := e.c
	np := len(ch.Palette)
	// palette entries may repeat; canon maps each to its first occurrence
	if cap(e.canon) < np {
		e.canon = make([]uint32, np)
		e.localMap = make([]int32, np)
		e.shape = make([]bool, np)
	}
	e.canon, e.localMap, e.shape = e.canon[:np], e.localMap[:np], e.shape[:np]
	e.secShape = e.secShape[:0]
	clear(e.first)
	for i, b := range ch.Palette {
		if j, ok := e.first[b]; ok {
			e.canon[i] = j
		} else {
			e.first[b] = uint32(i)
			e.canon[i] = uint32(i)
		}
		e.localMap[i] = -1
		e.shape[i] = NeedsShapeUpdate(b.Name())
	}
	biomePal := ch.BiomePalette
	if len(biomePal) == 0 {
		biomePal = []string{"minecraft:plains"}
	}
	w := &bw{b: e.buf[:0]}
	w.tag(nbt.TagCompound, "")
	w.int32("DataVersion", c.DataVersion)
	w.int32("xPos", ch.X)
	w.int32("zPos", ch.Z)
	w.int32("yPos", int32(ch.Bounds.MinSection))
	w.str("Status", "minecraft:full")
	w.int64("LastUpdate", 0)
	w.int64("InhabitedTime", 0)
	w.int8("isLightOn", 0)
	w.listHeader("sections", nbt.TagCompound, ch.Bounds.Height())
	for si := 0; si < ch.Bounds.Height(); si++ {
		w.int8("Y", int8(ch.Bounds.MinSection+si))
		w.tag(nbt.TagCompound, "block_states")
		s := ch.Sections[si]
		hasShape := false
		if s == nil {
			w.listHeader("palette", nbt.TagCompound, 1)
			w.b = append(w.b, c.palettePayload(ch.Palette[0])...)
		} else {
			pal := e.pal[:0]
			for i, v := range s.Blocks {
				v = e.canon[v]
				l := e.localMap[v]
				if l < 0 {
					l = int32(len(pal))
					e.localMap[v] = l
					pal = append(pal, int(v))
				}
				e.idx[i] = uint32(l)
			}
			e.pal = pal
			w.listHeader("palette", nbt.TagCompound, len(pal))
			for _, g := range pal {
				w.b = append(w.b, c.palettePayload(ch.Palette[g])...)
				e.localMap[g] = -1
				hasShape = hasShape || e.shape[g]
			}
			if len(pal) > 1 {
				e.longs = packInto(e.longs, e.idx[:], max(4, bits.Len(uint(len(pal)-1))))
				w.longArray("data", e.longs)
			}
		}
		w.end() // block_states
		e.secShape = append(e.secShape, hasShape)
		// biomes
		var bsec *world.BiomeSection
		if si < len(ch.Biomes) {
			bsec = ch.Biomes[si]
		}
		if bsec == nil {
			for d := 1; d < len(ch.Biomes) && bsec == nil; d++ {
				if si-d >= 0 && ch.Biomes[si-d] != nil {
					bsec = ch.Biomes[si-d]
				} else if si+d < len(ch.Biomes) && ch.Biomes[si+d] != nil {
					bsec = ch.Biomes[si+d]
				}
			}
		}
		w.tag(nbt.TagCompound, "biomes")
		if bsec == nil {
			w.listHeader("palette", nbt.TagString, 1)
			w.b = nbt.AppendString(w.b, biomePal[0], nbt.BigEndian)
		} else {
			var bl [64]uint16
			var bidx [64]uint32
			n := 0
			for i, v := range bsec {
				j := 0
				for j < n && bl[j] != v {
					j++
				}
				if j == n {
					bl[n] = v
					n++
				}
				bidx[i] = uint32(j)
			}
			w.listHeader("palette", nbt.TagString, n)
			for _, v := range bl[:n] {
				name := "minecraft:plains"
				if int(v) < len(biomePal) {
					name = biomePal[v]
				}
				w.b = nbt.AppendString(w.b, name, nbt.BigEndian)
			}
			if n > 1 {
				var tmp [64]int64
				w.longArray("data", packInto(tmp[:0], bidx[:], bits.Len(uint(n-1))))
			}
		}
		w.end() // biomes
		w.end() // section
	}
	// block entities
	w.listHeader("block_entities", nbt.TagCompound, len(ch.BlockEntities))
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
		w.b = nbt.AppendPayload(w.b, t, nbt.BigEndian)
	}
	w.listHeader("block_ticks", nbt.TagEnd, 0)
	w.listHeader("fluid_ticks", nbt.TagEnd, 0)
	// post processing: positions whose shape the game recomputes on load
	pp, _ := ch.Misc["postProcess"].(bool)
	w.listHeader("PostProcessing", nbt.TagList, ch.Bounds.Height())
	for si, sec := range ch.Sections {
		if !pp || sec == nil || !e.secShape[si] {
			w.b = append(w.b, nbt.TagShort, 0, 0, 0, 0)
			continue
		}
		start := len(w.b)
		w.b = append(w.b, nbt.TagShort, 0, 0, 0, 0)
		cnt := 0
		for p, v := range sec.Blocks {
			if e.shape[e.canon[v]] {
				x, y, z := p&15, p>>8, (p>>4)&15
				w.b = binary.BigEndian.AppendUint16(w.b, uint16(x|y<<4|z<<8))
				cnt++
			}
		}
		binary.BigEndian.PutUint32(w.b[start+1:], uint32(cnt))
	}
	w.tag(nbt.TagCompound, "structures")
	w.tag(nbt.TagCompound, "References")
	w.end()
	w.tag(nbt.TagCompound, "starts")
	w.end()
	w.end()
	w.tag(nbt.TagCompound, "Heightmaps")
	w.end()
	w.end() // root
	e.buf = w.b
	return w.b
}
