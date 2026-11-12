package extractor

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// readGenericObjectJSON reads obj's fields via its embedded TypeTree (if
// any) and returns them as indented JSON, for exploring classes that have
// no dedicated extractor (MonoBehaviour, Sprite, AnimationClip, ...).
func readGenericObjectJSON(sf *SerializedFile, obj objectEntry) (name string, data []byte, err error) {
	if int(obj.TypeID) < 0 || int(obj.TypeID) >= len(sf.Types) {
		return "", nil, fmt.Errorf("no type information for this object")
	}
	nodes := sf.Types[obj.TypeID].Nodes
	if len(nodes) == 0 {
		return "", nil, fmt.Errorf("%s has no embedded TypeTree in this file, so its fields can't be read generically", classIDName(obj.ClassID))
	}
	if obj.ByteStart < 0 || obj.ByteSize == 0 || obj.ByteStart+int64(obj.ByteSize) > int64(len(sf.Data)) {
		return "", nil, fmt.Errorf("object byte range out of bounds")
	}

	r := NewReader(sf.Data, !sf.LittleEndian)
	r.SeekTo(int(obj.ByteStart))
	fields, err := readObjectFields(nodes, r)
	if err != nil {
		return "", nil, fmt.Errorf("type-tree read: %w", err)
	}
	name, _ = fields["m_Name"].(string)

	clean := sanitizeForJSON(fields)
	js, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return name, nil, fmt.Errorf("encoding json: %w", err)
	}
	return name, js, nil
}

// sanitizeForJSON recursively converts a readGenericValue() result tree
// into something encoding/json can render usefully: byte slices become
// either plain strings (if they look like text) or a short "<binary: N
// bytes>" placeholder (avoids dumping megabytes of base64 for a mesh or
// animation curve's raw data).
func sanitizeForJSON(v interface{}) interface{} {
	switch val := v.(type) {
	case []byte:
		if len(val) > 0 && len(val) <= 4096 && utf8.Valid(val) && isMostlyPrintable(val) {
			return string(val)
		}
		return fmt.Sprintf("<binary: %d bytes>", len(val))
	case map[string]interface{}:
		out := make(map[string]interface{}, len(val))
		for k, e := range val {
			out[k] = sanitizeForJSON(e)
		}
		return out
	case []interface{}:
		if len(val) > 2000 {
			return fmt.Sprintf("<array: %d elements, truncated>", len(val))
		}
		out := make([]interface{}, len(val))
		for i, e := range val {
			out[i] = sanitizeForJSON(e)
		}
		return out
	default:
		return val
	}
}

func isMostlyPrintable(b []byte) bool {
	printable := 0
	for _, r := range string(b) {
		if r == '\n' || r == '\r' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			printable++
		}
	}
	return printable > len(b)*9/10
}
