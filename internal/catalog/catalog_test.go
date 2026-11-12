package catalog

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeRejectsGarbageWithoutPanicking(t *testing.T) {
	cases := map[string][]byte{
		"nil":          nil,
		"empty":        {},
		"html error":   []byte("<html><body>503 Service Unavailable</body></html>"),
		"short header": {0x00, 0x01, 0x02, 0x03},
	}
	for name, data := range cases {
		if cat, err := Decode(data, "catalog_S1.bin"); err == nil {
			t.Errorf("%s: expected an error, got a catalog with %d bundles", name, len(cat.Bundles))
		}
	}
	// Random bytes must never crash the caller, whatever they decode to.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 300; i++ {
		buf := make([]byte, rng.Intn(400))
		rng.Read(buf)
		_, _ = Decode(buf, "x.bin")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	name, build, locator := "bundle-a", "buildhash", "AddressablesMainContentCatalog"
	cat := &Catalog{
		SourceFile: "catalog_S1.bin",
		Meta:       Meta{Version: 2, LocatorID: &locator, BuildResultHash: &build},
		Stats:      Stats{Keys: 1, Locations: 1, Bundles: 1},
		Bundles:    []Bundle{{LocationID: 0, BundleName: &name, Hash: "h", Crc: 7, SizeBytes: 99, Path: "https://x/y.bundle"}},
		Locations:  []Location{{ID: 0, PrimaryKey: "k", InternalID: "https://x/y.bundle", IsBundle: true, Dependencies: []int{}}},
		Keys:       map[string][]int{"k": {0}},
	}
	dir := t.TempDir()
	p1, p2 := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	if err := cat.Save(p1); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(p1)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BuildHash() != build || loaded.Bundles[0].Name() != name || loaded.Bundles[0].SizeBytes != 99 {
		t.Fatalf("round trip lost data: %+v", loaded)
	}
	if err := loaded.Save(p2); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(p1)
	b, _ := os.ReadFile(p2)
	if !bytes.Equal(a, b) {
		t.Error("Save(Load(Save(x))) is not byte-identical")
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file should fail")
	}
	p := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(p, []byte("{ nope"), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("corrupt JSON should fail")
	}
}

func TestBundleNameAndBuildHashAreNilSafe(t *testing.T) {
	if (Bundle{}).Name() != "" || (&Catalog{}).BuildHash() != "" {
		t.Error("nil pointers must read as empty strings")
	}
}

// TestRealCatalogMatchesLegacyJSON checks that decoding a real catalog_S1.bin
// yields exactly the JSON the original standalone unpack_catalog tool wrote.
//
//	LIMBUS_SAMPLE_BIN=catalog_S1.bin LIMBUS_SAMPLE_JSON=catalog_S1_unpacked.json \
//	  go test ./internal/catalog -run RealCatalog -v
func TestRealCatalogMatchesLegacyJSON(t *testing.T) {
	bin, want := os.Getenv("LIMBUS_SAMPLE_BIN"), os.Getenv("LIMBUS_SAMPLE_JSON")
	if bin == "" || want == "" {
		t.Skip("set LIMBUS_SAMPLE_BIN and LIMBUS_SAMPLE_JSON to compare against a real catalog")
	}
	cat, err := DecodeFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.json")
	if err := cat.Save(out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	exp, _ := os.ReadFile(want)
	if !bytes.Equal(got, exp) {
		t.Fatalf("decoded JSON differs from %s (%d vs %d bytes)", want, len(got), len(exp))
	}
	// the legacy file must also survive Load -> Save unchanged
	re, err := Load(want)
	if err != nil {
		t.Fatal(err)
	}
	out2 := filepath.Join(t.TempDir(), "re.json")
	if err := re.Save(out2); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(out2); !bytes.Equal(again, exp) {
		t.Error("Load -> Save of the legacy file is not byte-identical")
	}
	t.Logf("%d bundles, %d locations, %d keys: identical to %s", cat.Stats.Bundles, cat.Stats.Locations, cat.Stats.Keys, filepath.Base(want))
}
