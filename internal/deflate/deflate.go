// Package deflate compresses and decompresses zlib and raw deflate data
// (Java region chunks and Bedrock LevelDB blocks).
//
// Built with cgo (a C compiler is available) it uses libdeflate, which is
// linked into the executable; otherwise the pure Go zfast decoder and
// klauspost/compress encoder. Both produce standard streams, so either build
// reads the other's output.
package deflate

// Backend names the implementation in use ("libdeflate" or "go").
var Backend = backend

// Zlib decompresses a zlib stream, reusing dst's storage when possible.
func Zlib(dst, src []byte) ([]byte, error) { return decompress(dst, src, true) }

// Raw decompresses a raw deflate stream, reusing dst's storage when possible.
func Raw(dst, src []byte) ([]byte, error) { return decompress(dst, src, false) }

// CompressZlib compresses src as a zlib stream appended to dst[:0].
// level: 0 = stored, 1 (fastest) to 9; -2 (Huffman only) counts as 1.
func CompressZlib(dst, src []byte, level int) []byte { return compress(dst, src, level, true) }

// CompressRaw compresses src as a raw deflate stream appended to dst[:0].
func CompressRaw(dst, src []byte, level int) []byte { return compress(dst, src, level, false) }
