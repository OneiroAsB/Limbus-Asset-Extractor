package pipeline

import "testing"

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0:          "0 B",
		1023:       "1023 B",
		1024:       "1.0 KiB",
		1536:       "1.5 KiB",
		5 << 20:    "5.0 MiB",
		2834238566: "2.6 GiB", // real size of one update's downloads
		457 << 20:  "457.0 MiB",
		3 << 40:    "3.0 TiB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
