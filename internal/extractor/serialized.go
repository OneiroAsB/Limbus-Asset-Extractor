package extractor

import "fmt"

// SerializedFileFormatVersion thresholds, matching Unity's own history.
const (
	fvUnknown2                  = 2
	fvUnknown3                  = 3
	fvUnknown5                  = 5
	fvUnknown6                  = 6
	fvUnknown7                  = 7
	fvUnknown8                  = 8
	fvUnknown9                  = 9
	fvUnknown10                 = 10
	fvHasScriptTypeIndex        = 11
	fvUnknown12                 = 12
	fvHasTypeTreeHashes         = 13
	fvUnknown14                 = 14
	fvSupportsStrippedObject    = 15
	fvRefactoredClassId         = 16
	fvRefactorTypeData          = 17
	fvTypeTreeNodeWithTypeFlags = 19
	fvStoresTypeDependencies    = 21
	fvLargeFilesSupport         = 22
)

type typeTreeNode struct {
	Level    uint8
	Type     string
	Name     string
	MetaFlag int32
}

type serializedType struct {
	ClassID int32
	Nodes   []typeTreeNode // nil if this file has no embedded TypeTree for this type
}

type objectEntry struct {
	PathID    int64
	ByteStart int64
	ByteSize  uint32
	ClassID   int32
	TypeID    int32
}

type SerializedFile struct {
	Objects      []objectEntry
	Types        []serializedType
	UnityVersion [4]int
	Data         []byte // the file's own buffer; ByteStart/ByteSize index into this
	LittleEndian bool
	Platform     int32
}

