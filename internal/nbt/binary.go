package nbt

import (
	"encoding/binary"
	"math"
	"unicode/utf8"
)

// Encoding selects the binary flavour.
type Encoding int

const (
	// BigEndian is the Java Edition encoding (strings are modified UTF-8).
	BigEndian Encoding = iota
	// LittleEndian is the Bedrock Edition disk encoding (strings are raw UTF-8).
	LittleEndian
)

const maxDepth = 512

type decoder struct {
	buf   []byte
	off   int
	le    bool
	depth int
	skip  map[string]bool
}

// Decode reads one named root tag. It returns the root name, the value and the
// number of bytes consumed.
func Decode(data []byte, enc Encoding) (name string, value any, n int, err error) {
	d := decoder{buf: data, le: enc == LittleEndian}
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(decodeError); ok {
				err = e.err
				return
			}
			panic(r)
		}
	}()
	t := d.u8()
	if t == TagEnd {
		return "", nil, d.off, nil
	}
	name = d.str()
	value = d.payload(t)
	return name, value, d.off, nil
}

// DecodeCompound reads a root tag that must be a compound.
func DecodeCompound(data []byte, enc Encoding) (Compound, int, error) {
	_, v, n, err := Decode(data, enc)
	if err != nil {
		return nil, n, err
	}
	c, ok := v.(Compound)
	if !ok {
		return nil, n, errorf("root tag is not a compound")
	}
	return c, n, nil
}

// DecodeCompoundSkip is DecodeCompound but compound entries whose key is in
// skip are not decoded (at any depth). Used to avoid allocating light data,
// heightmaps etc. that the converter does not need.
func DecodeCompoundSkip(data []byte, enc Encoding, skip map[string]bool) (c Compound, n int, err error) {
	d := decoder{buf: data, le: enc == LittleEndian, skip: skip}
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(decodeError); ok {
				err = e.err
				return
			}
			panic(r)
		}
	}()
	t := d.u8()
	if t != TagCompound {
		return nil, d.off, errorf("root tag is not a compound")
	}
	d.str()
	c, _ = d.payload(t).(Compound)
	return c, d.off, nil
}

// skipPayload advances over a payload without decoding it.
func (d *decoder) skipPayload(t byte) {
	switch t {
	case TagByte:
		d.need(1)
	case TagShort:
		d.need(2)
	case TagInt, TagFloat:
		d.need(4)
	case TagLong, TagDouble:
		d.need(8)
	case TagByteArray:
		d.need(d.length())
	case TagString:
		d.need(int(d.u16()))
	case TagIntArray:
		d.need(d.length() * 4)
	case TagLongArray:
		d.need(d.length() * 8)
	case TagList:
		et := d.u8()
		n := int(int32(d.u32()))
		for i := 0; i < n; i++ {
			d.skipPayload(et)
		}
	case TagCompound:
		for {
			ct := d.u8()
			if ct == TagEnd {
				return
			}
			d.need(int(d.u16()))
			d.skipPayload(ct)
		}
	default:
		d.fail("unknown tag type %d", t)
	}
}

// DecodeAll reads consecutive root compounds until the data is exhausted
// (the Bedrock block entity / pending tick list format).
func DecodeAll(data []byte, enc Encoding) ([]Compound, error) {
	var out []Compound
	for len(data) > 0 {
		c, n, err := DecodeCompound(data, enc)
		if err != nil {
			return out, err
		}
		if n == 0 {
			break
		}
		out = append(out, c)
		data = data[n:]
	}
	return out, nil
}

type decodeError struct{ err error }

func (d *decoder) fail(format string, args ...any) {
	panic(decodeError{errorf(format, args...)})
}

func (d *decoder) need(n int) []byte {
	if n < 0 || d.off+n > len(d.buf) {
		d.fail("unexpected end of data at %d (need %d)", d.off, n)
	}
	b := d.buf[d.off : d.off+n]
	d.off += n
	return b
}

func (d *decoder) u8() byte { return d.need(1)[0] }

func (d *decoder) u16() uint16 {
	b := d.need(2)
	if d.le {
		return binary.LittleEndian.Uint16(b)
	}
	return binary.BigEndian.Uint16(b)
}

func (d *decoder) u32() uint32 {
	b := d.need(4)
	if d.le {
		return binary.LittleEndian.Uint32(b)
	}
	return binary.BigEndian.Uint32(b)
}

func (d *decoder) u64() uint64 {
	b := d.need(8)
	if d.le {
		return binary.LittleEndian.Uint64(b)
	}
	return binary.BigEndian.Uint64(b)
}

