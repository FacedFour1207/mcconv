package convert

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mcconv/internal/anvil"
	"mcconv/internal/bedrock"
	"mcconv/internal/config"
	"mcconv/internal/java"
	"mcconv/internal/leveldb"
	"mcconv/internal/nbt"
	"mcconv/internal/translate"
	"mcconv/internal/world"
)

// Options configure a conversion.
type Options struct {
	Workers           int
	Dimensions        []world.Dimension
	IncludeUnfinished bool // also convert chunks that are not fully generated
	CompressionLevel  int  // 0..9 (Bedrock tables / Java zlib)
	Logf              func(string, ...any)
	Progress          func(done, total int64)
	Verbose           bool // log phase timings
	// Config is the data directory configuration (nil = built-in defaults).
	Config *config.Config
	// Cancel, when closed, stops the conversion with ErrCanceled (the
	// output is left incomplete).
	Cancel <-chan struct{}
	// LevelName, if set, renames the output world.
	LevelName string
}

// ErrCanceled is returned when Options.Cancel was closed.
var ErrCanceled = errors.New("conversion canceled")

func (o *Options) canceled() bool {
	select {
	case <-o.Cancel:
		return true
	default:
		return false
	}
}

// Stats summarises a conversion.
type Stats struct {
	Chunks        int64
	Skipped       int64
	Failed        int64
	BlockEntities int64
	Entities      int64
	PerPosBlocks  int64
	TranslateErrs int64
	Duration      time.Duration
}

func (o *Options) normalize() {
	if o.Workers <= 0 {
		o.Workers = runtime.GOMAXPROCS(0)
	}
	if len(o.Dimensions) == 0 {
		o.Dimensions = []world.Dimension{world.Overworld, world.Nether, world.End}
	}
	if o.CompressionLevel < -2 || o.CompressionLevel > 9 {
		o.CompressionLevel = 1
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
}

// Detect reports "java" or "bedrock" for a world directory.
func Detect(dir string) (string, error) {
	if st, err := os.Stat(filepath.Join(dir, "db")); err == nil && st.IsDir() {
		return "bedrock", nil
	}
	if _, err := os.Stat(filepath.Join(dir, "level.dat")); err == nil {
		return "java", nil
	}
	return "", fmt.Errorf("%s does not look like a Minecraft world", dir)
}

func ensureEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) > 0 {
		return fmt.Errorf("output directory %s is not empty", dir)
	}
	return os.MkdirAll(dir, 0o755)
}

