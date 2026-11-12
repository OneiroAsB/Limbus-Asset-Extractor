package extractor

import "fmt"

const (
	archiveCompressionTypeMask         = 0x3f
	archiveBlocksInfoAtTheEnd          = 0x80
	archiveBlockInfoNeedPaddingAtStart = 0x200
)

const (
	compressionNone  = 0
	compressionLzma  = 1
	compressionLz4   = 2
	compressionLz4HC = 3
	compressionLzham = 4
)

type bundleNode struct {
	Offset int64
	Size   int64
	Flags  uint32
	Path   string
}

type storageBlock struct {
	UncompressedSize uint32
	CompressedSize   uint32
	Flags            uint16
}

// ParseBundle reads a UnityFS .bundle file and returns the decompressed
// contents of every entry it contains, keyed by the entry's base file name
// (e.g. "CAB-xxxxxxxx" for the main serialized file, "CAB-xxxxxxxx.resS"
// for its streamed resource data).
func ParseBundle(raw []byte) (map[string][]byte, error) {
	r := NewReader(raw, true) // headers are big-endian

	signature, err := r.ReadCString()
	if err != nil {
		return nil, fmt.Errorf("reading signature: %w", err)
	}
	version, err := r.ReadU32()
	if err != nil {
		return nil, err
	}
	if _, err := r.ReadCString(); err != nil { // unityVersion
		return nil, err
	}
	if _, err := r.ReadCString(); err != nil { // unityRevision
		return nil, err
	}

	if signature != "UnityFS" {
		return nil, fmt.Errorf("unsupported bundle signature %q (only modern \"UnityFS\" bundles are supported)", signature)
	}

	size, err := r.ReadI64()
	if err != nil {
		return nil, err
	}
	compressedBlocksInfoSize, err := r.ReadU32()
	if err != nil {
		return nil, err
	}
	uncompressedBlocksInfoSize, err := r.ReadU32()
	if err != nil {
		return nil, err
	}
	flags, err := r.ReadU32()
	if err != nil {
		return nil, err
	}
	_ = size

	if version >= 7 {
		r.Align(16)
	}

	var blocksInfoBytes []byte
	if flags&archiveBlocksInfoAtTheEnd != 0 {
		saved := r.Pos()
		r.SeekTo(len(raw) - int(compressedBlocksInfoSize))
		blocksInfoBytes, err = r.ReadBytes(int(compressedBlocksInfoSize))
		if err != nil {
			return nil, fmt.Errorf("reading trailing blocks-info: %w", err)
		}
		r.SeekTo(saved)
	} else {
		blocksInfoBytes, err = r.ReadBytes(int(compressedBlocksInfoSize))
		if err != nil {
			return nil, fmt.Errorf("reading blocks-info: %w", err)
		}
	}

	compressionType := flags & archiveCompressionTypeMask
	var blocksInfoPlain []byte
	switch compressionType {
	case compressionNone:
		blocksInfoPlain = blocksInfoBytes
	case compressionLz4, compressionLz4HC:
		blocksInfoPlain, err = lz4DecompressBlock(blocksInfoBytes, int(uncompressedBlocksInfoSize))
		if err != nil {
			return nil, fmt.Errorf("decompressing blocks-info: %w", err)
		}
	case compressionLzma:
		return nil, fmt.Errorf("this bundle's directory is LZMA-compressed, which this tool does not implement yet")
	default:
		return nil, fmt.Errorf("unsupported blocks-info compression type %d", compressionType)
	}

	bi := NewReader(blocksInfoPlain, true)
	if _, err := bi.ReadBytes(16); err != nil { // uncompressedDataHash
		return nil, err
	}
	blockCount, err := bi.ReadI32()
	if err != nil {
		return nil, err
	}
	blocks := make([]storageBlock, blockCount)
	for i := range blocks {
		u, err := bi.ReadU32()
		if err != nil {
			return nil, err
		}
		c, err := bi.ReadU32()
		if err != nil {
			return nil, err
		}
		f, err := bi.ReadU16()
		if err != nil {
			return nil, err
		}
		blocks[i] = storageBlock{UncompressedSize: u, CompressedSize: c, Flags: f}
	}

	nodeCount, err := bi.ReadI32()
	if err != nil {
		return nil, err
	}
	nodes := make([]bundleNode, nodeCount)
	for i := range nodes {
		off, err := bi.ReadI64()
		if err != nil {
			return nil, err
		}
		sz, err := bi.ReadI64()
		if err != nil {
			return nil, err
		}
		fl, err := bi.ReadU32()
		if err != nil {
			return nil, err
		}
		path, err := bi.ReadCString()
		if err != nil {
			return nil, err
		}
		nodes[i] = bundleNode{Offset: off, Size: sz, Flags: fl, Path: path}
	}

	if flags&archiveBlockInfoNeedPaddingAtStart != 0 {
		r.Align(16)
	}

	// Decompress every storage block into one contiguous buffer; directory
	// nodes are then simple byte ranges within it.
	var combined []byte
	for _, b := range blocks {
		compressed, err := r.ReadBytes(int(b.CompressedSize))
		if err != nil {
			return nil, fmt.Errorf("reading storage block: %w", err)
		}
		blockCompression := uint32(b.Flags) & archiveCompressionTypeMask
		switch blockCompression {
		case compressionNone:
			combined = append(combined, compressed...)
		case compressionLz4, compressionLz4HC:
			plain, err := lz4DecompressBlock(compressed, int(b.UncompressedSize))
			if err != nil {
				return nil, fmt.Errorf("decompressing storage block: %w", err)
			}
			combined = append(combined, plain...)
		case compressionLzma:
			return nil, fmt.Errorf("this bundle uses LZMA-compressed data blocks, which this tool does not implement yet")
		default:
			return nil, fmt.Errorf("unsupported storage block compression type %d", blockCompression)
		}
	}

	entries := make(map[string][]byte, len(nodes))
	for _, n := range nodes {
		if n.Offset < 0 || n.Size < 0 || int(n.Offset+n.Size) > len(combined) {
			return nil, fmt.Errorf("directory entry %q has an out-of-range byte range", n.Path)
		}
		entries[baseName(n.Path)] = combined[n.Offset : n.Offset+n.Size]
	}
	return entries, nil
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}