func (d *decoder) length() int {
	n := int32(d.u32())
	if n < 0 {
		return 0
	}
	if int(n) > len(d.buf)-d.off {
		d.fail("length %d exceeds remaining data", n)
	}
	return int(n)
}

func (d *decoder) str() string {
	n := int(d.u16())
	b := d.need(n)
	if d.le {
		return string(b)
	}
	return decodeMUTF8(b)
}

func (d *decoder) payload(t byte) any {
	switch t {
	case TagByte:
		return int8(d.u8())
	case TagShort:
		return int16(d.u16())
	case TagInt:
		return int32(d.u32())
	case TagLong:
		return int64(d.u64())
	case TagFloat:
		return math.Float32frombits(d.u32())
	case TagDouble:
		return math.Float64frombits(d.u64())
	case TagByteArray:
		n := d.length()
		return append([]byte(nil), d.need(n)...)
	case TagString:
		return d.str()
	case TagList:
		d.depth++
		if d.depth > maxDepth {
			d.fail("nesting too deep")
		}
		et := d.u8()
		n := int(int32(d.u32()))
		if n < 0 {
			n = 0
		}
		if n > len(d.buf)-d.off && et != TagEnd {
			d.fail("list length %d exceeds remaining data", n)
		}
		l := &List{Type: et}
		if et == TagEnd {
			n = 0
		}
		if n > 0 {
			l.Items = make([]any, n)
			for i := range n {
				l.Items[i] = d.payload(et)
			}
		}
		d.depth--
		return l
	case TagCompound:
		d.depth++
		if d.depth > maxDepth {
			d.fail("nesting too deep")
		}
		c := Compound{}
		for {
			ct := d.u8()
			if ct == TagEnd {
				break
			}
			k := d.str()
			if d.skip != nil && d.skip[k] {
				d.skipPayload(ct)
				continue
			}
			c[k] = d.payload(ct)
		}
		d.depth--
		return c
	case TagIntArray:
		n := d.length() // element count; bounds checked again by need
		b := d.need(n * 4)
		out := make([]int32, n)
		if d.le {
			for i := range out {
				out[i] = int32(binary.LittleEndian.Uint32(b[i*4:]))
			}
		} else {
			for i := range out {
				out[i] = int32(binary.BigEndian.Uint32(b[i*4:]))
			}
		}
		return out
	case TagLongArray:
		n := d.length()
		b := d.need(n * 8)
		out := make([]int64, n)
		if d.le {
			for i := range out {
				out[i] = int64(binary.LittleEndian.Uint64(b[i*8:]))
			}
		} else {
			for i := range out {
				out[i] = int64(binary.BigEndian.Uint64(b[i*8:]))
			}
		}
		return out
	}
	d.fail("unknown tag type %d at %d", t, d.off)
	return nil
}

// Encoder appends NBT to a byte slice.
type encoder struct {
	buf []byte
	le  bool
}

// Encode serialises a named root tag.
func Encode(name string, v any, enc Encoding) []byte {
	return AppendEncode(nil, name, v, enc)
}

// AppendEncode appends a named root tag to dst.
func AppendEncode(dst []byte, name string, v any, enc Encoding) []byte {
	e := encoder{buf: dst, le: enc == LittleEndian}
	t := TypeOf(v)
	e.buf = append(e.buf, t)
	e.str(name)
	e.payload(v)
	return e.buf
}

func (e *encoder) u16(v uint16) {
	if e.le {
		e.buf = binary.LittleEndian.AppendUint16(e.buf, v)
	} else {
		e.buf = binary.BigEndian.AppendUint16(e.buf, v)
	}
}

func (e *encoder) u32(v uint32) {
	if e.le {
		e.buf = binary.LittleEndian.AppendUint32(e.buf, v)
	} else {
		e.buf = binary.BigEndian.AppendUint32(e.buf, v)
	}
}

func (e *encoder) u64(v uint64) {
	if e.le {
		e.buf = binary.LittleEndian.AppendUint64(e.buf, v)
	} else {
		e.buf = binary.BigEndian.AppendUint64(e.buf, v)
	}
}

func (e *encoder) str(s string) {
	if !e.le {
		if b, ok := encodeMUTF8(s); ok {
			if len(b) > 0xFFFF {
				b = b[:0xFFFF]
			}
			e.u16(uint16(len(b)))
			e.buf = append(e.buf, b...)
			return
		}
	}
	if len(s) > 0xFFFF {
		s = s[:0xFFFF]
	}
	e.u16(uint16(len(s)))
	e.buf = append(e.buf, s...)
}