// parseSerializedFile parses a Unity "SerializedFile" (the main asset
// listing inside a bundle entry, e.g. "CAB-xxxxxxxx"). Only the parts
// needed to locate objects by class ID are read; script/external/user
// metadata that follows the object table is never touched.
//
// fallbackVersion is used when the file's own embedded Unity version
// string is blank or "0.0.0" (some build pipelines strip it deliberately),
// since Texture2D field layouts depend on the engine version.
func parseSerializedFile(buf []byte, fallbackVersion [4]int) (*SerializedFile, error) {
	r := NewReader(buf, true) // header is always big-endian

	metadataSize, err := r.ReadU32()
	if err != nil {
		return nil, err
	}
	_ = metadataSize
	fileSize32, err := r.ReadU32()
	if err != nil {
		return nil, err
	}
	version, err := r.ReadU32()
	if err != nil {
		return nil, err
	}
	if version < 3 || version > 30 {
		return nil, fmt.Errorf("implausible SerializedFile format version %d (not a serialized file?)", version)
	}
	dataOffset64 := int64(0)
	dataOffset32, err := r.ReadU32()
	if err != nil {
		return nil, err
	}
	dataOffset64 = int64(dataOffset32)

	var fileEndianess byte
	if version >= fvUnknown9 {
		fileEndianess, err = r.ReadByte()
		if err != nil {
			return nil, err
		}
		if _, err := r.ReadBytes(3); err != nil { // reserved
			return nil, err
		}
	} else {
		r.SeekTo(int(fileSize32) - int(metadataSize))
		fileEndianess, err = r.ReadByte()
		if err != nil {
			return nil, err
		}
	}

	if version >= fvLargeFilesSupport {
		if _, err := r.ReadU32(); err != nil { // metadataSize (64-bit era re-read)
			return nil, err
		}
		fs, err := r.ReadI64()
		if err != nil {
			return nil, err
		}
		_ = fs
		dataOffset64, err = r.ReadI64()
		if err != nil {
			return nil, err
		}
		if _, err := r.ReadI64(); err != nil { // unknown
			return nil, err
		}
	}

	littleEndian := fileEndianess == 0
	r.SetEndian(!littleEndian)

	var unityVersionStr string
	if version >= fvUnknown7 {
		unityVersionStr, err = r.ReadCString()
		if err != nil {
			return nil, err
		}
	}
	unityVersion := parseUnityVersion(unityVersionStr)
	if unityVersionStr == "" || unityVersionStr == "0.0.0" {
		// Stripped versions require a caller-supplied field-layout fallback.
		unityVersion = fallbackVersion
	}

	var targetPlatform int32 = -1 // BuildTarget.NoTarget default for very old files without this field
	if version >= fvUnknown8 {
		targetPlatform, err = r.ReadI32()
		if err != nil {
			return nil, err
		}
	}

	enableTypeTree := true
	if version >= fvHasTypeTreeHashes {
		enableTypeTree, err = r.ReadBool()
		if err != nil {
			return nil, err
		}
	}

	typeCount, err := r.ReadI32()
	if err != nil {
		return nil, err
	}
	if typeCount < 0 || typeCount > 1_000_000 {
		return nil, fmt.Errorf("implausible type count %d", typeCount)
	}
	types := make([]serializedType, typeCount)
	for i := range types {
		t, err := readSerializedType(r, version, enableTypeTree)
		if err != nil {
			return nil, fmt.Errorf("reading type %d: %w", i, err)
		}
		types[i] = t
	}

	bigIDEnabled := int32(0)
	if version >= fvUnknown7 && version < fvUnknown14 {
		bigIDEnabled, err = r.ReadI32()
		if err != nil {
			return nil, err
		}
	}

	objectCount, err := r.ReadI32()
	if err != nil {
		return nil, err
	}
	if objectCount < 0 || objectCount > 50_000_000 {
		return nil, fmt.Errorf("implausible object count %d", objectCount)
	}
	objects := make([]objectEntry, objectCount)
	for i := 0; i < int(objectCount); i++ {
		var pathID int64
		if bigIDEnabled != 0 {
			pathID, err = r.ReadI64()
		} else if version < fvUnknown14 {
			var p32 int32
			p32, err = r.ReadI32()
			pathID = int64(p32)
		} else {
			r.Align(4)
			pathID, err = r.ReadI64()
		}
		if err != nil {
			return nil, err
		}

		var byteStart int64
		if version >= fvLargeFilesSupport {
			byteStart, err = r.ReadI64()
		} else {
			var b32 uint32
			b32, err = r.ReadU32()
			byteStart = int64(b32)
		}
		if err != nil {
			return nil, err
		}
		byteStart += dataOffset64

		byteSize, err := r.ReadU32()
		if err != nil {
			return nil, err
		}
		typeID, err := r.ReadI32()
		if err != nil {
			return nil, err
		}

		var classID int32
		if version < fvRefactoredClassId {
			cid, err := r.ReadU16()
			if err != nil {
				return nil, err
			}
			classID = int32(cid)
		} else {
			if int(typeID) < 0 || int(typeID) >= len(types) {
				return nil, fmt.Errorf("object %d has out-of-range typeID %d", i, typeID)
			}
			classID = types[typeID].ClassID
		}

		if version < fvHasScriptTypeIndex {
			if _, err := r.ReadU16(); err != nil { // isDestroyed
				return nil, err
			}
		}
		if version >= fvHasScriptTypeIndex && version < fvRefactorTypeData {
			if _, err := r.ReadI16(); err != nil { // scriptTypeIndex
				return nil, err
			}
		}
		if version == fvSupportsStrippedObject || version == fvRefactoredClassId {
			if _, err := r.ReadByte(); err != nil { // stripped
				return nil, err
			}
		}

		objects[i] = objectEntry{PathID: pathID, ByteStart: byteStart, ByteSize: byteSize, ClassID: classID, TypeID: typeID}
	}

	return &SerializedFile{
		Objects:      objects,
		Types:        types,
		UnityVersion: unityVersion,
		Data:         buf,
		LittleEndian: littleEndian,
		Platform:     targetPlatform,
	}, nil
}

func readSerializedType(r *Reader, version uint32, enableTypeTree bool) (serializedType, error) {
	classID, err := r.ReadI32()
	if err != nil {
		return serializedType{}, err
	}

	if version >= fvRefactoredClassId {
		if _, err := r.ReadBool(); err != nil { // m_IsStrippedType
			return serializedType{}, err
		}
	}

	scriptTypeIndex := int16(-1)
	if version >= fvRefactorTypeData {
		scriptTypeIndex, err = r.ReadI16()
		if err != nil {
			return serializedType{}, err
		}
	}

	if version >= fvHasTypeTreeHashes {
		needsScriptID := (version < fvRefactoredClassId && classID < 0) || (version >= fvRefactoredClassId && classID == 114)
		if needsScriptID {
			if _, err := r.ReadBytes(16); err != nil { // m_ScriptID
				return serializedType{}, err
			}
		}
		if _, err := r.ReadBytes(16); err != nil { // m_OldTypeHash
			return serializedType{}, err
		}
	}
	_ = scriptTypeIndex

	var nodes []typeTreeNode
	if enableTypeTree {
		if version >= fvUnknown12 || version == fvUnknown10 {
			nodes, err = readTypeTreeBlob(r, version)
			if err != nil {
				return serializedType{}, err
			}
		} else {
			nodes, err = readTypeTreeLegacy(r, version)
			if err != nil {
				return serializedType{}, err
			}
		}
		if version >= fvStoresTypeDependencies {
			// isRefType is always false here (we never parse the m_RefTypes
			// list, which comes after the object table), so this is always
			// m_TypeDependencies: an Int32Array (count, then count*4 bytes),
			// NOT a UInt8Array - the element size differs.
			n, err := r.ReadI32()
			if err != nil {
				return serializedType{}, err
			}
			if n < 0 || n > 1_000_000 {
				return serializedType{}, fmt.Errorf("implausible m_TypeDependencies count %d", n)
			}
			if _, err := r.ReadBytes(int(n) * 4); err != nil {
				return serializedType{}, err
			}
		}
	}

	return serializedType{ClassID: classID, Nodes: nodes}, nil
}

