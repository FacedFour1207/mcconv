// Command nbtdump prints an NBT file as SNBT (gzip or raw, big or little
// endian): go run ./tools/nbtdump [-le] [-n max chars] file
package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"

	"mcconv/internal/nbt"
)

func main() {
	le := flag.Bool("le", false, "little endian")
	max := flag.Int("n", 4000, "max output characters")
	flag.Parse()
	data, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		panic(err)
	}
	if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, _ := gzip.NewReader(bytes.NewReader(data))
		data, _ = io.ReadAll(zr)
	}
	enc := nbt.BigEndian
	if *le {
		enc = nbt.LittleEndian
	}
	for len(data) > 0 {
		name, v, n, err := nbt.Decode(data, enc)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		s := nbt.ToSNBT(v)
		if len(s) > *max {
			s = s[:*max] + "..."
		}
		fmt.Printf("%q: %s\n", name, s)
		data = data[n:]
	}
}
