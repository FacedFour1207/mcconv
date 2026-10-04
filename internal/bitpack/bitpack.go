// Package bitpack packs and unpacks the block index arrays of chunk
// sections: Bedrock stores them in 32-bit little endian words, Java in
// 64-bit longs; in both formats an entry never spans two words.
//
// When built with GOEXPERIMENT=simd on amd64 and the CPU has AVX2, SIMD
// versions are used (simd_amd64.go); otherwise the scalar code below.
package bitpack

import "encoding/binary"

// The implementations in use (replaced by the SIMD versions when available).
var (
	unpack32 = unpack32Scalar
	pack32   = pack32Scalar
	unpack64 = unpack64Scalar
	pack64   = pack64Scalar
)

// SIMD reports whether the SIMD implementations are in use.
var SIMD = false

// Unpack32 extracts len(out) values of bits bits from 32-bit little endian
// words. Values past the end of words are 0.
func Unpack32(words []byte, bits int, out []uint32) { unpack32(words, bits, out) }

// Pack32 packs idx into 32-bit little endian words written to out, which
// must hold Words32(len(idx), bits)*4 bytes.
func Pack32(out []byte, idx []uint16, bits int) { pack32(out, idx, bits) }

// Unpack64 extracts len(out) values of bits bits from 64-bit longs. Values
// past the end of data are 0.
func Unpack64(data []uint64, bits int, out []uint32) { unpack64(data, bits, out) }

// Pack64 packs idx into 64-bit longs (dst must hold Words64(len(idx), bits)).
func Pack64(dst []uint64, idx []uint32, bits int) { pack64(dst, idx, bits) }

// Words32 is the number of 32-bit words n values of bits bits need.
func Words32(n, bits int) int { p := 32 / bits; return (n + p - 1) / p }

// Words64 is the number of longs n values of bits bits need.
func Words64(n, bits int) int { p := 64 / bits; return (n + p - 1) / p }

func unpack32Scalar(words []byte, bits int, out []uint32) {
	p := 32 / bits
	mask := uint32(1)<<bits - 1
	i, n := 0, len(out)
	for w := 0; w+4 <= len(words) && i < n; w += 4 {
		word := binary.LittleEndian.Uint32(words[w:])
		end := min(i+p, n)
		for ; i < end; i++ {
			out[i] = word & mask
			word >>= bits
		}
	}
	clear(out[i:])
}

func pack32Scalar(out []byte, idx []uint16, bits int) {
	p := 32 / bits
	words := Words32(len(idx), bits)
	full := len(idx) / p // words without padding at the end
	switch bits {
	case 1:
		for w := 0; w < full; w++ {
			v := idx[w*32 : w*32+32 : w*32+32]
			var word uint32
			for j := 31; j >= 0; j-- {
				word = word<<1 | uint32(v[j])
			}
			binary.LittleEndian.PutUint32(out[w*4:], word)
		}
	case 2:
		for w := 0; w < full; w++ {
			v := idx[w*16 : w*16+16 : w*16+16]
			var word uint32
			for j := 15; j >= 0; j-- {
				word = word<<2 | uint32(v[j])
			}
			binary.LittleEndian.PutUint32(out[w*4:], word)
		}
	case 3:
		for w := 0; w < full; w++ {
			v := idx[w*10 : w*10+10 : w*10+10]
			word := uint32(v[0]) | uint32(v[1])<<3 | uint32(v[2])<<6 | uint32(v[3])<<9 | uint32(v[4])<<12 |
				uint32(v[5])<<15 | uint32(v[6])<<18 | uint32(v[7])<<21 | uint32(v[8])<<24 | uint32(v[9])<<27
			binary.LittleEndian.PutUint32(out[w*4:], word)
		}
	case 4:
		for w := 0; w < full; w++ {
			v := idx[w*8 : w*8+8 : w*8+8]
			word := uint32(v[0]) | uint32(v[1])<<4 | uint32(v[2])<<8 | uint32(v[3])<<12 |
				uint32(v[4])<<16 | uint32(v[5])<<20 | uint32(v[6])<<24 | uint32(v[7])<<28
			binary.LittleEndian.PutUint32(out[w*4:], word)
		}
	case 5:
		for w := 0; w < full; w++ {
			v := idx[w*6 : w*6+6 : w*6+6]
			word := uint32(v[0]) | uint32(v[1])<<5 | uint32(v[2])<<10 | uint32(v[3])<<15 | uint32(v[4])<<20 | uint32(v[5])<<25
			binary.LittleEndian.PutUint32(out[w*4:], word)
		}
	case 6:
		for w := 0; w < full; w++ {
			v := idx[w*5 : w*5+5 : w*5+5]
			word := uint32(v[0]) | uint32(v[1])<<6 | uint32(v[2])<<12 | uint32(v[3])<<18 | uint32(v[4])<<24
			binary.LittleEndian.PutUint32(out[w*4:], word)
		}
	case 8:
		for w := 0; w < full; w++ {
			v := idx[w*4 : w*4+4 : w*4+4]
			out[w*4], out[w*4+1], out[w*4+2], out[w*4+3] = byte(v[0]), byte(v[1]), byte(v[2]), byte(v[3])
		}
	case 16:
		for i, v := range idx[:full*2] {
			binary.LittleEndian.PutUint16(out[i*2:], v)
		}
	default:
		full = 0
	}
	packTail32(out, idx, full, words, bits)
}

// packTail32 packs words [from, to) generically.
func packTail32(out []byte, idx []uint16, from, to, bits int) {
	p := 32 / bits
	for w := from; w < to; w++ {
		var word uint32
		for j, i := 0, w*p; j < p && i < len(idx); j, i = j+1, i+1 {
			word |= uint32(idx[i]) << (uint(j) * uint(bits))
		}
		binary.LittleEndian.PutUint32(out[w*4:], word)
	}
}

func unpack64Scalar(data []uint64, bits int, out []uint32) {
	p := 64 / bits
	mask := uint64(1)<<bits - 1
	i, n := 0, len(out)
	for _, v := range data {
		end := min(i+p, n)
		for ; i < end; i++ {
			out[i] = uint32(v & mask)
			v >>= bits
		}
		if i >= n {
			break
		}
	}
	clear(out[i:])
}

func pack64Scalar(dst []uint64, idx []uint32, bits int) {
	p := 64 / bits
	i := 0
	for l := range dst {
		var word uint64
		end := min(i+p, len(idx))
		for shift := uint(0); i < end; i, shift = i+1, shift+uint(bits) {
			word |= uint64(idx[i]) << shift
		}
		dst[l] = word
	}
}
