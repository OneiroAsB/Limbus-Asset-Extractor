package extractor

import (
	"encoding/binary"
	"fmt"
	"image"
)

// Unity TextureFormat enum values (subset we support decoding for).
const (
	tfAlpha8    = 1
	tfARGB4444  = 2
	tfRGB24     = 3
	tfRGBA32    = 4
	tfARGB32    = 5
	tfRGB565    = 7
	tfR16       = 9
	tfDXT1      = 10
	tfDXT3      = 11
	tfDXT5      = 12
	tfRGBA4444  = 13
	tfBGRA32    = 14
	tfBC7       = 25
	tfBC4       = 26
	tfBC5       = 27
	tfDXT1Crnch = 28
	tfDXT5Crnch = 29
	tfRG16      = 62
	tfR8        = 63
)

var textureFormatNames = map[int32]string{
	1: "Alpha8", 2: "ARGB4444", 3: "RGB24", 4: "RGBA32", 5: "ARGB32", 6: "ARGBFloat",
	7: "RGB565", 8: "BGR24", 9: "R16", 10: "DXT1", 11: "DXT3", 12: "DXT5", 13: "RGBA4444",
	14: "BGRA32", 15: "RHalf", 16: "RGHalf", 17: "RGBAHalf", 18: "RFloat", 19: "RGFloat",
	20: "RGBAFloat", 21: "YUY2", 22: "RGB9e5Float", 23: "RGBFloat", 24: "BC6H", 25: "BC7",
	26: "BC4", 27: "BC5", 28: "DXT1Crunched", 29: "DXT5Crunched", 30: "PVRTC_RGB2",
	31: "PVRTC_RGBA2", 32: "PVRTC_RGB4", 33: "PVRTC_RGBA4", 34: "ETC_RGB4", 35: "ATC_RGB4",
	36: "ATC_RGBA8", 41: "EAC_R", 42: "EAC_R_SIGNED", 43: "EAC_RG", 44: "EAC_RG_SIGNED",
	45: "ETC2_RGB", 46: "ETC2_RGBA1", 47: "ETC2_RGBA8", 48: "ASTC_RGB_4x4", 49: "ASTC_RGB_5x5",
	50: "ASTC_RGB_6x6", 51: "ASTC_RGB_8x8", 52: "ASTC_RGB_10x10", 53: "ASTC_RGB_12x12",
	54: "ASTC_RGBA_4x4", 55: "ASTC_RGBA_5x5", 56: "ASTC_RGBA_6x6", 57: "ASTC_RGBA_8x8",
	58: "ASTC_RGBA_10x10", 59: "ASTC_RGBA_12x12", 60: "ETC_RGB4_3DS", 61: "ETC_RGBA8_3DS",
	62: "RG16", 63: "R8", 64: "ETC_RGB4Crunched", 65: "ETC2_RGBA8Crunched",
	66: "ASTC_HDR_4x4", 67: "ASTC_HDR_5x5", 68: "ASTC_HDR_6x6", 69: "ASTC_HDR_8x8",
	70: "ASTC_HDR_10x10", 71: "ASTC_HDR_12x12", 72: "RG32", 73: "RGB48", 74: "RGBA64",
}

func textureFormatName(f int32) string {
	if n, ok := textureFormatNames[f]; ok {
		return n
	}
	return fmt.Sprintf("format %d", f)
}

