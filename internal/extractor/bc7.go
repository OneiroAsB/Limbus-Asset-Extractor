package extractor

type bc7ModeInfo struct {
	partitions    int // subset count - 1 (0=1 subset, 1=2 subsets, 2=3 subsets)
	partitionBits int
	pBits         int
	rotationBits  int
	indexModeBits int
	indexPrec     int
	indexPrec2    int
	prec          [4]int // R,G,B,A precision without p-bit
	precP         [4]int // R,G,B,A precision with p-bit applied
}

// Mode parameters exactly as specified by the BC7 (BPTC) format, one entry
// per of the 8 possible modes encoded in a block's low bits.
var bc7Modes = [8]bc7ModeInfo{
	{2, 4, 6, 0, 0, 3, 0, [4]int{4, 4, 4, 0}, [4]int{5, 5, 5, 0}},
	{1, 6, 2, 0, 0, 3, 0, [4]int{6, 6, 6, 0}, [4]int{7, 7, 7, 0}},
	{2, 6, 0, 0, 0, 2, 0, [4]int{5, 5, 5, 0}, [4]int{5, 5, 5, 0}},
	{1, 6, 4, 0, 0, 2, 0, [4]int{7, 7, 7, 0}, [4]int{8, 8, 8, 0}},
	{0, 0, 0, 2, 1, 2, 3, [4]int{5, 5, 5, 6}, [4]int{5, 5, 5, 6}},
	{0, 0, 0, 2, 0, 2, 2, [4]int{7, 7, 7, 8}, [4]int{7, 7, 7, 8}},
	{0, 0, 2, 0, 0, 4, 0, [4]int{7, 7, 7, 7}, [4]int{8, 8, 8, 8}},
	{1, 6, 4, 0, 0, 2, 0, [4]int{5, 5, 5, 5}, [4]int{6, 6, 6, 6}},
}

type bc7BitReader struct {
	data []byte // 16 bytes
	pos  int    // bit offset, LSB of data[0] is bit 0
}

func (b *bc7BitReader) getBit() uint8 {
	idx := b.pos >> 3
	v := (b.data[idx] >> uint(b.pos-idx*8)) & 1
	b.pos++
	return v
}

// getBits reads n<=8 bits, matching CBits::GetBits (may span two bytes).
func (b *bc7BitReader) getBits(n int) uint8 {
	if n <= 0 {
		return 0
	}
	idx := b.pos >> 3
	base := b.pos - idx*8
	var ret uint8
	if base+n > 8 {
		firstBits := 8 - base
		nextBits := n - firstBits
		ret = uint8((uint32(b.data[idx]) >> uint(base)) | ((uint32(b.data[idx+1]) & ((1 << uint(nextBits)) - 1)) << uint(firstBits)))
	} else {
		ret = uint8((b.data[idx] >> uint(base)) & ((1 << uint(n)) - 1))
	}
	b.pos += n
	return ret
}

func bc7DetectMode(data []byte) int {
	pos := 0
	for pos < 128 {
		idx := pos >> 3
		bit := (data[idx] >> uint(pos-idx*8)) & 1
		pos++
		if bit != 0 {
			return pos - 1
		}
	}
	return 8 // reserved/invalid: decode as transparent black
}

func bc7IsAnchor(subsets, shape, pixel int) bool {
	if pixel == 0 {
		return true
	}
	switch subsets {
	case 1:
		return int(bc7Anchor2[shape]) == pixel
	case 2:
		a := bc7Anchor3[shape]
		return int(a[0]) == pixel || int(a[1]) == pixel
	default:
		return false
	}
}

func bc7Unquantize(comp uint8, prec int) uint8 {
	v := uint32(comp) << uint(8-prec)
	return uint8(v | (v >> uint(prec)))
}

func bc7Interpolate(c0, c1 uint8, w, prec int) uint8 {
	var weights []int
	switch prec {
	case 2:
		weights = bc7Weights2[:]
	case 3:
		weights = bc7Weights3[:]
	case 4:
		weights = bc7Weights4[:]
	default:
		return 0
	}
	wt := weights[w]
	v := (uint32(c0)*uint32(64-wt) + uint32(c1)*uint32(wt) + 32) >> 6
	return uint8(v)
}