func (e *encoder) payload(v any) {
	switch t := v.(type) {
	case int8:
		e.buf = append(e.buf, byte(t))
	case int16:
		e.u16(uint16(t))
	case int32:
		e.u32(uint32(t))
	case int64:
		e.u64(uint64(t))
	case float32:
		e.u32(math.Float32bits(t))
	case float64:
		e.u64(math.Float64bits(t))
	case []byte:
		e.u32(uint32(len(t)))
		e.buf = append(e.buf, t...)
	case string:
		e.str(t)
	case *List:
		if t == nil || len(t.Items) == 0 {
			typ := TagEnd
			if t != nil {
				typ = t.Type
			}
			e.buf = append(e.buf, typ)
			e.u32(0)
			return
		}
		typ := t.Type
		if typ == TagEnd {
			typ = TypeOf(t.Items[0])
		}
		e.buf = append(e.buf, typ)
		e.u32(uint32(len(t.Items)))
		for _, it := range t.Items {
			if TypeOf(it) != typ {
				it = Zero(typ)
			}
			e.payload(it)
		}
	case Compound:
		keys := t.SortedKeys()
		for _, k := range keys {
			val := t[k]
			tt := TypeOf(val)
			if tt == TagEnd {
				continue
			}
			e.buf = append(e.buf, tt)
			e.str(k)
			e.payload(val)
		}
		e.buf = append(e.buf, TagEnd)
	case []int32:
		e.u32(uint32(len(t)))
		for _, x := range t {
			e.u32(uint32(x))
		}
	case []int64:
		e.u32(uint32(len(t)))
		for _, x := range t {
			e.u64(uint64(x))
		}
	default:
		// unsupported values are written as an empty compound to keep the stream valid
		e.buf = append(e.buf, TagEnd)
	}
}

// decodeMUTF8 converts Java's modified UTF-8 to a Go string.
func decodeMUTF8(b []byte) string {
	simple := true
	for _, c := range b {
		if c >= 0x80 {
			simple = false
			break
		}
	}
	if simple {
		return string(b)
	}
	out := make([]rune, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c < 0x80:
			out = append(out, rune(c))
			i++
		case c&0xE0 == 0xC0 && i+1 < len(b):
			out = append(out, rune(c&0x1F)<<6|rune(b[i+1]&0x3F))
			i += 2
		case c&0xF0 == 0xE0 && i+2 < len(b):
			r := rune(c&0x0F)<<12 | rune(b[i+1]&0x3F)<<6 | rune(b[i+2]&0x3F)
			i += 3
			// combine surrogate pairs
			if r >= 0xD800 && r <= 0xDBFF && i+2 < len(b) && b[i] == 0xED {
				r2 := rune(b[i]&0x0F)<<12 | rune(b[i+1]&0x3F)<<6 | rune(b[i+2]&0x3F)
				if r2 >= 0xDC00 && r2 <= 0xDFFF {
					r = 0x10000 + (r-0xD800)<<10 + (r2 - 0xDC00)
					i += 3
				}
			}
			out = append(out, r)
		default:
			out = append(out, utf8.RuneError)
			i++
		}
	}
	return string(out)
}

// encodeMUTF8 returns the modified UTF-8 form, or ok=false if s is plain ASCII
// without NUL (in which case the bytes are identical).
func encodeMUTF8(s string) ([]byte, bool) {
	special := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 || s[i] == 0 {
			special = true
			break
		}
	}
	if !special {
		return nil, false
	}
	out := make([]byte, 0, len(s)+8)
	for _, r := range s {
		switch {
		case r != 0 && r < 0x80:
			out = append(out, byte(r))
		case r < 0x800:
			out = append(out, byte(0xC0|r>>6), byte(0x80|r&0x3F))
		case r < 0x10000:
			out = append(out, byte(0xE0|r>>12), byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
		default:
			r -= 0x10000
			hi, lo := 0xD800+(r>>10), 0xDC00+(r&0x3FF)
			out = append(out, byte(0xE0|hi>>12), byte(0x80|(hi>>6)&0x3F), byte(0x80|hi&0x3F))
			out = append(out, byte(0xE0|lo>>12), byte(0x80|(lo>>6)&0x3F), byte(0x80|lo&0x3F))
		}
	}
	return out, true
}

// AppendPayload appends a tag payload (no type byte or name).
func AppendPayload(dst []byte, v any, enc Encoding) []byte {
	e := encoder{buf: dst, le: enc == LittleEndian}
	e.payload(v)
	return e.buf
}

// AppendString appends a string payload.
func AppendString(dst []byte, s string, enc Encoding) []byte {
	e := encoder{buf: dst, le: enc == LittleEndian}
	e.str(s)
	return e.buf
}