// decodeTexture turns raw Unity texture bytes into a top-down NRGBA image.
// It matches AssetStudio's default export behaviour, which decodes into the
// GPU's native (bottom-up) row order and then flips vertically.
func decodeTexture(format int32, data []byte, width, height int) (*image.NRGBA, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid dimensions %dx%d", width, height)
	}
	// Guards against a misparsed object (wrong byte offset, unsupported
	// field layout, etc.) turning into a multi-gigabyte allocation attempt,
	// which would crash the whole batch with an unrecoverable Go runtime
	// OOM rather than a normal, skippable error. No real game texture
	// comes close to this.
	const maxDim = 16384
	const maxPixels = maxDim * maxDim
	if width > maxDim || height > maxDim || width*height > maxPixels {
		return nil, fmt.Errorf("implausible dimensions %dx%d (likely a parsing misalignment for this object)", width, height)
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))

	switch format {
	case tfAlpha8:
		if err := needLen(data, width*height); err != nil {
			return nil, err
		}
		for i := 0; i < width*height; i++ {
			setPixelRowMajor(img, width, height, i, 255, 255, 255, data[i])
		}
	case tfR8:
		if err := needLen(data, width*height); err != nil {
			return nil, err
		}
		for i := 0; i < width*height; i++ {
			setPixelRowMajor(img, width, height, i, data[i], 0, 0, 255)
		}
	case tfRG16:
		if err := needLen(data, width*height*2); err != nil {
			return nil, err
		}
		for i := 0; i < width*height; i++ {
			setPixelRowMajor(img, width, height, i, data[i*2], data[i*2+1], 0, 255)
		}
	case tfR16:
		if err := needLen(data, width*height*2); err != nil {
			return nil, err
		}
		for i := 0; i < width*height; i++ {
			v := binary.LittleEndian.Uint16(data[i*2:])
			r := downscale16to8(v)
			setPixelRowMajor(img, width, height, i, r, 0, 0, 255)
		}
	case tfRGB24:
		if err := needLen(data, width*height*3); err != nil {
			return nil, err
		}
		for i := 0; i < width*height; i++ {
			setPixelRowMajor(img, width, height, i, data[i*3], data[i*3+1], data[i*3+2], 255)
		}
	case tfRGBA32:
		if err := needLen(data, width*height*4); err != nil {
			return nil, err
		}
		for i := 0; i < width*height; i++ {
			o := i * 4
			setPixelRowMajor(img, width, height, i, data[o], data[o+1], data[o+2], data[o+3])
		}
	case tfARGB32:
		if err := needLen(data, width*height*4); err != nil {
			return nil, err
		}
		for i := 0; i < width*height; i++ {
			o := i * 4
			setPixelRowMajor(img, width, height, i, data[o+1], data[o+2], data[o+3], data[o])
		}
	case tfBGRA32:
		if err := needLen(data, width*height*4); err != nil {
			return nil, err
		}
		for i := 0; i < width*height; i++ {
			o := i * 4
			setPixelRowMajor(img, width, height, i, data[o+2], data[o+1], data[o], data[o+3])
		}
	case tfRGB565:
		if err := needLen(data, width*height*2); err != nil {
			return nil, err
		}
		for i := 0; i < width*height; i++ {
			p := uint32(binary.LittleEndian.Uint16(data[i*2:]))
			r := uint8(((p >> 8) & 0xf8) | (p >> 13))
			g := uint8(((p >> 3) & 0xfc) | ((p >> 9) & 3))
			b := uint8((p << 3) | ((p >> 2) & 7))
			setPixelRowMajor(img, width, height, i, r, g, b, 255)
		}
	case tfARGB4444:
		if err := needLen(data, width*height*2); err != nil {
			return nil, err
		}
		for i := 0; i < width*height; i++ {
			p := binary.LittleEndian.Uint16(data[i*2:])
			b := expand4(uint8(p & 0xf))
			g := expand4(uint8((p >> 4) & 0xf))
			r := expand4(uint8((p >> 8) & 0xf))
			a := expand4(uint8((p >> 12) & 0xf))
			setPixelRowMajor(img, width, height, i, r, g, b, a)
		}
	case tfRGBA4444:
		if err := needLen(data, width*height*2); err != nil {
			return nil, err
		}
		for i := 0; i < width*height; i++ {
			p := binary.LittleEndian.Uint16(data[i*2:])
			r := expand4(uint8((p >> 4) & 0xf))
			g := expand4(uint8((p >> 8) & 0xf))
			b := expand4(uint8((p >> 12) & 0xf))
			a := expand4(uint8(p & 0xf))
			setPixelRowMajor(img, width, height, i, r, g, b, a)
		}
	case tfDXT1, tfDXT1Crnch:
		if err := decodeBlocks(img, width, height, data, 8, decodeBC1Block); err != nil {
			return nil, err
		}
	case tfDXT3:
		if err := decodeBlocks(img, width, height, data, 16, decodeBC2Block); err != nil {
			return nil, err
		}
	case tfDXT5, tfDXT5Crnch:
		if err := decodeBlocks(img, width, height, data, 16, decodeBC3Block); err != nil {
			return nil, err
		}
	case tfBC4:
		if err := decodeBlocks(img, width, height, data, 8, decodeBC4Block); err != nil {
			return nil, err
		}
	case tfBC5:
		if err := decodeBlocks(img, width, height, data, 16, decodeBC5Block); err != nil {
			return nil, err
		}
	case tfBC7:
		if err := decodeBlocks(img, width, height, data, 16, decodeBC7Block); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported texture format: %s", textureFormatName(format))
	}

	return flipVertical(img), nil
}