func decodeBC7Block(block []byte) [16][4]uint8 {
	var out [16][4]uint8

	mode := bc7DetectMode(block)
	if mode >= 8 {
		return out // spec: reserved mode decodes to transparent black
	}
	info := bc7Modes[mode]
	br := &bc7BitReader{data: block, pos: mode + 1}

	numEndpoints := (info.partitions + 1) * 2
	shape := int(br.getBits(info.partitionBits))
	rotation := int(br.getBits(info.rotationBits))
	indexSelector := int(br.getBits(info.indexModeBits))

	var c [6][4]uint8 // [endpoint index][R,G,B,A], endpoints 2i/2i+1 belong to subset i

	// Channel-major read order (all endpoints' R, then all G, then all B,
	// then all A) exactly matches how the bits were written.
	for ch := 0; ch < 4; ch++ {
		prec := info.prec[ch]
		for i := 0; i < numEndpoints; i++ {
			if prec == 0 {
				if ch == 3 {
					c[i][ch] = 255
				}
				continue
			}
			c[i][ch] = br.getBits(prec)
		}
	}

	var p [6]uint8
	for i := 0; i < info.pBits; i++ {
		p[i] = br.getBit()
	}
	if info.pBits > 0 {
		for i := 0; i < numEndpoints; i++ {
			pi := i * info.pBits / numEndpoints
			for ch := 0; ch < 4; ch++ {
				if info.prec[ch] != info.precP[ch] {
					c[i][ch] = (c[i][ch] << 1) | p[pi]
				}
			}
		}
	}

	for i := 0; i < numEndpoints; i++ {
		for ch := 0; ch < 4; ch++ {
			if ch == 3 && info.precP[3] == 0 {
				c[i][ch] = 255
				continue
			}
			c[i][ch] = bc7Unquantize(c[i][ch], info.precP[ch])
		}
	}

	var w1, w2 [16]uint8
	for i := 0; i < 16; i++ {
		nb := info.indexPrec
		if bc7IsAnchor(info.partitions, shape, i) {
			nb--
		}
		w1[i] = br.getBits(nb)
	}
	if info.indexPrec2 != 0 {
		for i := 0; i < 16; i++ {
			nb := info.indexPrec2
			if i == 0 {
				nb--
			}
			w2[i] = br.getBits(nb)
		}
	}

	var partition [16]uint8
	switch info.partitions {
	case 1:
		partition = bc7Partition2[shape]
	case 2:
		partition = bc7Partition3[shape]
	} // case 0: leave as all-zero (single subset)

	for i := 0; i < 16; i++ {
		region := int(partition[i])
		e0 := c[region*2]
		e1 := c[region*2+1]
		var px [4]uint8
		switch {
		case info.indexPrec2 == 0:
			for ch := 0; ch < 3; ch++ {
				px[ch] = bc7Interpolate(e0[ch], e1[ch], int(w1[i]), info.indexPrec)
			}
			px[3] = bc7Interpolate(e0[3], e1[3], int(w1[i]), info.indexPrec)
		case indexSelector == 0:
			for ch := 0; ch < 3; ch++ {
				px[ch] = bc7Interpolate(e0[ch], e1[ch], int(w1[i]), info.indexPrec)
			}
			px[3] = bc7Interpolate(e0[3], e1[3], int(w2[i]), info.indexPrec2)
		default:
			for ch := 0; ch < 3; ch++ {
				px[ch] = bc7Interpolate(e0[ch], e1[ch], int(w2[i]), info.indexPrec2)
			}
			px[3] = bc7Interpolate(e0[3], e1[3], int(w1[i]), info.indexPrec)
		}

		switch rotation {
		case 1:
			px[0], px[3] = px[3], px[0]
		case 2:
			px[1], px[3] = px[3], px[1]
		case 3:
			px[2], px[3] = px[3], px[2]
		}
		out[i] = px
	}
	return out
}
