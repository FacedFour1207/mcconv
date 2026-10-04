// Package convert implements the Java <-> Bedrock conversion pipeline.
package convert

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"

	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// blockConverter translates chunks in one direction. Block states are
// translated once (results are cached by the shared input pointers); the
// few blocks whose result depends on their block entity or neighbours run a
// rule per position, cached by everything the rule looked at.
type blockConverter struct {
	t         *translate.Tables
	toBedrock bool
	be        *beConverter

	ptrCache sync.Map // *translate.Block (shared input) -> *cacheEntry
	cache    sync.Map // input key -> *cacheEntry
	outs     sync.Map // output key -> *translate.Block (interned outputs)
	// perPos caches rule results by their inputs: perPosKey -> *translate.Block;
	// probes: *translate.Block -> *probeInfo
	perPos sync.Map
	probes sync.Map

	// override maps source block ids to destination ids (overrides.json).
	override map[string]string
	// unknown counts source blocks missing from the tables
	// (name -> *atomic.Int64, number of chunk palettes containing it).
	unknown sync.Map

	errOnce sync.Map // error key -> struct{}
	logf    func(string, ...any)

	perPosCount atomic.Int64
	perPosNames sync.Map // debug: block name -> *atomic.Int64
	errorCount  atomic.Int64
}

func newBlockConverter(t *translate.Tables, toBedrock bool, override map[string]string, logf func(string, ...any)) *blockConverter {
	return &blockConverter{t: t, toBedrock: toBedrock, override: override, logf: logf}
}

// perPosKey identifies a rule result by everything it depends on: the
// block, its block entity data and the neighbouring blocks the rule read.
type perPosKey struct {
	block *translate.Block
	nbt   string
	nb    [maxProbes]*translate.Block
	n     uint8
}

const maxProbes = 6

type probe struct{ dx, dy, dz int8 }

// probeInfo records which neighbours a block's rule reads (the same for
// every instance of a block state); ok=false if it is not cacheable.
type probeInfo struct {
	list []probe
	ok   bool
}

type cacheEntry struct {
	out *translate.Block
	// tpl is a block entity every instance of the block gets (Bedrock
	// blocks whose block entity holds what Java keeps in the block state).
	tpl  *translate.BlockEntity
	rule rule // non-nil: the result depends on the position
}

var airBlock = translate.NewBlock("minecraft:air", nil)

func (bc *blockConverter) reportErr(err error) {
	if err == nil {
		return
	}
	bc.errorCount.Add(1)
	msg := err.Error()
	if _, dup := bc.errOnce.LoadOrStore(msg, struct{}{}); !dup && bc.logf != nil {
		bc.logf("warning: %s", msg)
	}
}

// lookup translates a block without positional information. Input blocks
// from the chunk readers are shared pointers, so the pointer cache answers
// almost every call without building a key.
func (bc *blockConverter) lookup(b *translate.Block) *cacheEntry {
	if v, ok := bc.ptrCache.Load(b); ok {
		return v.(*cacheEntry)
	}
	key := b.Key()
	if v, ok := bc.cache.Load(key); ok {
		bc.ptrCache.Store(b, v)
		return v.(*cacheEntry)
	}
	e := bc.translate(b)
	v, _ := bc.cache.LoadOrStore(key, e)
	bc.ptrCache.Store(b, v)
	return v.(*cacheEntry)
}

