package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

func writeAsset(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCompareAssetDirsKeepsOnlyContentDifferences(t *testing.T) {
	root := t.TempDir()
	old, after, before := filepath.Join(root, "old"), filepath.Join(root, "after"), filepath.Join(root, "before")
	writeAsset(t, old, "same.png", "same decoded texture")
	writeAsset(t, old, "nested/change.txt", "before")
	writeAsset(t, old, "removed.txt", "removed original")
	writeAsset(t, after, "same.png", "same decoded texture")
	writeAsset(t, after, "nested/change.txt", "after!") // same length, different content
	writeAsset(t, after, "new.png", "new texture")
	d, err := compareAssetDirs(old, after, before)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Added, []string{"new.png"}) || !reflect.DeepEqual(d.Changed, []string{"nested/change.txt"}) || !reflect.DeepEqual(d.Removed, []string{"removed.txt"}) || d.Unchanged != 1 {
		t.Fatalf("diff = %+v", d)
	}
	if exists(filepath.Join(after, "same.png")) || exists(filepath.Join(after, "removed.txt")) {
		t.Fatal("after contains unchanged/removed files")
	}
	for _, file := range []string{"nested/change.txt", "removed.txt"} {
		want, _ := os.ReadFile(filepath.Join(old, file))
		got, err := os.ReadFile(filepath.Join(before, file))
		if err != nil || string(got) != string(want) {
			t.Fatalf("before/%s = %q, err=%v", file, got, err)
		}
	}
	if exists(filepath.Join(before, "same.png")) || exists(filepath.Join(before, "new.png")) {
		t.Fatal("before contains unnecessary files")
	}
	if !exists(filepath.Join(old, "same.png")) {
		t.Fatal("comparison changed its source")
	}
}

func TestAssetDiffUsesOldBundleAndResumesEmptyDifferences(t *testing.T) {
	sc := newScenario(t)
	sc.srv.set("/rel/"+sc.aOld, true) // same pixels, different bundle hash/CRC
	p, l := newTestPipeline(t, func(c *Config) { c.AssetDiff = true; c.DeleteBundles = true })
	saveBaseline(t, l, sc.old)
	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.StateAdvanced || len(rep.AssetDiffs) != 2 || rep.BaselineDownloaded != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if exists(filepath.Join(l.outputDir(testLabel), "alpha")) {
		t.Fatal("metadata-only change must not leave a bundle folder")
	}
	if !exists(filepath.Join(l.outputDir(testLabel), "gamma", "TestTex.png")) {
		t.Fatal("new bundle must keep all selected files")
	}
	if rep.AssetDiffs[0].Unchanged != 1 || len(rep.AssetDiffs[0].Changed) != 0 || len(rep.AssetDiffs[1].Added) != 1 {
		t.Fatalf("diffs = %+v %+v", rep.AssetDiffs[0], rep.AssetDiffs[1])
	}
	if sc.srv.hitCount("/rel/"+sc.bFile) != 0 || len(bundleFilesIn(l.bundlesDir())) != 0 {
		t.Fatal("unchanged bundle was fetched or successful bundles were retained")
	}

	// Explicitly compare the same two releases again. Empty deltas must be
	// resumed even though their output directory intentionally does not exist.
	saveBaseline(t, l, sc.old)
	hits := sc.srv.totalHits()
	rep, err = process(t, p, l, sc.cur)
	if err != nil || rep.Resumed != 2 || sc.srv.totalHits() != hits || len(rep.AssetDiffs) != 2 {
		t.Fatalf("resume: report=%+v err=%v", rep, err)
	}

	// A different comparison baseline invalidates the journal.
	sc.old.Bundles[0].Crc++
	saveBaseline(t, l, sc.old)
	rep, err = process(t, p, l, sc.cur)
	if err != nil || rep.Resumed != 0 {
		t.Fatalf("changed baseline resumed old comparison: %+v err=%v", rep, err)
	}
}

func TestAssetDiffCannotAdvanceWhenOldBundleIsUnavailable(t *testing.T) {
	sc := newScenario(t)
	p, l := newTestPipeline(t, func(c *Config) { c.AssetDiff = true })
	saveBaseline(t, l, sc.old)
	rep, err := process(t, p, l, sc.cur)
	if err == nil || rep.StateAdvanced || len(rep.DownloadFailed) != 1 {
		t.Fatalf("report=%+v err=%v", rep, err)
	}
}

func TestAssetDiffCannotAdvanceOnIncompleteExtraction(t *testing.T) {
	sc := newScenario(t)
	sc.srv.set("/rel/"+sc.aOld, true)
	sc.srv.garbage["/rel/"+sc.c] = true
	sc.cur.Bundles[2].SizeBytes = uint32(len("<html>this is not a bundle</html>"))
	p, l := newTestPipeline(t, func(c *Config) { c.AssetDiff = true })
	saveBaseline(t, l, sc.old)
	rep, err := process(t, p, l, sc.cur)
	if err == nil || rep.StateAdvanced || len(rep.ExtractFailed) != 1 {
		t.Fatalf("report=%+v err=%v", rep, err)
	}
}

func TestAssetDiffRequiresBaselineAndExtraction(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Full = true },
		func(c *Config) { c.NoExtract = true },
	} {
		cfg := Config{CatalogPath: "unused.bin", AssetDiff: true}
		mutate(&cfg)
		if err := cfg.validate(); err == nil {
			t.Fatal("incompatible flags were accepted")
		}
	}
	sc := newScenario(t)
	p, l := newTestPipeline(t, func(c *Config) { c.AssetDiff = true })
	rep, err := process(t, p, l, sc.cur)
	if err == nil || rep.StateAdvanced || sc.srv.totalHits() != 0 {
		t.Fatalf("missing baseline: %+v, err=%v", rep, err)
	}
}

func TestExplicitComparisonFetchesCatalogEvenWhenHashWasAlreadyProcessed(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit baseline", true: "full"}[full], func(t *testing.T) {
			var requestedCatalog atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if filepath.Ext(r.URL.Path) == ".hash" {
					w.Write([]byte("SAME"))
					return
				}
				requestedCatalog.Store(true)
				http.NotFound(w, r) // no real binary catalog is needed to test the shortcut
			}))
			defer srv.Close()
			p, l := newTestPipeline(t, func(c *Config) { c.CDNBase = srv.URL; c.Full = full })
			saveBaseline(t, l, mkCat("NEW"))
			if !full {
				p.Cfg.BaselinePath = l.baselineFile()
			}
			if err := writeJSON(l.lastFile(), State{Hash: "SAME"}); err != nil {
				t.Fatal(err)
			}
			rep, err := p.Run(context.Background())
			if err == nil || rep.UpToDate || !requestedCatalog.Load() {
				t.Fatalf("explicit comparison was skipped: %+v, err=%v", rep, err)
			}
		})
	}
}
