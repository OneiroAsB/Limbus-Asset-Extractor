package extractor

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

type bw struct{ buf bytes.Buffer }

func (w *bw) u8(v uint8) { w.buf.WriteByte(v) }
func (w *bw) bool8(v bool) {
	if v {
		w.u8(1)
	} else {
		w.u8(0)
	}
}
func (w *bw) u16be(v uint16) { var b [2]byte; binary.BigEndian.PutUint16(b[:], v); w.buf.Write(b[:]) }
func (w *bw) u32be(v uint32) { var b [4]byte; binary.BigEndian.PutUint32(b[:], v); w.buf.Write(b[:]) }
func (w *bw) i64be(v int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	w.buf.Write(b[:])
}
func (w *bw) u32le(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.buf.Write(b[:])
}
func (w *bw) i32le(v int32) { w.u32le(uint32(v)) }
func (w *bw) i16le(v int16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], uint16(v))
	w.buf.Write(b[:])
}
func (w *bw) i64le(v int64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(v))
	w.buf.Write(b[:])
}
func (w *bw) f32le(v float32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
	w.buf.Write(b[:])
}
func (w *bw) cstr(s string) { w.buf.WriteString(s); w.u8(0) }
func (w *bw) alignedStringLE(s string) {
	w.i32le(int32(len(s)))
	w.buf.WriteString(s)
	for w.buf.Len()%4 != 0 {
		w.u8(0)
	}
}
func (w *bw) raw(b []byte) { w.buf.Write(b) }
func (w *bw) zeros(n int)  { w.buf.Write(make([]byte, n)) }
func (w *bw) alignLE(n int) {
	for w.buf.Len()%n != 0 {
		w.u8(0)
	}
}

