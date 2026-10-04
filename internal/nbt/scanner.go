package nbt

import (
	"encoding/binary"
)

// Scanner walks big endian (Java) NBT without building maps, for hot paths
// that only need a few fields. Methods panic with a decode error on
// malformed input; use Catch to turn that into an error.
type Scanner struct {
	d decoder
}

// NewScanner returns a scanner over data.
func NewScanner(data []byte) *Scanner {
	return &Scanner{d: decoder{buf: data}}
}

// Reset reuses the scanner for new data.
func (s *Scanner) Reset(data []byte) { s.d = decoder{buf: data} }

// Catch converts a scanner panic into an error. Use as: defer s.Catch(&err).
func (s *Scanner) Catch(err *error) {
	if r := recover(); r != nil {
		if e, ok := r.(decodeError); ok {
			*err = e.err
			return
		}
		panic(r)
	}
}

// Root reads the root tag header and returns its type.
func (s *Scanner) Root() byte {
	t := s.d.u8()
	if t != TagEnd {
		s.d.need(int(s.d.u16()))
	}
	return t
}

// Next reads the next entry header of a compound. It returns TagEnd at the end.
// The name slice is only valid until the next call.
func (s *Scanner) Next() (t byte, name []byte) {
	t = s.d.u8()
	if t == TagEnd {
		return t, nil
	}
	n := int(s.d.u16())
	return t, s.d.need(n)
}

// ListHeader reads a list payload header.
func (s *Scanner) ListHeader() (elem byte, n int) {
	elem = s.d.u8()
	n = int(int32(s.d.u32()))
	if n < 0 || elem == TagEnd {
		n = 0
	}
	return elem, n
}

// Skip skips a payload.
func (s *Scanner) Skip(t byte) { s.d.skipPayload(t) }

// Bytes reads a string payload as a byte view (valid while data is).
func (s *Scanner) Bytes() []byte {
	n := int(s.d.u16())
	return s.d.need(n)
}

// Value decodes a payload generically.
func (s *Scanner) Value(t byte) any { return s.d.payload(t) }

// Int reads any integer payload of type t as int64 (other types are skipped).
func (s *Scanner) Int(t byte) (int64, bool) {
	switch t {
	case TagByte:
		return int64(int8(s.d.u8())), true
	case TagShort:
		return int64(int16(s.d.u16())), true
	case TagInt:
		return int64(int32(s.d.u32())), true
	case TagLong:
		return int64(s.d.u64()), true
	}
	s.d.skipPayload(t)
	return 0, false
}

// LongArray reads a long array payload into dst (reused).
func (s *Scanner) LongArray(dst []uint64) []uint64 {
	n := s.d.length()
	b := s.d.need(n * 8)
	if cap(dst) < n {
		dst = make([]uint64, n)
	}
	dst = dst[:n]
	for i := range dst {
		dst[i] = binary.BigEndian.Uint64(b[i*8:])
	}
	return dst
}

// RootLen returns the encoded length of the root tag at the start of data
// without decoding it.
func RootLen(data []byte, enc Encoding) (n int, err error) {
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
		return d.off, nil
	}
	d.need(int(d.u16()))
	d.skipPayload(t)
	return d.off, nil
}