// JavaToBedrock converts a Java world into a new Bedrock world.
func JavaToBedrock(srcDir, dstDir string, opts Options) (*Stats, error) {
	opts.normalize()
	start := time.Now()
	data, err := setup(&opts)
	if err != nil {
		return nil, err
	}
	src, err := java.Open(srcDir)
	if err != nil {
		return nil, fmt.Errorf("open java world: %w", err)
	}
	if dv := src.ReadDataVersion(); dv > data.JavaDataVersion {
		warnNewer(opts, fmt.Sprintf("Java DataVersion %d", dv))
	}
	if err := ensureEmptyDir(dstDir); err != nil {
		return nil, err
	}
	info, err := src.ReadLevel()
	if err != nil {
		return nil, fmt.Errorf("read level.dat: %w", err)
	}
	if opts.LevelName != "" {
		info.Name = opts.LevelName
	}
	dst, err := bedrock.Create(dstDir, opts.CompressionLevel)
	if err != nil {
		return nil, err
	}
	bc := newBlockConverter(data, true, blockOverrides(opts.Config, true), opts.Logf)
	items := &itemConverter{t: data, toBedrock: true}
	bc.be = &beConverter{t: data, items: items, toBedrock: true}
	jc := &java.Codec{}
	bcodec := &bedrock.Codec{Biomes: data, BlockVersion: bedrockBlockVersion(opts, data)}
	ec := newEntityConverter(data, items, bc)
	st := &Stats{}

	// Every chunk of the world, sorted in Bedrock key order. Workers convert
	// contiguous runs of this list and stream the records straight into
	// their own table files: no global sort, no temporary files.
	var refs []chunkRef
	var regions []*anvil.Region
	defer func() {
		for _, r := range regions {
			r.Close()
		}
	}()
	type regionFile struct {
		path string
		dim  world.Dimension
		r    *anvil.Region
		er   *anvil.Region
	}
	var rfs []*regionFile
	for _, dim := range opts.Dimensions {
		files, err := anvil.ListRegions(src.RegionDir(dim))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			rfs = append(rfs, &regionFile{path: f, dim: dim})
		}
	}
	// open (map + prefetch) region files in parallel
	var owg sync.WaitGroup
	sem := make(chan struct{}, opts.Workers)
	for _, rf := range rfs {
		owg.Add(1)
		sem <- struct{}{}
		go func() {
			defer owg.Done()
			defer func() { <-sem }()
			r, err := anvil.OpenRegion(rf.path)
			if err != nil {
				opts.Logf("warning: %s: %v", rf.path, err)
				return
			}
			rf.r = r
			if e, err := anvil.OpenRegion(filepath.Join(src.EntitiesDir(rf.dim), filepath.Base(rf.path))); err == nil {
				rf.er = e
			}
		}()
	}
	owg.Wait()
	for _, rf := range rfs {
		if rf.r == nil {
			continue
		}
		regions = append(regions, rf.r)
		if rf.er != nil {
			regions = append(regions, rf.er)
		}
		for _, c := range rf.r.Chunks() {
			refs = append(refs, newChunkRef(rf.r, rf.er, c[0], c[1], rf.dim))
		}
	}
	sort.Slice(refs, func(i, j int) bool { return bytes.Compare(refs[i].key[:], refs[j].key[:]) < 0 })
	phase(opts, "open %d region files", start, len(regions))
	convStart := time.Now()
	total := int64(len(refs))
	var done atomic.Int64
	work := make(chan []chunkRef, opts.Workers*2)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for range opts.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &j2bWorker{jc: jc, fd: jc.NewFastDecoder(), enc: bcodec.NewEncoder(), bc: bc, ec: ec, dst: dst, opts: opts, st: st}
			w.fd.Sections = &w.secs
			for refs := range work {
				if opts.canceled() {
					continue // drain
				}
				stream := dst.ChunkStream()
				for _, t := range refs {
					if opts.canceled() {
						break
					}
					n, err := w.convert(t, stream)
					switch {
					case err == java.ErrSkip:
						atomic.AddInt64(&st.Skipped, 1)
					case err != nil:
						atomic.AddInt64(&st.Failed, 1)
						opts.Logf("warning: chunk %d,%d in %v: %v", t.region.RX*32+t.lx, t.region.RZ*32+t.lz, t.dim, err)
					default:
						atomic.AddInt64(&st.Chunks, 1)
						atomic.AddInt64(&st.BlockEntities, int64(n))
					}
					d := done.Add(1)
					if opts.Progress != nil {
						opts.Progress(d, total)
					}
				}
				if err := stream.Close(); err != nil {
					errOnce.Do(func() { firstErr = err })
				}
			}
		}()
	}
	// guided scheduling: batches shrink as the work runs out, so slow cores
	// (E/LP cores) do not leave the others waiting at the end
	for i := 0; i < len(refs); {
		n := min(max((len(refs)-i)/(opts.Workers*4), 32), 4096)
		end := min(i+n, len(refs))
		work <- refs[i:end]
		i = end
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if opts.canceled() {
		return nil, ErrCanceled
	}
	phase(opts, "convert %d chunks", convStart, len(refs))
	finStart := time.Now()
	if err := dst.Finish(); err != nil {
		return nil, err
	}
	phase(opts, "finish database", finStart)
	// flat world layers need Bedrock block ids
	flatJSON := ""
	if info.Flat {
		var layers []world.FlatLayer
		for _, l := range info.FlatLayers {
			layers = append(layers, world.FlatLayer{Block: bc.convertName(l.Block), Height: l.Height})
		}
		biome := info.FlatBiome
		if biome == "" {
			biome = "minecraft:plains"
		}
		id, _ := data.BedrockBiomeID(data.BiomeJavaToBedrock(biome))
		flatJSON = bedrock.FlatLayersJSON(layers, id)
	}
	if err := dst.WriteLevel(info, flatJSON); err != nil {
		return nil, err
	}
	copyIcon(filepath.Join(srcDir, "icon.png"), filepath.Join(dstDir, "world_icon.jpeg"))
	reportUnknown(opts, bc)
	bc.dumpPerPos(opts.Logf)
	st.PerPosBlocks = bc.perPosCount.Load()
	st.TranslateErrs = bc.errorCount.Load()
	st.Entities = ec.count.Load()
	st.Duration = time.Since(start)
	return st, nil
}

