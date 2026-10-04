package leveldb

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/binary"
	"fmt"
	"github.com/klauspost/compress/flate"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
)

const (
	writeBlockSize       = 64 * 1024
	writeRestartInterval = 16
	writeTableSize       = 8 << 20 // uncompressed bytes per table file
	// bulk data is placed on the deepest level so the game never has to
	// compact it immediately after the world is opened.
	writeLevel = 6
)

// Options controls how a database is written.
type Options struct {
	// Compression level for Mojang's raw-deflate block compression.
	// 0 stores blocks uncompressed.
	CompressionLevel int
	// Workers is the number of parallel table builders (default GOMAXPROCS).
	Workers int
}

// Writer creates a new database from keys added in strictly increasing order.
type Writer struct {
	dir     string
	opts    Options
	nextNum uint64

	cur      []kv
	curBytes int

	jobs  chan tableJob
	wg    sync.WaitGroup
	mu    sync.Mutex
	files []fileMeta
	err   error
}

type kv struct {
	key, value []byte
}

type tableJob struct {
	number  uint64
	entries []kv
}

// NewWriter creates dir (which must not contain a database) and returns a writer.
func NewWriter(dir string, opts Options) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if opts.Workers <= 0 {
		opts.Workers = runtime.GOMAXPROCS(0)
	}
	w := &Writer{dir: dir, opts: opts, nextNum: 3, jobs: make(chan tableJob, opts.Workers)}
	for range opts.Workers {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			for job := range w.jobs {
				meta, err := buildTable(dir, job.number, job.entries, opts.CompressionLevel)
				w.mu.Lock()
				if err != nil && w.err == nil {
					w.err = err
				}
				if err == nil {
					w.files = append(w.files, meta)
				}
				w.mu.Unlock()
			}
		}()
	}
	return w, nil
}

// Add appends a key/value pair. Keys must be strictly increasing. The slices
// are retained, so callers must not modify them afterwards.
func (w *Writer) Add(key, value []byte) {
	w.cur = append(w.cur, kv{key, value})
	w.curBytes += len(key) + len(value) + 16
	if w.curBytes >= writeTableSize {
		w.flush()
	}
}

func (w *Writer) flush() {
	if len(w.cur) == 0 {
		return
	}
	w.jobs <- tableJob{number: w.nextNum, entries: w.cur}
	w.nextNum++
	w.cur = nil
	w.curBytes = 0
}

// Close finishes all tables and writes the MANIFEST and CURRENT files.
func (w *Writer) Close() error {
	w.flush()
	close(w.jobs)
	w.wg.Wait()
	if w.err != nil {
		return w.err
	}
	sort.Slice(w.files, func(i, j int) bool { return w.files[i].number < w.files[j].number })
	logNum := w.nextNum
	manifestNum := w.nextNum + 1
	ve := &versionEdit{
		comparator: "leveldb.BytewiseComparator", hasComparatr: true,
		logNumber: logNum, hasLog: true,
		prevLog: 0, hasPrevLog: true,
		nextFile: manifestNum + 1, hasNextFile: true,
		lastSeq: 1, hasLastSeq: true,
		added: w.files,
	}
	mname := fmt.Sprintf("MANIFEST-%06d", manifestNum)
	f, err := os.Create(filepath.Join(w.dir, mname))
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	lw := &logWriter{w: bw}
	if err := lw.addRecord(ve.encode()); err != nil {
		f.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(w.dir, fmt.Sprintf("%06d.log", logNum)), nil, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.dir, "CURRENT"), []byte(mname+"\n"), 0o644)
}

var flatePool sync.Pool

