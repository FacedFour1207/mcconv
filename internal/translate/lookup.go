package translate

import (
	"strings"
	"sync"
)

// renamedJavaBlocks maps block ids of older Java versions to their current
// name (only used when the old name is unknown to the tables).
var renamedJavaBlocks = map[string]string{
	"minecraft:grass":      "minecraft:short_grass",
	"minecraft:chain":      "minecraft:iron_chain",
	"minecraft:grass_path": "minecraft:dirt_path",
}

var liquidAliases = map[string]string{
	"minecraft:water": "minecraft:flowing_water", "minecraft:flowing_water": "minecraft:water",
	"minecraft:lava": "minecraft:flowing_lava", "minecraft:flowing_lava": "minecraft:lava",
}

// alwaysWaterlogged are Java blocks that are implicitly in water.
var alwaysWaterlogged = map[string]bool{
	"minecraft:seagrass": true, "minecraft:tall_seagrass": true, "minecraft:kelp": true,
	"minecraft:kelp_plant": true, "minecraft:bubble_column": true,
}

var waterCache sync.Map // *Block (Bedrock) -> *Block with a water layer

// HasJavaBlock reports whether the tables know a Java block id.
func (t *Tables) HasJavaBlock(name string) bool {
	_, ok := t.byName[name]
	return ok
}

// JavaStateID returns the state id of a Java block. Unknown or missing
// property values take the block's default value.
func (t *Tables) JavaStateID(b *Block) (int32, bool) {
	jb := t.byName[b.Name()]
	if jb == nil {
		if n, ok := renamedJavaBlocks[b.Name()]; ok {
			jb = t.byName[n]
		}
		if jb == nil {
			return 0, false
		}
	}
	id := jb.first
	for i, p := range jb.Props {
		vi := -1
		if v, ok := b.Props[p.Name]; ok {
			s, _ := v.(string)
			for j, pv := range p.Values {
				if pv == s {
					vi = j
					break
				}
			}
		}
		if vi < 0 {
			vi = indexOf(p.Values, jb.defaultVal[i])
		}
		id += int32(vi) * jb.strides[i]
	}
	return id, true
}

// JavaBlock returns the (shared) Java block of a state id.
func (t *Tables) JavaBlock(id int32) *Block {
	cache := t.javaState
	if b := cache[id].Load(); b != nil {
		return b
	}
	jb := &t.blocks[t.stateOf[id]]
	props := make(map[string]any, len(jb.Props))
	for i, val := range jb.values(id - jb.first) {
		props[jb.Props[i].Name] = val
	}
	b := NewBlock(jb.Name, props)
	if !cache[id].CompareAndSwap(nil, b) {
		return cache[id].Load()
	}
	return b
}

// JavaDefault returns the default state of a Java block.
func (t *Tables) JavaDefault(name string) (*Block, bool) {
	jb := t.byName[name]
	if jb == nil {
		return nil, false
	}
	return t.JavaBlock(jb.first + int32(jb.Default)), true
}

// WithJavaProp returns the state of the same Java block with one property
// changed (b itself if the property or value is unknown).
func (t *Tables) WithJavaProp(b *Block, prop, val string) *Block {
	id, ok := t.JavaStateID(b)
	if !ok {
		return b
	}
	jb := &t.blocks[t.stateOf[id]]
	for i, p := range jb.Props {
		if p.Name != prop {
			continue
		}
		vi := indexOf(p.Values, val)
		if vi < 0 {
			return b
		}
		cur := (id - jb.first) / jb.strides[i] % int32(len(p.Values))
		return t.JavaBlock(id + (int32(vi)-cur)*jb.strides[i])
	}
	return b
}

// JavaRename returns another Java block with the same properties where they
// exist (e.g. red_bed -> blue_bed); ok is false if name is unknown.
func (t *Tables) JavaRename(b *Block, name string) (*Block, bool) {
	if t.byName[name] == nil {
		return b, false
	}
	nb := &Block{Namespace: "minecraft", BaseName: strings.TrimPrefix(name, "minecraft:"), Props: b.Props}
	if i := strings.IndexByte(name, ':'); i >= 0 {
		nb.Namespace, nb.BaseName = name[:i], name[i+1:]
	}
	id, _ := t.JavaStateID(nb)
	return t.JavaBlock(id), true
}

