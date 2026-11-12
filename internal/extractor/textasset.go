package extractor

import "fmt"

type TextAssetInfo struct {
	Name   string
	PathID int64
	Script string
}

// readTextAsset reads m_Name and m_Script using TypeTree, with a fixed-layout fallback.
func readTextAsset(sf *SerializedFile, obj objectEntry) (*TextAssetInfo, error) {
	if obj.ByteStart < 0 || obj.ByteSize == 0 || obj.ByteStart+int64(obj.ByteSize) > int64(len(sf.Data)) {
		return nil, fmt.Errorf("object byte range out of bounds")
	}

	if int(obj.TypeID) >= 0 && int(obj.TypeID) < len(sf.Types) {
		if nodes := sf.Types[obj.TypeID].Nodes; len(nodes) > 0 {
			r := NewReader(sf.Data, !sf.LittleEndian)
			r.SeekTo(int(obj.ByteStart))
			fields, err := readObjectFields(nodes, r)
			if err == nil {
				name, _ := fields["m_Name"].(string)
				script, ok := fields["m_Script"].(string)
				if !ok {
					// Some Unity versions store m_Script as a raw byte
					// vector rather than a "string"-typed node.
					script = string(ttBytes(fields["m_Script"]))
				}
				return &TextAssetInfo{Name: name, PathID: obj.PathID, Script: script}, nil
			}
		}
	}

	// Hardcoded fallback: NamedObject.m_Name, then m_Script, both plain
	// length-prefixed strings, nothing version-dependent in between.
	r := NewReader(sf.Data, !sf.LittleEndian)
	r.SeekTo(int(obj.ByteStart))
	name, err := r.ReadAlignedString()
	if err != nil {
		return nil, fmt.Errorf("reading m_Name: %w", err)
	}
	script, err := r.ReadAlignedString()
	if err != nil {
		return nil, fmt.Errorf("reading m_Script: %w", err)
	}
	return &TextAssetInfo{Name: name, PathID: obj.PathID, Script: script}, nil
}
