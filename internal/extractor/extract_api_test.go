package extractor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureBundle is a tiny valid UnityFS bundle holding one 4x4 RGBA32
// Texture2D named "TestTex", built with the same helpers as the end-to-end
// tests.
func fixtureBundle(t *testing.T) []byte {
	t.Helper()
	row := func(r, g, b, a byte) []byte { return bytes.Repeat([]byte{r, g, b, a}, 4) }
	var pixels []byte
	pixels = append(pixels, row(255, 0, 0, 255)...)
	pixels = append(pixels, row(0, 255, 0, 255)...)
	pixels = append(pixels, row(0, 0, 255, 255)...)
	pixels = append(pixels, row(255, 255, 255, 255)...)
	return buildBundle(t, "CAB-test", buildSerializedFile(t, pixels))
}

// TestWriteTestdata regenerates testdata/rgba32.bundle, which other
// packages' tests use as a known-good bundle. Run with:
//
//	WRITE_TESTDATA=1 go test ./internal/extractor -run TestWriteTestdata
func TestWriteTestdata(t *testing.T) {
	if os.Getenv("WRITE_TESTDATA") == "" {
		t.Skip("set WRITE_TESTDATA=1 to regenerate testdata/rgba32.bundle")
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("testdata/rgba32.bundle", fixtureBundle(t), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractWritesPNG(t *testing.T) {
	out := t.TempDir()
	var log bytes.Buffer
	st, err := Extract(fixtureBundle(t), out, Options{Log: &log})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if st.Textures != 1 || st.Texts != 0 || st.Skipped != 0 {
		t.Fatalf("unexpected stats: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(out, "TestTex.png")); err != nil {
		t.Fatalf("PNG not written: %v", err)
	}
	if !strings.Contains(log.String(), "TestTex") {
		t.Errorf("log should mention the texture, got %q", log.String())
	}
}

func TestExtractSilentWhenNoLog(t *testing.T) {
	// nil Log must not panic and must still extract.
	if _, err := Extract(fixtureBundle(t), t.TempDir(), Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyTexturePlaceholderDoesNotFailExtraction(t *testing.T) {
	for _, hasPixels := range []bool{false, true} {
		var pixels []byte
		if hasPixels {
			pixels = make([]byte, 64)
		}
		data := buildSerializedFile(t, pixels)
		sf, err := parseSerializedFile(data, [4]int{2022, 3, 22, 1})
		if err != nil {
			t.Fatal(err)
		}
		r := NewReader(data, false)
		r.SeekTo(int(sf.Objects[0].ByteStart))
		if _, err := r.ReadAlignedString(); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ReadBytes(6); err != nil {
			t.Fatal(err)
		} // fallback format + two flags
		r.Align(4)
		clear(data[r.Pos() : r.Pos()+8]) // width and height are both zero
		out := t.TempDir()
		st, err := Extract(buildBundle(t, "CAB-test", data), out, Options{})
		if err != nil || st.Textures != 0 {
			t.Fatalf("pixels=%v stats=%+v err=%v", hasPixels, st, err)
		}
		if !hasPixels && (st.Filtered != 1 || st.Skipped != 0) {
			t.Fatalf("empty placeholder failed: %+v", st)
		}
		if hasPixels && st.Skipped != 1 {
			t.Fatalf("invalid dimensions with pixel data must still fail: %+v", st)
		}
		files, err := os.ReadDir(out)
		if err != nil || len(files) != 0 {
			t.Fatalf("unexpected output: %v err=%v", files, err)
		}
	}
}

func TestExtractFilterAndOnly(t *testing.T) {
	b := fixtureBundle(t)

	out := t.TempDir()
	st, err := Extract(b, out, Options{Filter: "nomatch"})
	if err != nil || st.Extracted() != 0 {
		t.Fatalf("filter that matches nothing: stats=%+v err=%v", st, err)
	}

	out = t.TempDir()
	st, err = Extract(b, out, Options{Filter: "TESTT"}) // case-insensitive
	if err != nil || st.Textures != 1 {
		t.Fatalf("case-insensitive filter: stats=%+v err=%v", st, err)
	}

	out = t.TempDir()
	st, err = Extract(b, out, Options{Only: "text"}) // textures not wanted
	if err != nil || st.Extracted() != 0 {
		t.Fatalf("only=text should skip the texture: stats=%+v err=%v", st, err)
	}
}

func TestExtractRejectsGarbageWithoutPanicking(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":     {},
		"html page": []byte("<html><body>403 Forbidden</body></html>"),
		"truncated": fixtureBundle(t)[:40],
	} {
		if _, err := Extract(data, t.TempDir(), Options{}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestExtractBadOnlyValue(t *testing.T) {
	if _, err := Extract(fixtureBundle(t), t.TempDir(), Options{Only: "bogus"}); err == nil {
		t.Fatal("expected error for unknown -only value")
	}
}

func TestParseSelection(t *testing.T) {
	sel, err := parseSelection("")
	if err != nil || !sel.texture || !sel.text || sel.any {
		t.Fatalf("empty spec should mean texture,text: %+v err=%v", sel, err)
	}
	sel, err = parseSelection("114, Texture")
	if err != nil || !sel.texture || sel.text || !sel.classIDs[114] {
		t.Fatalf("mixed spec: %+v err=%v", sel, err)
	}
	if !sel.wants(114) || sel.wants(49) {
		t.Fatal("wants() disagrees with the parsed spec")
	}
}

func TestList(t *testing.T) {
	var buf bytes.Buffer
	if err := List(fixtureBundle(t), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// The fixture has no embedded TypeTree, so sample names (read through
	// it) are not expected here - only the per-class count.
	if !strings.Contains(out, "Texture2D") || !strings.Contains(out, "1 object") {
		t.Errorf("list output missing class summary:\n%s", out)
	}
}

func TestExcludeNamesSkipsBeforeDecoding(t *testing.T) {
	b := fixtureBundle(t)

	out := t.TempDir()
	var log bytes.Buffer
	st, err := Extract(b, out, Options{ExcludeNames: []string{"nomatch", "TESTT"}, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if st.Textures != 0 || st.Filtered != 1 || st.Skipped != 0 {
		t.Fatalf("an excluded texture is filtered, not extracted and not 'skipped': %+v", st)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("excluded texture was written anyway: %v", entries)
	}
	if !strings.Contains(log.String(), "[excluded] TestTex") || !strings.Contains(log.String(), "testt") {
		t.Errorf("log should say which rule matched:\n%s", log.String())
	}

	st, err = Extract(b, t.TempDir(), Options{ExcludeNames: []string{"nomatch", " "}})
	if err != nil || st.Textures != 1 || st.Filtered != 0 {
		t.Fatalf("exclusions that match nothing must not change the result: %+v err=%v", st, err)
	}
}

func TestExcludesByName(t *testing.T) {
	ex := newExcludes(Options{ExcludeNames: []string{"fx", "VFX", "sactx", "  ", "mask"}})
	for name, want := range map[string]bool{
		"FX_PC_9_Ryoshu_Hit": true, "hit_vfx_01": true, "sactx-0-2048x2048-Uncompressed-ui": true,
		"Mask_Circle": true, "portrait_01": false, "": false,
	} {
		if _, hit := ex.byName(name); hit != want {
			t.Errorf("byName(%q) = %v, want %v", name, hit, want)
		}
	}
}

func TestTextAssetFileNameAndExtExclusion(t *testing.T) {
	for _, c := range []struct{ name, script, want string }{
		{"abbattle-tkt", `{"a":1}`, "abbattle-tkt.json"}, // no extension in the name: guessed from content
		{"list", "[1,2]", "list.json"},
		{"notes", "plain words", "notes.txt"},
		{"data.json", "whatever", "data.json"}, // the name's own extension wins
		{"table.CSV", "a,b", "table.CSV"},
		{"", `{}`, "textasset_7.json"},
	} {
		if got := textAssetFileName(&TextAssetInfo{Name: c.name, PathID: 7, Script: c.script}); got != c.want {
			t.Errorf("textAssetFileName(%q) = %q, want %q", c.name, got, c.want)
		}
	}

	ex := newExcludes(Options{ExcludeTextExts: []string{"json", ".JSON", " .Txt "}})
	if len(ex.textExts) != 2 || !ex.textExts[".json"] || !ex.textExts[".txt"] {
		t.Errorf("extensions not normalised: %v", ex.textExts)
	}
}

func TestStatsAddIncludesFiltered(t *testing.T) {
	a := Stats{Textures: 1, Filtered: 2}
	a.Add(Stats{Texts: 3, Skipped: 4, Filtered: 5})
	if a != (Stats{Textures: 1, Texts: 3, Skipped: 4, Filtered: 7}) {
		t.Errorf("%+v", a)
	}
}
