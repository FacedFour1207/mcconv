package bedrock

import (
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"sync"

	"mcconv/internal/bitpack"
	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// Palette entries are interned by their raw NBT bytes, so a block state is
// parsed once per conversion instead of once per sub-chunk, and equal states
// share one *translate.Block (which keeps the translation caches small).
type paletteIntern struct {
	seed   maphash.Seed
	shards [64]struct {
		mu sync.RWMutex
		m  map[string]paletteEntry
	}
	stackMu sync.Mutex
	stacks  map[[2]*translate.Block]*translate.Block
}

var interned = func() *paletteIntern {
	p := &paletteIntern{seed: maphash.MakeSeed(), stacks: map[[2]*translate.Block]*translate.Block{}}
	for i := range p.shards {
		p.shards[i].m = map[string]paletteEntry{}
	}
	return p
}()

func internEntry(raw []byte) (paletteEntry, error) {
	sh := &interned.shards[maphash.Bytes(interned.seed, raw)&63]
	sh.mu.RLock()
	pe, ok := sh.m[string(raw)]
	sh.mu.RUnlock()
	if ok {
		return pe, nil
	}
	root, _, err := nbt.DecodeCompound(raw, nbt.LittleEndian)
	if err != nil {
		return paletteEntry{}, err
	}
	pe = blockFromNBT(root)
	if pe.air && len(pe.block.Props) == 0 { // air translates the same in every version
		pe.block = airBlock
	}
	sh.mu.Lock()
	if old, ok := sh.m[string(raw)]; ok {
		pe = old
	} else {
		sh.m[string(raw)] = pe
	}
	sh.mu.Unlock()
	return pe, nil
}

// internStack returns the shared block for layer0 + layer1.
func internStack(l0, l1 *translate.Block) *translate.Block {
	k := [2]*translate.Block{l0, l1}
	interned.stackMu.Lock()
	defer interned.stackMu.Unlock()
	if b, ok := interned.stacks[k]; ok {
		return b
	}
	b := &translate.Block{Namespace: l0.Namespace, BaseName: l0.BaseName, Props: l0.Props, Version: l0.Version, Extra: []*translate.Block{l1}}
	interned.stacks[k] = b
	return b
}

// Decoder decodes chunks reusing scratch buffers; use one per goroutine.
type Decoder struct {
	c        *Codec
	storages []*storage
	keyIndex map[*translate.Block]uint32
	local    []uint32
	// Sections, if set, supplies section arrays (per-goroutine free list).
	Sections *world.SectionCache
}

// NewDecoder returns a decoder bound to the codec.
func (c *Codec) NewDecoder() *Decoder {
	return &Decoder{c: c, keyIndex: map[*translate.Block]uint32{}}
}

// DecodeChunk converts raw records into a chunk.
func (c *Codec) DecodeChunk(r *RawChunk) (*world.Chunk, error) {
	return c.NewDecoder().Decode(r)
}

func (d *Decoder) storage(i int) *storage {
	for len(d.storages) <= i {
		d.storages = append(d.storages, &storage{})
	}
	return d.storages[i]
}

// Decode converts raw records into a chunk.
func (d *Decoder) Decode(r *RawChunk) (*world.Chunk, error) {
	bounds := Bounds(r.Dim)
	ch := world.NewChunk(r.X, r.Z, bounds)
	ch.Palette = append(ch.Palette, airBlock)
	clear(d.keyIndex)
	d.keyIndex[airBlock] = 0
	addBlock := func(b *translate.Block) uint32 {
		if i, ok := d.keyIndex[b]; ok {
			return i
		}
		i := uint32(len(ch.Palette))
		d.keyIndex[b] = i
		ch.Palette = append(ch.Palette, b)
		return i
	}
	for sy, data := range r.Sub {
		si := int(sy) - bounds.MinSection
		if si < 0 || si >= bounds.Height() || len(data) == 0 {
			continue
		}
		n, err := d.decodeSubChunk(data)
		if err != nil {
			return nil, fmt.Errorf("chunk %d,%d sub %d: %w", r.X, r.Z, sy, err)
		}
		if n == 0 {
			continue
		}
		layers := d.storages[:n]
		// drop extra layers that are entirely air
		for len(layers) > 1 && allAir(layers[len(layers)-1]) {
			layers = layers[:len(layers)-1]
		}
		sec := d.Sections.Get()
		if len(layers) == 1 {
			st := layers[0]
			local := d.local[:0]
			for _, p := range st.palette {
				local = append(local, addBlock(p.block))
			}
			d.local = local
			if len(local) == 1 {
				v := local[0]
				for i := range sec.Blocks {
					sec.Blocks[i] = v
				}
			} else {
				for xzy, v := range st.indices {
					if int(v) >= len(local) {
						v = 0
					}
					sec.Blocks[xzyToYZX[xzy]] = local[v]
				}
			}
		} else {
			// two (or more) layers: index by the combination of layer 0 and 1
			l0, l1 := layers[0], layers[1]
			combos := map[uint32]uint32{}
			for xzy := 0; xzy < 4096; xzy++ {
				a, b := l0.indices[xzy], l1.indices[xzy]
				k := uint32(a)<<16 | uint32(b)
				idx, ok := combos[k]
				if !ok {
					if int(a) >= len(l0.palette) {
						a = 0
					}
					if int(b) >= len(l1.palette) {
						b = 0
					}
					p0, p1 := l0.palette[a], l1.palette[b]
					blk := p0.block
					if !p1.air {
						blk = internStack(p0.block, p1.block)
					}
					idx = addBlock(blk)
					combos[k] = idx
				}
				sec.Blocks[xzyToYZX[xzy]] = idx
			}
		}
		ch.Sections[si] = sec
	}
	c := d.c
	if d3, ok := r.Data[TagData3D]; ok && len(d3) > 512 {
		c.decodeBiomes3D(ch, d3[512:])
	} else if d2, ok := r.Data[TagData2D]; ok && len(d2) >= 768 {
		c.decodeBiomes2D(ch, d2[512:768])
	}
	if raw, ok := r.Data[TagBlockEntity]; ok {
		list, _ := nbt.DecodeAll(raw, nbt.LittleEndian)
		for _, be := range list {
			id, _ := be.String("id")
			bx, ok1 := be.Number("x")
			by, ok2 := be.Number("y")
			bz, ok3 := be.Number("z")
			if id == "" || !ok1 || !ok2 || !ok3 {
				continue
			}
			delete(be, "id")
			delete(be, "x")
			delete(be, "y")
			delete(be, "z")
			ch.BlockEntities[world.Pos{X: int32(bx), Y: int32(by), Z: int32(bz)}] = &translate.BlockEntity{Namespace: "", BaseName: id, NBT: be}
		}
	}
	return ch, nil
}

func allAir(st *storage) bool {
	for _, p := range st.palette {
		if !p.air {
			return false
		}
	}
	return true
}

// decodeSubChunk fills d.storages and returns how many were decoded.
func (d *Decoder) decodeSubChunk(data []byte) (int, error) {
	var count int
	switch data[0] {
	case 1:
		count = 1
		data = data[1:]
	case 8:
		if len(data) < 2 {
			return 0, fmt.Errorf("truncated sub-chunk")
		}
		count = int(data[1])
		data = data[2:]
	case 9:
		if len(data) < 3 {
			return 0, fmt.Errorf("truncated sub-chunk")
		}
		count = int(data[1])
		data = data[3:]
	default:
		return 0, nil // legacy formats are not supported
	}
	for i := 0; i < count; i++ {
		rest, err := d.decodeStorageInto(d.storage(i), data)
		if err != nil {
			return 0, err
		}
		data = rest
	}
	return count, nil
}

func (d *Decoder) decodeStorageInto(st *storage, data []byte) ([]byte, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("truncated storage")
	}
	bitsPer := int(data[0] >> 1)
	data = data[1:]
	palLen := 1
	if bitsPer > 0 {
		if bitsPer > 16 {
			return nil, fmt.Errorf("bad bits per block %d", bitsPer)
		}
		perWord := 32 / bitsPer
		words := (4096 + perWord - 1) / perWord
		if len(data) < words*4+4 {
			return nil, fmt.Errorf("truncated storage words")
		}
		bitpack.Unpack32(data[:words*4], bitsPer, st.indices[:])
		data = data[words*4:]
		palLen = int(int32(binary.LittleEndian.Uint32(data)))
		data = data[4:]
	} else {
		st.indices = [4096]uint32{}
	}
	st.palette = st.palette[:0]
	for range palLen {
		n, err := nbt.RootLen(data, nbt.LittleEndian)
		if err != nil {
			return nil, err
		}
		pe, err := internEntry(data[:n])
		if err != nil {
			return nil, err
		}
		data = data[n:]
		st.palette = append(st.palette, pe)
	}
	if len(st.palette) == 0 {
		st.palette = append(st.palette, paletteEntry{block: airBlock, air: true})
	}
	return data, nil
}
