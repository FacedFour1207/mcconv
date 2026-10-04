package deflate

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"io"
	"math/rand"
	"testing"
)

func testData() [][]byte {
	rng := rand.New(rand.NewSource(1))
	var out [][]byte
	for _, n := range []int{0, 1, 100, 5000, 70000, 600000} {
		b := make([]byte, n)
		for i := range b { // compressible: few distinct bytes with runs
			if i > 0 && rng.Intn(4) != 0 {
				b[i] = b[i-1]
			} else {
				b[i] = byte(rng.Intn(16))
			}
		}
		out = append(out, b)
	}
	return out
}

// Streams written by this package are standard (read by the Go standard
// library) and streams from the standard library are read back correctly.
func TestRoundTrip(t *testing.T) {
	t.Logf("backend: %s", Backend)
	for _, data := range testData() {
		for _, level := range []int{-2, 0, 1, 6, 9} {
			z := CompressZlib(nil, data, level)
			r, err := zlib.NewReader(bytes.NewReader(z))
			if err != nil {
				t.Fatalf("level %d len %d: %v", level, len(data), err)
			}
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("zlib level %d len %d: stdlib read mismatch (%v)", level, len(data), err)
			}
			if got, err := Zlib(nil, z); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("zlib level %d len %d: round trip mismatch (%v)", level, len(data), err)
			}
			raw := CompressRaw(make([]byte, 3), data, level)
			got, err = io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("raw level %d len %d: stdlib read mismatch (%v)", level, len(data), err)
			}
			// a too small destination is grown
			if got, err := Raw(make([]byte, 0, 1), raw); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("raw level %d len %d: round trip mismatch (%v)", level, len(data), err)
			}
		}
	}
	if _, err := Zlib(nil, []byte{0x78, 0x9c, 1, 2, 3}); err == nil {
		t.Error("corrupt input accepted")
	}
}
