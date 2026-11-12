package extractor

import "fmt"

// lz4DecompressBlock decodes a raw LZ4 "block" (no frame header/magic,
// which is exactly what Unity stores inside its bundle StorageBlocks) into
// a buffer of the known decompressed size. LZ4HC-compressed data uses the
// identical block format on the decode side, so this covers both.
func lz4DecompressBlock(src []byte, dstLen int) ([]byte, error) {
	dst := make([]byte, dstLen)
	var si, di int

	for si < len(src) {
		token := src[si]
		si++

		litLen := int(token >> 4)
		if litLen == 15 {
			for {
				if si >= len(src) {
					return nil, fmt.Errorf("lz4: truncated literal-length byte")
				}
				b := src[si]
				si++
				litLen += int(b)
				if b != 255 {
					break
				}
			}
		}

		if si+litLen > len(src) {
			return nil, fmt.Errorf("lz4: literal run overruns source")
		}
		if di+litLen > dstLen {
			return nil, fmt.Errorf("lz4: literal run overruns destination")
		}
		copy(dst[di:di+litLen], src[si:si+litLen])
		si += litLen
		di += litLen

		// A block can legally end right after a literal run.
		if si >= len(src) {
			break
		}

		if si+2 > len(src) {
			return nil, fmt.Errorf("lz4: truncated match offset")
		}
		offset := int(src[si]) | int(src[si+1])<<8
		si += 2
		if offset == 0 || offset > di {
			return nil, fmt.Errorf("lz4: invalid match offset %d at output position %d", offset, di)
		}

		matchLen := int(token&0x0F) + 4
		if token&0x0F == 15 {
			for {
				if si >= len(src) {
					return nil, fmt.Errorf("lz4: truncated match-length byte")
				}
				b := src[si]
				si++
				matchLen += int(b)
				if b != 255 {
					break
				}
			}
		}

		if di+matchLen > dstLen {
			return nil, fmt.Errorf("lz4: match run overruns destination")
		}
		copyFrom := di - offset
		// Byte-by-byte copy: offset can be smaller than matchLen (repeating
		// pattern), so this must not use copy() with overlapping slices.
		for i := 0; i < matchLen; i++ {
			dst[di+i] = dst[copyFrom+i]
		}
		di += matchLen
	}

	if di != dstLen {
		return nil, fmt.Errorf("lz4: produced %d bytes, expected %d", di, dstLen)
	}
	return dst, nil
}
