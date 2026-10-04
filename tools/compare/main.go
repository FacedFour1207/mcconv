// Command compare diffs the blocks of two worlds of the same edition (used to
// check conversion results against a reference world).
package main

import (
	"fmt"
	"os"
	"sort"

	"mcconv/internal/anvil"
	"mcconv/internal/bedrock"
	"mcconv/internal/convert"
	"mcconv/internal/java"
	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

func main() {
	a := load(os.Args[1])
	b := load(os.Args[2])
	if len(os.Args) > 3 {
		only := load(os.Args[3])
		for k := range a {
			if only[k] == nil {
				delete(a, k)
			}
		}
	}
	diffs := map[string]int{}
	beDiff := map[string]int{}
	total, same, chunks, missing := 0, 0, 0, 0
	for k, ca := range a {
		cb := b[k]
		if cb == nil {
			missing++
			continue
		}
		chunks++
		for y := ca.Bounds.MinSection * 16; y < ca.Bounds.MaxSection*16; y++ {
			for z := 0; z < 16; z++ {
				for x := 0; x < 16; x++ {
					ia, ib := ca.PaletteIndex(x, y, z), cb.PaletteIndex(x, y, z)
					if ia < 0 || ib < 0 {
						continue
					}
					ka, kb := ca.Palette[ia].Key(), cb.Palette[ib].Key()
					total++
					if ka == kb {
						same++
					} else {
						diffs[ka+"  <=>  "+kb]++
					}
				}
			}
		}
		for pos, be := range ca.BlockEntities {
			other := cb.BlockEntities[pos]
			if other == nil {
				beDiff["missing in B: "+be.BaseName]++
				continue
			}
			sa, sb := nbt.ToSNBT(be.NBT), nbt.ToSNBT(other.NBT)
			if sa != sb || be.BaseName != other.BaseName {
				beDiff[be.BaseName+": "+sa+"  <=>  "+other.BaseName+": "+sb]++
			}
		}
		for pos, be := range cb.BlockEntities {
			if ca.BlockEntities[pos] == nil {
				beDiff["missing in A: "+be.BaseName]++
			}
		}
	}
	fmt.Printf("chunks compared %d (missing in B %d), blocks %d, identical %.4f%%\n", chunks, missing, total, float64(same)*100/float64(max(total, 1)))
	printTop("block diffs", diffs, 60)
	printTop("block entity diffs", beDiff, 30)
}

func load(dir string) map[[2]int32]*world.Chunk {
	data, _ := translate.Load()
	out := map[[2]int32]*world.Chunk{}
	kind, err := convert.Detect(dir)
	check(err)
	if kind == "bedrock" {
		w, err := bedrock.Open(dir)
		check(err)
		codec := &bedrock.Codec{Biomes: data}
		check(w.ForEachChunk(func(rc *bedrock.RawChunk) error {
			if _, ok := rc.Data[bedrock.TagVersion]; !ok || rc.Dim != world.Overworld {
				return nil
			}
			ch, err := codec.DecodeChunk(rc)
			check(err)
			out[[2]int32{rc.X, rc.Z}] = ch
			return nil
		}))
		return out
	}
	w, err := java.Open(dir)
	check(err)
	codec := &java.Codec{}
	files, err := anvil.ListRegions(w.RegionDir(world.Overworld))
	check(err)
	for _, f := range files {
		r, err := anvil.OpenRegion(f)
		check(err)
		for _, c := range r.Chunks() {
			raw, err := r.ReadRaw(c[0], c[1])
			check(err)
			root, _, err := nbt.DecodeCompound(raw, nbt.BigEndian)
			check(err)
			ch, _, err := codec.DecodeChunk(root, java.Bounds(world.Overworld))
			check(err)
			out[[2]int32{ch.X, ch.Z}] = ch
		}
	}
	return out
}

func printTop(title string, m map[string]int, n int) {
	type kv struct {
		k string
		v int
	}
	var l []kv
	for k, v := range m {
		l = append(l, kv{k, v})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].v > l[j].v })
	fmt.Printf("== %s (%d kinds)\n", title, len(l))
	for i, e := range l {
		if i >= n {
			break
		}
		s := e.k
		if len(s) > 400 {
			s = s[:400]
		}
		fmt.Printf("%8d  %s\n", e.v, s)
	}
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
