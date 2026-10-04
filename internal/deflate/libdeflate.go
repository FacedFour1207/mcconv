//go:build cgo

package deflate

/*
#cgo CFLAGS: -O2 -I${SRCDIR}/libdeflate
#include "libdeflate.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

const backend = "libdeflate"

var errCorrupt = errors.New("deflate: corrupt or truncated data")

// libdeflate (de)compressors are not thread safe and allocating them is not
// free, so idle ones are kept in small free lists.
var decompressors = make(chan *C.struct_libdeflate_decompressor, 64)

var compressors [13]chan *C.struct_libdeflate_compressor

func init() {
	for i := range compressors {
		compressors[i] = make(chan *C.struct_libdeflate_compressor, 64)
	}
}

func ptr(b []byte) *C.uint8_t { return (*C.uint8_t)(unsafe.Pointer(unsafe.SliceData(b))) }

func decompress(dst, src []byte, zlib bool) ([]byte, error) {
	var d *C.struct_libdeflate_decompressor
	select {
	case d = <-decompressors:
	default:
		if d = C.libdeflate_alloc_decompressor(); d == nil {
			return dst, errors.New("deflate: out of memory")
		}
	}
	defer func() {
		select {
		case decompressors <- d:
		default:
			C.libdeflate_free_decompressor(d)
		}
	}()
	// the decompressed size is not stored: guess, and grow while too small
	size := max(cap(dst), len(src)*6, 4096)
	for {
		if cap(dst) < size {
			dst = make([]byte, size)
		}
		dst = dst[:cap(dst)]
		var n C.size_t
		var r C.enum_libdeflate_result
		if zlib {
			r = C.libdeflate_zlib_decompress(d, unsafe.Pointer(ptr(src)), C.size_t(len(src)), unsafe.Pointer(ptr(dst)), C.size_t(len(dst)), &n)
		} else {
			r = C.libdeflate_deflate_decompress(d, unsafe.Pointer(ptr(src)), C.size_t(len(src)), unsafe.Pointer(ptr(dst)), C.size_t(len(dst)), &n)
		}
		switch r {
		case C.LIBDEFLATE_SUCCESS:
			return dst[:n], nil
		case C.LIBDEFLATE_INSUFFICIENT_SPACE:
			if size > 1<<30 {
				return dst[:0], errCorrupt
			}
			size = len(dst) * 4
		default:
			return dst[:0], errCorrupt
		}
	}
}

func compress(dst, src []byte, level int, zlib bool) []byte {
	switch {
	case level < 0: // Huffman only (klauspost) has no libdeflate equivalent
		level = 1
	case level > 12:
		level = 12
	}
	var c *C.struct_libdeflate_compressor
	select {
	case c = <-compressors[level]:
	default:
		if c = C.libdeflate_alloc_compressor(C.int(level)); c == nil {
			panic("deflate: out of memory")
		}
	}
	defer func() {
		select {
		case compressors[level] <- c:
		default:
			C.libdeflate_free_compressor(c)
		}
	}()
	var bound C.size_t
	if zlib {
		bound = C.libdeflate_zlib_compress_bound(c, C.size_t(len(src)))
	} else {
		bound = C.libdeflate_deflate_compress_bound(c, C.size_t(len(src)))
	}
	if cap(dst) < int(bound) {
		dst = make([]byte, bound)
	}
	dst = dst[:bound]
	var n C.size_t
	if zlib {
		n = C.libdeflate_zlib_compress(c, unsafe.Pointer(ptr(src)), C.size_t(len(src)), unsafe.Pointer(ptr(dst)), bound)
	} else {
		n = C.libdeflate_deflate_compress(c, unsafe.Pointer(ptr(src)), C.size_t(len(src)), unsafe.Pointer(ptr(dst)), bound)
	}
	if n == 0 {
		panic("deflate: compression bound too small")
	}
	return dst[:n]
}
