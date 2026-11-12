package extractor

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Reader is a cursor over an in-memory byte buffer that can read Unity's
// primitive types in either big- or little-endian order. Unity bundle/
// serialized-file headers are big-endian; once a SerializedFile's own
// endianness byte is read, the rest of that file switches accordingly
// (little-endian on every desktop/mobile platform in practice).
type Reader struct {
	buf []byte
	pos int
	big bool
}

func NewReader(buf []byte, big bool) *Reader {
	return &Reader{buf: buf, big: big}
}

func (r *Reader) Len() int           { return len(r.buf) }
func (r *Reader) Pos() int           { return r.pos }
func (r *Reader) Remaining() int     { return len(r.buf) - r.pos }
func (r *Reader) SeekTo(pos int)     { r.pos = pos }
func (r *Reader) SetEndian(big bool) { r.big = big }

func (r *Reader) need(n int) error {
	if n < 0 || r.pos+n > len(r.buf) {
		return fmt.Errorf("unexpected end of data at offset %d (need %d bytes, have %d)", r.pos, n, len(r.buf)-r.pos)
	}
	return nil
}

func (r *Reader) ReadBytes(n int) ([]byte, error) {
	if err := r.need(n); err != nil {
		return nil, err
	}
	b := r.buf[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

func (r *Reader) ReadByte() (byte, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	b := r.buf[r.pos]
	r.pos++
	return b, nil
}

func (r *Reader) ReadBool() (bool, error) {
	b, err := r.ReadByte()
	return b != 0, err
}

func (r *Reader) order() binary.ByteOrder {
	if r.big {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

func (r *Reader) ReadU16() (uint16, error) {
	b, err := r.ReadBytes(2)
	if err != nil {
		return 0, err
	}
	return r.order().Uint16(b), nil
}

func (r *Reader) ReadU32() (uint32, error) {
	b, err := r.ReadBytes(4)
	if err != nil {
		return 0, err
	}
	return r.order().Uint32(b), nil
}

func (r *Reader) ReadU64() (uint64, error) {
	b, err := r.ReadBytes(8)
	if err != nil {
		return 0, err
	}
	return r.order().Uint64(b), nil
}

func (r *Reader) ReadI16() (int16, error) { v, err := r.ReadU16(); return int16(v), err }
func (r *Reader) ReadI32() (int32, error) { v, err := r.ReadU32(); return int32(v), err }
func (r *Reader) ReadI64() (int64, error) { v, err := r.ReadU64(); return int64(v), err }

func (r *Reader) ReadF32() (float32, error) {
	v, err := r.ReadU32()
	return math.Float32frombits(v), err
}

// ReadCString reads a NUL-terminated string (used in bundle headers).
func (r *Reader) ReadCString() (string, error) {
	start := r.pos
	for r.pos < len(r.buf) && r.buf[r.pos] != 0 {
		r.pos++
	}
	if r.pos >= len(r.buf) {
		return "", fmt.Errorf("unterminated string at offset %d", start)
	}
	s := string(r.buf[start:r.pos])
	r.pos++ // skip the NUL
	return s, nil
}

// Align advances the position to the next multiple of n.
func (r *Reader) Align(n int) {
	if mod := r.pos % n; mod != 0 {
		r.pos += n - mod
	}
}

// ReadAlignedString reads an int32 length-prefixed UTF-8 string, then
// aligns to 4 bytes, matching Unity's serialization of `string` fields.
func (r *Reader) ReadAlignedString() (string, error) {
	length, err := r.ReadI32()
	if err != nil {
		return "", err
	}
	if length < 0 || int(length) > r.Remaining() {
		return "", nil
	}
	data, err := r.ReadBytes(int(length))
	if err != nil {
		return "", err
	}
	r.Align(4)
	return string(data), nil
}

// ReadUint8Array reads an int32 length-prefixed byte array (no alignment).
func (r *Reader) ReadUint8Array() ([]byte, error) {
	n, err := r.ReadI32()
	if err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, fmt.Errorf("negative array length %d", n)
	}
	return r.ReadBytes(int(n))
}
