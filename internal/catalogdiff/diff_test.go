package catalogdiff

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"limbus-asset-extractor/internal/catalog"
)

func bundle(name, hash string, crc, size uint32, path string) catalog.Bundle {
	n := name
	return catalog.Bundle{BundleName: &n, Hash: hash, Crc: crc, SizeBytes: size, Path: path}
}

// cat builds a catalog whose location ids start at idBase, to prove that ids
// are not used as identity.
func cat(build string, idBase int, keys map[string][]string, bundles ...catalog.Bundle) *catalog.Catalog {
	c := &catalog.Catalog{Bundles: bundles, Keys: map[string][]int{}}
	c.Meta.BuildResultHash = &build
	ids := map[string]int{}
	next := idBase
	add := func(identity string, l catalog.Location) int {
		if id, ok := ids[identity]; ok {
			return id
		}
		l.ID = next
		ids[identity] = next
		c.Locations = append(c.Locations, l)
		next++
		return l.ID
	}
	for _, b := range bundles {
		bi := &catalog.AssetBundleRequestOptions{BundleName: b.BundleName}
		add("bundle:"+b.Name(), catalog.Location{PrimaryKey: b.Name(), IsBundle: true, BundleInfo: bi})
	}
	for key, targets := range keys {
		for _, tg := range targets {
			var id int
			if strings.HasPrefix(tg, "bundle:") {
				id = ids[tg]
			} else {
				id = add(tg, catalog.Location{PrimaryKey: strings.TrimPrefix(tg, "key:")})
			}
			c.Keys[key] = append(c.Keys[key], id)
		}
	}
	c.Stats = catalog.Stats{Keys: len(c.Keys), Locations: len(c.Locations), Bundles: len(bundles)}
	return c
}

func TestBundleAddedRemovedChanged(t *testing.T) {
	old := cat("OLD", 0, nil,
		bundle("same", "h1", 1, 10, "p"),
		bundle("hash", "h1", 1, 10, "p"),
		bundle("crc", "h1", 1, 10, "p"),
		bundle("size", "h1", 1, 10, "p"),
		bundle("gone", "h1", 1, 10, "p"),
	)
	cur := cat("NEW", 0, nil,
		bundle("same", "h1", 1, 10, "p"),
		bundle("hash", "h2", 1, 10, "p"),
		bundle("crc", "h1", 9, 10, "p"),
		bundle("size", "h1", 1, 11, "p"),
		bundle("fresh", "h1", 1, 10, "p"),
	)
	d := Compare(old, cur, "old", "new")

	if len(d.Bundles.Added) != 1 || d.Bundles.Added[0].Name() != "fresh" {
		t.Errorf("added = %+v", d.Bundles.Added)
	}
	if len(d.Bundles.Removed) != 1 || d.Bundles.Removed[0].Name() != "gone" {
		t.Errorf("removed = %+v", d.Bundles.Removed)
	}
	if len(d.Bundles.Changed) != 3 {
		t.Fatalf("changed = %+v", d.Bundles.Changed)
	}
	by := map[string]BundleChange{}
	for _, c := range d.Bundles.Changed {
		by[c.BundleName] = c
	}
	if c := by["hash"]; c.Hash == nil || c.Hash.Old != "h1" || c.Hash.New != "h2" || c.CRC != nil || c.SizeBytes != nil {
		t.Errorf("hash change = %+v", c)
	}
	if c := by["crc"]; c.CRC == nil || c.CRC.New != 9 || c.Hash != nil {
		t.Errorf("crc change = %+v", c)
	}
	if c := by["size"]; c.SizeBytes == nil || c.SizeBytes.Old != 10 || c.SizeBytes.New != 11 {
		t.Errorf("size change = %+v", c)
	}
	if _, ok := by["same"]; ok {
		t.Error("an identical bundle must not be reported")
	}
	if d.Catalog.BuildResultHash == nil || d.Catalog.BuildResultHash.Old != "OLD" || d.Catalog.BuildResultHash.New != "NEW" {
		t.Errorf("build hash change = %+v", d.Catalog.BuildResultHash)
	}
	if d.OldFile != "old" || d.NewFile != "new" {
		t.Errorf("names = %q, %q", d.OldFile, d.NewFile)
	}
}

func TestKeysAreComparedByTargetIdentityNotLocationIDs(t *testing.T) {
	keys := map[string][]string{"a": {"bundle:b1"}, "b": {"key:asset-x", "bundle:b1"}}
	// Same content, but every location id is shifted: must be "no change".
	old := cat("X", 0, keys, bundle("b1", "h", 1, 1, "p"))
	cur := cat("X", 1000, keys, bundle("b1", "h", 1, 1, "p"))
	d := Compare(old, cur, "o", "n")
	if len(d.Keys.Added)+len(d.Keys.Removed)+len(d.Keys.Changed) != 0 {
		t.Fatalf("renumbered ids produced phantom key changes: %+v", d.Keys)
	}
	if d.Catalog.BuildResultHash != nil {
		t.Error("equal build hashes must not be reported")
	}
}

