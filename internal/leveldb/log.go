package leveldb

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

const (
	logBlockSize  = 32 * 1024
	logHeaderSize = 7

	recFull   = 1
	recFirst  = 2
	recMiddle = 3
	recLast   = 4
)

// readLogRecords splits a log file (WAL or MANIFEST) into records. Corrupt
// tails are ignored, like LevelDB does on recovery.
func readLogRecords(data []byte) [][]byte {
	var out [][]byte
	var cur []byte
	inFrag := false
	for blockStart := 0; blockStart < len(data); blockStart += logBlockSize {
		end := min(blockStart+logBlockSize, len(data))
		b := data[blockStart:end]
		for len(b) >= logHeaderSize {
			length := int(binary.LittleEndian.Uint16(b[4:6]))
			typ := b[6]
			if typ == 0 && length == 0 {
				break // zero padding (preallocated file)
			}
			if logHeaderSize+length > len(b) {
				return out
			}
			payload := b[logHeaderSize : logHeaderSize+length]
			want := unmaskCRC(binary.LittleEndian.Uint32(b[0:4]))
			got := crc32.Update(crc32.Checksum([]byte{typ}, crcTable), crcTable, payload)
			b = b[logHeaderSize+length:]
			if want != got {
				inFrag = false
				cur = nil
				continue
			}
			switch typ {
			case recFull:
				out = append(out, append([]byte(nil), payload...))
				inFrag = false
			case recFirst:
				cur = append(cur[:0], payload...)
				inFrag = true
			case recMiddle:
				if inFrag {
					cur = append(cur, payload...)
				}
			case recLast:
				if inFrag {
					cur = append(cur, payload...)
					out = append(out, cur)
					cur = nil
				}
				inFrag = false
			}
		}
	}
	return out
}

// logWriter writes records in the LevelDB log format.
type logWriter struct {
	w           io.Writer
	blockOffset int
}

func (lw *logWriter) addRecord(rec []byte) error {
	begin := true
	for {
		left := logBlockSize - lw.blockOffset
		if left < logHeaderSize {
			if left > 0 {
				if _, err := lw.w.Write(make([]byte, left)); err != nil {
					return err
				}
			}
			lw.blockOffset = 0
			left = logBlockSize
		}
		avail := left - logHeaderSize
		frag := rec
		if len(frag) > avail {
			frag = frag[:avail]
		}
		end := len(frag) == len(rec)
		var typ byte
		switch {
		case begin && end:
			typ = recFull
		case begin:
			typ = recFirst
		case end:
			typ = recLast
		default:
			typ = recMiddle
		}
		hdr := make([]byte, logHeaderSize)
		crc := crc32.Update(crc32.Checksum([]byte{typ}, crcTable), crcTable, frag)
		binary.LittleEndian.PutUint32(hdr[0:4], maskCRC(crc))
		binary.LittleEndian.PutUint16(hdr[4:6], uint16(len(frag)))
		hdr[6] = typ
		if _, err := lw.w.Write(hdr); err != nil {
			return err
		}
		if _, err := lw.w.Write(frag); err != nil {
			return err
		}
		lw.blockOffset += logHeaderSize + len(frag)
		rec = rec[len(frag):]
		begin = false
		if end {
			return nil
		}
	}
}

// version edits ----------------------------------------------------------------

const (
	tagComparator     = 1
	tagLogNumber      = 2
	tagNextFileNumber = 3
	tagLastSequence   = 4
	tagCompactPointer = 5
	tagDeletedFile    = 6
	tagNewFile        = 7
	tagPrevLogNumber  = 9
)

type fileMeta struct {
	level    int
	number   uint64
	size     uint64
	smallest []byte
	largest  []byte
}

type versionEdit struct {
	comparator   string
	logNumber    uint64
	hasLog       bool
	prevLog      uint64
	nextFile     uint64
	lastSeq      uint64
	deleted      [][2]uint64 // level, number
	added        []fileMeta
	hasNextFile  bool
	hasLastSeq   bool
	hasPrevLog   bool
	hasComparatr bool
}

func decodeVersionEdit(b []byte) (*versionEdit, error) {
	ve := &versionEdit{}
	bad := fmt.Errorf("leveldb: corrupt version edit")
	uv := func() uint64 {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			panic(bad)
		}
		b = b[n:]
		return v
	}
	bs := func() []byte {
		l := uv()
		if uint64(len(b)) < l {
			panic(bad)
		}
		v := b[:l]
		b = b[l:]
		return append([]byte(nil), v...)
	}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = bad
			}
		}()
		for len(b) > 0 {
			switch tag := uv(); tag {
			case tagComparator:
				ve.comparator = string(bs())
				ve.hasComparatr = true
			case tagLogNumber:
				ve.logNumber = uv()
				ve.hasLog = true
			case tagNextFileNumber:
				ve.nextFile = uv()
				ve.hasNextFile = true
			case tagLastSequence:
				ve.lastSeq = uv()
				ve.hasLastSeq = true
			case tagCompactPointer:
				uv()
				bs()
			case tagDeletedFile:
				lvl := uv()
				num := uv()
				ve.deleted = append(ve.deleted, [2]uint64{lvl, num})
			case tagNewFile:
				f := fileMeta{level: int(uv()), number: uv(), size: uv()}
				f.smallest = bs()
				f.largest = bs()
				ve.added = append(ve.added, f)
			case tagPrevLogNumber:
				ve.prevLog = uv()
				ve.hasPrevLog = true
			default:
				panic(bad)
			}
		}
	}()
	return ve, err
}

func (ve *versionEdit) encode() []byte {
	var b []byte
	if ve.hasComparatr {
		b = binary.AppendUvarint(b, tagComparator)
		b = binary.AppendUvarint(b, uint64(len(ve.comparator)))
		b = append(b, ve.comparator...)
	}
	if ve.hasLog {
		b = binary.AppendUvarint(b, tagLogNumber)
		b = binary.AppendUvarint(b, ve.logNumber)
	}
	if ve.hasPrevLog {
		b = binary.AppendUvarint(b, tagPrevLogNumber)
		b = binary.AppendUvarint(b, ve.prevLog)
	}
	if ve.hasNextFile {
		b = binary.AppendUvarint(b, tagNextFileNumber)
		b = binary.AppendUvarint(b, ve.nextFile)
	}
	if ve.hasLastSeq {
		b = binary.AppendUvarint(b, tagLastSequence)
		b = binary.AppendUvarint(b, ve.lastSeq)
	}
	for _, f := range ve.added {
		b = binary.AppendUvarint(b, tagNewFile)
		b = binary.AppendUvarint(b, uint64(f.level))
		b = binary.AppendUvarint(b, f.number)
		b = binary.AppendUvarint(b, f.size)
		b = binary.AppendUvarint(b, uint64(len(f.smallest)))
		b = append(b, f.smallest...)
		b = binary.AppendUvarint(b, uint64(len(f.largest)))
		b = append(b, f.largest...)
	}
	return b
}