func (bc *blockConverter) translate(b *translate.Block) *cacheEntry {
	t := bc.t
	if target, ok := bc.override[b.Name()]; ok {
		return &cacheEntry{out: bc.overrideBlock(b, target)}
	}
	if bc.toBedrock {
		out, ok := t.JavaToBedrock(b)
		if !ok {
			bc.countUnknown(b.Name())
			// copied by name; the game fills in default states
			out = translate.NewBlock(b.Name(), nil)
			out.Version = t.BedrockBlockVersion
			if t.IsWaterlogged(b) {
				out.Extra = []*translate.Block{translate.NewBlock("minecraft:water", map[string]any{"liquid_depth": int32(0)})}
			}
			out = bc.intern(out)
		}
		return &cacheEntry{out: out, tpl: bc.bedrockTemplate(b)}
	}
	out, ok := t.BedrockToJava(b)
	if !ok {
		if !silentAir[b.Name()] {
			bc.countUnknown(b.Name())
		}
		out = airBlock
		for _, e := range b.Extra { // keep the water of a waterlogged unknown block
			if j, ok := t.BedrockToJava(e); ok && j.Name() == "minecraft:water" {
				out = j
			}
		}
	}
	return &cacheEntry{out: out, rule: javaRules(out)}
}

// silentAir lists Bedrock blocks that are not blocks in Java Edition and are
// handled elsewhere (item frames become entities).
var silentAir = map[string]bool{"minecraft:frame": true, "minecraft:glow_frame": true}

func (bc *blockConverter) overrideBlock(b *translate.Block, target string) *translate.Block {
	t := bc.t
	if bc.toBedrock {
		if j, ok := t.JavaDefault(target); ok { // a Java id: use its Bedrock form
			out, _ := t.JavaToBedrock(j)
			return out
		}
		out := translate.NewBlock(target, nil)
		out.Version = t.BedrockBlockVersion
		return bc.intern(out)
	}
	if j, ok := t.JavaDefault(target); ok {
		return j
	}
	return bc.intern(translate.NewBlock(target, nil))
}

func (bc *blockConverter) countUnknown(name string) {
	c, _ := bc.unknown.LoadOrStore(name, new(atomic.Int64))
	c.(*atomic.Int64).Add(1)
}

// unknownBlocks returns source blocks missing from the tables, most common first.
func (bc *blockConverter) unknownBlocks() []string {
	type kv struct {
		name string
		n    int64
	}
	var l []kv
	bc.unknown.Range(func(k, v any) bool {
		l = append(l, kv{k.(string), v.(*atomic.Int64).Load()})
		return true
	})
	sort.Slice(l, func(i, j int) bool { return l[i].n > l[j].n || l[i].n == l[j].n && l[i].name < l[j].name })
	out := make([]string, len(l))
	for i, e := range l {
		out[i] = fmt.Sprintf("%s (%d)", e.name, e.n)
	}
	return out
}

// intern returns the shared instance of an output block.
func (bc *blockConverter) intern(b *translate.Block) *translate.Block {
	k := b.Key()
	if v, ok := bc.outs.Load(k); ok {
		return v.(*translate.Block)
	}
	v, _ := bc.outs.LoadOrStore(k, b)
	return v.(*translate.Block)
}

// NeighborFunc returns the source chunk at chunk coordinates (cx, cz) or nil.
type NeighborFunc func(cx, cz int32) *world.Chunk

