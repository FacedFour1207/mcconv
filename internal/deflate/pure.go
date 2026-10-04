//go:build !cgo

package deflate

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"io"
	"sync"

	kflate "github.com/klauspost/compress/flate"
	kzlib "github.com/klauspost/compress/zlib"

	"mcconv/internal/zfast"
)

const backend = "go"

var decoders = sync.Pool{New: func() any { return new(zfast.Decoder) }}

func decompress(dst, src []byte, isZlib bool) ([]byte, error) {
	d := decoders.Get().(*zfast.Decoder)
	defer decoders.Put(d)
	if cap(dst) == 0 {
		dst = make([]byte, 0, max(len(src)*6, 4096))
	}
	var out []byte
	var err error
	if isZlib {
		out, err = d.Zlib(dst[:0], src)
	} else {
		out, err = d.Raw(dst[:0], src)
	}
	if err == nil {
		return out, nil
	}
	// fall back to the standard library for anything unusual
	var r io.ReadCloser
	if isZlib {
		if r, err = zlib.NewReader(bytes.NewReader(src)); err != nil {
			return dst[:0], err
		}
	} else {
		r = flate.NewReader(bytes.NewReader(src))
	}
	defer r.Close()
	buf := bytes.NewBuffer(dst[:0])
	_, err = buf.ReadFrom(r)
	return buf.Bytes(), err
}

var (
	zlibWriters [12]sync.Pool // level + 2
	rawWriters  [12]sync.Pool
)

func compress(dst, src []byte, level int, isZlib bool) []byte {
	level = min(max(level, -2), 9)
	buf := bytes.NewBuffer(dst[:0])
	if isZlib {
		var w *kzlib.Writer
		if v := zlibWriters[level+2].Get(); v != nil {
			w = v.(*kzlib.Writer)
			w.Reset(buf)
		} else {
			w, _ = kzlib.NewWriterLevel(buf, level)
		}
		w.Write(src)
		w.Close()
		zlibWriters[level+2].Put(w)
	} else {
		var w *kflate.Writer
		if v := rawWriters[level+2].Get(); v != nil {
			w = v.(*kflate.Writer)
			w.Reset(buf)
		} else {
			w, _ = kflate.NewWriter(buf, level)
		}
		w.Write(src)
		w.Close()
		rawWriters[level+2].Put(w)
	}
	return buf.Bytes()
}
