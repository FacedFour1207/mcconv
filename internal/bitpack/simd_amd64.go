//go:build goexperiment.simd && amd64

package bitpack

import (
	"encoding/binary"
	"simd/archsimd"
)

// AVX2 versions. Unpacking broadcasts a word to all lanes and shifts each
// lane by a different amount; packing shifts 8 (4) consecutive values into
// place and ORs the lanes together. Lanes past the end of a word get a
// shift count of 32 (64), which yields 0.

func init() {
	if archsimd.X86.AVX2() {
		unpack32, pack32, unpack64, pack64 = unpack32AVX2, pack32AVX2, unpack64AVX2, pack64AVX2
		SIMD = true
	}
}

func shifts32(bits, g int) archsimd.Uint32x8 {
	p := 32 / bits
	var s [8]uint32
	for j := range s {
		if k := g*8 + j; k < p {
			s[j] = uint32(k * bits)
		} else {
			s[j] = 32
		}
	}
	return archsimd.LoadUint32x8(&s)
}

func unpack32AVX2(words []byte, bits int, out []uint32) {
	if bits > 6 { // few values per word: scalar is as fast
		unpack32Scalar(words, bits, out)
		return
	}
	p := 32 / bits
	groups := (p + 7) / 8
	var sh [4]archsimd.Uint32x8
	for g := 0; g < groups; g++ {
		sh[g] = shifts32(bits, g)
	}
	mask := archsimd.BroadcastUint32x8(1<<bits - 1)
	i, n := 0, len(out)
	for w := 0; w+4 <= len(words) && i < n; w += 4 {
		v := archsimd.BroadcastUint32x8(binary.LittleEndian.Uint32(words[w:]))
		for g := 0; g < groups && i < n; g++ {
			r := v.ShiftRight(sh[g]).And(mask)
			cnt := min(8, p-g*8)
			if i+8 <= n {
				r.StoreSlice(out[i:]) // extra lanes are overwritten by the next values
			} else {
				r.StoreSlicePart(out[i:min(i+cnt, n)])
			}
			i += cnt
		}
	}
	clear(out[min(i, n):])
}

func pack32AVX2(out []byte, idx []uint16, bits int) {
	if bits > 2 { // the unrolled scalar code is faster for 3+ bits (benchmarked)
		pack32Scalar(out, idx, bits)
		return
	}
	p := 32 / bits
	groups := (p + 7) / 8
	var sh [4]archsimd.Uint32x8
	for g := 0; g < groups; g++ {
		sh[g] = shifts32(bits, g)
	}
	full := len(idx) / p
	for w := 0; w < full; w++ {
		base := w * p
		var acc archsimd.Uint32x8
		for g := 0; g < groups; g++ {
			off := base + g*8
			var v archsimd.Uint16x8
			if off+8 <= len(idx) {
				v = archsimd.LoadUint16x8Slice(idx[off:])
			} else {
				v = archsimd.LoadUint16x8SlicePart(idx[off:])
			}
			acc = acc.Or(v.ExtendToUint32().ShiftLeft(sh[g]))
		}
		x := acc.GetLo().Or(acc.GetHi())
		x = x.Or(x.PermuteScalars(2, 3, 0, 1))
		x = x.Or(x.PermuteScalars(1, 0, 3, 2))
		binary.LittleEndian.PutUint32(out[w*4:], x.GetElem(0))
	}
	packTail32(out, idx, full, Words32(len(idx), bits), bits)
}

// shifts64 returns the shift counts of entries g*8 + 2j + odd.
func shifts64(bits, g, odd int) archsimd.Uint64x4 {
	p := 64 / bits
	var s [4]uint64
	for j := range s {
		if k := g*8 + 2*j + odd; k < p {
			s[j] = uint64(k * bits)
		} else {
			s[j] = 64
		}
	}
	return archsimd.LoadUint64x4(&s)
}

func unpack64AVX2(data []uint64, bits int, out []uint32) {
	p := 64 / bits
	groups := (p + 7) / 8
	var even, odd [8]archsimd.Uint64x4
	for g := 0; g < groups; g++ {
		even[g], odd[g] = shifts64(bits, g, 0), shifts64(bits, g, 1)
	}
	mask := archsimd.BroadcastUint64x4(1<<bits - 1)
	i, n := 0, len(out)
	for _, l := range data {
		if i >= n {
			break
		}
		v := archsimd.BroadcastUint64x4(l)
		for g := 0; g < groups && i < n; g++ {
			// even entries in the low, odd entries in the high halves: in
			// memory order that is 8 consecutive entries
			e := v.ShiftRight(even[g]).And(mask)
			o := v.ShiftRight(odd[g]).And(mask)
			r := e.Or(o.ShiftAllLeft(32)).AsUint32x8()
			cnt := min(8, p-g*8)
			if i+8 <= n {
				r.StoreSlice(out[i:])
			} else {
				r.StoreSlicePart(out[i:min(i+cnt, n)])
			}
			i += cnt
		}
	}
	clear(out[min(i, n):])
}

func shiftsPack64(bits, g int) archsimd.Uint64x4 {
	p := 64 / bits
	var s [4]uint64
	for j := range s {
		if k := g*4 + j; k < p {
			s[j] = uint64(k * bits)
		} else {
			s[j] = 64
		}
	}
	return archsimd.LoadUint64x4(&s)
}

func pack64AVX2(dst []uint64, idx []uint32, bits int) {
	p := 64 / bits
	groups := (p + 3) / 4
	var sh [16]archsimd.Uint64x4
	for g := 0; g < groups; g++ {
		sh[g] = shiftsPack64(bits, g)
	}
	full := min(len(idx)/p, len(dst))
	for l := 0; l < full; l++ {
		base := l * p
		var acc archsimd.Uint64x4
		for g := 0; g < groups; g++ {
			off := base + g*4
			var v archsimd.Uint32x4
			if off+4 <= len(idx) {
				v = archsimd.LoadUint32x4Slice(idx[off:])
			} else {
				v = archsimd.LoadUint32x4SlicePart(idx[off:])
			}
			acc = acc.Or(v.ExtendToUint64().ShiftLeft(sh[g]))
		}
		x := acc.GetLo().Or(acc.GetHi())
		dst[l] = x.GetElem(0) | x.GetElem(1)
	}
	if full < len(dst) {
		pack64Scalar(dst[full:], idx[min(full*p, len(idx)):], bits)
	}
}