func TestKeyAddedRemovedChanged(t *testing.T) {
	old := cat("O", 0, map[string][]string{"keep": {"bundle:b1"}, "gone": {"bundle:b1"}, "move": {"bundle:b1"}}, bundle("b1", "h", 1, 1, "p"), bundle("b2", "h", 1, 1, "p"))
	cur := cat("N", 0, map[string][]string{"keep": {"bundle:b1"}, "new": {"bundle:b1"}, "move": {"bundle:b2"}}, bundle("b1", "h", 1, 1, "p"), bundle("b2", "h", 1, 1, "p"))
	d := Compare(old, cur, "o", "n")
	if len(d.Keys.Added) != 1 || d.Keys.Added[0] != "new" {
		t.Errorf("added = %v", d.Keys.Added)
	}
	if len(d.Keys.Removed) != 1 || d.Keys.Removed[0] != "gone" {
		t.Errorf("removed = %v", d.Keys.Removed)
	}
	if len(d.Keys.Changed) != 1 || d.Keys.Changed[0].Key != "move" ||
		d.Keys.Changed[0].Old[0] != "bundle:b1" || d.Keys.Changed[0].New[0] != "bundle:b2" {
		t.Errorf("changed = %+v", d.Keys.Changed)
	}
}

func TestDanglingLocationReferenceStaysVisible(t *testing.T) {
	broken := &catalog.Catalog{Keys: map[string][]int{"k": {42}}}
	d := Compare(&catalog.Catalog{Keys: map[string][]int{"k": {}}}, broken, "o", "n")
	if len(d.Keys.Changed) != 1 || !strings.Contains(d.Keys.Changed[0].New[0], "missing-location") {
		t.Fatalf("dangling reference was hidden: %+v", d.Keys)
	}
}

func TestJSONShapeKeepsLegacyFieldNames(t *testing.T) {
	d := Compare(cat("O", 0, nil, bundle("a", "h", 1, 1, "p")), cat("N", 0, nil, bundle("a", "h2", 1, 1, "p")), "o", "n")
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"old_file"`, `"new_file"`, `"build_result_hash"`, `"bundles"`, `"added"`, `"removed"`, `"changed"`, `"keys"`, `"bundle_name"`, `"hash"`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("diff JSON lost the %s field: %s", field, raw)
		}
	}
}

func TestSummary(t *testing.T) {
	old := cat("O", 0, nil, bundle("a", "h", 1, 1, "p"))
	cur := cat("N", 0, nil, bundle("a", "h", 1, 1, "p"), bundle("b", "h", 1, 1, "p"))
	s := Compare(old, cur, "o", "n").Summary(old, cur)
	for _, want := range []string{"O -> N", "bundles: +1  -0  ~0", "bundles=2"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
}

// TestRealCatalogsMatchLegacyDiff compares against the report the original
// standalone catalog-diff tool produced for the same two catalogs.
//
//	LIMBUS_OLD_JSON=old_unpacked.json LIMBUS_NEW_JSON=new_unpacked.json LIMBUS_DIFF_JSON=diff.json \
//	  go test ./internal/catalogdiff -run RealCatalogs -v
func TestRealCatalogsMatchLegacyDiff(t *testing.T) {
	oldP, newP, diffP := os.Getenv("LIMBUS_OLD_JSON"), os.Getenv("LIMBUS_NEW_JSON"), os.Getenv("LIMBUS_DIFF_JSON")
	if oldP == "" || newP == "" || diffP == "" {
		t.Skip("set LIMBUS_OLD_JSON, LIMBUS_NEW_JSON and LIMBUS_DIFF_JSON to compare against a real diff")
	}
	old, err := catalog.Load(oldP)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := catalog.Load(newP)
	if err != nil {
		t.Fatal(err)
	}
	got := Compare(old, cur, "X", "Y")

	raw, err := os.ReadFile(diffP)
	if err != nil {
		t.Fatal(err)
	}
	var want Diff
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	want.OldFile, want.NewFile = "X", "Y" // the legacy tool recorded absolute paths here
	a, _ := json.MarshalIndent(got, "", "  ")
	b, _ := json.MarshalIndent(&want, "", "  ")
	if string(a) != string(b) {
		t.Fatalf("diff differs from %s", diffP)
	}
	t.Logf("bundles +%d -%d ~%d, keys +%d -%d ~%d: identical to the legacy report",
		len(got.Bundles.Added), len(got.Bundles.Removed), len(got.Bundles.Changed),
		len(got.Keys.Added), len(got.Keys.Removed), len(got.Keys.Changed))
}
