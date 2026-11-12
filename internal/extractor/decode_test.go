package extractor

import (
	"encoding/binary"
	"testing"
)

func TestLZ4DecompressLiteralAndMatch(t *testing.T) {
	// "ABCD" literal + a match copying "ABCD" from 4 bytes back -> "ABCDABCD"
	src := []byte{0x40, 'A', 'B', 'C', 'D', 0x04, 0x00}
	got, err := lz4DecompressBlock(src, 8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "ABCDABCD" {
		t.Fatalf("got %q, want %q", got, "ABCDABCD")
	}
}

func TestLZ4DecompressOverlappingMatch(t *testing.T) {
	// literal "A" + match(offset=1, len=7) -> "AAAAAAAA" (offset < matchLen,
	// exercising the byte-by-byte overlapping copy).
	src := []byte{0x13, 'A', 0x01, 0x00}
	got, err := lz4DecompressBlock(src, 8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "AAAAAAAA" {
		t.Fatalf("got %q, want %q", got, "AAAAAAAA")
	}
}

func TestLZ4DecompressLongLiteral(t *testing.T) {
	// 20-byte literal run: token high nibble=15 (extend), +1 extra byte (5),
	// total literal length 15+5=20, then block ends (no match follows).
	payload := make([]byte, 20)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	src := append([]byte{0xF0, 5}, payload...)
	got, err := lz4DecompressBlock(src, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestUnpack565(t *testing.T) {
	cases := []struct {
		v       uint16
		r, g, b uint8
	}{
		{0xF800, 255, 0, 0},
		{0x07E0, 0, 255, 0},
		{0x001F, 0, 0, 255},
		{0xFFFF, 255, 255, 255},
		{0x0000, 0, 0, 0},
	}
	for _, c := range cases {
		r, g, b := unpack565(c.v)
		if r != c.r || g != c.g || b != c.b {
			t.Errorf("unpack565(%#04x) = (%d,%d,%d), want (%d,%d,%d)", c.v, r, g, b, c.r, c.g, c.b)
		}
	}
}

func TestBC1SolidColor(t *testing.T) {
	// c0 = c1 = pure red (0xF800), all 16 indices = 0 -> solid opaque red,
	// regardless of which color2/3 formula applies (they're never selected).
	block := make([]byte, 8)
	binary.LittleEndian.PutUint16(block[0:2], 0xF800)
	binary.LittleEndian.PutUint16(block[2:4], 0xF800)
	// bitmap left as 0 -> every 2-bit index is 0
	pixels := decodeBC1Block(block)
	for i, p := range pixels {
		if p != [4]uint8{255, 0, 0, 255} {
			t.Fatalf("pixel %d = %v, want opaque red", i, p)
		}
	}
}

func TestBC1PunchThroughAlpha(t *testing.T) {
	// c0 < c1 numerically -> punch-through mode; index 3 must be fully
	// transparent black regardless of color1's value.
	block := make([]byte, 8)
	binary.LittleEndian.PutUint16(block[0:2], 0x0010)     // c0, small value
	binary.LittleEndian.PutUint16(block[2:4], 0xF800)     // c1 = red, c0 < c1
	binary.LittleEndian.PutUint32(block[4:8], 0xFFFFFFFF) // every pixel index = 3
	pixels := decodeBC1Block(block)
	for i, p := range pixels {
		if p != [4]uint8{0, 0, 0, 0} {
			t.Fatalf("pixel %d = %v, want transparent black", i, p)
		}
	}
}

func TestVersionParse(t *testing.T) {
	cases := map[string][4]int{
		"2022.3.22f1": {2022, 3, 22, 0},
		"5.6.0f3":     {5, 6, 0, 0},
		"2019.4.40f1": {2019, 4, 40, 0},
		"":            {0, 0, 0, 0},
	}
	for in, want := range cases {
		got := parseUnityVersion(in)
		if got != want {
			t.Errorf("parseUnityVersion(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	in := `a<b>c:d"e/f\g|h?i*j`
	want := `a_b_c_d_e_f_g_h_i_j`
	got := sanitizeFilename(in)
	if got != want {
		t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
	}
}

func TestBC3SolidColorFullAlpha(t *testing.T) {
	block := make([]byte, 16)
	block[0] = 255 // alpha0
	block[1] = 255 // alpha1 (equal -> ramp[idx]=255 for any idx 0/1, avoids the 6/8-value branch ambiguity)
	// alpha indices (block[2:8]) left as 0 -> everyone picks ramp[0]=255
	binary.LittleEndian.PutUint16(block[8:10], 0xF800)  // c0 = red
	binary.LittleEndian.PutUint16(block[10:12], 0xF800) // c1 = red
	// color indices (block[12:16]) left as 0 -> everyone picks palette[0]=red
	pixels := decodeBC3Block(block)
	for i, p := range pixels {
		if p != [4]uint8{255, 0, 0, 255} {
			t.Fatalf("pixel %d = %v, want opaque red", i, p)
		}
	}
}

func TestBC4SingleChannelEndpoints(t *testing.T) {
	block := make([]byte, 8)
	block[0] = 100
	block[1] = 100 // equal endpoints -> every index maps to 100
	pixels := decodeBC4Block(block)
	for i, p := range pixels {
		if p != [4]uint8{100, 0, 0, 255} {
			t.Fatalf("pixel %d = %v, want R=100", i, p)
		}
	}
}

func TestBC5TwoChannelEndpoints(t *testing.T) {
	block := make([]byte, 16)
	block[0], block[1] = 50, 50   // red channel
	block[8], block[9] = 150, 150 // green channel
	pixels := decodeBC5Block(block)
	for i, p := range pixels {
		if p != [4]uint8{50, 150, 0, 255} {
			t.Fatalf("pixel %d = %v, want (50,150,0,255)", i, p)
		}
	}
}
