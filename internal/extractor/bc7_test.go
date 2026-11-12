package extractor

import "testing"

// bc7BitWriter mirrors bc7BitReader's exact bit-addressing (LSB of byte 0
// is bit 0), so bits written here are guaranteed to line up with what
// decodeBC7Block expects.
type bc7BitWriter struct {
	data [16]byte
	pos  int
}

func (w *bc7BitWriter) putBits(v uint32, n int) {
	for i := 0; i < n; i++ {
		bit := (v >> uint(i)) & 1
		idx := w.pos >> 3
		off := w.pos - idx*8
		w.data[idx] |= byte(bit << uint(off))
		w.pos++
	}
}

func TestBC7ReservedModeIsTransparentBlack(t *testing.T) {
	var block [16]byte // all zero -> no set bit anywhere -> reserved mode 8
	pixels := decodeBC7Block(block[:])
	for i, p := range pixels {
		if p != [4]uint8{0, 0, 0, 0} {
			t.Fatalf("pixel %d = %v, want transparent black for reserved mode", i, p)
		}
	}
}

// TestBC7Mode6SolidColor hand-packs a real Mode 6 block (1 subset, RGBA,
// no partition/rotation/index-select bits, 7-bit color + 2 p-bits + 4-bit
// indices) with both endpoints identical, and checks every pixel decodes
// to that exact, fully-unquantized color.
func TestBC7Mode6SolidColor(t *testing.T) {
	var w bc7BitWriter
	w.putBits(1<<6, 7) // mode indicator: 6 leading zero bits then a 1 bit = mode 6

	// Endpoints: mode 6 has prec (7,7,7,7), precP (8,8,8,8) - one p-bit
	// appended per channel per endpoint (pBits=2, numEndpoints=2, so each
	// endpoint gets its own p-bit). Choose a 7-bit base value per channel
	// and p-bit=1 for both endpoints so both endpoints end up identical
	// after unquantize, regardless of channel.
	const base7 = 0x55 // 7-bit value, same for every channel/endpoint
	for ch := 0; ch < 4; ch++ {
		w.putBits(base7, 7) // endpoint 0
		w.putBits(base7, 7) // endpoint 1
	}
	w.putBits(1, 1) // p-bit for endpoint 0
	w.putBits(1, 1) // p-bit for endpoint 1

	// 16 pixel indices, 4 bits each; anchor pixel (index 0, the only
	// subset's implicit anchor) gets 3 bits (one less) per the format.
	for i := 0; i < 16; i++ {
		if i == 0 {
			w.putBits(0, 3)
		} else {
			w.putBits(5, 4) // arbitrary - both endpoints are equal anyway
		}
	}

	quantized := uint32(base7)<<1 | 1
	want := bc7Unquantize(uint8(quantized), 8)

	pixels := decodeBC7Block(w.data[:])
	for i, p := range pixels {
		for ch := 0; ch < 4; ch++ {
			if p[ch] != want {
				t.Fatalf("pixel %d channel %d = %d, want %d (full pixel %v)", i, ch, p[ch], want, p)
			}
		}
	}
}
