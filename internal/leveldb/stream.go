package leveldb

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"mcconv/internal/deflate"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
)

// DBBuilder writes a new database whose tables are produced by independent
// streams. Each stream receives keys in ascending order; the key ranges of
// streams written to the same level must not overlap. No sorting or
// re-writing happens, so tables can be produced in parallel straight from the
// conversion workers.
type DBBuilder struct {
	dir         string
	compression int
	nextNum     atomic.Uint64

	mu    sync.Mutex
	files []fileMeta
}

// streamTableSize is the compressed size at which a stream starts a new file.
const streamTableSize = 32 << 20

// NewDBBuilder creates dir and returns a builder.
func NewDBBuilder(dir string, compressionLevel int) (*DBBuilder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	b := &DBBuilder{dir: dir, compression: compressionLevel}
	b.nextNum.Store(2)
	return b, nil
}

// TableStream builds tables from ascending keys.
type TableStream struct {
	db    *DBBuilder
	level int

	f      *os.File
	w      *bufio.Writer
	number uint64
	offset uint64

	blk      blockBuilder
	index    blockBuilder
	smallest []byte
	lastKey  []byte
	ikey     []byte
	hasData  bool

	compBuf []byte
	handle  []byte
	err     error
}

// NewStream starts a stream that writes tables at the given level (0-6).
func (b *DBBuilder) NewStream(level int) *TableStream {
	return &TableStream{db: b, level: level}
}

func (t *TableStream) open() error {
	t.number = t.db.nextNum.Add(1)
	f, err := os.Create(filepath.Join(t.db.dir, fmt.Sprintf("%06d.ldb", t.number)))
	if err != nil {
		return err
	}
	t.f = f
	t.w = bufio.NewWriterSize(f, 1<<20)
	t.offset = 0
	t.blk.reset()
	t.index.reset()
	t.smallest = t.smallest[:0]
	t.hasData = false
	return nil
}

// Add appends a key/value pair; keys must be strictly ascending.
func (t *TableStream) Add(key, value []byte) error {
	if t.err != nil {
		return t.err
	}
	if t.f == nil {
		if t.err = t.open(); t.err != nil {
			return t.err
		}
	}
	t.ikey = makeIKey(t.ikey[:0], key, 1, kindValue)
	if !t.hasData {
		t.smallest = append(t.smallest[:0], t.ikey...)
		t.hasData = true
	}
	t.blk.add(t.ikey, value)
	t.lastKey = append(t.lastKey[:0], t.ikey...)
	if t.blk.size() >= writeBlockSize {
		if t.err = t.flushBlock(); t.err != nil {
			return t.err
		}
		if t.offset >= streamTableSize {
			t.err = t.finishFile()
		}
	}
	return t.err
}

func (t *TableStream) writeBlock(data []byte, kind byte) blockHandle {
	h := blockHandle{offset: t.offset, length: uint64(len(data))}
	t.w.Write(data)
	crc := crc32.Update(crc32.Checksum(data, crcTable), crcTable, []byte{kind})
	var tr [5]byte
	tr[0] = kind
	binary.LittleEndian.PutUint32(tr[1:], maskCRC(crc))
	t.w.Write(tr[:])
	t.offset += uint64(len(data)) + blockTrailer
	return h
}

func (t *TableStream) flushBlock() error {
	raw := t.blk.finishInPlace()
	data, kind := raw, byte(compNone)
	if t.db.compression != 0 {
		t.compBuf = deflate.CompressRaw(t.compBuf, raw, t.db.compression)
		if len(t.compBuf) < len(raw)-len(raw)/8 {
			data, kind = t.compBuf, compRawZlib
		}
	}
	h := t.writeBlock(data, kind)
	t.handle = h.append(t.handle[:0])
	t.index.add(t.lastKey, t.handle)
	t.blk.reset()
	return nil
}

func (t *TableStream) finishFile() error {
	if t.f == nil {
		return nil
	}
	if !t.blk.empty() {
		if err := t.flushBlock(); err != nil {
			return err
		}
	}
	var meta blockBuilder
	metaH := t.writeBlock(meta.finish(), compNone)
	idxH := t.writeBlock(t.index.finish(), compNone)
	var footer [footerLen]byte
	idxH.append(metaH.append(footer[:0]))
	binary.LittleEndian.PutUint64(footer[40:], tableMagic)
	t.w.Write(footer[:])
	t.offset += footerLen
	if err := t.w.Flush(); err != nil {
		t.f.Close()
		return err
	}
	if err := t.f.Close(); err != nil {
		return err
	}
	name := t.f.Name()
	t.f = nil
	if !t.hasData {
		return os.Remove(name)
	}
	t.db.mu.Lock()
	t.db.files = append(t.db.files, fileMeta{
		level: t.level, number: t.number, size: t.offset,
		smallest: append([]byte(nil), t.smallest...), largest: append([]byte(nil), t.lastKey...),
	})
	t.db.mu.Unlock()
	return nil
}

// Close finishes the current table.
func (t *TableStream) Close() error {
	if t.err != nil {
		if t.f != nil {
			t.f.Close()
		}
		return t.err
	}
	return t.finishFile()
}

// Finish writes the MANIFEST and CURRENT files.
func (b *DBBuilder) Finish() error {
	sort.Slice(b.files, func(i, j int) bool { return b.files[i].number < b.files[j].number })
	logNum := b.nextNum.Add(1)
	manifestNum := b.nextNum.Add(1)
	ve := &versionEdit{
		comparator: "leveldb.BytewiseComparator", hasComparatr: true,
		logNumber: logNum, hasLog: true,
		prevLog: 0, hasPrevLog: true,
		nextFile: manifestNum + 1, hasNextFile: true,
		lastSeq: 1, hasLastSeq: true,
		added: b.files,
	}
	mname := fmt.Sprintf("MANIFEST-%06d", manifestNum)
	f, err := os.Create(filepath.Join(b.dir, mname))
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
	if err := os.WriteFile(filepath.Join(b.dir, fmt.Sprintf("%06d.log", logNum)), nil, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.dir, "CURRENT"), []byte(mname+"\n"), 0o644)
}
