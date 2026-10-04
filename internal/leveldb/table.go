package leveldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"sort"
	"sync"

	"mcconv/internal/mmap"
)

// table is an open SSTable.
type table struct {
	f        *os.File
	data     []byte // memory mapped file
	unmap    func() error
	size     int64
	number   uint64
	index    []indexEntry
	cache    *blockCache
	smallest []byte // user keys (from the manifest)
	largest  []byte
}

type indexEntry struct {
	lastKey []byte // internal key >= every key in the block
	handle  blockHandle
}

func openTable(path string, number uint64, cache *blockCache) (*table, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	t := &table{f: f, size: st.Size(), number: number, cache: cache}
	if t.size < footerLen {
		f.Close()
		return nil, fmt.Errorf("leveldb: table %s too small", path)
	}
	if t.data, t.unmap, err = mmap.Map(f, t.size); err != nil {
		f.Close()
		return nil, err
	}
	footer := t.data[t.size-footerLen:]
	if binary.LittleEndian.Uint64(footer[40:]) != tableMagic {
		f.Close()
		return nil, fmt.Errorf("leveldb: bad table magic in %s", path)
	}
	_, n := decodeHandle(footer)
	indexHandle, n2 := decodeHandle(footer[n:])
	if n == 0 || n2 == 0 {
		f.Close()
		return nil, fmt.Errorf("leveldb: bad footer in %s", path)
	}
	idx, err := t.readBlockRaw(indexHandle)
	if err != nil {
		f.Close()
		return nil, err
	}
	it := newBlockIter(idx)
	for it.next() {
		h, _ := decodeHandle(it.value)
		t.index = append(t.index, indexEntry{lastKey: append([]byte(nil), it.key...), handle: h})
	}
	if it.err != nil {
		f.Close()
		return nil, it.err
	}
	return t, nil
}

func (t *table) close() {
	if t.unmap != nil {
		t.unmap()
	}
	t.f.Close()
}

func (t *table) readBlockRaw(h blockHandle) ([]byte, error) {
	if h.offset+h.length+blockTrailer > uint64(len(t.data)) {
		return nil, fmt.Errorf("leveldb: block beyond end of table %d", t.number)
	}
	buf := t.data[h.offset : h.offset+h.length+blockTrailer]
	data := buf[:h.length]
	kind := buf[h.length]
	want := unmaskCRC(binary.LittleEndian.Uint32(buf[h.length+1:]))
	if got := crc32.Checksum(buf[:h.length+1], crcTable); got != want {
		return nil, fmt.Errorf("leveldb: block checksum mismatch in table %d", t.number)
	}
	return decompressBlock(kind, data)
}

func (t *table) readBlock(i int) ([]byte, error) {
	h := t.index[i].handle
	if t.cache != nil {
		if b := t.cache.get(t.number, h.offset); b != nil {
			return b, nil
		}
	}
	b, err := t.readBlockRaw(h)
	if err != nil {
		return nil, err
	}
	if t.cache != nil {
		t.cache.put(t.number, h.offset, b)
	}
	return b, nil
}

// get finds the newest entry for ukey with sequence <= any.
func (t *table) get(ukey []byte) (value []byte, seq uint64, kind byte, found bool, err error) {
	target := makeIKey(nil, ukey, 1<<56-1, kindValue)
	i := sort.Search(len(t.index), func(i int) bool { return compareIKey(t.index[i].lastKey, target) >= 0 })
	if i >= len(t.index) {
		return nil, 0, 0, false, nil
	}
	blk, err := t.readBlock(i)
	if err != nil {
		return nil, 0, 0, false, err
	}
	it := newBlockIter(blk)
	for it.next() {
		if compareIKey(it.key, target) < 0 {
			continue
		}
		uk, s, k, ok := splitIKey(it.key)
		if !ok || !bytes.Equal(uk, ukey) {
			return nil, 0, 0, false, nil
		}
		return append([]byte(nil), it.value...), s, k, true, nil
	}
	return nil, 0, 0, false, it.err
}

// tableIter iterates over all entries of a table in order.
type tableIter struct {
	t      *table
	blk    int
	bi     *blockIter
	key    []byte
	value  []byte
	err    error
	skipTo []byte // internal key to seek to inside the first block
}

func (t *table) iter() *tableIter { return &tableIter{t: t, blk: -1} }