func needLen(data []byte, n int) error {
	if len(data) < n {
		return fmt.Errorf("image data too short: have %d bytes, need %d", len(data), n)
	}
	return nil
}

func expand4(v uint8) uint8 { return (v << 4) | v }

func downscale16to8(v uint16) uint8 {
	return uint8((uint32(v)*255 + 32895) >> 16)
}

// setPixelRowMajor writes pixel index i (i = y*width+x) in plain top-down
// row-major order. Every decoder in this file writes in this same natural
// order; decodeTexture's single flipVertical() call at the end is what
// turns that into the GPU's actual bottom-up row order, matching how
// AssetStudio/UnityPy export textures.
func setPixelRowMajor(img *image.NRGBA, width, height, i int, r, g, b, a uint8) {
	x := i % width
	y := i / width
	o := img.PixOffset(x, y)
	img.Pix[o] = r
	img.Pix[o+1] = g
	img.Pix[o+2] = b
	img.Pix[o+3] = a
}

func flipVertical(img *image.NRGBA) *image.NRGBA {
	h := img.Bounds().Dy()
	stride := img.Stride
	for y := 0; y < h/2; y++ {
		top := img.Pix[y*stride : y*stride+stride]
		bot := img.Pix[(h-1-y)*stride : (h-1-y)*stride+stride]
		for i := range top {
			top[i], bot[i] = bot[i], top[i]
		}
	}
	return img
}

// decodeBlocks walks 4x4-pixel compressed blocks left-to-right, top-to-bottom
// and writes each decoded block's pixels into img, clipping against the
// image's true (possibly non-multiple-of-4) width/height.
func decodeBlocks(img *image.NRGBA, width, height int, data []byte, blockBytes int, decode func([]byte) [16][4]uint8) error {
	blocksWide := (width + 3) / 4
	blocksHigh := (height + 3) / 4
	need := blocksWide * blocksHigh * blockBytes
	if err := needLen(data, need); err != nil {
		return err
	}
	pos := 0
	for by := 0; by < blocksHigh; by++ {
		for bx := 0; bx < blocksWide; bx++ {
			block := data[pos : pos+blockBytes]
			pos += blockBytes
			pixels := decode(block)
			for py := 0; py < 4; py++ {
				y := by*4 + py
				if y >= height {
					continue
				}
				for px := 0; px < 4; px++ {
					x := bx*4 + px
					if x >= width {
						continue
					}
					p := pixels[py*4+px]
					o := img.PixOffset(x, y)
					img.Pix[o] = p[0]
					img.Pix[o+1] = p[1]
					img.Pix[o+2] = p[2]
					img.Pix[o+3] = p[3]
				}
			}
		}
	}
	return nil
}

func unpack565(v uint16) (r, g, b uint8) {
	r5 := uint32(v>>11) & 0x1f
	g6 := uint32(v>>5) & 0x3f
	b5 := uint32(v) & 0x1f
	r = uint8((r5 << 3) | (r5 >> 2))
	g = uint8((g6 << 2) | (g6 >> 4))
	b = uint8((b5 << 3) | (b5 >> 2))
	return
}

