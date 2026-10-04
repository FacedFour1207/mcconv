package convert

import (
	"strconv"
	"strings"
	"sync"

	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// Bedrock does not store the leaf "distance" property. Java leaves with
// distance 7 that are not persistent decay, so after converting to Java the
// distance must be computed from the surrounding logs like LeavesBlock does.

const (
	clsOther = 0
	clsLog   = 1
	clsLeaf  = 2
)

func isJavaLog(name string) bool {
	n := strings.TrimPrefix(name, "minecraft:")
	if n == "mushroom_stem" {
		return false
	}
	return strings.HasSuffix(n, "_log") || strings.HasSuffix(n, "_wood") ||
		strings.HasSuffix(n, "_stem") || strings.HasSuffix(n, "_hyphae")
}

func isJavaLeaves(name string) bool { return strings.HasSuffix(name, "_leaves") }

func classOf(name string) int8 {
	switch {
	case isJavaLog(name):
		return clsLog
	case isJavaLeaves(name):
		return clsLeaf
	}
	return clsOther
}

// leafFixer computes leaf distances for Java output chunks.
type leafFixer struct {
	bc      *blockConverter
	inClass sync.Map // input block key -> int8 class (after translation)
}

func (lf *leafFixer) classOfInput(b *translate.Block) int8 {
	k := b.Key()
	if v, ok := lf.inClass.Load(k); ok {
		return v.(int8)
	}
	c := int8(clsOther)
	if e := lf.bc.lookup(b); e != nil && e.out != nil {
		c = classOf(e.out.Name())
	}
	lf.inClass.Store(k, c)
	return c
}

// leafReach is how far (in blocks) a log keeps leaves alive.
const leafReach = 6

type leafScratch struct {
	grid  []int8
	queue []int32
}

var leafScratchPool = sync.Pool{New: func() any { return new(leafScratch) }}

// fix updates the distance of all leaves in out (a Java chunk).
//
// Distances are computed inside the chunk (BFS from logs, like
// LeavesBlock). Leaves that are not reached but lie within 6 blocks of the
// chunk border may be held by a log in the neighbouring chunk; they get
// distance 6 instead of loading the neighbour: they do not decay, and as soon
// as a neighbour changes the game recomputes the real value (a group of such
// leaves cannot keep itself alive, since 6+1 = 7).
func (lf *leafFixer) fix(out *world.Chunk, _ NeighborFunc) {
	cls := make([]int8, len(out.Palette))
	hasLeaves := false
	for i, b := range out.Palette {
		cls[i] = classOf(b.Name())
		if cls[i] == clsLeaf {
			hasLeaves = true
		}
	}
	if !hasLeaves {
		return
	}
	sc := leafScratchPool.Get().(*leafScratch)
	defer leafScratchPool.Put(sc)
	const W = 16
	H := out.Bounds.Height() * 16
	n := W * W * H
	if cap(sc.grid) < n {
		sc.grid = make([]int8, n)
	}
	grid := sc.grid[:n]
	// grid index = y<<8 | z<<4 | x (same as world.Section within a section)
	queue := sc.queue[:0]
	for si, sec := range out.Sections {
		base := si << 12
		if sec == nil {
			for i := 0; i < 4096; i++ {
				grid[base+i] = -1
			}
			continue
		}
		for p, v := range sec.Blocks {
			switch cls[v] {
			case clsLog:
				grid[base+p] = 0
				queue = append(queue, int32(base+p))
			case clsLeaf:
				grid[base+p] = 7
			default:
				grid[base+p] = -1
			}
		}
	}
	for qi := 0; qi < len(queue); qi++ {
		i := int(queue[qi])
		d := grid[i]
		if d >= leafReach {
			continue
		}
		x, z := i&15, (i>>4)&15
		try := func(j int) {
			if grid[j] > d+1 {
				grid[j] = d + 1
				queue = append(queue, int32(j))
			}
		}
		if x > 0 {
			try(i - 1)
		}
		if x < 15 {
			try(i + 1)
		}
		if z > 0 {
			try(i - 16)
		}
		if z < 15 {
			try(i + 16)
		}
		if i >= 256 {
			try(i - 256)
		}
		if i+256 < n {
			try(i + 256)
		}
	}
	sc.queue = queue
	// write distances back
	keyIndex := map[*translate.Block]uint32{}
	variant := map[[2]uint32]uint32{} // (palette idx, distance) -> palette idx
	for si, sec := range out.Sections {
		if sec == nil {
			continue
		}
		base := si << 12
		for p, v := range sec.Blocks {
			if cls[v] != clsLeaf {
				continue
			}
			d := grid[base+p]
			if d == 7 {
				x, z := p&15, (p>>4)&15
				if x < leafReach || x >= 16-leafReach || z < leafReach || z >= 16-leafReach {
					d = 6
				}
			}
			if d < 1 {
				d = 7
			}
			k := [2]uint32{v, uint32(d)}
			nv, ok := variant[k]
			if !ok {
				b := out.Palette[v]
				if cur, _ := b.Props["distance"].(string); cur == strconv.Itoa(int(d)) {
					nv = v
				} else {
					nb := lf.withDistance(b, int(d))
					if i, ok := keyIndex[nb]; ok {
						nv = i
					} else {
						nv = uint32(len(out.Palette))
						out.Palette = append(out.Palette, nb)
						cls = append(cls, clsLeaf)
						keyIndex[nb] = nv
					}
				}
				variant[k] = nv
			}
			sec.Blocks[p] = nv
		}
	}
}

// withDistance returns the shared leaf block with the given distance.
func (lf *leafFixer) withDistance(b *translate.Block, d int) *translate.Block {
	nb := b.Clone()
	nb.Props["distance"] = strconv.Itoa(d)
	return lf.bc.intern(nb)
}
