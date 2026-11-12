// Package catalog decodes a Unity Addressables binary content catalog
// ("catalog_S1.bin") into a plain Go structure that serialises to the same
// JSON shape the standalone unpack_catalog tool produced.
//
// It is a direct, field-for-field port of the binary decoding logic in the
// `addressablestools` Python package (itself a port of nesrak1's
// AddressablesTools C# library): string interning, dynamic/split strings,
// offset arrays and typed object records. Binary catalog format versions
// 2 and 3 are supported. Only the fields the JSON export needs are decoded;
// resource-provider bootstrap data is parsed far enough to skip over.
package catalog

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	uint32Max        = 0xFFFFFFFF
	uint32MaxMinus1  = 0xFFFFFFFE
	dynamicStringBit = 0x40000000
	unicodeStringBit = 0x80000000
	offsetMask       = 0x3FFFFFFF
)

func u32(buf []byte, off uint32) uint32 { return binary.LittleEndian.Uint32(buf[off : off+4]) }
func i32(buf []byte, off uint32) int32  { return int32(u32(buf, off)) }

type SerializedType struct {
	Assembly *string
	Class    *string
}

// matchName formats a serialized type name for the catalog version.
func (t SerializedType) matchName(version int) string {
	if t.Class == nil {
		return ""
	}
	if version >= 3 && t.Assembly == nil {
		return *t.Class
	}
	short := ""
	if t.Assembly != nil {
		if idx := strings.IndexByte(*t.Assembly, ','); idx >= 0 {
			short = (*t.Assembly)[:idx]
		} else {
			short = *t.Assembly
		}
	}
	return short + "; " + *t.Class
}

type valueKind int

const (
	kindNil valueKind = iota
	kindInt32
	kindInt64
	kindBool
	kindString
	kindHash128
	kindBundleOptions
)

type decodedValue struct {
	kind   valueKind
	i32    int32
	i64    int64
	b      bool
	str    string
	bundle *AssetBundleRequestOptions
}

type CommonInfo struct {
	Timeout                            int16  `json:"timeout"`
	RedirectLimit                      uint8  `json:"redirect_limit"`
	RetryCount                         uint8  `json:"retry_count"`
	AssetLoadMode                      string `json:"asset_load_mode"`
	ChunkedTransfer                    bool   `json:"chunked_transfer"`
	UseCrcForCachedBundle              bool   `json:"use_crc_for_cached_bundle"`
	UseUnityWebRequestForLocalBundles  bool   `json:"use_unity_web_request_for_local_bundles"`
	ClearOtherCachedVersionsWhenLoaded bool   `json:"clear_other_cached_versions_when_loaded"`
	Version                            int    `json:"version"`
}

type AssetBundleRequestOptions struct {
	Hash       string      `json:"hash"`
	Crc        uint32      `json:"crc"`
	CommonInfo *CommonInfo `json:"common_info"`
	BundleName *string     `json:"bundle_name"`
	BundleSize uint32      `json:"bundle_size"`
}

type ResourceLocation struct {
	PrimaryKey         string
	InternalID         string
	ProviderID         string
	Type               SerializedType
	Dependencies       []*ResourceLocation
	Data               decodedValue
	DependencyHashCode int32
}

type resourceEntry struct {
	key  string
	locs []*ResourceLocation
}

type Reader struct {
	buf     []byte
	version int

	typeCache       map[uint32]SerializedType
	locCache        map[uint32]*ResourceLocation
	bundleCache     map[uint32]*AssetBundleRequestOptions
	commonInfoCache map[uint32]*CommonInfo
	stringCache     map[uint64]stringResult
	offsetArrCache  map[uint32][]uint32
}

type stringResult struct {
	val   string
	isNil bool
}

func newReader(buf []byte, version int) *Reader {
	return &Reader{
		buf:             buf,
		version:         version,
		typeCache:       make(map[uint32]SerializedType),
		locCache:        make(map[uint32]*ResourceLocation),
		bundleCache:     make(map[uint32]*AssetBundleRequestOptions),
		commonInfoCache: make(map[uint32]*CommonInfo),
		stringCache:     make(map[uint64]stringResult),
		offsetArrCache:  make(map[uint32][]uint32),
	}
}