// IsWaterlogged reports whether a Java block contains water.
func (t *Tables) IsWaterlogged(b *Block) bool {
	if alwaysWaterlogged[b.Name()] {
		return true
	}
	s, _ := b.Props["waterlogged"].(string)
	return s == "true"
}

// JavaToBedrock converts a Java block state. Waterlogged blocks get a water
// layer.
func (t *Tables) JavaToBedrock(b *Block) (*Block, bool) {
	id, ok := t.JavaStateID(b)
	if !ok {
		return nil, false
	}
	out := t.toBedrock[id]
	if t.IsWaterlogged(b) && out.Name() != "minecraft:water" {
		out = t.withWater(out)
	}
	return out, true
}

// BedrockOfJavaState returns the Bedrock block of a Java state id.
func (t *Tables) BedrockOfJavaState(id int32) *Block { return t.toBedrock[id] }

func (t *Tables) withWater(b *Block) *Block {
	if v, ok := waterCache.Load(b); ok {
		return v.(*Block)
	}
	w := &Block{Namespace: b.Namespace, BaseName: b.BaseName, Props: b.Props, Version: b.Version, Extra: []*Block{t.waterBlock}}
	v, _ := waterCache.LoadOrStore(b, w)
	return v.(*Block)
}

// BedrockToJava converts a Bedrock block (with its extra layers). Old block
// states are upgraded first. ok is false for blocks without a Java form.
func (t *Tables) BedrockToJava(b *Block) (*Block, bool) {
	name, props := b.Name(), b.Props
	key := stateKey(name, props)
	id, found := t.b2j[key]
	if !found && (b.Version == 0 || b.Version <= t.BedrockBlockVersion) && t.up != nil {
		if n, p, changed := t.up.upgrade(name, props, b.Version); changed {
			name, props = n, p
			key = stateKey(name, props)
			id, found = t.b2j[key]
		}
	}
	if !found {
		// still and flowing liquids are separate ids in Bedrock but worlds
		// mix them up (e.g. "water" with a depth)
		if alias, ok := liquidAliases[name]; ok {
			id, found = t.b2j[stateKey(alias, props)]
		}
	}
	if !found {
		id, found = t.fuzzyBedrock(key, name, props)
		if !found {
			return nil, false
		}
	}
	out := t.JavaBlock(id)
	if jb := &t.blocks[t.stateOf[id]]; jb.waterlog >= 0 {
		// waterlogged exactly when Bedrock stores a water layer
		wet := "false"
		for _, e := range b.Extra {
			if e.BaseName == "water" || e.BaseName == "flowing_water" {
				wet = "true"
				break
			}
		}
		out = t.WithJavaProp(out, "waterlogged", wet)
	}
	return out, true
}

// fuzzyBedrock finds the Java state for a Bedrock state no Java state maps
// to exactly (e.g. Bedrock-only property values): the known state of the
// same block sharing the most property values.
func (t *Tables) fuzzyBedrock(key, name string, props map[string]any) (int32, bool) {
	if v, ok := t.fuzzy.Load(key); ok {
		id := v.(int32)
		return id, id >= 0
	}
	best, bestScore := int32(-1), -1
	for _, c := range t.byBedrock[name] {
		s := 0
		for k, v := range c.states {
			if pv, ok := props[k]; ok && sameValue(pv, v) {
				s++
			}
		}
		if s > bestScore {
			best, bestScore = c.java, s
		}
	}
	t.fuzzy.Store(key, best)
	return best, best >= 0
}

// HasBedrockBlock reports whether some Java block converts to the Bedrock
// block id.
func (t *Tables) HasBedrockBlock(name string) bool {
	_, ok := t.byBedrock[name]
	return ok
}

func stateKey(name string, props map[string]any) string {
	return (&Block{Namespace: "", BaseName: name, Props: props}).Key()[1:] // drop the empty namespace's ':'
}

func sameValue(a, b any) bool {
	switch x := a.(type) {
	case int8:
		switch y := b.(type) {
		case int8:
			return x == y
		case int32:
			return int32(x) == y
		}
	case int32:
		switch y := b.(type) {
		case int32:
			return x == y
		case int8:
			return x == int32(y)
		}
	case string:
		y, ok := b.(string)
		return ok && x == y
	}
	return false
}

func indexOf(l []string, s string) int {
	for i, v := range l {
		if v == s {
			return i
		}
	}
	return -1
}
