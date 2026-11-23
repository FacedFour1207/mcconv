package gen

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"mcconv/internal/nbt"
	"mcconv/internal/translate"
)

// BedrockBlocks reads a GeyserMC block palette (gzip compressed big endian
// NBT, {blocks: [{name, states}, ...]}) into the list of Bedrock blocks.
// The values of each state keep the palette's order, so the first value is
// the one of the block's first state.
func BedrockBlocks(gz []byte) ([]translate.BedrockBlockDef, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	_, root, _, err := nbt.Decode(data, nbt.BigEndian)
	if err != nil {
		return nil, fmt.Errorf("block palette: %w", err)
	}
	rc, _ := root.(nbt.Compound)
	list, ok := rc.List("blocks")
	if !ok || len(list.Items) == 0 {
		return nil, fmt.Errorf("block palette: no blocks")
	}
	type stateVals struct {
		vals []string
		seen map[string]bool
	}
	blocks := map[string]map[string]*stateVals{}
	var order []string
	for _, it := range list.Items {
		e, _ := it.(nbt.Compound)
		name, ok := e.String("name")
		if !ok {
			continue
		}
		states := blocks[name]
		if states == nil {
			states = map[string]*stateVals{}
			blocks[name] = states
			order = append(order, name)
		}
		st, _ := e.Compound("states")
		for k, v := range st {
			sv := states[k]
			if sv == nil {
				sv = &stateVals{seen: map[string]bool{}}
				states[k] = sv
			}
			s := nbt.ToSNBT(v)
			if !sv.seen[s] {
				sv.seen[s] = true
				sv.vals = append(sv.vals, s)
			}
		}
	}
	sort.Strings(order)
	out := make([]translate.BedrockBlockDef, 0, len(order))
	for _, name := range order {
		d := translate.BedrockBlockDef{Name: name}
		var keys []string
		for k := range blocks[name] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			d.States = append(d.States, translate.BedrockState{Name: k, Values: blocks[name][k].vals})
		}
		out = append(out, d)
	}
	return out, nil
}

// AddBedrockBlocks adds the Bedrock block list of a GeyserMC block palette
// to an existing table file.
func AddBedrockBlocks(tables, palette []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(tables))
	if err != nil {
		return nil, err
	}
	var tf translate.TableFile
	if err := json.NewDecoder(zr).Decode(&tf); err != nil {
		return nil, err
	}
	if tf.BedrockBlocks, err = BedrockBlocks(palette); err != nil {
		return nil, err
	}
	has := false
	for _, s := range tf.Sources {
		has = has || s == paletteSource
	}
	if !has {
		tf.Sources = append(tf.Sources, paletteSource)
	}
	return encode(&tf)
}