// readEncodedString decodes a catalog string. A zero dynamicSep disables
// dynamic-string handling regardless of the encoded offset flag.
func (r *Reader) readEncodedString(encodedOffset uint32, dynamicSep byte) (string, bool) {
	if encodedOffset == uint32Max || encodedOffset == uint32MaxMinus1 {
		return "", true
	}
	isDynamic := (encodedOffset&dynamicStringBit) != 0 && dynamicSep != 0
	cacheSep := byte(0)
	if isDynamic {
		cacheSep = dynamicSep
	}
	cacheKey := uint64(encodedOffset)<<8 | uint64(cacheSep)
	if v, ok := r.stringCache[cacheKey]; ok {
		return v.val, v.isNil
	}

	isUnicode := (encodedOffset & unicodeStringBit) != 0
	offset := encodedOffset & offsetMask

	var val string
	var isNil bool
	if isDynamic {
		val, isNil = r.readDynamicString(offset, dynamicSep)
	} else {
		val = r.readBasicString(offset, isUnicode)
	}
	r.stringCache[cacheKey] = stringResult{val, isNil}
	return val, isNil
}

func (r *Reader) readBasicString(offset uint32, unicode bool) string {
	length := i32(r.buf, offset-4)
	data := r.buf[offset : offset+uint32(length)]
	if !unicode {
		return string(data)
	}
	u16 := make([]uint16, len(data)/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(data[i*2 : i*2+2])
	}
	return string(utf16.Decode(u16))
}

func (r *Reader) readDynamicString(offset uint32, sep byte) (string, bool) {
	var parts []string
	visited := map[uint32]bool{}
	next := offset
	for {
		if visited[next] {
			break // cycle guard (shouldn't occur in real catalogs)
		}
		visited[next] = true
		partStrOffset := u32(r.buf, next)
		nextPart := u32(r.buf, next+4)
		part, isNil := r.readEncodedString(partStrOffset, 0)
		if !isNil {
			parts = append(parts, part)
		}
		if nextPart == uint32Max {
			break
		}
		next = nextPart
	}
	if len(parts) == 0 {
		return "", true
	}
	if r.version > 1 {
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
	}
	if len(parts) == 1 {
		return parts[0], false
	}
	return strings.Join(parts, string(rune(sep))), false
}

func (r *Reader) readOffsetArray(encodedOffset uint32) []uint32 {
	if encodedOffset == uint32Max {
		return nil
	}
	if v, ok := r.offsetArrCache[encodedOffset]; ok {
		return v
	}
	byteSize := i32(r.buf, encodedOffset-4)
	count := int(byteSize) / 4
	result := make([]uint32, count)
	for i := 0; i < count; i++ {
		result[i] = u32(r.buf, encodedOffset+uint32(i*4))
	}
	r.offsetArrCache[encodedOffset] = result
	return result
}

func (r *Reader) readSerializedType(offset uint32) SerializedType {
	if v, ok := r.typeCache[offset]; ok {
		return v
	}
	assemblyOff := u32(r.buf, offset)
	classOff := u32(r.buf, offset+4)
	assembly, assemblyNil := r.readEncodedString(assemblyOff, '.')
	class, classNil := r.readEncodedString(classOff, '.')
	var t SerializedType
	if !assemblyNil {
		t.Assembly = &assembly
	}
	if !classNil {
		t.Class = &class
	}
	r.typeCache[offset] = t
	return t
}

func hash128Hex(a, b, c, d uint32) string {
	buf := make([]byte, 16)
	binary.LittleEndian.PutUint32(buf[0:4], a)
	binary.LittleEndian.PutUint32(buf[4:8], b)
	binary.LittleEndian.PutUint32(buf[8:12], c)
	binary.LittleEndian.PutUint32(buf[12:16], d)
	return hex.EncodeToString(buf)
}

