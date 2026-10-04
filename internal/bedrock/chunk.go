// Package bedrock reads and writes Bedrock Edition (LevelDB) worlds.
package bedrock

import (
	"encoding/binary"
	"sync"

	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// Chunk record tags.
const (
	TagData3D         = 0x2B
	TagVersion        = 0x2C
	TagData2D         = 0x2D
	TagSubChunk       = 0x2F
	TagBlockEntity    = 0x31
	TagEntity         = 0x32
	TagPendingTicks   = 0x33
	TagFinalizedState = 0x36
	TagLegacyVersion  = 0x76
)

// ChunkVersion is written to record 0x2C (set from config.Versions).
var ChunkVersion byte = 42

// IsChunkTag reports whether b is a known chunk record tag.
func IsChunkTag(b byte) bool {
	return (b >= 0x2B && b <= 0x41) || b == TagLegacyVersion
}

// ChunkKey builds the key prefix (x, z, [dim]).
func ChunkKey(x, z int32, dim world.Dimension) []byte {
	k := make([]byte, 8, 14)
	binary.LittleEndian.PutUint32(k, uint32(x))
	binary.LittleEndian.PutUint32(k[4:], uint32(z))
	if dim != world.Overworld {
		k = binary.LittleEndian.AppendUint32(k, uint32(dim))
	}
	return k
}

// ParseChunkKey decodes a chunk record key.
func ParseChunkKey(k []byte) (x, z int32, dim world.Dimension, tag byte, sub int8, ok bool) {
	switch len(k) {
	case 9, 10:
		tag = k[8]
		if len(k) == 10 {
			if tag != TagSubChunk {
				return
			}
			sub = int8(k[9])
		}
	case 13, 14:
		d := binary.LittleEndian.Uint32(k[8:])
		if d != 1 && d != 2 {
			return
		}
		dim = world.Dimension(d)
		tag = k[12]
		if len(k) == 14 {
			if tag != TagSubChunk {
				return
			}
			sub = int8(k[13])
		}
	default:
		return
	}
	if !IsChunkTag(tag) {
		return
	}
	x = int32(binary.LittleEndian.Uint32(k))
	z = int32(binary.LittleEndian.Uint32(k[4:]))
	return x, z, dim, tag, sub, true
}

// RawChunk holds the undecoded records of one chunk.
type RawChunk struct {
	X, Z int32
	Dim  world.Dimension
	Data map[byte][]byte
	Sub  map[int8][]byte
}

// Bounds of a Bedrock dimension.
func Bounds(d world.Dimension) world.Bounds {
	switch d {
	case world.Nether:
		return world.Bounds{MinSection: 0, MaxSection: 8}
	case world.End:
		return world.Bounds{MinSection: 0, MaxSection: 16}
	}
	return world.Bounds{MinSection: -4, MaxSection: 20}
}

// Codec converts between raw records and world.Chunk.
type Codec struct {
	// Biomes maps numeric biome ids to names.
	Biomes BiomeTable
	// BlockVersion is the block state "version" written to palettes.
	BlockVersion int32

	encCache  sync.Map // *translate.Block -> *blockEnc
	layerOnce sync.Once
	layerTab  *layerTable
}

// BiomeTable maps Bedrock biome ids to their names ("plains") and back.
type BiomeTable interface {
	BedrockBiomeName(id int) (string, bool)
	BedrockBiomeID(name string) (int, bool)
}

func (c *Codec) blockVersion() int32 { return c.BlockVersion }

func (c *Codec) biomeName(id int) string {
	if n, ok := c.Biomes.BedrockBiomeName(id); ok {
		return n
	}
	return "plains"
}

var airBlock = translate.NewBlock("minecraft:air", nil)

// FinalizedState returns the chunk's generation state (2 = finished).
func (r *RawChunk) FinalizedState() int {
	v, ok := r.Data[TagFinalizedState]
	if !ok {
		return 2
	}
	switch len(v) {
	case 1:
		return int(int8(v[0]))
	case 4:
		return int(int32(binary.LittleEndian.Uint32(v)))
	}
	return 2
}

type paletteEntry struct {
	block *translate.Block
	air   bool
}

type storage struct {
	indices [4096]uint32 // XZY order
	palette []paletteEntry
}

func blockFromNBT(root nbt.Compound) paletteEntry {
	name, _ := root.String("name")
	props := map[string]any{}
	if states, ok := root.Compound("states"); ok {
		for k, v := range states {
			props[k] = v
		}
	} else if val, ok := root.Number("val"); ok {
		props["block_data"] = int32(val)
	}
	b := translate.NewBlock(name, props)
	if v, ok := root.Number("version"); ok {
		b.Version = int32(v)
	}
	n := b.Name()
	return paletteEntry{block: b, air: n == "minecraft:air"}
}

func (c *Codec) decodeBiomes3D(ch *world.Chunk, data []byte) {
	idx := map[int]uint16{}
	pal := func(id int) uint16 {
		if i, ok := idx[id]; ok {
			return i
		}
		name := c.biomeName(id)
		i := uint16(len(ch.BiomePalette))
		idx[id] = i
		ch.BiomePalette = append(ch.BiomePalette, name)
		return i
	}
	var prev *world.BiomeSection
	for si := 0; len(data) > 0; si++ {
		h := data[0]
		data = data[1:]
		var sec *world.BiomeSection
		if h == 0xFF {
			sec = prev
		} else {
			bitsPer := int(h >> 1)
			sec = &world.BiomeSection{}
			if bitsPer == 0 {
				if len(data) < 4 {
					return
				}
				v := pal(int(int32(binary.LittleEndian.Uint32(data))))
				data = data[4:]
				for i := range sec {
					sec[i] = v
				}
			} else {
				if bitsPer > 16 {
					return
				}
				perWord := 32 / bitsPer
				words := (4096 + perWord - 1) / perWord
				if len(data) < words*4+4 {
					return
				}
				var raw [4096]uint16
				mask := uint32(1)<<bitsPer - 1
				i := 0
				for w := 0; w < words; w++ {
					word := binary.LittleEndian.Uint32(data[w*4:])
					for j := 0; j < perWord && i < 4096; j++ {
						raw[i] = uint16(word & mask)
						word >>= bitsPer
						i++
					}
				}
				data = data[words*4:]
				n := int(binary.LittleEndian.Uint32(data))
				data = data[4:]
				if len(data) < n*4 {
					return
				}
				local := make([]uint16, n)
				for k := range n {
					local[k] = pal(int(int32(binary.LittleEndian.Uint32(data[k*4:]))))
				}
				data = data[n*4:]
				for y4 := 0; y4 < 4; y4++ {
					for z4 := 0; z4 < 4; z4++ {
						for x4 := 0; x4 < 4; x4++ {
							v := raw[(x4*4)<<8|(z4*4)<<4|(y4*4)]
							if int(v) >= len(local) {
								v = 0
							}
							sec[y4<<4|z4<<2|x4] = local[v]
						}
					}
				}
			}
		}
		prev = sec
		if si < len(ch.Biomes) {
			ch.Biomes[si] = sec
		}
	}
}

func (c *Codec) decodeBiomes2D(ch *world.Chunk, data []byte) {
	idx := map[int]uint16{}
	sec := &world.BiomeSection{}
	for z4 := 0; z4 < 4; z4++ {
		for x4 := 0; x4 < 4; x4++ {
			id := int(data[(z4*4)*16+x4*4])
			v, ok := idx[id]
			if !ok {
				name := c.biomeName(id)
				v = uint16(len(ch.BiomePalette))
				idx[id] = v
				ch.BiomePalette = append(ch.BiomePalette, name)
			}
			for y4 := 0; y4 < 4; y4++ {
				sec[y4<<4|z4<<2|x4] = v
			}
		}
	}
	for i := range ch.Biomes {
		ch.Biomes[i] = sec
	}
}
