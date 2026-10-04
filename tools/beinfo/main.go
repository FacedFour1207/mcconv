// Command beinfo counts block entity ids (and their blocks) in a Bedrock world.
package main

import (
	"fmt"
	"os"
	"sort"

	"mcconv/internal/bedrock"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

func main() {
	w, err := bedrock.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	data, _ := translate.Load()
	codec := &bedrock.Codec{Biomes: data}
	counts := map[string]int{}
	n := 0
	w.ForEachChunk(func(rc *bedrock.RawChunk) error {
		if n > 20000 {
			return nil
		}
		if _, ok := rc.Data[bedrock.TagBlockEntity]; !ok {
			return nil
		}
		n++
		ch, err := codec.DecodeChunk(rc)
		if err != nil {
			return nil
		}
		for pos, be := range ch.BlockEntities {
			pi := ch.PaletteIndex(int(pos.X&15), int(pos.Y), int(pos.Z&15))
			name := "?"
			if pi >= 0 {
				name = ch.Palette[pi].Key()
			}
			counts[be.BaseName+" @ "+name]++
		}
		_ = world.Pos{}
		return nil
	})
	type kv struct {
		k string
		v int
	}
	var l []kv
	for k, v := range counts {
		l = append(l, kv{k, v})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].v > l[j].v })
	for i, e := range l {
		if i > 25 {
			break
		}
		fmt.Println(e.v, e.k)
	}
}