func buildTable(dir string, number uint64, entries []kv, level int) (fileMeta, error) {
	var out bytes.Buffer
	var index blockBuilder
	var blk blockBuilder
	var compBuf bytes.Buffer
	var lastKey []byte
	ikeyBuf := make([]byte, 0, 64)

	writeBlock := func(b []byte) (blockHandle, error) {
		kind := byte(compNone)
		data := b
		if level > 0 {
			compBuf.Reset()
			var fw *flate.Writer
			if v := flatePool.Get(); v != nil {
				fw = v.(*flate.Writer)
				fw.Reset(&compBuf)
			} else {
				var err error
				fw, err = flate.NewWriter(&compBuf, level)
				if err != nil {
					return blockHandle{}, err
				}
			}
			fw.Write(b)
			fw.Close()
			flatePool.Put(fw)
			if compBuf.Len() < len(b)-len(b)/8 {
				kind = compRawZlib
				data = compBuf.Bytes()
			}
		}
		h := blockHandle{offset: uint64(out.Len()), length: uint64(len(data))}
		out.Write(data)
		crc := crc32.Update(crc32.Checksum(data, crcTable), crcTable, []byte{kind})
		var tr [5]byte
		tr[0] = kind
		binary.LittleEndian.PutUint32(tr[1:], maskCRC(crc))
		out.Write(tr[:])
		return h, nil
	}

	var smallest []byte
	for i, e := range entries {
		ikeyBuf = makeIKey(ikeyBuf[:0], e.key, 1, kindValue)
		if i == 0 {
			smallest = append([]byte(nil), ikeyBuf...)
		}
		blk.add(ikeyBuf, e.value)
		lastKey = append(lastKey[:0], ikeyBuf...)
		if blk.size() >= writeBlockSize {
			h, err := writeBlock(blk.finish())
			if err != nil {
				return fileMeta{}, err
			}
			index.add(lastKey, h.append(nil))
			blk.reset()
		}
	}
	if !blk.empty() {
		h, err := writeBlock(blk.finish())
		if err != nil {
			return fileMeta{}, err
		}
		index.add(lastKey, h.append(nil))
	}
	// empty metaindex block, then the index block (both uncompressed)
	var meta blockBuilder
	metaH, _ := writeRawBlock(&out, meta.finish())
	idxH, _ := writeRawBlock(&out, index.finish())
	var footer [footerLen]byte
	idxH.append(metaH.append(footer[:0]))
	binary.LittleEndian.PutUint64(footer[40:], tableMagic)
	out.Write(footer[:])

	path := filepath.Join(dir, fmt.Sprintf("%06d.ldb", number))
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		return fileMeta{}, err
	}
	return fileMeta{level: writeLevel, number: number, size: uint64(out.Len()), smallest: smallest, largest: append([]byte(nil), lastKey...)}, nil
}

func writeRawBlock(out *bytes.Buffer, b []byte) (blockHandle, error) {
	h := blockHandle{offset: uint64(out.Len()), length: uint64(len(b))}
	out.Write(b)
	crc := crc32.Update(crc32.Checksum(b, crcTable), crcTable, []byte{compNone})
	var tr [5]byte
	binary.LittleEndian.PutUint32(tr[1:], maskCRC(crc))
	out.Write(tr[:])
	return h, nil
}

type blockBuilder struct {
	buf      []byte
	restarts []uint32
	counter  int
	lastKey  []byte
}

func (b *blockBuilder) reset() {
	b.buf = b.buf[:0]
	b.restarts = b.restarts[:0]
	b.counter = 0
	b.lastKey = b.lastKey[:0]
}

func (b *blockBuilder) empty() bool { return len(b.buf) == 0 }

func (b *blockBuilder) size() int { return len(b.buf) + 4*len(b.restarts) + 4 }

func (b *blockBuilder) add(key, value []byte) {
	shared := 0
	if b.counter < writeRestartInterval && len(b.buf) > 0 {
		n := min(len(key), len(b.lastKey))
		for shared < n && key[shared] == b.lastKey[shared] {
			shared++
		}
	} else {
		b.restarts = append(b.restarts, uint32(len(b.buf)))
		b.counter = 0
	}
	b.buf = binary.AppendUvarint(b.buf, uint64(shared))
	b.buf = binary.AppendUvarint(b.buf, uint64(len(key)-shared))
	b.buf = binary.AppendUvarint(b.buf, uint64(len(value)))
	b.buf = append(b.buf, key[shared:]...)
	b.buf = append(b.buf, value...)
	b.lastKey = append(b.lastKey[:0], key...)
	b.counter++
}

func (b *blockBuilder) finish() []byte {
	if len(b.restarts) == 0 {
		b.restarts = append(b.restarts, 0)
	}
	out := append([]byte(nil), b.buf...)
	for _, r := range b.restarts {
		out = binary.LittleEndian.AppendUint32(out, r)
	}
	return binary.LittleEndian.AppendUint32(out, uint32(len(b.restarts)))
}

// BulkLoader accepts key/value pairs in any order from many goroutines,
// spills sorted runs to disk when memory grows, and finally writes a database.
type BulkLoader struct {
	mu       sync.Mutex
	buf      []kv
	bufBytes int
	limit    int
	tmpDir   string
	runs     []string
	err      error
}

// NewBulkLoader creates a loader that keeps up to memLimit bytes in memory.
func NewBulkLoader(tmpDir string, memLimit int) *BulkLoader {
	return &BulkLoader{tmpDir: tmpDir, limit: memLimit}
}

