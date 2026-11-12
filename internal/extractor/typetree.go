package extractor

import "fmt"

const alignFlag = 0x4000

// ttGetNodes collects node i and every descendant of it (anything with a
// deeper Level, stopping at the first node whose Level is <= node i's own).
func ttGetNodes(nodes []typeTreeNode, index int) []typeTreeNode {
	level := nodes[index].Level
	out := []typeTreeNode{nodes[index]}
	for i := index + 1; i < len(nodes); i++ {
		if nodes[i].Level <= level {
			break
		}
		out = append(out, nodes[i])
	}
	return out
}

// readGenericValue reads the value described by nodes[*i] (and, for
// composite types, its descendants), advancing *i to the last node index it
// consumed. This mirrors Unity's own TypeTree interpretation: known
// primitive type names are read directly; "string" and "TypelessData" are
// special-cased; anything immediately followed by a child literally named
// type "Array" is a dynamic array; everything else is a nested struct whose
// children are read in order.
func readGenericValue(nodes []typeTreeNode, i *int, r *Reader) (interface{}, error) {
	node := nodes[*i]
	align := node.MetaFlag&alignFlag != 0

	readPrimitive := func() (interface{}, bool, error) {
		switch node.Type {
		case "SInt8":
			v, err := r.ReadByte()
			return int8(v), true, err
		case "UInt8", "char":
			v, err := r.ReadByte()
			return v, true, err
		case "short", "SInt16":
			v, err := r.ReadI16()
			return v, true, err
		case "UInt16", "unsigned short":
			v, err := r.ReadU16()
			return v, true, err
		case "int", "SInt32":
			v, err := r.ReadI32()
			return v, true, err
		case "UInt32", "unsigned int", "Type*":
			v, err := r.ReadU32()
			return v, true, err
		case "long long", "SInt64":
			v, err := r.ReadI64()
			return v, true, err
		case "UInt64", "unsigned long long", "FileSize":
			v, err := r.ReadU64()
			return v, true, err
		case "float":
			v, err := r.ReadF32()
			return v, true, err
		case "bool":
			v, err := r.ReadBool()
			return v, true, err
		default:
			return nil, false, nil
		}
	}

	if v, handled, err := readPrimitive(); handled {
		if err != nil {
			return nil, err
		}
		if align {
			r.Align(4)
		}
		return v, nil
	}

	switch node.Type {
	case "string":
		v, err := r.ReadAlignedString() // Unity always length-prefixes+aligns strings itself
		if err != nil {
			return nil, err
		}
		skip := ttGetNodes(nodes, *i)
		*i += len(skip) - 1
		return v, nil

	case "TypelessData":
		size, err := r.ReadI32()
		if err != nil {
			return nil, err
		}
		if size < 0 || int(size) > r.Remaining() {
			return nil, fmt.Errorf("implausible TypelessData size %d", size)
		}
		data, err := r.ReadBytes(int(size))
		if err != nil {
			return nil, err
		}
		*i += 2 // this node + the "size" child AssetStudio's format always emits here
		return data, nil

	default:
		if *i < len(nodes)-1 && nodes[*i+1].Type == "Array" {
			if nodes[*i+1].MetaFlag&alignFlag != 0 {
				align = true
			}
			whole := ttGetNodes(nodes, *i)
			*i += len(whole) - 1
			size, err := r.ReadI32()
			if err != nil {
				return nil, err
			}
			if size < 0 || size > 50_000_000 {
				return nil, fmt.Errorf("implausible array size %d for %s", size, node.Name)
			}
			// whole[0]=array field itself, whole[1]="Array" node,
			// whole[2]="size" node, whole[3]=the element type node.
			if len(whole) < 4 {
				return nil, fmt.Errorf("malformed array type-tree for %s", node.Name)
			}
			elemNode := whole[3:]
			list := make([]interface{}, size)
			for j := int32(0); j < size; j++ {
				tmp := 0
				v, err := readGenericValue(elemNode, &tmp, r)
				if err != nil {
					return nil, err
				}
				list[j] = v
			}
			if align {
				r.Align(4)
			}
			return list, nil
		}

		// Nested struct/class: read every direct child in order.
		whole := ttGetNodes(nodes, *i)
		*i += len(whole) - 1
		obj := make(map[string]interface{}, len(whole)-1)
		for j := 1; j < len(whole); {
			child := whole[j]
			v, err := readGenericValue(whole, &j, r)
			if err != nil {
				return nil, err
			}
			obj[child.Name] = v
			j++
		}
		if align {
			r.Align(4)
		}
		return obj, nil
	}
}

// readObjectFields reads every top-level field of an object whose root
// TypeTree node is nodes[0] (e.g. "Texture2D"), returning field name ->
// value for everything at the top level.
func readObjectFields(nodes []typeTreeNode, r *Reader) (map[string]interface{}, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no type-tree nodes available")
	}
	obj := make(map[string]interface{})
	for i := 1; i < len(nodes); {
		child := nodes[i]
		v, err := readGenericValue(nodes, &i, r)
		if err != nil {
			return nil, err
		}
		obj[child.Name] = v
		i++
	}
	return obj, nil
}
