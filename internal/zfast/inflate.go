// Package zfast is a fast DEFLATE/zlib decoder for in-memory buffers.
//
// compress/flate reads its input one byte at a time through an interface;
// this decoder works on a []byte with a 64-bit bit buffer and two-level
// Huffman lookup tables, which is several times faster for the small zlib
// streams found in region files.
package zfast

import (
	"encoding/binary"
	"errors"
	"math/bits"
)

var (
	errCorrupt = errors.New("zfast: corrupt deflate stream")
	errHeader  = errors.New("zfast: bad zlib header")
)

const (
	litBits  = 10 // primary table bits for literal/length codes
	distBits = 8  // primary table bits for distance codes
)

// table entry layout (uint32):
//
//	bits 0-7   : number of bits to consume (code length, or primary bits for links)
//	bits 8-15  : kind: 0 = literal/symbol, 1 = link to subtable, 2 = invalid
//	bits 16-31 : symbol, or subtable offset for links
//
// for links, bits 0-7 hold the subtable index width (extra bits after the primary).
type table struct {
	entries []uint32
	bits    uint
}

const (
	kindSym  = 0
	kindLink = 1
	kindBad  = 2
)

// build constructs a canonical Huffman decoding table from code lengths.
func (t *table) build(lengths []uint8, primary uint) error {
	var count [16]int
	maxLen := 0
	for _, l := range lengths {
		count[l]++
		if int(l) > maxLen {
			maxLen = int(l)
		}
	}
	count[0] = 0
	if maxLen == 0 {
		// no codes: every lookup is invalid
		t.bits = 1
		t.entries = append(t.entries[:0], kindBad<<8|1, kindBad<<8|1)
		return nil
	}
	// check for over-subscription
	left := 1
	for l := 1; l <= 15; l++ {
		left <<= 1
		left -= count[l]
		if left < 0 {
			return errCorrupt
		}
	}
	var next [16]int
	code := 0
	for l := 1; l <= 15; l++ {
		code = (code + count[l-1]) << 1
		next[l] = code
	}
	if uint(maxLen) < primary {
		primary = uint(maxLen)
	}
	t.bits = primary
	size := 1 << primary
	t.entries = t.entries[:0]
	for i := 0; i < size; i++ {
		t.entries = append(t.entries, kindBad<<8|uint32(primary))
	}
	// codes are stored bit-reversed because deflate packs them MSB first
	type long struct {
		rev uint32
		len uint8
		sym uint16
	}
	longs := make([]long, 0, 64)
	for sym, l := range lengths {
		if l == 0 {
			continue
		}
		c := next[l]
		next[l]++
		rev := reverse(uint32(c), uint(l))
		if uint(l) <= primary {
			e := uint32(sym)<<16 | kindSym<<8 | uint32(l)
			for i := rev; i < uint32(size); i += 1 << l {
				t.entries[i] = e
			}
		} else {
			longs = append(longs, long{rev, l, uint16(sym)})
		}
	}
	if len(longs) == 0 {
		return nil
	}
	// subtables keyed by the low `primary` bits of the reversed code
	mask := uint32(size - 1)
	var subBits [1 << 11]uint8 // primary <= 10 bits
	var subOff [1 << 11]int32
	for _, lc := range longs {
		p := lc.rev & mask
		if extra := lc.len - uint8(primary); extra > subBits[p] {
			subBits[p] = extra
		}
	}
	for _, lc := range longs {
		p := lc.rev & mask
		if subOff[p] != 0 {
			continue
		}
		extra := subBits[p]
		off := len(t.entries)
		subOff[p] = int32(off)
		for i := 0; i < 1<<extra; i++ {
			t.entries = append(t.entries, kindBad<<8|uint32(extra))
		}
		t.entries[p] = uint32(off)<<16 | kindLink<<8 | uint32(extra)
	}
	for _, lc := range longs {
		p := lc.rev & mask
		extra := uint(subBits[p])
		off := int(subOff[p])
		rest := lc.rev >> primary
		l := uint(lc.len) - primary
		e := uint32(lc.sym)<<16 | kindSym<<8 | uint32(l)
		for i := rest; i < 1<<extra; i += 1 << l {
			t.entries[off+int(i)] = e
		}
	}
	return nil
}