func (r *Reader) decodeValue(offset uint32) decodedValue {
	if offset == uint32Max {
		return decodedValue{kind: kindNil}
	}
	typeNameOffset := u32(r.buf, offset)
	objectOffset := u32(r.buf, offset+4)
	isDefault := objectOffset == uint32Max
	stype := r.readSerializedType(typeNameOffset)
	matchName := stype.matchName(r.version)

	switch matchName {
	case "mscorlib; System.Int32", "System.Int32":
		if isDefault {
			return decodedValue{kind: kindInt32}
		}
		return decodedValue{kind: kindInt32, i32: i32(r.buf, objectOffset)}
	case "mscorlib; System.Int64", "System.Int64":
		if isDefault {
			return decodedValue{kind: kindInt64}
		}
		return decodedValue{kind: kindInt64, i64: int64(binary.LittleEndian.Uint64(r.buf[objectOffset : objectOffset+8]))}
	case "mscorlib; System.Boolean", "System.Boolean":
		if isDefault {
			return decodedValue{kind: kindBool}
		}
		return decodedValue{kind: kindBool, b: r.buf[objectOffset] != 0}
	case "mscorlib; System.String", "System.String":
		if isDefault {
			return decodedValue{kind: kindNil}
		}
		strOffset := u32(r.buf, objectOffset)
		sepByte := r.buf[objectOffset+4]
		s, isNil := r.readEncodedString(strOffset, sepByte)
		if isNil {
			return decodedValue{kind: kindNil}
		}
		return decodedValue{kind: kindString, str: s}
	case "UnityEngine.CoreModule; UnityEngine.Hash128":
		if isDefault {
			return decodedValue{kind: kindNil}
		}
		a, b, c, d := u32(r.buf, objectOffset), u32(r.buf, objectOffset+4), u32(r.buf, objectOffset+8), u32(r.buf, objectOffset+12)
		return decodedValue{kind: kindHash128, str: hash128Hex(a, b, c, d)}
	case "Unity.ResourceManager; UnityEngine.ResourceManagement.ResourceProviders.AssetBundleRequestOptions":
		if isDefault {
			return decodedValue{kind: kindNil}
		}
		return decodedValue{kind: kindBundleOptions, bundle: r.decodeBundleOptions(objectOffset)}
	default:
		panic(fmt.Sprintf("unsupported serialized object type: %q", matchName))
	}
}

func (r *Reader) decodeCommonInfo(offset uint32) *CommonInfo {
	if v, ok := r.commonInfoCache[offset]; ok {
		return v
	}
	timeout := int16(binary.LittleEndian.Uint16(r.buf[offset : offset+2]))
	redirectLimit := r.buf[offset+2]
	retryCount := r.buf[offset+3]
	flags := i32(r.buf, offset+4)

	loadMode := "REQUESTED_ASSET_AND_DEPENDENCIES"
	if flags&1 != 0 {
		loadMode = "ALL_PACKED_ASSETS_AND_DEPENDENCIES"
	}
	ci := &CommonInfo{
		Timeout:                            timeout,
		RedirectLimit:                      redirectLimit,
		RetryCount:                         retryCount,
		AssetLoadMode:                      loadMode,
		ChunkedTransfer:                    flags&2 != 0,
		UseCrcForCachedBundle:              flags&4 != 0,
		UseUnityWebRequestForLocalBundles:  flags&8 != 0,
		ClearOtherCachedVersionsWhenLoaded: flags&16 != 0,
	}
	r.commonInfoCache[offset] = ci
	return ci
}

func (r *Reader) decodeBundleOptions(offset uint32) *AssetBundleRequestOptions {
	if v, ok := r.bundleCache[offset]; ok {
		return v
	}
	hashOffset := u32(r.buf, offset)
	bundleNameOffset := u32(r.buf, offset+4)
	crc := u32(r.buf, offset+8)
	bundleSize := u32(r.buf, offset+12)
	commonInfoOffset := u32(r.buf, offset+16)

	a, b, c, d := u32(r.buf, hashOffset), u32(r.buf, hashOffset+4), u32(r.buf, hashOffset+8), u32(r.buf, hashOffset+12)
	hashHex := hash128Hex(a, b, c, d)

	ci := r.decodeCommonInfo(commonInfoOffset)
	ci.Version = 3 // matches decode_asset_bundle_request_options_binary override

	bundleName, isNil := r.readEncodedString(bundleNameOffset, '_')
	var bnPtr *string
	if !isNil {
		bnPtr = &bundleName
	}

	opts := &AssetBundleRequestOptions{
		Hash:       hashHex,
		Crc:        crc,
		CommonInfo: ci,
		BundleName: bnPtr,
		BundleSize: bundleSize,
	}
	r.bundleCache[offset] = opts
	return opts
}