// chunkRef locates a Java chunk and its Bedrock sort key.
type chunkRef struct {
	key    [9]byte // x LE, z LE, dimension order (nether, end, overworld)
	region *anvil.Region
	ents   *anvil.Region
	lx, lz int
	dim    world.Dimension
}

func newChunkRef(r, er *anvil.Region, lx, lz int, dim world.Dimension) chunkRef {
	t := chunkRef{region: r, ents: er, lx: lx, lz: lz, dim: dim}
	binary.LittleEndian.PutUint32(t.key[0:], uint32(int32(r.RX*32+lx)))
	binary.LittleEndian.PutUint32(t.key[4:], uint32(int32(r.RZ*32+lz)))
	switch dim {
	case world.Nether:
		t.key[8] = 1
	case world.End:
		t.key[8] = 2
	default:
		t.key[8] = 3 // overworld record tags (0x2B..) sort after dimension ids
	}
	return t
}

// j2bWorker holds per-goroutine state for Java -> Bedrock conversion.
type j2bWorker struct {
	jc   *java.Codec
	fd   *java.FastDecoder
	enc  *bedrock.Encoder
	bc   *blockConverter
	ec   *entityConverter
	dst  *bedrock.Writer
	opts Options
	st   *Stats
	dec  anvil.Decompressor
	edec anvil.Decompressor
	secs world.SectionCache
}

func (w *j2bWorker) convert(t chunkRef, stream *leveldb.TableStream) (int, error) {
	raw, err := w.dec.ReadChunk(t.region, t.lx, t.lz)
	if err != nil {
		return 0, err
	}
	if raw == nil {
		return 0, java.ErrSkip
	}
	ch, res, err := w.fd.Decode(raw, java.Bounds(t.dim))
	if err != nil {
		return 0, err
	}
	status := res.Status
	if !w.opts.IncludeUnfinished && status != "" && status != "minecraft:full" && status != "full" {
		ch.ReleaseTo(&w.secs)
		return 0, java.ErrSkip
	}
	out := w.bc.convertChunk(ch, bedrock.Bounds(t.dim), nil, &w.secs)
	defer releaseConverted(ch, out, &w.secs)
	var ents *nbt.List
	if t.ents != nil {
		if eraw, err := w.edec.ReadChunk(t.ents, t.lx, t.lz); err == nil && eraw != nil {
			if eroot, _, err := nbt.DecodeCompound(eraw, nbt.BigEndian); err == nil {
				ents, _ = eroot.List("Entities")
			}
		}
	}
	// legacy worlds keep entities in the chunk itself
	if ents == nil {
		ents = res.Entities
	}
	if ents != nil && len(ents.Items) > 0 {
		w.ec.javaToBedrock(ents, out, t.dim, w.dst.PutExtra)
	}
	for _, r := range w.enc.Encode(out, t.dim) {
		if err := stream.Add(r.Key, r.Value); err != nil {
			return 0, err
		}
	}
	return len(out.BlockEntities), nil
}

// copyIcon converts the world icon: Java uses a 64x64 PNG (icon.png),
// Bedrock a JPEG (world_icon.jpeg).
func copyIcon(src, dst string) {
	f, err := os.Open(src)
	if err != nil {
		return
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return
	}
	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer out.Close()
	if strings.HasSuffix(dst, ".png") {
		png.Encode(out, scaleImage(img, 64, 64))
	} else {
		jpeg.Encode(out, img, &jpeg.Options{Quality: 90})
	}
}

// scaleImage resizes with nearest neighbour sampling (icons are small).
func scaleImage(src image.Image, w, h int) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*b.Dx()/w, b.Min.Y+y*b.Dy()/h))
		}
	}
	return dst
}