func readTypeTreeBlob(r *Reader, version uint32) ([]typeTreeNode, error) {
	nodeCount, err := r.ReadI32()
	if err != nil {
		return nil, err
	}
	stringBufferSize, err := r.ReadI32()
	if err != nil {
		return nil, err
	}
	if nodeCount < 0 || nodeCount > 5_000_000 || stringBufferSize < 0 || stringBufferSize > 200_000_000 {
		return nil, fmt.Errorf("implausible type-tree blob sizes (nodes=%d, stringBuf=%d)", nodeCount, stringBufferSize)
	}

	type rawNode struct {
		level         uint8
		typeStrOffset uint32
		nameStrOffset uint32
		metaFlag      int32
	}
	raw := make([]rawNode, nodeCount)
	for i := range raw {
		if _, err := r.ReadU16(); err != nil { // node version (unused)
			return nil, err
		}
		level, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if _, err := r.ReadByte(); err != nil { // type flags / is-array (unused: dispatch goes by type name)
			return nil, err
		}
		typeOff, err := r.ReadU32()
		if err != nil {
			return nil, err
		}
		nameOff, err := r.ReadU32()
		if err != nil {
			return nil, err
		}
		if _, err := r.ReadI32(); err != nil { // byteSize (unused)
			return nil, err
		}
		if _, err := r.ReadI32(); err != nil { // index (unused)
			return nil, err
		}
		metaFlag, err := r.ReadI32()
		if err != nil {
			return nil, err
		}
		if version >= fvTypeTreeNodeWithTypeFlags {
			if _, err := r.ReadU64(); err != nil { // refTypeHash (unused)
				return nil, err
			}
		}
		raw[i] = rawNode{level: level, typeStrOffset: typeOff, nameStrOffset: nameOff, metaFlag: metaFlag}
	}

	strBuf, err := r.ReadBytes(int(stringBufferSize))
	if err != nil {
		return nil, err
	}

	nodes := make([]typeTreeNode, nodeCount)
	for i, rn := range raw {
		nodes[i] = typeTreeNode{
			Level:    rn.level,
			Type:     resolveTypeTreeString(rn.typeStrOffset, strBuf),
			Name:     resolveTypeTreeString(rn.nameStrOffset, strBuf),
			MetaFlag: rn.metaFlag,
		}
	}
	return nodes, nil
}

func readTypeTreeLegacy(r *Reader, version uint32) ([]typeTreeNode, error) {
	var nodes []typeTreeNode
	var walk func(level uint8) error
	walk = func(level uint8) error {
		typ, err := r.ReadCString()
		if err != nil {
			return err
		}
		name, err := r.ReadCString()
		if err != nil {
			return err
		}
		if _, err := r.ReadI32(); err != nil { // byteSize
			return err
		}
		if version == fvUnknown2 {
			if _, err := r.ReadI32(); err != nil { // variableCount
				return err
			}
		}
		if version != fvUnknown3 {
			if _, err := r.ReadI32(); err != nil { // index
				return err
			}
		}
		if _, err := r.ReadI32(); err != nil { // typeFlags
			return err
		}
		if _, err := r.ReadI32(); err != nil { // node version
			return err
		}
		metaFlag := int32(0)
		if version != fvUnknown3 {
			metaFlag, err = r.ReadI32()
			if err != nil {
				return err
			}
		}
		nodes = append(nodes, typeTreeNode{Level: level, Type: typ, Name: name, MetaFlag: metaFlag})

		childCount, err := r.ReadI32()
		if err != nil {
			return err
		}
		for i := int32(0); i < childCount; i++ {
			if err := walk(level + 1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(0); err != nil {
		return nil, err
	}
	return nodes, nil
}