func reverse(v uint32, n uint) uint32 {
	return uint32(bits.Reverse16(uint16(v))) >> (16 - n)
}

// Decoder decompresses streams; reuse it to avoid allocations.
type Decoder struct {
	in    []byte
	pos   int
	bits  uint64
	nbits uint
	out   []byte

	lit, dist, clen table
	lengths         [320]uint8
	fixedLit        table
	fixedDist       table
	fixedReady      bool
}

func (d *Decoder) refill() {
	if d.pos+8 <= len(d.in) {
		v := binary.LittleEndian.Uint64(d.in[d.pos:])
		d.bits |= v << d.nbits
		n := (63 - d.nbits) >> 3
		d.pos += int(n)
		d.nbits += n * 8
		return
	}
	for d.nbits <= 56 && d.pos < len(d.in) {
		d.bits |= uint64(d.in[d.pos]) << d.nbits
		d.pos++
		d.nbits += 8
	}
}

func (d *Decoder) need(n uint) error {
	if d.nbits < n {
		d.refill()
		if d.nbits < n {
			return errCorrupt
		}
	}
	return nil
}

func (d *Decoder) take(n uint) uint32 {
	v := uint32(d.bits & (1<<n - 1))
	d.bits >>= n
	d.nbits -= n
	return v
}

// decodeSym reads one Huffman symbol.
func (d *Decoder) decodeSym(t *table) (int, error) {
	if d.nbits < 15 {
		d.refill()
	}
	e := t.entries[d.bits&(1<<t.bits-1)]
	switch e >> 8 & 0xFF {
	case kindSym:
		n := uint(e & 0xFF)
		if n > d.nbits {
			return 0, errCorrupt
		}
		d.bits >>= n
		d.nbits -= n
		return int(e >> 16), nil
	case kindLink:
		if t.bits > d.nbits {
			return 0, errCorrupt
		}
		extra := uint(e & 0xFF)
		idx := (d.bits >> t.bits) & (1<<extra - 1)
		e2 := t.entries[int(e>>16)+int(idx)]
		if e2>>8&0xFF != kindSym {
			return 0, errCorrupt
		}
		n := t.bits + uint(e2&0xFF)
		if n > d.nbits {
			return 0, errCorrupt
		}
		d.bits >>= n
		d.nbits -= n
		return int(e2 >> 16), nil
	}
	return 0, errCorrupt
}

