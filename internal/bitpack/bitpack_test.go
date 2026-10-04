package bitpack

import (
	"math/rand"
	"slices"
	"testing"
)

// The exported functions (SIMD when available) must match the scalar code
// for every width, including partial last words.
func TestMatchesScalar(t *testing.T) {
	t.Logf("SIMD: %v", SIMD)
	rng := rand.New(rand.NewSource(1))
	for _, n := range []int{4096, 64, 4095, 7} {
		for bits := 1; bits <= 16; bits++ {
			idx16 := make([]uint16, n)
			idx32 := make([]uint32, n)
			for i := range idx16 {
				v := rng.Intn(1 << bits)
				idx16[i], idx32[i] = uint16(v), uint32(v)
			}
			// 32-bit words
			got := make([]byte, Words32(n, bits)*4)
			want := make([]byte, len(got))
			Pack32(got, idx16, bits)
			pack32Scalar(want, idx16, bits)
			if !slices.Equal(got, want) {
				t.Fatalf("Pack32 bits=%d n=%d differs", bits, n)
			}
			back := make([]uint32, n)
			Unpack32(got, bits, back)
			if !slices.Equal(back, idx32) {
				t.Fatalf("Unpack32 bits=%d n=%d differs", bits, n)
			}
			// 64-bit longs
			gl := make([]uint64, Words64(n, bits))
			wl := make([]uint64, len(gl))
			Pack64(gl, idx32, bits)
			pack64Scalar(wl, idx32, bits)
			if !slices.Equal(gl, wl) {
				t.Fatalf("Pack64 bits=%d n=%d differs", bits, n)
			}
			back64 := make([]uint32, n)
			Unpack64(gl, bits, back64)
			if !slices.Equal(back64, idx32) {
				t.Fatalf("Unpack64 bits=%d n=%d differs", bits, n)
			}
			// truncated input: missing values are 0
			short := make([]uint32, n)
			Unpack64(gl[:len(gl)/2], bits, short)
			ref := make([]uint32, n)
			unpack64Scalar(gl[:len(gl)/2], bits, ref)
			if !slices.Equal(short, ref) {
				t.Fatalf("Unpack64 truncated bits=%d n=%d differs", bits, n)
			}
		}
	}
}

func benchData(bits int) ([]uint16, []uint32) {
	rng := rand.New(rand.NewSource(1))
	a, b := make([]uint16, 4096), make([]uint32, 4096)
	for i := range a {
		v := rng.Intn(1 << bits)
		a[i], b[i] = uint16(v), uint32(v)
	}
	return a, b
}

func BenchmarkUnpack32(b *testing.B) {
	for _, bits := range []int{1, 2, 3, 4, 5} {
		idx, _ := benchData(bits)
		words := make([]byte, Words32(4096, bits)*4)
		pack32Scalar(words, idx, bits)
		out := make([]uint32, 4096)
		b.Run("simd/"+string(rune('0'+bits)), func(b *testing.B) {
			for b.Loop() {
				Unpack32(words, bits, out)
			}
		})
		b.Run("scalar/"+string(rune('0'+bits)), func(b *testing.B) {
			for b.Loop() {
				unpack32Scalar(words, bits, out)
			}
		})
	}
}

func BenchmarkPack32(b *testing.B) {
	for _, bits := range []int{1, 2, 3, 4, 5} {
		idx, _ := benchData(bits)
		out := make([]byte, Words32(4096, bits)*4)
		b.Run("simd/"+string(rune('0'+bits)), func(b *testing.B) {
			for b.Loop() {
				Pack32(out, idx, bits)
			}
		})
		b.Run("scalar/"+string(rune('0'+bits)), func(b *testing.B) {
			for b.Loop() {
				pack32Scalar(out, idx, bits)
			}
		})
	}
}

func BenchmarkUnpack64(b *testing.B) {
	for _, bits := range []int{4, 5, 6} {
		_, idx := benchData(bits)
		data := make([]uint64, Words64(4096, bits))
		pack64Scalar(data, idx, bits)
		out := make([]uint32, 4096)
		b.Run("simd/"+string(rune('0'+bits)), func(b *testing.B) {
			for b.Loop() {
				Unpack64(data, bits, out)
			}
		})
		b.Run("scalar/"+string(rune('0'+bits)), func(b *testing.B) {
			for b.Loop() {
				unpack64Scalar(data, bits, out)
			}
		})
	}
}

func BenchmarkPack64(b *testing.B) {
	for _, bits := range []int{4, 5, 6} {
		_, idx := benchData(bits)
		data := make([]uint64, Words64(4096, bits))
		b.Run("simd/"+string(rune('0'+bits)), func(b *testing.B) {
			for b.Loop() {
				Pack64(data, idx, bits)
			}
		})
		b.Run("scalar/"+string(rune('0'+bits)), func(b *testing.B) {
			for b.Loop() {
				pack64Scalar(data, idx, bits)
			}
		})
	}
}
