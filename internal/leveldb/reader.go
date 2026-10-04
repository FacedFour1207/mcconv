package leveldb

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DB is a read-only view of a LevelDB database.
type DB struct {
	dir    string
	tables []*table
	mem    []memEntry // sorted by user key, newest version only
}

type memEntry struct {
	key   []byte
	value []byte
	seq   uint64
	kind  byte
}

// Open opens a database directory read-only.
func Open(dir string) (*DB, error) {
	cur, err := os.ReadFile(filepath.Join(dir, "CURRENT"))
	if err != nil {
		return nil, fmt.Errorf("leveldb: %w", err)
	}
	manifestName := strings.TrimSpace(string(cur))
	mdata, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, fmt.Errorf("leveldb: %w", err)
	}
	type fkey struct {
		level  int
		number uint64
	}
	live := map[fkey]fileMeta{}
	var logNumber, prevLog uint64
	for _, rec := range readLogRecords(mdata) {
		ve, err := decodeVersionEdit(rec)
		if err != nil {
			return nil, err
		}
		if ve.hasLog {
			logNumber = ve.logNumber
		}
		if ve.hasPrevLog {
			prevLog = ve.prevLog
		}
		for _, d := range ve.deleted {
			delete(live, fkey{int(d[0]), d[1]})
		}
		for _, f := range ve.added {
			live[fkey{f.level, f.number}] = f
		}
	}
	db := &DB{dir: dir}
	cache := newBlockCache(64 << 20)
	for _, f := range live {
		path := filepath.Join(dir, fmt.Sprintf("%06d.ldb", f.number))
		if _, err := os.Stat(path); err != nil {
			path = filepath.Join(dir, fmt.Sprintf("%06d.sst", f.number))
		}
		t, err := openTable(path, f.number, cache)
		if err != nil {
			db.Close()
			return nil, err
		}
		if uk, _, _, ok := splitIKey(f.smallest); ok {
			t.smallest = uk
		}
		if uk, _, _, ok := splitIKey(f.largest); ok {
			t.largest = uk
		}
		db.tables = append(db.tables, t)
	}
	// replay write-ahead logs that are not yet compacted
	entries, err := os.ReadDir(dir)
	if err != nil {
		db.Close()
		return nil, err
	}
	var logs []uint64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".log") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(name, ".log"), 10, 64)
		if err != nil {
			continue
		}
		if n >= logNumber || (prevLog != 0 && n == prevLog) {
			logs = append(logs, n)
		}
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i] < logs[j] })
	mem := map[string]memEntry{}
	for _, n := range logs {
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%06d.log", n)))
		if err != nil {
			db.Close()
			return nil, err
		}
		for _, rec := range readLogRecords(data) {
			applyBatch(rec, mem)
		}
	}
	db.mem = make([]memEntry, 0, len(mem))
	for _, e := range mem {
		db.mem = append(db.mem, e)
	}
	sort.Slice(db.mem, func(i, j int) bool { return bytes.Compare(db.mem[i].key, db.mem[j].key) < 0 })
	return db, nil
}

func applyBatch(rec []byte, mem map[string]memEntry) {
	if len(rec) < 12 {
		return
	}
	seq := binary.LittleEndian.Uint64(rec)
	count := binary.LittleEndian.Uint32(rec[8:])
	p := rec[12:]
	for i := uint32(0); i < count && len(p) > 0; i++ {
		kind := p[0]
		p = p[1:]
		kl, n := binary.Uvarint(p)
		if n <= 0 || uint64(len(p)-n) < kl {
			return
		}
		key := p[n : n+int(kl)]
		p = p[n+int(kl):]
		var val []byte
		if kind == kindValue {
			vl, n := binary.Uvarint(p)
			if n <= 0 || uint64(len(p)-n) < vl {
				return
			}
			val = p[n : n+int(vl)]
			p = p[n+int(vl):]
		}
		s := seq + uint64(i)
		if old, ok := mem[string(key)]; ok && old.seq > s {
			continue
		}
		mem[string(key)] = memEntry{key: append([]byte(nil), key...), value: append([]byte(nil), val...), seq: s, kind: kind}
	}
}

// Close releases file handles.
func (db *DB) Close() {
	for _, t := range db.tables {
		t.close()
	}
	db.tables = nil
}

// Get returns the value of key or (nil, false).
func (db *DB) Get(key []byte) ([]byte, bool, error) {
	var bestSeq uint64
	var best []byte
	found, deleted := false, false
	i := sort.Search(len(db.mem), func(i int) bool { return bytes.Compare(db.mem[i].key, key) >= 0 })
	if i < len(db.mem) && bytes.Equal(db.mem[i].key, key) {
		e := db.mem[i]
		found, bestSeq, best, deleted = true, e.seq, e.value, e.kind == kindDelete
	}
	for _, t := range db.tables {
		// skip tables whose key range cannot hold the key (avoids
		// decompressing a block in every table)
		if t.smallest != nil && bytes.Compare(key, t.smallest) < 0 || t.largest != nil && bytes.Compare(key, t.largest) > 0 {
			continue
		}
		v, s, k, ok, err := t.get(key)
		if err != nil {
			return nil, false, err
		}
		if ok && (!found || s > bestSeq) {
			found, bestSeq, best, deleted = true, s, v, k == kindDelete
		}
	}
	if !found || deleted {
		return nil, false, nil
	}
	return best, true, nil
}

