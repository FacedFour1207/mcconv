package zfast

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	kzlib "github.com/klauspost/compress/zlib"
)

func regionChunks(t testing.TB) [][]byte {
	files, _ := filepath.Glob("../../earth_1-500/region/*.mca")
	if len(files) == 0 {
		files, _ = filepath.Glob("../../test world(JE)/dimensions/minecraft/overworld/region/*.mca")
	}
	if len(files) == 0 {
		t.Skip("no region files")
	}
	var out [][]byte
	for _, f := range files[:min(2, len(files))] {
		data, _ := os.ReadFile(f)
		for i := 0; i < 1024 && len(data) >= 8192; i++ {
			o := binary.BigEndian.Uint32(data[i*4:])
			if o == 0 {
				continue
			}
			off := int(o>>8) * 4096
			n := int(binary.BigEndian.Uint32(data[off:]))
			if data[off+4] == 2 && off+4+n <= len(data) {
				out = append(out, data[off+5:off+4+n])
			}
		}
	}
	return out
}

func stdInflate(t testing.TB, src []byte) []byte {
	r, err := zlib.NewReader(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRegionChunks(t *testing.T) {
	var d Decoder
	var buf []byte
	chunks := regionChunks(t)
	for i, c := range chunks {
		want := stdInflate(t, c)
		got, err := d.Zlib(buf, c)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("chunk %d: err=%v len %d want %d", i, err, len(got), len(want))
		}
		buf = got
	}
	t.Logf("%d chunks ok", len(chunks))
}

func TestRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var d Decoder
	for iter := 0; iter < 300; iter++ {
		n := rng.Intn(200000)
		src := make([]byte, n)
		mode := iter % 3
		for i := range src {
			switch mode {
			case 0:
				src[i] = byte(rng.Intn(256))
			case 1:
				src[i] = byte(rng.Intn(4))
			default:
				if i > 10 && rng.Intn(4) > 0 {
					src[i] = src[i-1-rng.Intn(10)]
				} else {
					src[i] = byte(rng.Intn(30))
				}
			}
		}
		for _, level := range []int{flate.NoCompression, flate.BestSpeed, flate.DefaultCompression, flate.BestCompression, flate.HuffmanOnly} {
			var b bytes.Buffer
			w, _ := zlib.NewWriterLevel(&b, level)
			w.Write(src)
			w.Close()
			got, err := d.Zlib(nil, b.Bytes())
			if err != nil || !bytes.Equal(got, src) {
				t.Fatalf("iter %d level %d: err %v", iter, level, err)
			}
		}
	}
}

func BenchmarkZfast(b *testing.B) {
	chunks := regionChunks(b)
	var d Decoder
	var buf []byte
	total := 0
	for _, c := range chunks {
		total += len(stdInflate(b, c))
	}
	b.SetBytes(int64(total))
	b.ResetTimer()
	for range b.N {
		for _, c := range chunks {
			buf, _ = d.Zlib(buf, c)
		}
	}
}

func BenchmarkStdlib(b *testing.B) {
	chunks := regionChunks(b)
	total := 0
	for _, c := range chunks {
		total += len(stdInflate(b, c))
	}
	b.SetBytes(int64(total))
	var out bytes.Buffer
	var zr io.ReadCloser
	var src bytes.Reader
	b.ResetTimer()
	for range b.N {
		for _, c := range chunks {
			src.Reset(c)
			if zr == nil {
				zr, _ = zlib.NewReader(&src)
			} else {
				zr.(zlib.Resetter).Reset(&src, nil)
			}
			out.Reset()
			out.ReadFrom(zr)
		}
	}
}

func BenchmarkKlauspost(b *testing.B) {
	chunks := regionChunks(b)
	total := 0
	for _, c := range chunks {
		total += len(stdInflate(b, c))
	}
	b.SetBytes(int64(total))
	var out bytes.Buffer
	var zr io.ReadCloser
	var src bytes.Reader
	b.ResetTimer()
	for range b.N {
		for _, c := range chunks {
			src.Reset(c)
			if zr == nil {
				zr, _ = kzlib.NewReader(&src)
			} else {
				zr.(kzlib.Resetter).Reset(&src, nil)
			}
			out.Reset()
			out.ReadFrom(zr)
		}
	}
}
