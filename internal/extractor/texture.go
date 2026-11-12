package extractor

import "fmt"

type Texture2DInfo struct {
	Name      string
	PathID    int64
	Width     int32
	Height    int32
	Format    int32
	ImageData []byte
}

// fieldReader retains the first error while reading sequential fields.
type fieldReader struct {
	r   *Reader
	err error
}

func (f *fieldReader) i32() int32 {
	if f.err != nil {
		return 0
	}
	v, err := f.r.ReadI32()
	if err != nil {
		f.err = err
	}
	return v
}
func (f *fieldReader) u32() uint32 {
	if f.err != nil {
		return 0
	}
	v, err := f.r.ReadU32()
	if err != nil {
		f.err = err
	}
	return v
}
func (f *fieldReader) i64() int64 {
	if f.err != nil {
		return 0
	}
	v, err := f.r.ReadI64()
	if err != nil {
		f.err = err
	}
	return v
}
func (f *fieldReader) boolean() bool {
	if f.err != nil {
		return false
	}
	v, err := f.r.ReadBool()
	if err != nil {
		f.err = err
	}
	return v
}
func (f *fieldReader) f32() float32 {
	if f.err != nil {
		return 0
	}
	v, err := f.r.ReadF32()
	if err != nil {
		f.err = err
	}
	return v
}
func (f *fieldReader) alignedString() string {
	if f.err != nil {
		return ""
	}
	v, err := f.r.ReadAlignedString()
	if err != nil {
		f.err = err
	}
	return v
}
func (f *fieldReader) uint8Array() []byte {
	if f.err != nil {
		return nil
	}
	v, err := f.r.ReadUint8Array()
	if err != nil {
		f.err = err
	}
	return v
}
func (f *fieldReader) align(n int) {
	if f.err == nil {
		f.r.Align(n)
	}
}

// verGTE reports whether v is >= major.minor.
func verGTE(v [4]int, major, minor int) bool {
	return v[0] > major || (v[0] == major && v[1] >= minor)
}

// verGT reports whether v is > major.minor.
func verGT(v [4]int, major, minor int) bool {
	return v[0] > major || (v[0] == major && v[1] > minor)
}

func readTexture2D(sf *SerializedFile, obj objectEntry, resources map[string][]byte) (*Texture2DInfo, error) {
	if sf.Platform == -2 { // BuildTarget.NoTarget: editor-serialized asset, not a built game bundle
		return nil, fmt.Errorf("editor-serialized (non-build) Texture2D objects are not supported")
	}
	if obj.ByteStart < 0 || obj.ByteSize == 0 || obj.ByteStart+int64(obj.ByteSize) > int64(len(sf.Data)) {
		return nil, fmt.Errorf("object byte range out of bounds")
	}

	// Embedded TypeTree avoids guessing the field layout when the Unity
	// version has been stripped from the file.
	if int(obj.TypeID) >= 0 && int(obj.TypeID) < len(sf.Types) {
		if nodes := sf.Types[obj.TypeID].Nodes; len(nodes) > 0 {
			tex, err := readTexture2DGeneric(sf, obj, resources, nodes)
			if err == nil {
				return tex, nil
			}
			// fall through to the hardcoded reader as a second attempt
		}
	}
	return readTexture2DFallback(sf, obj, resources)
}

// readTexture2DGeneric extracts Texture2D fields by walking this file's own
// TypeTree for the object's type, instead of assuming a hardcoded,
// version-gated field layout.
func readTexture2DGeneric(sf *SerializedFile, obj objectEntry, resources map[string][]byte, nodes []typeTreeNode) (*Texture2DInfo, error) {
	r := NewReader(sf.Data, !sf.LittleEndian)
	r.SeekTo(int(obj.ByteStart))

	fields, err := readObjectFields(nodes, r)
	if err != nil {
		return nil, fmt.Errorf("type-tree read: %w", err)
	}

	name, _ := fields["m_Name"].(string)
	width := ttInt(fields["m_Width"])
	height := ttInt(fields["m_Height"])
	format := ttInt(fields["m_TextureFormat"])

	var imageData []byte
	if raw, ok := fields["image data"]; ok {
		imageData = ttBytes(raw)
	}

	var streamPath string
	var streamOffset, streamSize int64
	if sd, ok := fields["m_StreamData"].(map[string]interface{}); ok {
		streamPath, _ = sd["path"].(string)
		streamOffset = ttInt(sd["offset"])
		streamSize = ttInt(sd["size"])
	}

	if len(imageData) == 0 && streamPath != "" {
		res, ok := resources[baseName(streamPath)]
		if !ok {
			return nil, fmt.Errorf("streamed resource %q (for texture %q) was not found inside this bundle", streamPath, name)
		}
		if streamOffset < 0 || streamSize <= 0 || streamOffset+streamSize > int64(len(res)) {
			return nil, fmt.Errorf("streamed resource %q byte range out of bounds", streamPath)
		}
		imageData = res[streamOffset : streamOffset+streamSize]
	}

	if width == 0 && height == 0 {
		return nil, fmt.Errorf("type-tree read produced empty dimensions (likely missing m_Width/m_Height fields)")
	}

	return &Texture2DInfo{
		Name:      name,
		PathID:    obj.PathID,
		Width:     int32(width),
		Height:    int32(height),
		Format:    int32(format),
		ImageData: imageData,
	}, nil
}