// BedrockToJava converts a Bedrock world into a new Java world.
func BedrockToJava(srcDir, dstDir string, opts Options) (*Stats, error) {
	opts.normalize()
	start := time.Now()
	data, err := setup(&opts)
	if err != nil {
		return nil, err
	}
	if v := bedrock.LastOpenedWith(srcDir); len(v) >= 3 && newerBedrock(v, data.BedrockVersion) {
		warnNewer(opts, fmt.Sprintf("Bedrock %d.%d.%d", v[0], v[1], v[2]))
	}
	src, err := bedrock.Open(srcDir)
	if err != nil {
		return nil, fmt.Errorf("open bedrock world: %w", err)
	}
	defer src.Close()
	if err := ensureEmptyDir(dstDir); err != nil {
		return nil, err
	}
	info, err := src.ReadLevel()
	if err != nil {
		return nil, fmt.Errorf("read level.dat: %w", err)
	}
	if opts.LevelName != "" {
		info.Name = opts.LevelName
	}
	dst, err := java.Create(dstDir)
	if err != nil {
		return nil, err
	}
	bc := newBlockConverter(data, false, blockOverrides(opts.Config, false), opts.Logf)
	items := &itemConverter{t: data, toBedrock: false}
	bc.be = &beConverter{t: data, items: items, toBedrock: false}
	bcodec := &bedrock.Codec{Biomes: data}
	// chunks are written with the table version, but never newer than the
	// target game (the game upgrades older chunks, it rejects newer ones)
	chunkDV := opts.Config.Versions.JavaChunkDataVersion
	if chunkDV == 0 {
		chunkDV = data.JavaDataVersion
		if t := opts.Config.Versions.JavaDataVersion; t > 0 && t < chunkDV {
			chunkDV = t
		}
	}
	jc := &java.Codec{DataVersion: int32(chunkDV)}
	ec := newEntityConverter(data, items, bc)
	lf := &leafFixer{bc: bc}
	st := &Stats{}
	wantDim := map[world.Dimension]bool{}
	for _, d := range opts.Dimensions {
		wantDim[d] = true
	}
	regions := newRegionSet(func(dim world.Dimension, kind string) string {
		if kind == "entities" {
			return dst.EntitiesDir(dim)
		}
		return dst.RegionDir(dim)
	})
	var neighborLoads atomic.Int64
	neighbors := newChunkCache(256, func(x, z int32, dim world.Dimension) *world.Chunk {
		neighborLoads.Add(1)
		rc, ok, err := src.LoadRawChunk(x, z, dim)
		if err != nil || !ok {
			return nil
		}
		ch, err := bcodec.DecodeChunk(rc)
		if err != nil {
			return nil
		}
		return ch
	})
	if ec.digests, err = src.Digests(); err != nil {
		return nil, err
	}
	var spawnY atomic.Int32
	spawnY.Store(-1 << 20)
	// The database is split into key ranges that workers scan and convert
	// independently (no central reader).
	ranges := src.ChunkRanges(opts.Workers * 16)
	phase(opts, "open database (%d ranges)", start, len(ranges))
	convStart := time.Now()
	work := make(chan [2][]byte, len(ranges))
	for _, r := range ranges {
		work <- r
	}
	close(work)
	var wg sync.WaitGroup
	var done atomic.Int64
	var firstErr error
	var errOnce sync.Once
	for range opts.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &b2jWorker{src: src, dec: bcodec.NewDecoder(), jc: jc, jenc: jc.NewEncoder(), bc: bc, lf: lf, ec: ec,
				regions: regions, neighbors: neighbors, opts: opts, info: info, spawnY: &spawnY}
			w.dec.Sections = &w.secs
			for r := range work {
				if opts.canceled() {
					continue
				}
				err := src.ScanRange(r[0], r[1], func(rc *bedrock.RawChunk) error {
					if opts.canceled() {
						return ErrCanceled
					}
					if !wantDim[rc.Dim] {
						return nil
					}
					n, err := w.convert(rc)
					switch {
					case err == java.ErrSkip:
						atomic.AddInt64(&st.Skipped, 1)
					case err != nil:
						atomic.AddInt64(&st.Failed, 1)
						opts.Logf("warning: chunk %d,%d in %v: %v", rc.X, rc.Z, rc.Dim, err)
					default:
						atomic.AddInt64(&st.Chunks, 1)
						atomic.AddInt64(&st.BlockEntities, int64(n))
					}
					d := done.Add(1)
					if opts.Progress != nil {
						opts.Progress(d, -1)
					}
					return nil
				})
				if err != nil && err != ErrCanceled {
					errOnce.Do(func() { firstErr = err })
				}
			}
		}()
	}
	wg.Wait()
	if firstErr == nil && opts.canceled() {
		firstErr = ErrCanceled
	}
	if firstErr != nil {
		return nil, firstErr
	}
	phase(opts, "convert %d chunks", convStart, done.Load())
	if opts.Verbose {
		opts.Logf("neighbour chunk loads: %d", neighborLoads.Load())
	}
	if discardOutput {
		opts.Logf("MCCONV_DISCARD: %.1f GB of region data not written", float64(regions.discarded.Load())/1e9)
	}
	if err := regions.closeAll(); err != nil {
		return nil, err
	}
	if !info.SpawnYSet {
		if y := spawnY.Load(); y > -1<<20 {
			info.SpawnY = y
		} else {
			info.SpawnY = 64
		}
		info.SpawnYSet = true
	}
	if info.Flat {
		for i, l := range info.FlatLayers {
			info.FlatLayers[i].Block = bc.convertName(l.Block)
		}
		var id int
		fmt.Sscan(info.FlatBiome, &id)
		name, _ := data.BedrockBiomeName(id)
		info.FlatBiome = data.BiomeBedrockToJava(name)
	}
	if err := dst.WriteLevel(info); err != nil {
		return nil, err
	}
	copyIcon(filepath.Join(srcDir, "world_icon.jpeg"), filepath.Join(dstDir, "icon.png"))
	reportUnknown(opts, bc)
	bc.dumpPerPos(opts.Logf)
	st.PerPosBlocks = bc.perPosCount.Load()
	st.TranslateErrs = bc.errorCount.Load()
	st.Entities = ec.count.Load()
	st.Duration = time.Since(start)
	return st, nil
}

