package main

import (
	"reflect"
	"testing"
)

func TestDeleteAfterExtract(t *testing.T) {
	for _, c := range []struct{ keep, noExtract, want bool }{
		{false, false, true}, // the default: delete once extracted
		{true, false, false}, // -keep-bundles
		{false, true, false}, // -no-extract: downloads are the product, and Config would reject deleting them
		{true, true, false},
	} {
		if got := deleteAfterExtract(c.keep, c.noExtract); got != c.want {
			t.Errorf("deleteAfterExtract(keep=%v, noExtract=%v) = %v, want %v", c.keep, c.noExtract, got, c.want)
		}
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList(" fx, vfx ,,mask "); !reflect.DeepEqual(got, []string{"fx", "vfx", "mask"}) {
		t.Errorf("%q", got)
	}
	if got := splitList(""); len(got) != 0 {
		t.Errorf("%q", got)
	}
}