// buildSerializedFile constructs a minimal, but real, format-version-22
// SerializedFile containing exactly one Texture2D (RGBA32, 4x4, inline
// pixel data, TypeTree enabled with zero nodes).
func buildSerializedFile(t *testing.T, pixels []byte) []byte {
	t.Helper()

	// know its size before writing the object table entry. ----
	var data bw
	data.alignedStringLE("TestTex") // NamedObject.m_Name

	// Texture base (v>=2017.3)
	data.i32le(0)     // m_ForcedFallbackFormat
	data.bool8(false) // m_DownscaleFallback
	data.bool8(false) // m_IsAlphaChannelOptional (v>=2020.2)
	data.alignLE(4)

	data.i32le(4)                  // width
	data.i32le(4)                  // height
	data.i32le(int32(len(pixels))) // m_CompleteImageSize
	data.i32le(0)                  // m_MipsStripped (v>=2020)
	data.i32le(4)                  // m_TextureFormat = RGBA32
	data.i32le(1)                  // m_MipCount
	data.bool8(false)              // m_IsReadable
	data.bool8(false)              // m_IsPreProcessed
	data.bool8(false)              // m_IgnoreMasterTextureLimit
	data.bool8(false)              // m_StreamingMipmaps
	data.alignLE(4)
	data.i32le(0) // m_StreamingMipmapsPriority
	data.i32le(1) // m_ImageCount
	data.i32le(2) // m_TextureDimension
	data.i32le(0) // filterMode
	data.i32le(1) // aniso
	data.f32le(0) // mipBias
	data.i32le(0) // wrapU
	data.i32le(0) // wrapV
	data.i32le(0) // wrapW
	data.i32le(0) // m_LightmapFormat
	data.i32le(1) // m_ColorSpace
	data.i32le(0) // m_PlatformBlob length = 0
	data.alignLE(4)
	data.i32le(int32(len(pixels))) // image data size (non-zero -> inline)
	data.raw(pixels)
	if len(pixels) == 0 {
		data.alignLE(4)
		data.i64le(0)            // StreamingInfo offset
		data.u32le(0)            // size
		data.alignedStringLE("") // no streamed resource
	}

	objData := data.buf.Bytes()

	var m bw
	m.cstr("2022.3.22f1") // unity version
	m.i32le(19)           // target platform (arbitrary, != -2)
	m.bool8(true)         // m_EnableTypeTree

	m.i32le(1) // typeCount
	// type[0]: Texture2D
	m.i32le(28)    // classID
	m.bool8(false) // m_IsStrippedType (v>=16)
	m.i16le(-1)    // m_ScriptTypeIndex (v>=17)
	m.zeros(16)    // m_OldTypeHash (classID!=114, no m_ScriptID)
	// TypeTreeBlobRead (v>=12), zero nodes for a minimal fixture:
	m.i32le(0) // nodeCount
	m.i32le(0) // stringBufferSize
	// m_TypeDependencies (v>=21, isRefType=false): Int32Array, empty
	m.i32le(0)

	m.i32le(1) // objectCount
	// object[0]
	m.alignLE(4)
	m.i64le(1)                    // pathID
	m.i64le(0)                    // byteStart, relative to dataOffset (v>=22 -> int64)
	m.u32le(uint32(len(objData))) // byteSize
	m.i32le(0)                    // typeID

	metaBytes := m.buf.Bytes()

	var h bw
	h.u32be(0)  // m_MetadataSize (unchecked by our reader)
	h.u32be(0)  // m_FileSize (unchecked in the v22 path)
	h.u32be(22) // format version
	h.u32be(0)  // m_DataOffset (32-bit placeholder, superseded below)
	h.u8(0)     // endianess = little
	h.zeros(3)  // reserved
	h.u32be(0)  // metadataSize (64-bit era, unchecked)
	h.i64be(0)  // fileSize (unchecked)
	// dataOffset must equal "header length + metadata length" so the data
	// section starts immediately after metadata, with no gap. We don't know
	// the header's own final length yet (we're still writing it), so write
	// a placeholder and patch it in once everything is assembled.
	dataOffsetFieldPos := h.buf.Len()
	h.i64be(0) // m_DataOffset placeholder
	h.i64be(0) // unknown

	headerLen := h.buf.Len()
	realDataOffset := int64(headerLen + len(metaBytes))

	full := make([]byte, 0, headerLen+len(metaBytes)+len(objData))
	full = append(full, h.buf.Bytes()...)
	binary.BigEndian.PutUint64(full[dataOffsetFieldPos:dataOffsetFieldPos+8], uint64(realDataOffset))
	full = append(full, metaBytes...)
	full = append(full, objData...)

	if int64(len(full)) != realDataOffset+int64(len(objData)) {
		t.Fatalf("fixture length mismatch: len=%d dataOffset=%d objData=%d", len(full), realDataOffset, len(objData))
	}
	return full
}

// buildBundle wraps serializedFileBytes as the single entry "CAB-test"
// inside an uncompressed UnityFS bundle.
func buildBundle(t *testing.T, entryName string, entryBytes []byte) []byte {
	t.Helper()

	// blocksInfo: hash(16) + blockCount(i32 BE) + blocks + nodeCount(i32 BE) + nodes
	var blocksInfo bw
	blocksInfo.zeros(16)
	blocksInfo.u32be(1)                       // blockCount (read as i32 in bundle.go; BE bytes identical for small positive values)
	blocksInfo.u32be(uint32(len(entryBytes))) // block[0].uncompressedSize
	blocksInfo.u32be(uint32(len(entryBytes))) // block[0].compressedSize
	blocksInfo.u16be(0)                       // block[0].flags (compression=none)
	blocksInfo.u32be(1)                       // nodeCount
	blocksInfo.i64be(0)                       // node[0].offset
	blocksInfo.i64be(int64(len(entryBytes)))  // node[0].size
	blocksInfo.u32be(0)                       // node[0].flags
	blocksInfo.cstr(entryName)                // node[0].path

	biBytes := blocksInfo.buf.Bytes()

	var h bw
	h.cstr("UnityFS")
	h.u32be(6)                    // bundle format version (< 7, so no 16-byte align needed)
	h.cstr("5.x.x")               // unity version
	h.cstr("2022.3.22f1c1")       // unity revision
	h.i64be(0)                    // total size (unchecked by our parser)
	h.u32be(uint32(len(biBytes))) // compressedBlocksInfoSize
	h.u32be(uint32(len(biBytes))) // uncompressedBlocksInfoSize
	h.u32be(0)                    // flags: compression=none, no special bits

	full := append([]byte{}, h.buf.Bytes()...)
	full = append(full, biBytes...)
	full = append(full, entryBytes...)
	return full
}