// b2jWorker holds per-goroutine state for Bedrock -> Java conversion.
type b2jWorker struct {
	src       *bedrock.World
	dec       *bedrock.Decoder
	jc        *java.Codec
	jenc      *java.Encoder
	bc        *blockConverter
	lf        *leafFixer
	ec        *entityConverter
	regions   *regionSet
	neighbors *chunkCache
	opts      Options
	info      *world.LevelInfo
	spawnY    *atomic.Int32
	secs      world.SectionCache
}

func (w *b2jWorker) convert(rc *bedrock.RawChunk) (int, error) {
	if _, ok := rc.Data[bedrock.TagVersion]; !ok {
		if _, ok := rc.Data[bedrock.TagLegacyVersion]; !ok {
			return 0, java.ErrSkip // stray records without a chunk
		}
	}
	if !w.opts.IncludeUnfinished && rc.FinalizedState() < 2 {
		return 0, java.ErrSkip
	}
	ch, err := w.dec.Decode(rc)
	if err != nil {
		return 0, err
	}
	nf := func(cx, cz int32) *world.Chunk { return w.neighbors.get(cx, cz, rc.Dim) }
	out := w.bc.convertChunk(ch, java.Bounds(rc.Dim), nf, &w.secs)
	defer releaseConverted(ch, out, &w.secs)
	ents := w.ec.bedrockToJava(w.src, rc, ch, out)
	w.bc.fixVertical(out)
	w.lf.fix(out, nil)
	if out.Misc == nil {
		out.Misc = map[string]any{}
	}
	out.Misc["postProcess"] = true
	info := w.info
	if rc.Dim == world.Overworld && !info.SpawnYSet && info.SpawnX>>4 == rc.X && info.SpawnZ>>4 == rc.Z {
		if y, ok := topBlock(out, int(info.SpawnX&15), int(info.SpawnZ&15)); ok {
			w.spawnY.Store(int32(y + 1))
		}
	}
	if err := w.regions.writeChunk(rc.Dim, "region", rc.X, rc.Z, w.jenc.Encode(out), w.opts.CompressionLevel); err != nil {
		return 0, err
	}
	if len(ents.Items) > 0 {
		eroot := nbt.Compound{
			"DataVersion": int32(w.jc.DataVersion),
			"Position":    []int32{rc.X, rc.Z},
			"Entities":    ents,
		}
		if err := w.regions.writeChunk(rc.Dim, "entities", rc.X, rc.Z, nbt.Encode("", eroot, nbt.BigEndian), w.opts.CompressionLevel); err != nil {
			return 0, err
		}
	}
	return len(out.BlockEntities), nil
}

func topBlock(ch *world.Chunk, lx, lz int) (int, bool) {
	for si := len(ch.Sections) - 1; si >= 0; si-- {
		s := ch.Sections[si]
		if s == nil {
			continue
		}
		for y := 15; y >= 0; y-- {
			b := ch.Palette[s.Blocks[y<<8|lz<<4|lx]]
			if n := b.Name(); n != "minecraft:air" && n != "minecraft:cave_air" {
				return (ch.Bounds.MinSection+si)*16 + y, true
			}
		}
	}
	return 0, false
}

// regionSet lazily opens region writers.
type regionSet struct {
	mu      sync.Mutex
	dirFor  func(world.Dimension, string) string
	writers map[string]*anvil.Writer
	// bytes that would have been written with MCCONV_DISCARD
	discarded atomic.Int64
}