// Iterator walks all live keys in order.
type Iterator struct {
	hi      []byte // exclusive upper bound (nil = none)
	h       iterHeap
	key     []byte
	value   []byte
	err     error
	lastKey []byte
	started bool
}

type source interface {
	next() bool
	ikey() []byte
	val() []byte
	error() error
}

type tableSource struct{ it *tableIter }

func (s *tableSource) next() bool   { return s.it.next() }
func (s *tableSource) ikey() []byte { return s.it.key }
func (s *tableSource) val() []byte  { return s.it.value }
func (s *tableSource) error() error { return s.it.err }

type memSource struct {
	m   []memEntry
	i   int
	key []byte
}

func (s *memSource) next() bool {
	s.i++
	if s.i >= len(s.m) {
		return false
	}
	e := s.m[s.i]
	s.key = makeIKey(s.key[:0], e.key, e.seq, e.kind)
	return true
}
func (s *memSource) ikey() []byte { return s.key }
func (s *memSource) val() []byte  { return s.m[s.i].value }
func (s *memSource) error() error { return nil }

type iterHeap []source

func (h iterHeap) Len() int           { return len(h) }
func (h iterHeap) Less(i, j int) bool { return compareIKey(h[i].ikey(), h[j].ikey()) < 0 }
func (h iterHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *iterHeap) Push(x any)        { *h = append(*h, x.(source)) }
func (h *iterHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// NewIterator returns an iterator positioned before the first key.
func (db *DB) NewIterator() *Iterator {
	it := &Iterator{}
	add := func(s source) {
		if s.next() {
			it.h = append(it.h, s)
		} else if err := s.error(); err != nil && it.err == nil {
			it.err = err
		}
	}
	add(&memSource{m: db.mem, i: -1})
	for _, t := range db.tables {
		add(&tableSource{it: t.iter()})
	}
	heap.Init(&it.h)
	return it
}

// Next advances to the next live key.
func (it *Iterator) Next() bool {
	for it.err == nil && len(it.h) > 0 {
		s := it.h[0]
		ukey, _, kind, ok := splitIKey(s.ikey())
		if it.hi != nil && ok && bytes.Compare(ukey, it.hi) >= 0 {
			return false
		}
		dup := it.started && bytes.Equal(ukey, it.lastKey)
		var key, val []byte
		if !dup && ok {
			key = append([]byte(nil), ukey...)
			val = s.val()
		}
		if ok && !dup {
			it.lastKey = key
			it.started = true
		}
		if s.next() {
			heap.Fix(&it.h, 0)
		} else {
			if err := s.error(); err != nil {
				it.err = err
				return false
			}
			heap.Pop(&it.h)
		}
		if dup || !ok || kind == kindDelete {
			continue
		}
		it.key = key
		it.value = val
		return true
	}
	return false
}

// Key returns the current key (valid until the next call to Next).
func (it *Iterator) Key() []byte { return it.key }

// Value returns the current value. It may be retained by the caller only if copied.
func (it *Iterator) Value() []byte { return it.value }

// Err returns the first error encountered.
func (it *Iterator) Err() error { return it.err }

// NewRangeIterator iterates live keys in [lo, hi); nil bounds are open.
// Several range iterators may run concurrently.
func (db *DB) NewRangeIterator(lo, hi []byte) *Iterator {
	it := &Iterator{hi: hi}
	add := func(s source) {
		if s.next() {
			it.h = append(it.h, s)
		} else if err := s.error(); err != nil && it.err == nil {
			it.err = err
		}
	}
	start := 0
	if lo != nil {
		start = sort.Search(len(db.mem), func(i int) bool { return bytes.Compare(db.mem[i].key, lo) >= 0 })
	}
	add(&memSource{m: db.mem, i: start - 1})
	for _, t := range db.tables {
		if lo != nil && t.largest != nil && bytes.Compare(t.largest, lo) < 0 {
			continue
		}
		if hi != nil && t.smallest != nil && bytes.Compare(t.smallest, hi) >= 0 {
			continue
		}
		add(&tableSource{it: t.iterFrom(lo)})
	}
	heap.Init(&it.h)
	return it
}

// SplitKeys returns up to n-1 ascending keys that divide the database into
// ranges of similar size. Keys are truncated to prefixLen bytes so that all
// records sharing a prefix (e.g. one Bedrock chunk) stay in one range.
func (db *DB) SplitKeys(n, prefixLen int) [][]byte {
	var cand [][]byte
	for _, t := range db.tables {
		for _, e := range t.index {
			uk, _, _, ok := splitIKey(e.lastKey)
			if !ok || len(uk) < prefixLen {
				continue
			}
			cand = append(cand, uk[:prefixLen])
		}
	}
	sort.Slice(cand, func(i, j int) bool { return bytes.Compare(cand[i], cand[j]) < 0 })
	var out [][]byte
	for i := 1; i < n && len(cand) > 0; i++ {
		k := cand[i*len(cand)/n]
		if len(out) > 0 && bytes.Equal(out[len(out)-1], k) {
			continue
		}
		out = append(out, append([]byte(nil), k...))
	}
	return out
}