// ttInt coerces any of the integer-ish Go types readGenericValue can
// produce into an int64.
func ttInt(v interface{}) int64 {
	switch n := v.(type) {
	case int8:
		return int64(n)
	case uint8:
		return int64(n)
	case int16:
		return int64(n)
	case uint16:
		return int64(n)
	case int32:
		return int64(n)
	case uint32:
		return int64(n)
	case int64:
		return n
	case uint64:
		return int64(n)
	default:
		return 0
	}
}

// ttBytes coerces a value into a byte slice, whether it came from a
// TypelessData node ([]byte directly) or a generic Array-of-UInt8 ([]interface{}).
func ttBytes(v interface{}) []byte {
	switch b := v.(type) {
	case []byte:
		return b
	case []interface{}:
		out := make([]byte, len(b))
		for i, e := range b {
			out[i] = byte(ttInt(e))
		}
		return out
	default:
		return nil
	}
}

// readTexture2DFallback parses the version-dependent layout when TypeTree
// decoding is unavailable or fails.
func readTexture2DFallback(sf *SerializedFile, obj objectEntry, resources map[string][]byte) (*Texture2DInfo, error) {

	r := NewReader(sf.Data, !sf.LittleEndian)
	r.SeekTo(int(obj.ByteStart))
	f := &fieldReader{r: r}
	v := sf.UnityVersion

	// NamedObject.m_Name
	name := f.alignedString()

	// Texture (abstract base) fields
	if verGTE(v, 2017, 3) {
		f.i32()     // m_ForcedFallbackFormat
		f.boolean() // m_DownscaleFallback
		if verGTE(v, 2020, 2) {
			f.boolean() // m_IsAlphaChannelOptional
		}
		f.align(4)
	}

	width := f.i32()
	height := f.i32()
	f.i32() // m_CompleteImageSize
	if v[0] >= 2020 {
		f.i32() // m_MipsStripped
	}
	format := f.i32() // m_TextureFormat

	if v[0] < 5 || (v[0] == 5 && v[1] < 2) {
		f.boolean() // m_MipMap (old)
	} else {
		f.i32() // m_MipCount
	}

	if verGT(v, 2, 5) { // > 2.5, i.e. >2 or (==2 and >=6)
		f.boolean() // m_IsReadable
	}
	if v[0] >= 2020 {
		f.boolean() // m_IsPreProcessed
	}
	if verGT(v, 2019, 2) {
		f.boolean() // m_IgnoreMasterTextureLimit
	}
	if v[0] >= 3 {
		if v[0] < 5 || (v[0] == 5 && v[1] <= 4) {
			f.boolean() // m_ReadAllowed (old)
		}
	}
	if verGT(v, 2018, 1) {
		f.boolean() // m_StreamingMipmaps
	}
	f.align(4)
	if verGT(v, 2018, 1) {
		f.i32() // m_StreamingMipmapsPriority
	}
	f.i32() // m_ImageCount
	f.i32() // m_TextureDimension

	// GLTextureSettings
	f.i32() // filterMode
	f.i32() // aniso
	f.f32() // mipBias
	if v[0] >= 2017 {
		f.i32() // wrapU
		f.i32() // wrapV
		f.i32() // wrapW
	} else {
		f.i32() // wrapMode (single, old)
	}

	if v[0] >= 3 {
		f.i32() // m_LightmapFormat
	}
	if verGT(v, 3, 4) { // >3 or (==3,>=5)
		f.i32() // m_ColorSpace
	}
	if verGT(v, 2020, 1) {
		f.uint8Array() // m_PlatformBlob
		f.align(4)
	}

	imageDataSize := f.i32()

	var streamPath string
	var streamOffset int64
	var streamSize uint32
	hasStream := false
	if imageDataSize == 0 && (v[0] > 5 || (v[0] == 5 && v[1] >= 3)) {
		hasStream = true
		if v[0] >= 2020 {
			streamOffset = f.i64()
		} else {
			streamOffset = int64(f.u32())
		}
		streamSize = f.u32()
		streamPath = f.alignedString()
	}

	if f.err != nil {
		return nil, f.err
	}

	var imageData []byte
	if hasStream && streamPath != "" {
		res, ok := resources[baseName(streamPath)]
		if !ok {
			return nil, fmt.Errorf("streamed resource %q (for texture %q) was not found inside this bundle", streamPath, name)
		}
		if streamOffset < 0 || streamSize == 0 || streamOffset+int64(streamSize) > int64(len(res)) {
			return nil, fmt.Errorf("streamed resource %q byte range out of bounds", streamPath)
		}
		imageData = res[streamOffset : streamOffset+int64(streamSize)]
	} else {
		pos := r.Pos()
		if imageDataSize < 0 || pos+int(imageDataSize) > len(sf.Data) {
			return nil, fmt.Errorf("inline image data out of bounds for texture %q", name)
		}
		imageData = sf.Data[pos : pos+int(imageDataSize)]
	}

	return &Texture2DInfo{
		Name:      name,
		PathID:    obj.PathID,
		Width:     width,
		Height:    height,
		Format:    format,
		ImageData: imageData,
	}, nil
}