// discardOutput (MCCONV_DISCARD=1, for benchmarking without disk space)
// compresses Java chunks but does not write them.
var discardOutput = os.Getenv("MCCONV_DISCARD") == "1"

func newRegionSet(dirFor func(world.Dimension, string) string) *regionSet {
	return &regionSet{dirFor: dirFor, writers: map[string]*anvil.Writer{}}
}

func (rs *regionSet) writeChunk(dim world.Dimension, kind string, cx, cz int32, raw []byte, level int) error {
	comp := anvil.CompressZlib(raw, level)
	if discardOutput {
		rs.discarded.Add(int64(len(comp) + 5))
		return nil
	}
	rx, rz := cx>>5, cz>>5
	dir := rs.dirFor(dim, kind)
	path := filepath.Join(dir, fmt.Sprintf("r.%d.%d.mca", rx, rz))
	rs.mu.Lock()
	w := rs.writers[path]
	if w == nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			rs.mu.Unlock()
			return err
		}
		var err error
		w, err = anvil.CreateRegion(path)
		if err != nil {
			rs.mu.Unlock()
			return err
		}
		rs.writers[path] = w
	}
	rs.mu.Unlock()
	return w.WriteCompressed(int(cx&31), int(cz&31), comp)
}

func (rs *regionSet) closeAll() error {
	var first error
	for _, w := range rs.writers {
		if err := w.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// chunkCache is a small concurrent LRU of decoded source chunks.
type chunkCache struct {
	mu    sync.Mutex
	cap   int
	items map[[3]int32]*cacheItem
	tick  uint64
	load  func(x, z int32, dim world.Dimension) *world.Chunk
}

type cacheItem struct {
	ch   *world.Chunk
	used uint64
	ok   bool
}

func newChunkCache(capacity int, load func(x, z int32, dim world.Dimension) *world.Chunk) *chunkCache {
	return &chunkCache{cap: capacity, items: map[[3]int32]*cacheItem{}, load: load}
}

func (c *chunkCache) put(x, z int32, dim world.Dimension, ch *world.Chunk) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tick++
	c.items[[3]int32{x, z, int32(dim)}] = &cacheItem{ch: ch, used: c.tick, ok: true}
	c.evict()
}

// evict drops the least recently used quarter once the cache is over capacity.
func (c *chunkCache) evict() {
	if len(c.items) <= c.cap {
		return
	}
	ticks := make([]uint64, 0, len(c.items))
	for _, v := range c.items {
		ticks = append(ticks, v.used)
	}
	slices.Sort(ticks)
	limit := ticks[len(ticks)-c.cap*3/4]
	for k, v := range c.items {
		if v.used < limit {
			delete(c.items, k)
		}
	}
}

func (c *chunkCache) get(x, z int32, dim world.Dimension) *world.Chunk {
	k := [3]int32{x, z, int32(dim)}
	c.mu.Lock()
	if it, ok := c.items[k]; ok {
		c.tick++
		it.used = c.tick
		c.mu.Unlock()
		return it.ch
	}
	c.mu.Unlock()
	ch := c.load(x, z, dim)
	c.mu.Lock()
	c.tick++
	c.items[k] = &cacheItem{ch: ch, used: c.tick, ok: ch != nil}
	c.evict()
	c.mu.Unlock()
	return ch
}

func phase(opts Options, what string, since time.Time, args ...any) {
	if opts.Verbose {
		opts.Logf("[%6.2fs] "+what, append([]any{time.Since(since).Seconds()}, args...)...)
	}
}

// bedrockBlockVersion is the block state version written to Bedrock
// chunks: versions.json's bedrock_block_version, else the tables' version.
func bedrockBlockVersion(opts Options, t *translate.Tables) int32 {
	if v := opts.Config.Versions.BedrockBlockVersion; v > 0 {
		return int32(v)
	}
	return t.BedrockBlockVersion
}

// newerBedrock reports whether a Bedrock version (lastOpenedWithVersion) is
// newer than the tables' version ("1.26.50"); hotfixes (1.26.52) count as
// the same release.
func newerBedrock(v []int32, tables string) bool {
	var t [3]int32
	fmt.Sscanf(tables, "%d.%d.%d", &t[0], &t[1], &t[2])
	t[2] /= 10
	for i := 0; i < 3 && i < len(v); i++ {
		w := v[i]
		if i == 2 {
			w /= 10
		}
		if w != t[i] {
			return w > t[i]
		}
	}
	return false
}
