// Package anvil reads and writes Java Edition region (.mca) files.
package anvil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"mcconv/internal/deflate"
	"mcconv/internal/mmap"
)

const sectorSize = 4096

// Compression ids used in region files.
const (
	CompGzip = 1
	CompZlib = 2
	CompNone = 3
	CompLZ4  = 4
	// a set high bit means the chunk is stored in an external c.X.Z.mcc file
	compExternal = 128
)

// Region is an open region file for reading. The file is memory mapped so
// chunk reads are plain memory accesses (no system call per chunk).
type Region struct {
	path    string
	f       *os.File
	size    int64
	data    []byte
	unmap   func() error
	offsets [1024]uint32
	RX, RZ  int
}

var regionName = regexp.MustCompile(`^r\.(-?\d+)\.(-?\d+)\.mca$`)

// ParseRegionName extracts region coordinates from a file name.
func ParseRegionName(name string) (rx, rz int, ok bool) {
	m := regionName.FindStringSubmatch(name)
	if m == nil {
		return 0, 0, false
	}
	rx, _ = strconv.Atoi(m[1])
	rz, _ = strconv.Atoi(m[2])
	return rx, rz, true
}

// ListRegions returns the region files in dir.
func ListRegions(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if _, _, ok := ParseRegionName(e.Name()); ok && !e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// OpenRegion opens a region file.
func OpenRegion(path string) (*Region, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r := &Region{path: path, f: f, size: st.Size()}
	r.RX, r.RZ, _ = ParseRegionName(filepath.Base(path))
	if r.size < 2*sectorSize {
		r.unmap = func() error { return nil }
		return r, nil // empty region
	}
	r.data, r.unmap, err = mmap.Map(f, r.size)
	if err != nil {
		f.Close()
		return nil, err
	}
	hdr := r.data[:sectorSize]
	for i := range 1024 {
		r.offsets[i] = binary.BigEndian.Uint32(hdr[i*4:])
	}
	return r, nil
}

// Close unmaps and closes the file.
func (r *Region) Close() error {
	if r.unmap != nil {
		r.unmap()
	}
	return r.f.Close()
}

// Has reports whether local chunk (lx, lz) exists.
func (r *Region) Has(lx, lz int) bool { return r.offsets[(lz&31)*32+(lx&31)] != 0 }

// Chunks lists the local coordinates of stored chunks.
func (r *Region) Chunks() [][2]int {
	var out [][2]int
	for i, o := range r.offsets {
		if o != 0 {
			out = append(out, [2]int{i & 31, i >> 5})
		}
	}
	return out
}

// ReadRaw returns the decompressed NBT payload of a chunk, or nil if absent.
func (r *Region) ReadRaw(lx, lz int) ([]byte, error) {
	var d Decompressor
	out, err := d.ReadChunk(r, lx, lz)
	if out == nil {
		return nil, err
	}
	return append([]byte(nil), out...), err
}

// compressedChunk returns the stored payload and compression id of a chunk.
func (r *Region) compressedChunk(lx, lz int) ([]byte, byte, error) {
	o := r.offsets[(lz&31)*32+(lx&31)]
	if o == 0 {
		return nil, 0, nil
	}
	off := int64(o>>8) * sectorSize
	if off+5 > r.size {
		return nil, 0, fmt.Errorf("anvil: chunk offset beyond file end in %s", r.path)
	}
	length := int64(binary.BigEndian.Uint32(r.data[off:]))
	comp := r.data[off+4]
	if comp&compExternal != 0 {
		ext := filepath.Join(filepath.Dir(r.path), fmt.Sprintf("c.%d.%d.mcc", r.RX*32+lx, r.RZ*32+lz))
		payload, err := os.ReadFile(ext)
		return payload, comp &^ compExternal, err
	}
	if length < 1 || off+4+length > r.size {
		return nil, 0, fmt.Errorf("anvil: truncated chunk in %s", r.path)
	}
	return r.data[off+5 : off+4+length], comp, nil
}

// Decompressor decodes chunks reusing its buffers; one per goroutine.
type Decompressor struct {
	out []byte
}

// ReadChunk returns the decompressed chunk (valid until the next call) or nil.
func (d *Decompressor) ReadChunk(r *Region, lx, lz int) ([]byte, error) {
	payload, comp, err := r.compressedChunk(lx, lz)
	if err != nil || payload == nil {
		return nil, err
	}
	if comp != CompZlib {
		return Decompress(comp, payload)
	}
	out, err := deflate.Zlib(d.out, payload)
	if err != nil {
		// fall back to the standard library for anything unusual
		return Decompress(comp, payload)
	}
	d.out = out
	return out, nil
}

// Decompress decodes a chunk payload.
func Decompress(comp byte, data []byte) ([]byte, error) {
	switch comp {
	case CompZlib:
		zr, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		var out bytes.Buffer
		out.Grow(len(data) * 4)
		_, err = out.ReadFrom(zr)
		return out.Bytes(), err
	case CompGzip:
		gr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		return io.ReadAll(gr)
	case CompNone:
		return data, nil
	case CompLZ4:
		return decodeLZ4Block(data)
	}
	return nil, fmt.Errorf("anvil: unsupported compression %d", comp)
}

// Writer writes chunks into a region file incrementally. It is safe for
// concurrent use.
type Writer struct {
	mu         sync.Mutex
	f          *os.File
	next       uint32
	offsets    [1024]uint32
	timestamps [1024]uint32
	// pending holds sectors not yet written, starting at sector pendingAt;
	// writes are batched to avoid a system call per chunk.
	pending   []byte
	pendingAt uint32
	now       uint32
}

const flushBytes = 1 << 20

// CreateRegion creates (or truncates) a region file.
func CreateRegion(path string) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(make([]byte, 2*sectorSize)); err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{f: f, next: 2, pendingAt: 2, now: uint32(time.Now().Unix())}, nil
}