// Put stores a pair. Later puts of the same key replace earlier ones.
func (b *BulkLoader) Put(key, value []byte) {
	b.mu.Lock()
	b.buf = append(b.buf, kv{key, value})
	b.bufBytes += len(key) + len(value) + 48
	var spill []kv
	if b.bufBytes >= b.limit {
		spill = b.buf
		b.buf = nil
		b.bufBytes = 0
	}
	b.mu.Unlock()
	if spill != nil {
		if err := b.spill(spill); err != nil {
			b.mu.Lock()
			if b.err == nil {
				b.err = err
			}
			b.mu.Unlock()
		}
	}
}

// sortKVs sorts by key and drops all but the last-added value of each key.
func sortKVs(s []kv) []kv {
	sort.SliceStable(s, func(i, j int) bool { return bytes.Compare(s[i].key, s[j].key) < 0 })
	out := s[:0]
	for i, e := range s {
		if i+1 < len(s) && bytes.Equal(e.key, s[i+1].key) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (b *BulkLoader) spill(s []kv) error {
	s = sortKVs(s)
	if err := os.MkdirAll(b.tmpDir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(b.tmpDir, "run-*.tmp")
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	var hdr []byte
	for _, e := range s {
		hdr = binary.AppendUvarint(hdr[:0], uint64(len(e.key)))
		hdr = binary.AppendUvarint(hdr, uint64(len(e.value)))
		bw.Write(hdr)
		bw.Write(e.key)
		bw.Write(e.value)
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	b.mu.Lock()
	b.runs = append(b.runs, f.Name())
	b.mu.Unlock()
	return nil
}

type runReader struct {
	r     *bufio.Reader
	f     *os.File
	key   []byte
	value []byte
	order int // higher = newer
	mem   []kv
	mi    int
}

func (r *runReader) next() bool {
	if r.f == nil {
		r.mi++
		if r.mi >= len(r.mem) {
			return false
		}
		r.key, r.value = r.mem[r.mi].key, r.mem[r.mi].value
		return true
	}
	kl, err := binary.ReadUvarint(r.r)
	if err != nil {
		return false
	}
	vl, err := binary.ReadUvarint(r.r)
	if err != nil {
		return false
	}
	buf := make([]byte, kl+vl)
	if _, err := io.ReadFull(r.r, buf); err != nil {
		return false
	}
	r.key, r.value = buf[:kl], buf[kl:]
	return true
}

type runHeap []*runReader

func (h runHeap) Len() int { return len(h) }
func (h runHeap) Less(i, j int) bool {
	if c := bytes.Compare(h[i].key, h[j].key); c != 0 {
		return c < 0
	}
	return h[i].order > h[j].order
}
func (h runHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *runHeap) Push(x any)   { *h = append(*h, x.(*runReader)) }
func (h *runHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// Finish writes all pairs into a new database at dir and removes temp files.
func (b *BulkLoader) Finish(dir string, opts Options) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	defer func() {
		for _, r := range b.runs {
			os.Remove(r)
		}
	}()
	if b.err != nil {
		return b.err
	}
	w, err := NewWriter(dir, opts)
	if err != nil {
		return err
	}
	// the in-memory remainder is the newest run
	mem := b.buf
	b.buf = nil
	mem = sortKVs(mem)
	var h runHeap
	for i, path := range b.runs {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		r := &runReader{f: f, r: bufio.NewReaderSize(f, 1<<20), order: i}
		if r.next() {
			h = append(h, r)
		}
	}
	mr := &runReader{mem: mem, mi: -1, order: len(b.runs)}
	if mr.next() {
		h = append(h, mr)
	}
	heap.Init(&h)
	var last []byte
	first := true
	for len(h) > 0 {
		r := h[0]
		k, v := r.key, r.value
		if first || !bytes.Equal(k, last) {
			w.Add(k, v)
			last = k
			first = false
		}
		if r.next() {
			heap.Fix(&h, 0)
		} else {
			heap.Pop(&h)
		}
	}
	return w.Close()
}

// finishInPlace is finish without copying; the result is valid until reset.
func (b *blockBuilder) finishInPlace() []byte {
	if len(b.restarts) == 0 {
		b.restarts = append(b.restarts, 0)
	}
	for _, r := range b.restarts {
		b.buf = binary.LittleEndian.AppendUint32(b.buf, r)
	}
	return binary.LittleEndian.AppendUint32(b.buf, uint32(len(b.restarts)))
}
