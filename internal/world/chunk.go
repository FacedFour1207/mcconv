// Package world defines the edition independent chunk model that readers
// produce and writers consume.
package world

import (
	"mcconv/internal/nbt"
	"mcconv/internal/translate"
)

// Dimension identifiers.
type Dimension int

const (
	Overworld Dimension = 0
	Nether    Dimension = 1
	End       Dimension = 2
)

func (d Dimension) String() string {
	switch d {
	case Nether:
		return "nether"
	case End:
		return "end"
	}
	return "overworld"
}

// Bounds of a dimension in sections (16 blocks).
type Bounds struct {
	MinSection int // inclusive
	MaxSection int // exclusive
}

// Height returns the number of sections.
func (b Bounds) Height() int { return b.MaxSection - b.MinSection }

// Pos is an absolute block position.
type Pos struct{ X, Y, Z int32 }

// Section holds 16x16x16 palette indices in YZX order: (y<<8)|(z<<4)|x.
type Section struct {
	Blocks [4096]uint32
}

// BiomeSection holds 4x4x4 biome palette indices in YZX order: (y<<4)|(z<<2)|x.
type BiomeSection [64]uint16

// Chunk is a 16xHx16 column of blocks for one edition.
type Chunk struct {
	X, Z   int32
	Bounds Bounds
	// Sections[i] covers section Bounds.MinSection+i; nil means all air
	// (palette index 0 must be air when nil sections are used).
	Sections []*Section
	// Palette entries are version specific blocks; Extra holds layers.
	Palette []*translate.Block
	// BlockEntities keyed by absolute position (id/x/y/z stripped from NBT).
	BlockEntities map[Pos]*translate.BlockEntity
	// Biomes per section (same indexing as Sections); nil = use BiomePalette[0].
	Biomes       []*BiomeSection
	BiomePalette []string
	// Entities in the edition's native NBT form.
	Entities []nbt.Compound
	// Ticks etc. are dropped. Misc carries format specific values.
	Misc map[string]any
}

// NewChunk creates an empty chunk.
func NewChunk(x, z int32, b Bounds) *Chunk {
	return &Chunk{
		X: x, Z: z, Bounds: b,
		Sections:      make([]*Section, b.Height()),
		Biomes:        make([]*BiomeSection, b.Height()),
		BlockEntities: map[Pos]*translate.BlockEntity{},
	}
}

// PaletteIndex returns the palette index at local coordinates (lx, y, lz) with
// absolute y, or -1 if y is outside the chunk.
func (c *Chunk) PaletteIndex(lx int, y int, lz int) int {
	si := (y >> 4) - c.Bounds.MinSection
	if si < 0 || si >= len(c.Sections) {
		return -1
	}
	s := c.Sections[si]
	if s == nil {
		return 0
	}
	return int(s.Blocks[(y&15)<<8|lz<<4|lx])
}

// sectionPool is a free list of sections. Unlike sync.Pool it survives
// garbage collections, so the 16 KiB arrays are allocated only once.
var sectionPool = make(chan *Section, 8192)

// NewSection returns a recycled section. Its contents are undefined;
// callers must overwrite all 4096 entries.
func NewSection() *Section {
	select {
	case s := <-sectionPool:
		return s
	default:
		return new(Section)
	}
}

// Release returns the chunk's sections to the free list. The chunk must not
// be used afterwards.
func (c *Chunk) Release() {
	for i, s := range c.Sections {
		if s != nil {
			select {
			case sectionPool <- s:
			default:
			}
			c.Sections[i] = nil
		}
	}
}

// SectionCache is a per-goroutine free list of sections (no locking).
// A nil cache falls back to the shared free list.
type SectionCache struct {
	free []*Section
}

// Get returns a section whose contents are undefined.
func (c *SectionCache) Get() *Section {
	if c == nil {
		return NewSection()
	}
	if n := len(c.free); n > 0 {
		s := c.free[n-1]
		c.free = c.free[:n-1]
		return s
	}
	return new(Section)
}

// ReleaseTo returns the chunk's sections to the cache.
func (ch *Chunk) ReleaseTo(c *SectionCache) {
	if c == nil {
		ch.Release()
		return
	}
	for i, s := range ch.Sections {
		if s != nil {
			c.free = append(c.free, s)
			ch.Sections[i] = nil
		}
	}
}