var lenBase = [29]uint16{3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31, 35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258}
var lenExtra = [29]uint8{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}
var distBase = [30]uint16{1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193, 257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577}
var distExtra = [30]uint8{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13}
var clenOrder = [19]uint8{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

// Zlib decompresses a zlib stream, appending to dst (reused) and returning
// the result. The Adler-32 trailer is not verified.
func (d *Decoder) Zlib(dst, src []byte) ([]byte, error) {
	if len(src) < 2 || src[0]&0x0F != 8 || (uint16(src[0])<<8|uint16(src[1]))%31 != 0 || src[1]&0x20 != 0 {
		return dst, errHeader
	}
	return d.Raw(dst, src[2:])
}

// Raw decompresses a raw DEFLATE stream.
func (d *Decoder) Raw(dst, src []byte) ([]byte, error) {
	d.in, d.pos, d.bits, d.nbits, d.out = src, 0, 0, 0, dst[:0]
	for {
		if err := d.need(3); err != nil {
			return d.out, err
		}
		final := d.take(1)
		typ := d.take(2)
		var err error
		switch typ {
		case 0:
			err = d.stored()
		case 1:
			d.initFixed()
			err = d.block(&d.fixedLit, &d.fixedDist)
		case 2:
			if err = d.dynamicTables(); err == nil {
				err = d.block(&d.lit, &d.dist)
			}
		default:
			err = errCorrupt
		}
		if err != nil {
			return d.out, err
		}
		if final == 1 {
			return d.out, nil
		}
	}
}

func (d *Decoder) stored() error {
	// discard to byte boundary, then rewind unread whole bytes
	d.take(d.nbits & 7)
	d.pos -= int(d.nbits / 8)
	d.bits, d.nbits = 0, 0
	if d.pos+4 > len(d.in) {
		return errCorrupt
	}
	n := int(binary.LittleEndian.Uint16(d.in[d.pos:]))
	nn := int(binary.LittleEndian.Uint16(d.in[d.pos+2:]))
	if n != ^nn&0xFFFF {
		return errCorrupt
	}
	d.pos += 4
	if d.pos+n > len(d.in) {
		return errCorrupt
	}
	d.out = append(d.out, d.in[d.pos:d.pos+n]...)
	d.pos += n
	return nil
}

func (d *Decoder) initFixed() {
	if d.fixedReady {
		return
	}
	var l [288]uint8
	for i := range l {
		switch {
		case i < 144:
			l[i] = 8
		case i < 256:
			l[i] = 9
		case i < 280:
			l[i] = 7
		default:
			l[i] = 8
		}
	}
	d.fixedLit.build(l[:], litBits)
	var dl [30]uint8
	for i := range dl {
		dl[i] = 5
	}
	d.fixedDist.build(dl[:], distBits)
	d.fixedReady = true
}

func (d *Decoder) dynamicTables() error {
	if err := d.need(14); err != nil {
		return err
	}
	hlit := int(d.take(5)) + 257
	hdist := int(d.take(5)) + 1
	hclen := int(d.take(4)) + 4
	if hlit > 286 || hdist > 30 {
		return errCorrupt
	}
	var cl [19]uint8
	for i := 0; i < hclen; i++ {
		if err := d.need(3); err != nil {
			return err
		}
		cl[clenOrder[i]] = uint8(d.take(3))
	}
	if err := d.clen.build(cl[:], 7); err != nil {
		return err
	}
	n := hlit + hdist
	lens := d.lengths[:n]
	for i := 0; i < n; {
		sym, err := d.decodeSym(&d.clen)
		if err != nil {
			return err
		}
		switch {
		case sym < 16:
			lens[i] = uint8(sym)
			i++
		case sym == 16:
			if i == 0 {
				return errCorrupt
			}
			if err := d.need(2); err != nil {
				return err
			}
			rep := 3 + int(d.take(2))
			if i+rep > n {
				return errCorrupt
			}
			prev := lens[i-1]
			for ; rep > 0; rep-- {
				lens[i] = prev
				i++
			}
		default:
			var rep int
			if sym == 17 {
				if err := d.need(3); err != nil {
					return err
				}
				rep = 3 + int(d.take(3))
			} else {
				if err := d.need(7); err != nil {
					return err
				}
				rep = 11 + int(d.take(7))
			}
			if i+rep > n {
				return errCorrupt
			}
			for ; rep > 0; rep-- {
				lens[i] = 0
				i++
			}
		}
	}
	if lens[256] == 0 {
		return errCorrupt
	}
	if err := d.lit.build(lens[:hlit], litBits); err != nil {
		return err
	}
	return d.dist.build(lens[hlit:n], distBits)
}

// block decodes one compressed block. The hot loop keeps the bit buffer and
// output position in locals (no closures, so they stay in registers) and
// writes into pre-sized output.
func (d *Decoder) block(lit, dist *table) (err error) {
	in := d.in
	pos := d.pos
	bitbuf := d.bits
	nbits := d.nbits
	out := d.out
	o := len(out)
	out = out[:cap(out)]
	if len(out)-o < 1024 {
		out = grow(out, o, max(len(in)*4, 64<<10))
	}
	litE, litBitsN := lit.entries, lit.bits
	distE, distBitsN := dist.entries, dist.bits
	litMask := uint64(1)<<litBitsN - 1
	distMask := uint64(1)<<distBitsN - 1
	for {
		if nbits < 48 {
			if pos+8 <= len(in) {
				bitbuf |= binary.LittleEndian.Uint64(in[pos:]) << nbits
				n := (63 - nbits) >> 3
				pos += int(n)
				nbits += n * 8
			} else {
				for nbits <= 56 && pos < len(in) {
					bitbuf |= uint64(in[pos]) << nbits
					pos++
					nbits += 8
				}
			}
		}
		e := litE[bitbuf&litMask]
		var sym int
		if k := e >> 8 & 0xFF; k == kindSym {
			n := uint(e & 0xFF)
			if n > nbits {
				err = errCorrupt
				break
			}
			bitbuf >>= n
			nbits -= n
			sym = int(e >> 16)
		} else if k == kindLink {
			e2 := litE[int(e>>16)+int((bitbuf>>litBitsN)&(1<<(e&0xFF)-1))]
			n := litBitsN + uint(e2&0xFF)
			if e2>>8&0xFF != kindSym || n > nbits {
				err = errCorrupt
				break
			}
			bitbuf >>= n
			nbits -= n
			sym = int(e2 >> 16)
		} else {
			err = errCorrupt
			break
		}
		if sym < 256 {
			if o >= len(out) {
				out = grow(out, o, len(out))
			}
			out[o] = byte(sym)
			o++
			continue
		}
		if sym == 256 {
			break
		}
		sym -= 257
		if sym >= 29 {
			err = errCorrupt
			break
		}
		length := int(lenBase[sym])
		if eb := uint(lenExtra[sym]); eb > 0 {
			length += int(bitbuf & (1<<eb - 1))
			bitbuf >>= eb
			nbits -= eb
		}
		if nbits < 32 {
			if pos+8 <= len(in) {
				bitbuf |= binary.LittleEndian.Uint64(in[pos:]) << nbits
				n := (63 - nbits) >> 3
				pos += int(n)
				nbits += n * 8
			} else {
				for nbits <= 56 && pos < len(in) {
					bitbuf |= uint64(in[pos]) << nbits
					pos++
					nbits += 8
				}
			}
		}
		e = distE[bitbuf&distMask]
		var ds int
		if k := e >> 8 & 0xFF; k == kindSym {
			n := uint(e & 0xFF)
			if n > nbits {
				err = errCorrupt
				break
			}
			bitbuf >>= n
			nbits -= n
			ds = int(e >> 16)
		} else if k == kindLink {
			e2 := distE[int(e>>16)+int((bitbuf>>distBitsN)&(1<<(e&0xFF)-1))]
			n := distBitsN + uint(e2&0xFF)
			if e2>>8&0xFF != kindSym || n > nbits {
				err = errCorrupt
				break
			}
			bitbuf >>= n
			nbits -= n
			ds = int(e2 >> 16)
		} else {
			err = errCorrupt
			break
		}
		if ds >= 30 {
			err = errCorrupt
			break
		}
		distance := int(distBase[ds])
		if eb := uint(distExtra[ds]); eb > 0 {
			if nbits < eb {
				if pos+8 <= len(in) {
					bitbuf |= binary.LittleEndian.Uint64(in[pos:]) << nbits
					n := (63 - nbits) >> 3
					pos += int(n)
					nbits += n * 8
				} else {
					for nbits <= 56 && pos < len(in) {
						bitbuf |= uint64(in[pos]) << nbits
						pos++
						nbits += 8
					}
				}
			}
			if nbits < eb {
				err = errCorrupt
				break
			}
			distance += int(bitbuf & (1<<eb - 1))
			bitbuf >>= eb
			nbits -= eb
		}
		if distance > o {
			err = errCorrupt
			break
		}
		if o+length > len(out) {
			out = grow(out, o, max(len(out), length))
		}
		start := o - distance
		if distance >= length {
			copy(out[o:o+length], out[start:start+length])
		} else if distance == 1 {
			b := out[start]
			seg := out[o : o+length]
			for i := range seg {
				seg[i] = b
			}
		} else {
			for i := 0; i < length; i++ {
				out[o+i] = out[start+i]
			}
		}
		o += length
	}
	d.pos, d.bits, d.nbits = pos, bitbuf, nbits
	d.out = out[:o]
	return err
}

// grow returns a buffer with at least extra free bytes after o.
func grow(buf []byte, o, extra int) []byte {
	nb := make([]byte, o+extra+o/2)
	copy(nb, buf[:o])
	return nb
}