// convertChunk translates a chunk into the destination edition.
//
// The output palette is index-aligned with the input palette (it may contain
// duplicates), so sections without rule blocks or block entity templates are
// shared with the input instead of being rewritten. Use releaseConverted to
// free the sections afterwards.
func (bc *blockConverter) convertChunk(in *world.Chunk, dstBounds world.Bounds, neighbor NeighborFunc, cache *world.SectionCache) *world.Chunk {
	out := world.NewChunk(in.X, in.Z, dstBounds)
	np := len(in.Palette)
	out.Palette = make([]*translate.Block, np, np+4)
	const (
		flagRule = 1
		flagTpl  = 2
	)
	flags := make([]uint8, np)
	entries := make([]*cacheEntry, np)
	special := false
	for i, b := range in.Palette {
		e := bc.lookup(b)
		entries[i] = e
		out.Palette[i] = e.out
		if e.rule != nil {
			flags[i] |= flagRule
			special = true
		}
		if e.tpl != nil {
			flags[i] |= flagTpl
			special = true
		}
	}
	var extra map[*translate.Block]uint32
	add := func(b *translate.Block) uint32 {
		if i, ok := extra[b]; ok {
			return i
		}
		if extra == nil {
			extra = map[*translate.Block]uint32{}
		}
		i := uint32(len(out.Palette))
		extra[b] = i
		out.Palette = append(out.Palette, b)
		return i
	}
	baseX, baseZ := int(in.X)*16, int(in.Z)*16

	// source block at absolute coordinates (neighbouring chunks if needed)
	blockAt := func(x, y, z int) *translate.Block {
		cx, cz := int32(x>>4), int32(z>>4)
		ch := in
		if cx != in.X || cz != in.Z {
			if neighbor == nil {
				return nil
			}
			if ch = neighbor(cx, cz); ch == nil {
				return nil
			}
		}
		pi := ch.PaletteIndex(x&15, y, z&15)
		if pi < 0 || pi >= len(ch.Palette) {
			return nil
		}
		return ch.Palette[pi]
	}

	for si := 0; si < dstBounds.Height(); si++ {
		sy := dstBounds.MinSection + si
		srcSi := sy - in.Bounds.MinSection
		if srcSi < 0 || srcSi >= len(in.Sections) {
			continue
		}
		sec := in.Sections[srcSi]
		if sec == nil {
			continue // all palette index 0 (air)
		}
		need := false
		if special {
			for _, v := range sec.Blocks {
				if flags[v] != 0 {
					need = true
					break
				}
			}
		}
		if !need {
			out.Sections[si] = sec // shared with the input
			continue
		}
		os := cache.Get()
		os.Blocks = sec.Blocks
		for p, v := range sec.Blocks {
			f := flags[v]
			if f == 0 {
				continue
			}
			x, y, z := baseX+p&15, sy*16+p>>8, baseZ+(p>>4)&15
			pos := world.Pos{X: int32(x), Y: int32(y), Z: int32(z)}
			if f&flagTpl != 0 {
				out.BlockEntities[pos] = entries[v].tpl.Clone()
			}
			if f&flagRule != 0 {
				if nb := bc.runRule(entries[v], in.Palette[v], in.BlockEntities[pos], x, y, z, blockAt); nb != entries[v].out {
					os.Blocks[p] = add(nb)
				}
			}
		}
		out.Sections[si] = os
	}

	// block entities
	for pos, be := range in.BlockEntities {
		inBlock := blockAt(int(pos.X), int(pos.Y), int(pos.Z))
		outBlock := airBlock
		if pi := out.PaletteIndex(int(pos.X&15), int(pos.Y), int(pos.Z&15)); pi >= 0 && pi < len(out.Palette) {
			outBlock = out.Palette[pi]
		}
		if inBlock == nil {
			continue
		}
		if obe := bc.be.convert(inBlock, outBlock, be, pos); obe != nil {
			if tpl, ok := out.BlockEntities[pos]; ok {
				for k, v := range tpl.NBT { // template fields win
					obe.NBT[k] = v
				}
			}
			out.BlockEntities[pos] = obe
		}
	}

	// biomes
	if len(in.BiomePalette) > 0 {
		out.BiomePalette = make([]string, len(in.BiomePalette))
		for i, name := range in.BiomePalette {
			if bc.toBedrock {
				out.BiomePalette[i] = bc.t.BiomeJavaToBedrock(name)
			} else {
				out.BiomePalette[i] = bc.t.BiomeBedrockToJava(name)
			}
		}
		for si := 0; si < dstBounds.Height(); si++ {
			srcSi := dstBounds.MinSection + si - in.Bounds.MinSection
			if srcSi >= 0 && srcSi < len(in.Biomes) {
				out.Biomes[si] = in.Biomes[srcSi]
			}
		}
	}
	return out
}