func TestEndToEndRGBA32(t *testing.T) {
	// 4x4 RGBA32, row-major as Unity/GPU would store it (row 0 = bottom
	// after our decoder's mandatory vertical flip): row0=red, row1=green,
	// row2=blue, row3=white.
	px := func(r, g, b, a byte) []byte { return []byte{r, g, b, a} }
	row := func(p []byte) []byte { return bytes.Repeat(p, 4) }
	pixels := append(append(append(
		row(px(255, 0, 0, 255)),
		row(px(0, 255, 0, 255))...),
		row(px(0, 0, 255, 255))...),
		row(px(255, 255, 255, 255))...)

	sfBytes := buildSerializedFile(t, pixels)
	bundleBytes := buildBundle(t, "CAB-test", sfBytes)

	entries, err := ParseBundle(bundleBytes)
	if err != nil {
		t.Fatalf("ParseBundle: %v", err)
	}
	data, ok := entries["CAB-test"]
	if !ok {
		t.Fatalf("entry CAB-test missing, got: %v", keysOf(entries))
	}

	sf, err := parseSerializedFile(data, [4]int{2022, 3, 22, 0})
	if err != nil {
		t.Fatalf("parseSerializedFile: %v", err)
	}
	if len(sf.Objects) != 1 {
		t.Fatalf("expected 1 object, got %d", len(sf.Objects))
	}
	if sf.Objects[0].ClassID != 28 {
		t.Fatalf("expected classID 28 (Texture2D), got %d", sf.Objects[0].ClassID)
	}

	tex, err := readTexture2D(sf, sf.Objects[0], entries)
	if err != nil {
		t.Fatalf("readTexture2D: %v", err)
	}
	if tex.Name != "TestTex" || tex.Width != 4 || tex.Height != 4 || tex.Format != 4 {
		t.Fatalf("unexpected texture info: %+v", tex)
	}

	img, err := decodeTexture(tex.Format, tex.ImageData, int(tex.Width), int(tex.Height))
	if err != nil {
		t.Fatalf("decodeTexture: %v", err)
	}

	check := func(x, y int, want [4]uint8) {
		o := img.PixOffset(x, y)
		got := [4]uint8{img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3]}
		if got != want {
			t.Errorf("pixel (%d,%d): got %v, want %v", x, y, got, want)
		}
	}
	// After the mandatory vertical flip, source row0 (red) ends up at the
	// BOTTOM (y=3), and source row3 (white) ends up at the TOP (y=0).
	check(0, 0, [4]uint8{255, 255, 255, 255}) // was row3 (white)
	check(0, 1, [4]uint8{0, 0, 255, 255})     // was row2 (blue)
	check(0, 2, [4]uint8{0, 255, 0, 255})     // was row1 (green)
	check(0, 3, [4]uint8{255, 0, 0, 255})     // was row0 (red)
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// lz4EncodeLiteralOnly is a minimal (not space-efficient, but spec-valid)
// LZ4 block encoder used only to exercise the real LZ4-compression code
// path in ParseBundle with test data.
func lz4EncodeLiteralOnly(data []byte) []byte {
	var out []byte
	n := len(data)
	if n < 15 {
		out = append(out, byte(n<<4))
	} else {
		out = append(out, 0xF0)
		rem := n - 15
		for rem >= 255 {
			out = append(out, 255)
			rem -= 255
		}
		out = append(out, byte(rem))
	}
	out = append(out, data...)
	return out
}

