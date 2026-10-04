package bedrock

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

func refPack(idx []uint16, bitsPer int) []byte {
	out := []byte{byte(bitsPer << 1)}
	perWord := 32 / bitsPer
	words := (4096 + perWord - 1) / perWord
	i := 0
	for w := 0; w < words; w++ {
		var word uint32
		for j := 0; j < perWord && i < 4096; j++ {
			word |= uint32(idx[i]) << (uint(j) * uint(bitsPer))
			i++
		}
		out = binary.LittleEndian.AppendUint32(out, word)
	}
	return out
}

func TestPackAllWidths(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, b := range []int{1, 2, 3, 4, 5, 6, 8, 16} {
		idx := make([]uint16, 4096)
		for i := range idx {
			idx[i] = uint16(rng.Intn(1 << b))
		}
		got := appendPackedXZY(nil, idx, b, 0)
		if want := refPack(idx, b); !bytes.Equal(got, want) {
			t.Fatalf("bits %d: packing differs", b)
		}
		// round trip through the decoder
		st := &storage{}
		_, err := (&Decoder{}).decodeStorageInto(st, append(got, 0, 0, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		for i := range idx {
			if st.indices[i] != uint32(idx[i]) {
				t.Fatalf("bits %d: index %d", b, i)
			}
		}
	}
}