// iterFrom returns an iterator positioned before the first entry whose user
// key is >= lo.
func (t *table) iterFrom(lo []byte) *tableIter {
	ti := &tableIter{t: t, blk: -1}
	if lo == nil {
		return ti
	}
	target := makeIKey(nil, lo, 1<<56-1, kindValue)
	i := sort.Search(len(t.index), func(i int) bool { return compareIKey(t.index[i].lastKey, target) >= 0 })
	ti.blk = i - 1
	ti.skipTo = target
	return ti
}

func (ti *tableIter) next() bool {
	for {
		if ti.bi != nil && ti.bi.next() {
			if ti.skipTo != nil {
				if compareIKey(ti.bi.key, ti.skipTo) < 0 {
					continue
				}
				ti.skipTo = nil
			}
			ti.key, ti.value = ti.bi.key, ti.bi.value
			return true
		}
		if ti.bi != nil && ti.bi.err != nil {
			ti.err = ti.bi.err
			return false
		}
		ti.blk++
		if ti.blk >= len(ti.t.index) {
			return false
		}
		// sequential scans bypass the cache to avoid evicting hot blocks
		b, err := ti.t.readBlockRaw(ti.t.index[ti.blk].handle)
		if err != nil {
			ti.err = err
			return false
		}
		ti.bi = newBlockIter(b)
	}
}

// blockIter decodes entries of a data/index block.
type blockIter struct {
	data     []byte
	off      int
	limit    int
	key      []byte
	value    []byte
	err      error
	keyOwned bool
}

func newBlockIter(b []byte) *blockIter {
	if len(b) < 4 {
		return &blockIter{err: fmt.Errorf("leveldb: block too small")}
	}
	n := int(binary.LittleEndian.Uint32(b[len(b)-4:]))
	limit := len(b) - 4 - n*4
	if limit < 0 {
		return &blockIter{err: fmt.Errorf("leveldb: bad restart count")}
	}
	return &blockIter{data: b, limit: limit}
}

func (bi *blockIter) next() bool {
	if bi.err != nil || bi.off >= bi.limit {
		return false
	}
	p := bi.data[bi.off:bi.limit]
	shared, n1 := binary.Uvarint(p)
	nonShared, n2 := binary.Uvarint(p[max(n1, 0):])
	vlen, n3 := binary.Uvarint(p[max(n1+n2, 0):])
	if n1 <= 0 || n2 <= 0 || n3 <= 0 {
		bi.err = fmt.Errorf("leveldb: corrupt block entry")
		return false
	}
	h := n1 + n2 + n3
	if uint64(len(p)-h) < nonShared+vlen || shared > uint64(len(bi.key)) {
		bi.err = fmt.Errorf("leveldb: corrupt block entry")
		return false
	}
	// the key buffer is reused: callers must copy keys they keep
	bi.key = append(bi.key[:shared], p[h:h+int(nonShared)]...)
	bi.value = p[h+int(nonShared) : h+int(nonShared)+int(vlen)]
	bi.off += h + int(nonShared) + int(vlen)
	return true
}

// blockCache is a tiny sharded cache of decompressed blocks used by Get.
type blockCache struct {
	mu       sync.Mutex
	entries  map[[2]uint64]*cacheEntry
	order    []*cacheEntry
	capBytes int
	size     int
	clock    int
}

type cacheEntry struct {
	key  [2]uint64
	data []byte
	used bool
}

func newBlockCache(capBytes int) *blockCache {
	return &blockCache{entries: map[[2]uint64]*cacheEntry{}, capBytes: capBytes}
}

func (c *blockCache) get(file, off uint64) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[[2]uint64{file, off}]; ok {
		e.used = true
		return e.data
	}
	return nil
}

func (c *blockCache) put(file, off uint64, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := [2]uint64{file, off}
	if _, ok := c.entries[k]; ok {
		return
	}
	e := &cacheEntry{key: k, data: data}
	c.entries[k] = e
	c.order = append(c.order, e)
	c.size += len(data)
	// CLOCK eviction
	for c.size > c.capBytes && len(c.order) > 1 {
		if c.clock >= len(c.order) {
			c.clock = 0
		}
		v := c.order[c.clock]
		if v.used {
			v.used = false
			c.clock++
			continue
		}
		delete(c.entries, v.key)
		c.size -= len(v.data)
		c.order = append(c.order[:c.clock], c.order[c.clock+1:]...)
	}
}