func (r *Reader) resourceLocationFromBinary(offset uint32) *ResourceLocation {
	if v, ok := r.locCache[offset]; ok {
		return v
	}
	primaryKeyOffset := u32(r.buf, offset)
	internalIDOffset := u32(r.buf, offset+4)
	providerIDOffset := u32(r.buf, offset+8)
	dependenciesOffset := u32(r.buf, offset+12)
	dependencyHashCode := i32(r.buf, offset+16)
	dataOffset := u32(r.buf, offset+20)
	typeOffset := u32(r.buf, offset+24)

	primaryKey, _ := r.readEncodedString(primaryKeyOffset, '/')
	internalID, _ := r.readEncodedString(internalIDOffset, '/')
	providerID, _ := r.readEncodedString(providerIDOffset, '.')

	depOffsets := r.readOffsetArray(dependenciesOffset)
	deps := make([]*ResourceLocation, 0, len(depOffsets))
	for _, do := range depOffsets {
		deps = append(deps, r.resourceLocationFromBinary(do))
	}

	data := r.decodeValue(dataOffset)
	stype := r.readSerializedType(typeOffset)

	loc := &ResourceLocation{
		PrimaryKey:         primaryKey,
		InternalID:         internalID,
		ProviderID:         providerID,
		Type:               stype,
		Dependencies:       deps,
		Data:               data,
		DependencyHashCode: dependencyHashCode,
	}
	r.locCache[offset] = loc
	return loc
}

func decodedValueToKeyString(v decodedValue) string {
	switch v.kind {
	case kindString, kindHash128:
		return v.str
	case kindInt32:
		return strconv.FormatInt(int64(v.i32), 10)
	case kindInt64:
		return strconv.FormatInt(v.i64, 10)
	case kindBool:
		return strconv.FormatBool(v.b)
	default:
		return ""
	}
}

type header struct {
	version           int
	keysOffset        uint32
	idOffset          uint32
	buildResultHashOF uint32
}

func readHeader(buf []byte) (header, error) {
	var h header
	version := int(i32(buf, 4))
	if version < 1 || version > 3 {
		return h, fmt.Errorf("unsupported catalog version %d (only 1-3 are supported)", version)
	}
	h.version = version
	h.keysOffset = u32(buf, 8)
	h.idOffset = u32(buf, 12)
	// bytes 16,20,24 are instance/scene/init-objects offsets -- not needed
	// for this export, but still occupy their slots in the fixed layout.
	if version == 1 && h.keysOffset == 0x20 {
		h.buildResultHashOF = uint32Max
	} else {
		h.buildResultHashOF = u32(buf, 28)
	}
	return h, nil
}

// decodeBinaryResources walks key/location-list offset pairs in file order.
func (r *Reader) decodeBinaryResources(h header) []resourceEntry {
	pairs := r.readOffsetArray(h.keysOffset)
	entries := make([]resourceEntry, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		keyOffset := pairs[i]
		locListOffset := pairs[i+1]
		keyVal := r.decodeValue(keyOffset)
		keyStr := decodedValueToKeyString(keyVal)

		locOffsets := r.readOffsetArray(locListOffset)
		locs := make([]*ResourceLocation, 0, len(locOffsets))
		for _, lo := range locOffsets {
			locs = append(locs, r.resourceLocationFromBinary(lo))
		}
		entries = append(entries, resourceEntry{key: keyStr, locs: locs})
	}
	return entries
}

// TypeInfo is the (assembly, class) pair of a location's result type.
type TypeInfo struct {
	Assembly *string `json:"assembly"`
	Class    *string `json:"class"`
}

// Location is one resource location of the catalog.
type Location struct {
	ID           int                        `json:"id"`
	PrimaryKey   string                     `json:"primary_key"`
	InternalID   string                     `json:"internal_id"`
	ProviderID   string                     `json:"provider_id"`
	Type         TypeInfo                   `json:"type"`
	IsBundle     bool                       `json:"is_bundle"`
	Dependencies []int                      `json:"dependencies"`
	BundleInfo   *AssetBundleRequestOptions `json:"bundle_info,omitempty"`
}

// Bundle is a location that points at an AssetBundle file. Path is the
// location's internal id: either a full https URL on the CDN, or a
// "{UnityEngine.AddressableAssets.Addressables.RuntimePath}\\..." path for
// bundles that ship with the game itself.
type Bundle struct {
	LocationID int     `json:"location_id"`
	BundleName *string `json:"bundle_name"`
	Hash       string  `json:"hash"`
	Crc        uint32  `json:"crc"`
	SizeBytes  uint32  `json:"size_bytes"`
	Path       string  `json:"path"`
}

// Name returns the bundle's catalog name ("" when the catalog has none).
func (b Bundle) Name() string {
	if b.BundleName == nil {
		return ""
	}
	return *b.BundleName
}