func TestEndToEndRGBA32_LZ4Compressed(t *testing.T) {
	px := func(v byte) []byte { return []byte{v, v, v, 255} }
	pixels := bytes.Repeat(px(200), 16) // flat 4x4 grey, compression-friendly

	sfBytes := buildSerializedFile(t, pixels)
	compressed := lz4EncodeLiteralOnly(sfBytes)

	var blocksInfo bw
	blocksInfo.zeros(16)
	blocksInfo.u32be(1)
	blocksInfo.u32be(uint32(len(sfBytes)))    // uncompressedSize
	blocksInfo.u32be(uint32(len(compressed))) // compressedSize
	blocksInfo.u16be(2)                       // flags: compression = LZ4
	blocksInfo.u32be(1)                       // nodeCount
	blocksInfo.i64be(0)
	blocksInfo.i64be(int64(len(sfBytes)))
	blocksInfo.u32be(0)
	blocksInfo.cstr("CAB-test")
	biBytes := blocksInfo.buf.Bytes()

	var h bw
	h.cstr("UnityFS")
	h.u32be(6)
	h.cstr("5.x.x")
	h.cstr("2022.3.22f1c1")
	h.i64be(0)
	h.u32be(uint32(len(biBytes)))
	h.u32be(uint32(len(biBytes)))
	h.u32be(0)

	full := append([]byte{}, h.buf.Bytes()...)
	full = append(full, biBytes...)
	full = append(full, compressed...)

	entries, err := ParseBundle(full)
	if err != nil {
		t.Fatalf("ParseBundle: %v", err)
	}
	data, ok := entries["CAB-test"]
	if !ok || len(data) != len(sfBytes) {
		t.Fatalf("bad entry: ok=%v len=%d want=%d", ok, len(data), len(sfBytes))
	}

	sf, err := parseSerializedFile(data, [4]int{2022, 3, 22, 0})
	if err != nil {
		t.Fatalf("parseSerializedFile: %v", err)
	}
	tex, err := readTexture2D(sf, sf.Objects[0], entries)
	if err != nil {
		t.Fatalf("readTexture2D: %v", err)
	}
	img, err := decodeTexture(tex.Format, tex.ImageData, int(tex.Width), int(tex.Height))
	if err != nil {
		t.Fatalf("decodeTexture: %v", err)
	}
	o := img.PixOffset(0, 0)
	if img.Pix[o] != 200 || img.Pix[o+3] != 255 {
		t.Fatalf("pixel = %v, want grey 200", img.Pix[o:o+4])
	}
}

func TestReadTextAssetFallback(t *testing.T) {
	// TextAsset with no embedded TypeTree (nodes=nil): NamedObject.m_Name
	// then m_Script, both plain length-prefixed strings - exercises the
	// hardcoded fallback path directly.
	var data bw
	data.alignedStringLE("hello-quest")
	data.alignedStringLE(`{"id":1,"text":"hi"}`)

	sf := &SerializedFile{
		Data:         data.buf.Bytes(),
		LittleEndian: true,
		Types:        []serializedType{{ClassID: 49}}, // no Nodes -> forces fallback
	}
	obj := objectEntry{PathID: 1, ByteStart: 0, ByteSize: uint32(data.buf.Len()), ClassID: 49, TypeID: 0}

	txt, err := readTextAsset(sf, obj)
	if err != nil {
		t.Fatalf("readTextAsset: %v", err)
	}
	if txt.Name != "hello-quest" {
		t.Errorf("Name = %q, want %q", txt.Name, "hello-quest")
	}
	if txt.Script != `{"id":1,"text":"hi"}` {
		t.Errorf("Script = %q", txt.Script)
	}
}
