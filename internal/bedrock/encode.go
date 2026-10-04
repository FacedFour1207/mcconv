package bedrock

import (
	"bytes"
	"encoding/binary"
	"math/bits"
	"sort"
	"sync"

	"mcconv/internal/bitpack"
	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// Record is one LevelDB key/value pair.
type Record struct {
	Key, Value []byte
}

// blockEnc caches the encoded palette entries of a (multi-layer) block.
type blockEnc struct {
	ids  []int32  // global layer ids
	data [][]byte // encoded palette compound per layer
	air  bool
}

// layerTable assigns global ids to encoded layer blocks.
type layerTable struct {
	mu    sync.Mutex
	byKey map[string]int32
	n     int32
}

// Encoder converts chunks to records. It keeps scratch buffers, so use one
// Encoder per goroutine; all Encoders of a Codec share the block cache.
type Encoder struct {
	c *Codec

	stamp   []uint32
	localOf []int32
	gen     uint32
	pal     []int32
	palData [][]byte
	idx     [4096]uint16
	encs    []*blockEnc
	depth   []int
	isAir   []bool
	bufs    [][]byte // reusable record buffers (valid until the next Encode)
	nbuf    int
	recs    []Record
	seen    []uint32 // per chunk palette entry: generation it was seen in
	vLocal  []uint16 // per chunk palette entry: local storage index
	used    []uint32
}

// xzyToYZX maps a Bedrock (XZY) index to a world.Section (YZX) index.
var xzyToYZX = func() (t [4096]uint16) {
	for xzy := range 4096 {
		x, z, y := xzy>>8, (xzy>>4)&15, xzy&15
		t[xzy] = uint16(y<<8 | z<<4 | x)
	}
	return
}()

// NewEncoder returns an encoder bound to the codec.
func (c *Codec) NewEncoder() *Encoder { return &Encoder{c: c} }

func (c *Codec) layers() *layerTable {
	c.layerOnce.Do(func() { c.layerTab = &layerTable{byKey: map[string]int32{}} })
	return c.layerTab
}

// blockEnc returns the cached encoding of a palette block. Palette blocks are
// expected to be shared pointers (from the translation cache), so the cache
// is keyed by pointer.
func (c *Codec) blockEnc(b *translate.Block) *blockEnc {
	if v, ok := c.encCache.Load(b); ok {
		return v.(*blockEnc)
	}
	lt := c.layers()
	ls := b.Layers()
	e := &blockEnc{ids: make([]int32, len(ls)), data: make([][]byte, len(ls))}
	for i, l := range ls {
		k := l.Key()
		lt.mu.Lock()
		id, ok := lt.byKey[k]
		if !ok {
			id = lt.n
			lt.n++
			lt.byKey[k] = id
		}
		lt.mu.Unlock()
		e.ids[i] = id
		e.data[i] = c.blockNBT(l)
	}
	e.air = len(ls) == 1 && ls[0].Name() == "minecraft:air"
	v, _ := c.encCache.LoadOrStore(b, e)
	return v.(*blockEnc)
}

// blockNBT renders a block layer as a palette compound.
func (c *Codec) blockNBT(b *translate.Block) []byte {
	states := nbt.Compound{}
	for k, v := range b.Props {
		switch v.(type) {
		case int8, int16, int32, int64, string, float32, float64:
			states[k] = v
		}
	}
	return nbt.Encode("", nbt.Compound{
		"name":    b.Name(),
		"states":  states,
		"version": c.blockVersion(),
	}, nbt.LittleEndian)
}

func (e *Encoder) grow(id int32) {
	if int(id) >= len(e.stamp) {
		n := int(id)*2 + 64
		e.stamp = append(e.stamp, make([]uint32, n-len(e.stamp))...)
		e.localOf = append(e.localOf, make([]int32, n-len(e.localOf))...)
	}
}

// buf returns an empty reusable buffer; keep returns it after filling.
func (e *Encoder) buf() []byte {
	if e.nbuf == len(e.bufs) {
		e.bufs = append(e.bufs, make([]byte, 0, 4096))
	}
	b := e.bufs[e.nbuf][:0]
	return b
}

func (e *Encoder) keep(b []byte) []byte {
	e.bufs[e.nbuf] = b
	e.nbuf++
	return b
}

// Encode renders a chunk whose palette holds Bedrock blocks. Records are
// returned sorted by key; they (and their bytes) are only valid until the
// next call to Encode.
func (e *Encoder) Encode(ch *world.Chunk, dim world.Dimension) []Record {
	c := e.c
	e.nbuf = 0
	prefix := ChunkKey(ch.X, ch.Z, dim)
	key := func(tag byte, extra ...byte) []byte {
		k := append(append(e.buf(), prefix...), tag)
		return e.keep(append(k, extra...))
	}
	e.encs = e.encs[:0]
	e.depth = e.depth[:0]
	e.isAir = e.isAir[:0]
	for _, b := range ch.Palette {
		be := c.blockEnc(b)
		e.encs = append(e.encs, be)
		e.depth = append(e.depth, len(be.ids))
		e.isAir = append(e.isAir, be.air)
	}
	airEnc := c.blockEnc(airBlock)
	airID := airEnc.ids[0]
	e.grow(airID)

	recs := e.recs[:0]
	recs = append(recs, Record{key(TagData3D), e.keep(c.encodeData3D(e.buf(), ch, computeHeights(ch, e.isAir)))})
	recs = append(recs, Record{key(TagVersion), []byte{ChunkVersion}})
	subStart := len(recs)
	np := len(ch.Palette)
	if cap(e.seen) < np {
		e.seen = make([]uint32, np)
		e.vLocal = make([]uint16, np)
	}
	e.seen = e.seen[:np]
	e.vLocal = e.vLocal[:np]
	for si, sec := range ch.Sections {
		if sec == nil {
			continue
		}
		// One pass in storage (XZY) order: discover the section's palette
		// entries and map them to layer-0 local indices at the same time.
		e.gen++
		gen0 := e.gen
		e.gen++ // separate stamp generation for global layer ids
		lgen := e.gen
		used := e.used[:0]
		e.pal = e.pal[:0]
		e.palData = e.palData[:0]
		blocks := &sec.Blocks
		seen, vl := e.seen, e.vLocal
		for xzy := range e.idx {
			v := blocks[xzyToYZX[xzy]]
			if seen[v] != gen0 {
				seen[v] = gen0
				used = append(used, v)
				enc := e.encs[v]
				id, data := enc.ids[0], enc.data[0]
				if int(id) >= len(e.stamp) {
					e.grow(id)
				}
				if e.stamp[id] != lgen {
					e.stamp[id] = lgen
					e.localOf[id] = int32(len(e.pal))
					e.pal = append(e.pal, id)
					e.palData = append(e.palData, data)
				}
				vl[v] = uint16(e.localOf[id])
			}
			e.idx[xzy] = vl[v]
		}
		e.used = used
		depth := 0
		for _, v := range used {
			if !e.isAir[v] && e.depth[v] > depth {
				depth = e.depth[v]
			}
		}
		if depth == 0 {
			continue
		}
		sy := ch.Bounds.MinSection + si
		buf := e.buf()
		buf = append(buf, 9, byte(depth), byte(int8(sy)))
		for l := 0; l < depth; l++ {
			if l > 0 {
				// extra layers (e.g. water in waterlogged blocks)
				e.gen++
				e.pal = e.pal[:0]
				e.palData = e.palData[:0]
				for _, v := range used {
					enc := e.encs[v]
					id, data := airID, airEnc.data[0]
					if l < len(enc.ids) {
						id, data = enc.ids[l], enc.data[l]
					}
					if int(id) >= len(e.stamp) {
						e.grow(id)
					}
					if e.stamp[id] != e.gen {
						e.stamp[id] = e.gen
						e.localOf[id] = int32(len(e.pal))
						e.pal = append(e.pal, id)
						e.palData = append(e.palData, data)
					}
					vl[v] = uint16(e.localOf[id])
				}
				if len(e.pal) > 1 {
					for xzy := range e.idx {
						e.idx[xzy] = vl[blocks[xzyToYZX[xzy]]]
					}
				}
			}
			if len(e.pal) == 1 {
				buf = appendUniform(buf)
			} else {
				buf = appendPackedXZY(buf, e.idx[:], storageBits(len(e.pal)), 0)
			}
			buf = binary.LittleEndian.AppendUint32(buf, uint32(len(e.pal)))
			for _, d := range e.palData {
				buf = append(buf, d...)
			}
		}
		buf = e.keep(buf)
		recs = append(recs, Record{key(TagSubChunk, byte(int8(sy))), buf})
	}
	// sub-chunk keys sort by the index byte (negative sections last)
	subs := recs[subStart:]
	sort.Slice(subs, func(i, j int) bool { return bytes.Compare(subs[i].Key, subs[j].Key) < 0 })
	if len(ch.BlockEntities) > 0 {
		beBuf := e.buf()
		for pos, be := range ch.BlockEntities {
			t := be.NBT.Clone()
			if t == nil {
				t = nbt.Compound{}
			}
			t["id"] = be.BaseName
			t["x"], t["y"], t["z"] = pos.X, pos.Y, pos.Z
			if _, ok := t["isMovable"]; !ok {
				t["isMovable"] = int8(1)
			}
			beBuf = nbt.AppendEncode(beBuf, "", t, nbt.LittleEndian)
		}
		beBuf = e.keep(beBuf)
		recs = append(recs, Record{key(TagBlockEntity), beBuf})
	}
	recs = append(recs, Record{key(TagFinalizedState), finalized})
	e.recs = recs
	return recs
}

// EncodeChunk renders a chunk into a key/value map (convenience wrapper).
func (c *Codec) EncodeChunk(ch *world.Chunk, dim world.Dimension) map[string][]byte {
	out := map[string][]byte{}
	for _, r := range c.NewEncoder().Encode(ch, dim) {
		out[string(r.Key)] = append([]byte(nil), r.Value...)
	}
	return out
}

func storageBits(n int) int {
	b := bits.Len(uint(n - 1))
	switch {
	case b <= 1:
		return 1
	case b <= 6:
		return b
	case b <= 8:
		return 8
	}
	return 16
}

// appendUniform writes a 1-bit storage whose entries are all 0.
func appendUniform(dst []byte) []byte {
	dst = append(dst, 1<<1)
	return append(dst, zeroWords1[:]...)
}

var zeroWords1 [4096 / 32 * 4]byte

func appendPackedXZY(dst []byte, idx []uint16, bitsPer int, header byte) []byte {
	dst = append(dst, byte(bitsPer<<1)|header)
	start := len(dst)
	dst = append(dst, make([]byte, bitpack.Words32(len(idx), bitsPer)*4)...)
	bitpack.Pack32(dst[start:], idx, bitsPer)
	return dst
}

func computeHeights(ch *world.Chunk, isAir []bool) [256]int16 {
	var h [256]int16
	done := 0
	for si := len(ch.Sections) - 1; si >= 0 && done < 256; si-- {
		sec := ch.Sections[si]
		if sec == nil {
			continue
		}
		base := int16((ch.Bounds.MinSection+si)*16 + 1 - ch.Bounds.MinSection*16)
		for y := 15; y >= 0 && done < 256; y-- {
			row := sec.Blocks[y<<8 : y<<8+256]
			for zx, v := range row {
				if h[zx] == 0 && !isAir[v] {
					h[zx] = base + int16(y)
					done++
				}
			}
		}
	}
	return h
}

var finalized = []byte{2, 0, 0, 0}

func (c *Codec) encodeData3D(out []byte, ch *world.Chunk, heights [256]int16) []byte {
	for _, v := range heights {
		out = binary.LittleEndian.AppendUint16(out, uint16(v))
	}
	plains, _ := c.Biomes.BedrockBiomeID("plains")
	ids := make([]int32, len(ch.BiomePalette))
	for i, name := range ch.BiomePalette {
		id, ok := c.Biomes.BedrockBiomeID(name)
		if !ok {
			id = plains
		}
		ids[i] = int32(id)
	}
	idOf := func(v uint16) int32 {
		if int(v) < len(ids) {
			return ids[v]
		}
		return int32(plains)
	}
	var last *world.BiomeSection
	var lastStart, lastEnd int
	var idx [4096]uint16
	var localOf [64]uint8
	var palVals [64]uint16
	var pal [64]int32
	for si := 0; si < ch.Bounds.Height(); si++ {
		var sec *world.BiomeSection
		if si < len(ch.Biomes) {
			sec = ch.Biomes[si]
		}
		if sec == nil {
			sec = last
		}
		if sec == nil {
			for j := si + 1; j < len(ch.Biomes); j++ {
				if ch.Biomes[j] != nil {
					sec = ch.Biomes[j]
					break
				}
			}
		}
		if sec == nil {
			out = append(out, 1)
			out = binary.LittleEndian.AppendUint32(out, uint32(plains))
			continue
		}
		if last != nil && (sec == last || *sec == *last) {
			// repeat the previous storage; BDS crashes on the 0xFF
			// "copy previous" marker in converted worlds
			out = append(out, out[lastStart:lastEnd]...)
			continue
		}
		last = sec
		lastStart = len(out)
		n := 0
		for i, v := range sec {
			j := 0
			for j < n && palVals[j] != v {
				j++
			}
			if j == n {
				palVals[n] = v
				pal[n] = idOf(v)
				n++
			}
			localOf[i] = uint8(j)
		}
		if n == 1 {
			out = append(out, 1)
			out = binary.LittleEndian.AppendUint32(out, uint32(pal[0]))
			lastEnd = len(out)
			continue
		}
		for xzy := 0; xzy < 4096; xzy++ {
			x, z, y := xzy>>8, (xzy>>4)&15, xzy&15
			idx[xzy] = uint16(localOf[(y>>2)<<4|(z>>2)<<2|(x>>2)])
		}
		out = appendPackedXZY(out, idx[:], storageBits(n), 1)
		out = binary.LittleEndian.AppendUint32(out, uint32(n))
		for _, id := range pal[:n] {
			out = binary.LittleEndian.AppendUint32(out, uint32(id))
		}
		lastEnd = len(out)
	}
	return out
}