// bc1Palette builds the 4-entry RGBA palette for a BC1-style 8-byte color
// block. opaqueOnly forces the always-4-color-interpolation mode used by
// BC2/BC3 (their alpha is stored separately), matching DirectXTex's
// DecodeBC1(..., isbc1) parameter.
func bc1Palette(block []byte, opaqueOnly bool) (pal [4][4]uint8, c0, c1 uint16) {
	c0 = binary.LittleEndian.Uint16(block[0:2])
	c1 = binary.LittleEndian.Uint16(block[2:4])
	r0, g0, b0 := unpack565(c0)
	r1, g1, b1 := unpack565(c1)
	pal[0] = [4]uint8{r0, g0, b0, 255}
	pal[1] = [4]uint8{r1, g1, b1, 255}
	if !opaqueOnly && c0 <= c1 {
		pal[2] = [4]uint8{
			uint8((uint16(r0) + uint16(r1)) / 2),
			uint8((uint16(g0) + uint16(g1)) / 2),
			uint8((uint16(b0) + uint16(b1)) / 2),
			255,
		}
		pal[3] = [4]uint8{0, 0, 0, 0}
	} else {
		pal[2] = [4]uint8{
			uint8((2*uint16(r0) + uint16(r1)) / 3),
			uint8((2*uint16(g0) + uint16(g1)) / 3),
			uint8((2*uint16(b0) + uint16(b1)) / 3),
			255,
		}
		pal[3] = [4]uint8{
			uint8((uint16(r0) + 2*uint16(r1)) / 3),
			uint8((uint16(g0) + 2*uint16(g1)) / 3),
			uint8((uint16(b0) + 2*uint16(b1)) / 3),
			255,
		}
	}
	return
}

func decodeBC1Block(block []byte) [16][4]uint8 {
	var out [16][4]uint8
	pal, _, _ := bc1Palette(block, false)
	idxBits := binary.LittleEndian.Uint32(block[4:8])
	for p := 0; p < 16; p++ {
		idx := (idxBits >> uint(2*p)) & 3
		out[p] = pal[idx]
	}
	return out
}

func decodeBC2Block(block []byte) [16][4]uint8 {
	var out [16][4]uint8
	pal, _, _ := bc1Palette(block[8:16], true)
	idxBits := binary.LittleEndian.Uint32(block[12:16])
	for p := 0; p < 16; p++ {
		idx := (idxBits >> uint(2*p)) & 3
		byteIdx := p / 2
		var a4 uint8
		if p%2 == 0 {
			a4 = block[byteIdx] & 0xf
		} else {
			a4 = (block[byteIdx] >> 4) & 0xf
		}
		px := pal[idx]
		px[3] = expand4(a4)
		out[p] = px
	}
	return out
}

// decode8LevelRamp builds the 8-value interpolation ramp shared by BC3's
// alpha block and BC4/BC5's single-channel blocks.
func decode8LevelRamp(v0, v1 byte) [8]uint8 {
	var out [8]uint8
	out[0] = v0
	out[1] = v1
	if v0 > v1 {
		for i := 2; i <= 7; i++ {
			out[i] = uint8((int(v0)*(8-i) + int(v1)*(i-1)) / 7)
		}
	} else {
		for i := 2; i <= 5; i++ {
			out[i] = uint8((int(v0)*(6-i) + int(v1)*(i-1)) / 5)
		}
		out[6] = 0
		out[7] = 255
	}
	return out
}

func decode3BitIndices(b []byte) [16]uint8 {
	v := uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 | uint64(b[4])<<32 | uint64(b[5])<<40
	var out [16]uint8
	for i := 0; i < 16; i++ {
		out[i] = uint8((v >> uint(3*i)) & 7)
	}
	return out
}

func decodeBC3Block(block []byte) [16][4]uint8 {
	var out [16][4]uint8
	ramp := decode8LevelRamp(block[0], block[1])
	idx := decode3BitIndices(block[2:8])
	pal, _, _ := bc1Palette(block[8:16], true)
	for p := 0; p < 16; p++ {
		colIdx := (binary.LittleEndian.Uint32(block[12:16]) >> uint(2*p)) & 3
		px := pal[colIdx]
		px[3] = ramp[idx[p]]
		out[p] = px
	}
	return out
}

func decodeBC4Block(block []byte) [16][4]uint8 {
	var out [16][4]uint8
	ramp := decode8LevelRamp(block[0], block[1])
	idx := decode3BitIndices(block[2:8])
	for p := 0; p < 16; p++ {
		out[p] = [4]uint8{ramp[idx[p]], 0, 0, 255}
	}
	return out
}

func decodeBC5Block(block []byte) [16][4]uint8 {
	var out [16][4]uint8
	rRamp := decode8LevelRamp(block[0], block[1])
	rIdx := decode3BitIndices(block[2:8])
	gRamp := decode8LevelRamp(block[8], block[9])
	gIdx := decode3BitIndices(block[10:16])
	for p := 0; p < 16; p++ {
		out[p] = [4]uint8{rRamp[rIdx[p]], gRamp[gIdx[p]], 0, 255}
	}
	return out
}