// Meta is the catalog-level header information.
type Meta struct {
	Version         int     `json:"version"`
	LocatorID       *string `json:"locator_id"`
	BuildResultHash *string `json:"build_result_hash"`
}

// Stats are element counts, as exported to JSON.
type Stats struct {
	Keys      int `json:"keys"`
	Locations int `json:"locations"`
	Bundles   int `json:"bundles"`
}

// Catalog is the fully decoded catalog.
type Catalog struct {
	SourceFile string           `json:"source_file"`
	Meta       Meta             `json:"catalog"`
	Stats      Stats            `json:"stats"`
	Bundles    []Bundle         `json:"bundles"`
	Locations  []Location       `json:"locations"`
	Keys       map[string][]int `json:"keys"`
}

// BuildHash returns catalog.build_result_hash, or "" when absent.
func (c *Catalog) BuildHash() string {
	if c.Meta.BuildResultHash == nil {
		return ""
	}
	return *c.Meta.BuildResultHash
}

// Decode parses the bytes of a binary catalog. sourceName is recorded (base
// name only) in Catalog.SourceFile. Malformed input yields an error, never a
// panic: the decoder indexes raw offsets, so a truncated or non-catalog file
// (say, an HTML error page from a CDN) would otherwise crash the caller.
func Decode(buf []byte, sourceName string) (cat *Catalog, err error) {
	defer func() {
		if r := recover(); r != nil {
			cat, err = nil, fmt.Errorf("malformed catalog: %v", r)
		}
	}()

	h, err := readHeader(buf)
	if err != nil {
		return nil, err
	}
	r := newReader(buf, h.version)

	locatorID, locatorNil := r.readEncodedString(h.idOffset, 0)
	buildHash, buildHashNil := r.readEncodedString(h.buildResultHashOF, 0)

	entries := r.decodeBinaryResources(h)

	// Preserve first-seen location order for stable IDs.
	locID := make(map[*ResourceLocation]int)
	var allLocs []*ResourceLocation
	for _, e := range entries {
		for _, loc := range e.locs {
			if _, ok := locID[loc]; !ok {
				locID[loc] = len(allLocs)
				allLocs = append(allLocs, loc)
			}
		}
	}

	result := &Catalog{SourceFile: baseName(sourceName)}
	result.Meta.Version = h.version
	if !locatorNil {
		result.Meta.LocatorID = &locatorID
	}
	if !buildHashNil {
		result.Meta.BuildResultHash = &buildHash
	}

	result.Locations = make([]Location, len(allLocs))
	for i, loc := range allLocs {
		isBundle := loc.Data.kind == kindBundleOptions
		deps := make([]int, len(loc.Dependencies))
		for j, d := range loc.Dependencies {
			deps[j] = locID[d]
		}
		out := Location{
			ID:           i,
			PrimaryKey:   loc.PrimaryKey,
			InternalID:   loc.InternalID,
			ProviderID:   loc.ProviderID,
			Type:         TypeInfo{Assembly: loc.Type.Assembly, Class: loc.Type.Class},
			IsBundle:     isBundle,
			Dependencies: deps,
		}
		if isBundle {
			out.BundleInfo = loc.Data.bundle
			result.Bundles = append(result.Bundles, Bundle{
				LocationID: i,
				BundleName: loc.Data.bundle.BundleName,
				Hash:       loc.Data.bundle.Hash,
				Crc:        loc.Data.bundle.Crc,
				SizeBytes:  loc.Data.bundle.BundleSize,
				Path:       loc.InternalID,
			})
		}
		result.Locations[i] = out
	}

	result.Keys = make(map[string][]int, len(entries))
	for _, e := range entries {
		ids := make([]int, len(e.locs))
		for i, loc := range e.locs {
			ids[i] = locID[loc]
		}
		result.Keys[e.key] = ids
	}

	result.Stats.Keys = len(result.Keys)
	result.Stats.Locations = len(result.Locations)
	result.Stats.Bundles = len(result.Bundles)
	return result, nil
}

// DecodeFile reads and decodes a catalog_S1.bin from disk.
func DecodeFile(path string) (*Catalog, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(buf, path)
}

// Save writes the catalog as indented JSON.
func (c *Catalog) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Load reads catalog JSON.
func Load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	if c.Keys == nil {
		c.Keys = make(map[string][]int)
	}
	return &c, nil
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}
