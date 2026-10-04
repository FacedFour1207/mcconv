// Package leveldb is a minimal LevelDB implementation tailored to Minecraft
// Bedrock Edition worlds. It can read a database (tables + logs, including the
// Mojang zlib/raw-deflate block compression) and write a fresh database from a
// sorted stream of key/value pairs. It is not a general purpose database.
package leveldb

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"mcconv/internal/deflate"
)

// Block compression ids.
const (
	compNone    = 0
	compSnappy  = 1
	compZlib    = 2
	compZstd    = 3
	compRawZlib = 4
)

const (
	tableMagic   = 0xdb4775248b80fb57
	footerLen    = 48
	blockTrailer = 5

	kindDelete = 0
	kindValue  = 1
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

const maskDelta = 0xa282ead8

func maskCRC(c uint32) uint32   { return ((c >> 15) | (c << 17)) + maskDelta }
func unmaskCRC(c uint32) uint32 { c -= maskDelta; return (c >> 17) | (c << 15) }

// internal key helpers -------------------------------------------------------

func makeIKey(dst, ukey []byte, seq uint64, kind byte) []byte {
	dst = append(dst, ukey...)
	return binary.LittleEndian.AppendUint64(dst, seq<<8|uint64(kind))
}

func splitIKey(ikey []byte) (ukey []byte, seq uint64, kind byte, ok bool) {
	if len(ikey) < 8 {
		return nil, 0, 0, false
	}
	n := len(ikey) - 8
	t := binary.LittleEndian.Uint64(ikey[n:])
	return ikey[:n], t >> 8, byte(t), true
}

// compareIKey orders by user key ascending then sequence descending.
func compareIKey(a, b []byte) int {
	ua, sa := a[:len(a)-8], binary.LittleEndian.Uint64(a[len(a)-8:])
	ub, sb := b[:len(b)-8], binary.LittleEndian.Uint64(b[len(b)-8:])
	if c := bytes.Compare(ua, ub); c != 0 {
		return c
	}
	switch {
	case sa > sb:
		return -1
	case sa < sb:
		return 1
	}
	return 0
}

// block handle ---------------------------------------------------------------

type blockHandle struct {
	offset, length uint64
}

func decodeHandle(b []byte) (blockHandle, int) {
	off, n1 := binary.Uvarint(b)
	if n1 <= 0 {
		return blockHandle{}, 0
	}
	ln, n2 := binary.Uvarint(b[n1:])
	if n2 <= 0 {
		return blockHandle{}, 0
	}
	return blockHandle{off, ln}, n1 + n2
}

func (h blockHandle) append(dst []byte) []byte {
	dst = binary.AppendUvarint(dst, h.offset)
	return binary.AppendUvarint(dst, h.length)
}

// decompression --------------------------------------------------------------

func decompressBlock(kind byte, data []byte) ([]byte, error) {
	switch kind {
	case compNone:
		return data, nil
	case compSnappy:
		return snappyDecode(data)
	case compZlib, compRawZlib:
		var out []byte
		var err error
		if kind == compZlib {
			out, err = deflate.Zlib(make([]byte, 0, len(data)*4), data)
		} else {
			out, err = deflate.Raw(make([]byte, 0, len(data)*4), data)
		}
		if err == nil {
			return out, nil
		}
		// fall back to the standard library
		if kind == compZlib {
			r, err := zlib.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return readAllSized(r, len(data)*4)
		}
		r := flate.NewReader(bytes.NewReader(data))
		defer r.Close()
		return readAllSized(r, len(data)*4)
	}
	return nil, fmt.Errorf("leveldb: unsupported block compression %d", kind)
}

func readAllSized(r io.Reader, hint int) ([]byte, error) {
	buf := bytes.NewBuffer(make([]byte, 0, hint))
	_, err := buf.ReadFrom(r)
	return buf.Bytes(), err
}

// snappyDecode decodes the raw snappy block format.
func snappyDecode(src []byte) ([]byte, error) {
	dlen, n := binary.Uvarint(src)
	if n <= 0 || dlen > 1<<30 {
		return nil, fmt.Errorf("leveldb: corrupt snappy header")
	}
	src = src[n:]
	dst := make([]byte, 0, dlen)
	for len(src) > 0 {
		tag := src[0]
		switch tag & 3 {
		case 0: // literal
			l := int(tag >> 2)
			src = src[1:]
			switch {
			case l < 60:
			case l == 60:
				if len(src) < 1 {
					return nil, errSnappy
				}
				l, src = int(src[0]), src[1:]
			case l == 61:
				if len(src) < 2 {
					return nil, errSnappy
				}
				l, src = int(binary.LittleEndian.Uint16(src)), src[2:]
			case l == 62:
				if len(src) < 3 {
					return nil, errSnappy
				}
				l, src = int(src[0])|int(src[1])<<8|int(src[2])<<16, src[3:]
			default:
				if len(src) < 4 {
					return nil, errSnappy
				}
				l, src = int(binary.LittleEndian.Uint32(src)), src[4:]
			}
			l++
			if l > len(src) {
				return nil, errSnappy
			}
			dst = append(dst, src[:l]...)
			src = src[l:]
			continue
		case 1:
			if len(src) < 2 {
				return nil, errSnappy
			}
			length := 4 + int(tag>>2)&7
			offset := int(tag&0xe0)<<3 | int(src[1])
			src = src[2:]
			if err := snappyCopy(&dst, offset, length); err != nil {
				return nil, err
			}
		case 2:
			if len(src) < 3 {
				return nil, errSnappy
			}
			length := 1 + int(tag>>2)
			offset := int(binary.LittleEndian.Uint16(src[1:]))
			src = src[3:]
			if err := snappyCopy(&dst, offset, length); err != nil {
				return nil, err
			}
		case 3:
			if len(src) < 5 {
				return nil, errSnappy
			}
			length := 1 + int(tag>>2)
			offset := int(binary.LittleEndian.Uint32(src[1:]))
			src = src[5:]
			if err := snappyCopy(&dst, offset, length); err != nil {
				return nil, err
			}
		}
	}
	return dst, nil
}

var errSnappy = fmt.Errorf("leveldb: corrupt snappy data")

func snappyCopy(dst *[]byte, offset, length int) error {
	d := *dst
	if offset <= 0 || offset > len(d) {
		return errSnappy
	}
	start := len(d) - offset
	for i := 0; i < length; i++ {
		d = append(d, d[start+i])
	}
	*dst = d
	return nil
}