// runRule evaluates a position dependent rule, reusing earlier results with
// the same inputs.
func (bc *blockConverter) runRule(e *cacheEntry, inBlock *translate.Block, inBE *translate.BlockEntity, x, y, z int, blockAt func(x, y, z int) *translate.Block) *translate.Block {
	bc.perPosCount.Add(1)
	if debugPerPos {
		c, _ := bc.perPosNames.LoadOrStore(inBlock.Name(), new(atomic.Int64))
		c.(*atomic.Int64).Add(1)
	}
	pk := perPosKey{block: inBlock}
	var beNBT nbt.Compound
	if inBE != nil {
		beNBT = inBE.NBT
		pk.nbt = nbt.ToSNBT(inBE.NBT)
	}
	cacheable := len(pk.nbt) <= 256
	if cacheable {
		if v, ok := bc.probes.Load(inBlock); ok {
			info := v.(*probeInfo)
			if !info.ok {
				cacheable = false
			} else {
				for i, o := range info.list {
					pk.nb[i] = blockAt(x+int(o.dx), y+int(o.dy), z+int(o.dz))
				}
				pk.n = uint8(len(info.list))
				if c, ok := bc.perPos.Load(pk); ok {
					return c.(*translate.Block)
				}
			}
		}
	}
	r := &ruleIn{bc: bc, in: inBlock, be: beNBT, base: e.out, x: x, y: y, z: z}
	r.at = func(dx, dy, dz int) *translate.Block {
		b := blockAt(x+dx, y+dy, z+dz)
		if len(r.rec) < maxProbes {
			r.recBlocks[len(r.rec)] = b
			r.rec = append(r.rec, probe{int8(dx), int8(dy), int8(dz)})
		} else {
			r.overflow = true
		}
		if b == nil {
			return airBlock
		}
		return b
	}
	out := e.rule(r)
	if out == nil {
		out = e.out
	}
	if cacheable {
		ok := !r.locDep && !r.overflow
		if v, loaded := bc.probes.LoadOrStore(inBlock, &probeInfo{list: r.rec, ok: ok}); loaded {
			info := v.(*probeInfo)
			if info.ok && (!ok || !sameProbes(info.list, r.rec)) {
				bc.probes.Store(inBlock, &probeInfo{ok: false})
				ok = false
			}
			ok = ok && info.ok
		}
		if ok {
			key := perPosKey{block: inBlock, nbt: pk.nbt, nb: r.recBlocks, n: uint8(len(r.rec))}
			bc.perPos.Store(key, out)
		}
	}
	return out
}

// releaseConverted returns the sections of a converted chunk and its input to
// the cache without releasing shared sections twice.
func releaseConverted(in, out *world.Chunk, cache *world.SectionCache) {
	shared := map[*world.Section]bool{}
	for _, s := range in.Sections {
		if s != nil {
			shared[s] = true
		}
	}
	for i, s := range out.Sections {
		if s != nil && shared[s] {
			out.Sections[i] = nil
		}
	}
	out.ReleaseTo(cache)
	in.ReleaseTo(cache)
}

var debugPerPos = os.Getenv("MCCONV_DEBUG_PERPOS") != ""

func (bc *blockConverter) dumpPerPos(logf func(string, ...any)) {
	if !debugPerPos {
		return
	}
	bc.perPosNames.Range(func(k, v any) bool {
		logf("per-position: %s %d", k, v.(*atomic.Int64).Load())
		return true
	})
}

func sameProbes(a, b []probe) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// convertName translates a block id with default properties (superflat
// layers).
func (bc *blockConverter) convertName(name string) string {
	if bc.toBedrock {
		if j, ok := bc.t.JavaDefault(withNS(name)); ok {
			if out, ok := bc.t.JavaToBedrock(j); ok {
				return out.Name()
			}
		}
		return name
	}
	if out, ok := bc.t.BedrockToJava(translate.NewBlock(withNS(name), nil)); ok {
		return out.Name()
	}
	return name
}