// CompressZlib compresses a chunk payload with zlib.
func CompressZlib(data []byte, level int) []byte {
	return deflate.CompressZlib(nil, data, level)
}

// WriteCompressed stores an already zlib compressed chunk at local (lx, lz).
func (w *Writer) WriteCompressed(lx, lz int, compressed []byte) error {
	total := len(compressed) + 5
	sectors := (total + sectorSize - 1) / sectorSize
	if sectors > 255 {
		return fmt.Errorf("anvil: chunk too large (%d bytes)", total)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	pos := w.next
	w.next += uint32(sectors)
	start := len(w.pending)
	w.pending = append(w.pending, make([]byte, sectors*sectorSize)...)
	buf := w.pending[start:]
	binary.BigEndian.PutUint32(buf, uint32(len(compressed)+1))
	buf[4] = CompZlib
	copy(buf[5:], compressed)
	i := (lz&31)*32 + (lx & 31)
	w.offsets[i] = pos<<8 | uint32(sectors)
	w.timestamps[i] = w.now
	if len(w.pending) >= flushBytes {
		return w.flush()
	}
	return nil
}

func (w *Writer) flush() error {
	if len(w.pending) == 0 {
		return nil
	}
	if _, err := w.f.WriteAt(w.pending, int64(w.pendingAt)*sectorSize); err != nil {
		return err
	}
	w.pendingAt = w.next
	w.pending = w.pending[:0]
	return nil
}

// Close writes the header and closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.flush(); err != nil {
		w.f.Close()
		return err
	}
	hdr := make([]byte, 2*sectorSize)
	for i := range 1024 {
		binary.BigEndian.PutUint32(hdr[i*4:], w.offsets[i])
		binary.BigEndian.PutUint32(hdr[sectorSize+i*4:], w.timestamps[i])
	}
	if _, err := w.f.WriteAt(hdr, 0); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// decodeLZ4Block decodes the LZ4 block-stream framing used by Java 1.20.5+
// ("LZ4Block" header per block).
func decodeLZ4Block(data []byte) ([]byte, error) {
	var out []byte
	for len(data) >= 21 {
		if string(data[:8]) != "LZ4Block" {
			return nil, fmt.Errorf("anvil: bad lz4 block magic")
		}
		token := data[8]
		compLen := int(binary.LittleEndian.Uint32(data[9:]))
		rawLen := int(binary.LittleEndian.Uint32(data[13:]))
		data = data[21:]
		if compLen > len(data) {
			return nil, fmt.Errorf("anvil: truncated lz4 block")
		}
		block := data[:compLen]
		data = data[compLen:]
		if rawLen == 0 {
			break
		}
		switch token & 0xF0 {
		case 0x10: // raw
			out = append(out, block...)
		case 0x20: // lz4
			dec, err := lz4Decompress(block, rawLen)
			if err != nil {
				return nil, err
			}
			out = append(out, dec...)
		default:
			return nil, fmt.Errorf("anvil: unknown lz4 block method")
		}
	}
	return out, nil
}

func lz4Decompress(src []byte, size int) ([]byte, error) {
	dst := make([]byte, 0, size)
	for i := 0; i < len(src); {
		token := src[i]
		i++
		lit := int(token >> 4)
		if lit == 15 {
			for i < len(src) {
				b := src[i]
				i++
				lit += int(b)
				if b != 255 {
					break
				}
			}
		}
		if i+lit > len(src) {
			return nil, fmt.Errorf("anvil: corrupt lz4 literal")
		}
		dst = append(dst, src[i:i+lit]...)
		i += lit
		if i >= len(src) {
			break
		}
		if i+2 > len(src) {
			return nil, fmt.Errorf("anvil: corrupt lz4 offset")
		}
		off := int(src[i]) | int(src[i+1])<<8
		i += 2
		ml := int(token&15) + 4
		if token&15 == 15 {
			for i < len(src) {
				b := src[i]
				i++
				ml += int(b)
				if b != 255 {
					break
				}
			}
		}
		if off == 0 || off > len(dst) {
			return nil, fmt.Errorf("anvil: corrupt lz4 match")
		}
		start := len(dst) - off
		for k := 0; k < ml; k++ {
			dst = append(dst, dst[start+k])
		}
	}
	return dst, nil
}
