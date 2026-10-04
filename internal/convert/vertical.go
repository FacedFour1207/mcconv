package convert

import (
	"strconv"

	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// Some Java block states depend on the block directly above or below and
// Bedrock does not store them: snowy grass, and the stems of kelp and vines
// (one block in Bedrock, head and stem in Java). They are fixed in one pass
// over the converted chunk, which is much cheaper than a per-position rule
// for something as common as grass.

const (
	vNone = iota
	vSnowyBase
	vSnow
	vKelp
	vTwisting
	vWeeping
	vCaveVines
)

func verticalKind(name string) uint8 {
	switch name {
	case "minecraft:grass_block", "minecraft:podzol", "minecraft:mycelium":
		return vSnowyBase
	case "minecraft:snow", "minecraft:snow_block", "minecraft:powder_snow":
		return vSnow
	case "minecraft:kelp", "minecraft:kelp_plant":
		return vKelp
	case "minecraft:twisting_vines", "minecraft:twisting_vines_plant":
		return vTwisting
	case "minecraft:weeping_vines", "minecraft:weeping_vines_plant":
		return vWeeping
	case "minecraft:cave_vines", "minecraft:cave_vines_plant":
		return vCaveVines
	}
	return vNone
}

// fixVertical updates the vertical neighbour dependent properties of a
// converted Java chunk in place.
func (bc *blockConverter) fixVertical(ch *world.Chunk) {
	t := bc.t
	kind := make([]uint8, len(ch.Palette))
	any := false
	for i, b := range ch.Palette {
		kind[i] = verticalKind(b.Name())
		if kind[i] != vNone && kind[i] != vSnow {
			any = true
		}
	}
	if !any {
		return
	}
	at := func(si, p int) uint8 { // kind at section si, index p (y<<8|z<<4|x)
		if si < 0 || si >= len(ch.Sections) || ch.Sections[si] == nil {
			return vNone
		}
		v := ch.Sections[si].Blocks[p]
		if int(v) >= len(kind) {
			return vNone
		}
		return kind[v]
	}
	neighbour := func(si, p, dy int) uint8 {
		y := p>>8 + dy
		switch {
		case y > 15:
			return at(si+1, p&0xFF)
		case y < 0:
			return at(si-1, 15<<8|p&0xFF)
		}
		return at(si, y<<8|p&0xFF)
	}
	index := map[*translate.Block]uint32{}
	variant := func(v uint32, nb *translate.Block) uint32 {
		if i, ok := index[nb]; ok {
			return i
		}
		i := uint32(len(ch.Palette))
		index[nb] = i
		ch.Palette = append(ch.Palette, nb)
		kind = append(kind, kind[v]) // later neighbour checks see the same kind
		return i
	}
	// plant returns the head or stem block of a growing plant
	plant := func(b *translate.Block, continues bool, head, stem string) *translate.Block {
		name := head
		if continues {
			name = stem
		}
		if b.Name() == name {
			return b
		}
		nb, ok := t.JavaDefault(name)
		if !ok {
			return b
		}
		if berries, ok := b.Props["berries"].(string); ok {
			nb = t.WithJavaProp(nb, "berries", berries)
		}
		return nb
	}
	// results per (palette index, neighbour matches)
	memo := map[[2]uint32]uint32{}
	for si, sec := range ch.Sections {
		if sec == nil {
			continue
		}
		for p, v := range sec.Blocks {
			if int(v) >= len(kind) || kind[v] == vNone || kind[v] == vSnow {
				continue
			}
			var match bool
			switch kind[v] {
			case vSnowyBase:
				match = neighbour(si, p, 1) == vSnow
			case vKelp, vTwisting: // grow up
				match = neighbour(si, p, 1) == kind[v]
			case vWeeping, vCaveVines: // grow down
				match = neighbour(si, p, -1) == kind[v]
			}
			mk := [2]uint32{v, 0}
			if match {
				mk[1] = 1
			}
			nv, ok := memo[mk]
			if !ok {
				b := ch.Palette[v]
				nb := b
				switch kind[v] {
				case vSnowyBase:
					nb = t.WithJavaProp(b, "snowy", strconv.FormatBool(match))
				case vKelp:
					nb = plant(b, match, "minecraft:kelp", "minecraft:kelp_plant")
				case vTwisting:
					nb = plant(b, match, "minecraft:twisting_vines", "minecraft:twisting_vines_plant")
				case vWeeping:
					nb = plant(b, match, "minecraft:weeping_vines", "minecraft:weeping_vines_plant")
				case vCaveVines:
					nb = plant(b, match, "minecraft:cave_vines", "minecraft:cave_vines_plant")
				}
				nv = v
				if nb != b {
					nv = variant(v, nb)
				}
				memo[mk] = nv
			}
			sec.Blocks[p] = nv
		}
	}
}
