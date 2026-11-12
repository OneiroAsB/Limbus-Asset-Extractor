package pipeline

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"limbus-asset-extractor/internal/catalog"
	"limbus-asset-extractor/internal/cdn"
	"limbus-asset-extractor/internal/resolver"
)

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func fixtureBundle(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../extractor/testdata/rgba32.bundle")
	if err != nil {
		t.Fatalf("fixture missing (regenerate: WRITE_TESTDATA=1 go test ./internal/extractor -run TestWriteTestdata): %v", err)
	}
	return data
}

func hex32(c byte) string { return strings.Repeat(string(c), 32) }

// bundleFile builds a file name the way the real catalog does.
func bundleFile(stem string, c byte) string { return stem + "_assets_all_" + hex32(c) + ".bundle" }

func mkBundle(name, url string, size int, hashChar byte) catalog.Bundle {
	n := name
	return catalog.Bundle{BundleName: &n, Hash: hex32(hashChar), Crc: uint32(hashChar), SizeBytes: uint32(size), Path: url}
}

func mkCat(build string, bundles ...catalog.Bundle) *catalog.Catalog {
	c := &catalog.Catalog{SourceFile: "catalog_S1.bin", Bundles: bundles, Keys: map[string][]int{}}
	c.Meta.Version = 2
	c.Meta.BuildResultHash = &build
	c.Stats.Bundles = len(bundles)
	return c
}

// cdnServer serves the fixture bundle for every path in good, counts hits per
// path, and answers anything else with 404. Paths in garbage get non-bundle bytes.
type cdnServer struct {
	*httptest.Server
	mu      sync.Mutex
	hits    map[string]int
	good    map[string]bool
	garbage map[string]bool
	data    []byte
	gate    func(path string) // optional: called at the start of every request
}

func newCDN(t *testing.T, good ...string) *cdnServer {
	s := &cdnServer{hits: map[string]int{}, good: map[string]bool{}, garbage: map[string]bool{}, data: fixtureBundle(t)}
	for _, g := range good {
		s.good[g] = true
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		ok, bad, gate := s.good[r.URL.Path], s.garbage[r.URL.Path], s.gate
		s.mu.Unlock()
		if gate != nil {
			gate(r.URL.Path)
		}
		switch {
		case bad:
			w.Write([]byte("<html>this is not a bundle</html>"))
		case ok:
			w.Write(s.data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *cdnServer) setGate(f func(path string)) { s.mu.Lock(); s.gate = f; s.mu.Unlock() }
func (s *cdnServer) set(path string, good bool)  { s.mu.Lock(); s.good[path] = good; s.mu.Unlock() }
func (s *cdnServer) hitCount(path string) int    { s.mu.Lock(); defer s.mu.Unlock(); return s.hits[path] }
func (s *cdnServer) totalHits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, h := range s.hits {
		n += h
	}
	return n
}

func (s *cdnServer) url(file string) string { return s.URL + "/rel/" + file }

func newTestPipeline(t *testing.T, mutate func(*Config)) (*Pipeline, layout) {
	t.Helper()
	cfg := Config{WorkDir: t.TempDir(), VerifySize: true, DownloadWorkers: 3, ExtractWorkers: 2, Resolver: resolver.Manual("s20260917_testrelease")}
	if mutate != nil {
		mutate(&cfg)
	}
	client := cdn.NewClient()
	client.Backoff = time.Millisecond
	client.Retries = 1
	p, err := New(cfg, client, log.New(testWriter{t}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return p, layout{p.Cfg.WorkDir}
}

// newTestPipelineIn is newTestPipeline on an existing work directory.
func newTestPipelineIn(t *testing.T, l layout, mutate func(*Config)) (*Pipeline, layout) {
	t.Helper()
	return newTestPipeline(t, func(c *Config) {
		c.WorkDir = l.root
		if mutate != nil {
			mutate(c)
		}
	})
}

func saveBaseline(t *testing.T, l layout, c *catalog.Catalog) {
	t.Helper()
	if err := os.MkdirAll(l.stateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(l.baselineFile()); err != nil {
		t.Fatal(err)
	}
}

func process(t *testing.T, p *Pipeline, l layout, cat *catalog.Catalog) (*Report, error) {
	t.Helper()
	return p.process(context.Background(), &Report{StartedAt: time.Now()}, l, "s20260917_testrelease", "HASH-NEW", cat)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// scenario: A changed, B untouched, C added, E removed.
type scenario struct {
	srv                  *cdnServer
	old, cur             *catalog.Catalog
	aOld, aNew, bFile, c string // file names
}

func newScenario(t *testing.T) *scenario {
	sc := &scenario{}
	sc.aOld, sc.aNew, sc.bFile, sc.c = bundleFile("alpha", 'a'), bundleFile("alpha", 'b'), bundleFile("beta", 'c'), bundleFile("gamma", 'd')
	sc.srv = newCDN(t, "/rel/"+sc.aNew, "/rel/"+sc.c)
	size := len(sc.srv.data)
	sc.old = mkCat("OLD",
		mkBundle("A", sc.srv.url(sc.aOld), size, 'a'),
		mkBundle("B", sc.srv.url(sc.bFile), size, 'c'),
		mkBundle("E", sc.srv.url(bundleFile("echo", 'e')), size, 'e'),
	)
	sc.cur = mkCat("NEW",
		mkBundle("A", sc.srv.url(sc.aNew), size, 'b'),
		mkBundle("B", sc.srv.url(sc.bFile), size, 'c'),
		mkBundle("C", sc.srv.url(sc.c), size, 'd'),
	)
	return sc
}

func TestHappyPath(t *testing.T) {
	sc := newScenario(t)
	p, l := newTestPipeline(t, nil)
	saveBaseline(t, l, sc.old)

	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Diff.BundlesAdded != 1 || rep.Diff.BundlesChanged != 1 || rep.Diff.BundlesRemoved != 1 {
		t.Errorf("diff counts wrong: %+v", rep.Diff)
	}
	if rep.Planned != 2 || rep.Downloaded != 2 || rep.Cached != 0 {
		t.Errorf("planned/downloaded/cached = %d/%d/%d, want 2/2/0", rep.Planned, rep.Downloaded, rep.Cached)
	}
	if n := sc.srv.hitCount("/rel/" + sc.bFile); n != 0 {
		t.Errorf("unchanged bundle B was requested %d time(s)", n)
	}
	// stable output names: no content hash in the folder name
	for _, stem := range []string{"alpha", "gamma"} {
		if !exists(filepath.Join(l.outputDir("s20260917_testrelease"), stem, "TestTex.png")) {
			t.Errorf("output/%s/TestTex.png missing", stem)
		}
	}
	if rep.Extracted.Bundles != 2 || rep.Extracted.Textures != 2 || len(rep.ExtractFailed) != 0 {
		t.Errorf("extraction stats: %+v failed=%v", rep.Extracted, rep.ExtractFailed)
	}
	if !rep.StateAdvanced {
		t.Error("state should advance after a clean run")
	}
	st, err := loadState(l)
	if err != nil || st == nil || st.Hash != "HASH-NEW" || st.BuildHash != "NEW" {
		t.Errorf("saved state wrong: %+v err=%v", st, err)
	}
	if b, err := catalog.Load(l.baselineFile()); err != nil || b.BuildHash() != "NEW" {
		t.Errorf("baseline not replaced by the new catalog (err=%v)", err)
	}
	for _, f := range []string{"diff.json", "report.json", "catalog_S1_unpacked.json"} {
		if !exists(filepath.Join(l.runDir("s20260917_testrelease"), f)) {
			t.Errorf("runs/<label>/%s missing", f)
		}
	}
	if len(rep.Notes) != 0 {
		t.Errorf("unexpected notes: %v", rep.Notes)
	}
}

func TestDownloadFailureKeepsBaselineAndRerunContinues(t *testing.T) {
	sc := newScenario(t)
	sc.srv.set("/rel/"+sc.c, false) // C is 404 for now
	p, l := newTestPipeline(t, nil)
	saveBaseline(t, l, sc.old)

	rep, err := process(t, p, l, sc.cur)
	if err == nil {
		t.Fatal("a failed download must make the run fail")
	}
	if len(rep.DownloadFailed) != 1 || rep.DownloadFailed[0].Bundle != sc.c {
		t.Fatalf("DownloadFailed = %+v", rep.DownloadFailed)
	}
	if rep.StateAdvanced || exists(l.lastFile()) {
		t.Error("state must NOT advance while a bundle is missing")
	}
	if b, _ := catalog.Load(l.baselineFile()); b.BuildHash() != "OLD" {
		t.Error("baseline must still be the old catalog")
	}
	if !exists(filepath.Join(l.outputDir("s20260917_testrelease"), "alpha", "TestTex.png")) {
		t.Error("bundles that did download should still have been extracted")
	}
	if !exists(filepath.Join(l.runDir("s20260917_testrelease"), "report.json")) {
		t.Error("report.json must be written even for a failed run")
	}

	// the CDN recovers; rerun against the same (unchanged) baseline
	sc.srv.set("/rel/"+sc.c, true)
	rep, err = process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Resumed != 1 || rep.Downloaded != 1 {
		t.Errorf("rerun should skip A (already extracted by the first run) and fetch only C: resumed=%d downloaded=%d", rep.Resumed, rep.Downloaded)
	}
	if n := sc.srv.hitCount("/rel/" + sc.aNew); n != 1 {
		t.Errorf("A downloaded %d times in total, want 1", n)
	}
	if rep.Extracted.Bundles != 2 || rep.Extracted.Textures != 2 {
		t.Errorf("the report of the finished run must cover both bundles, resumed one included: %+v", rep.Extracted)
	}
	if !rep.StateAdvanced {
		t.Error("state should advance once everything is in")
	}
}

func TestDownloadOnlyRunReusesTheBundleCache(t *testing.T) {
	// Without extraction there is no journal: what was downloaded stays in the
	// cache and is reused.
	sc := newScenario(t)
	sc.srv.set("/rel/"+sc.c, false)
	p, l := newTestPipeline(t, func(c *Config) { c.NoExtract = true })
	saveBaseline(t, l, sc.old)
	if _, err := process(t, p, l, sc.cur); err == nil {
		t.Fatal("expected the C download to fail")
	}
	sc.srv.set("/rel/"+sc.c, true)
	rep, err := process(t, p, l, sc.cur)
	if err != nil || rep.Cached != 1 || rep.Downloaded != 1 || rep.Resumed != 0 {
		t.Fatalf("cached=%d downloaded=%d resumed=%d err=%v", rep.Cached, rep.Downloaded, rep.Resumed, err)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	sc := newScenario(t)
	p, l := newTestPipeline(t, func(c *Config) { c.DryRun = true })
	saveBaseline(t, l, sc.old)

	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || rep.Planned != 2 || rep.PlannedBytes != 2*int64(len(sc.srv.data)) {
		t.Errorf("dry-run report: %+v", rep)
	}
	if sc.srv.totalHits() != 0 {
		t.Error("dry-run must not touch the network")
	}
	if exists(l.bundlesDir()) || exists(l.outputDir("s20260917_testrelease")) || exists(l.lastFile()) {
		t.Error("dry-run must not create bundles/output/state")
	}
	if b, _ := catalog.Load(l.baselineFile()); b.BuildHash() != "OLD" {
		t.Error("dry-run must not replace the baseline")
	}
}

func TestFirstRunOnlyRecordsBaseline(t *testing.T) {
	sc := newScenario(t)
	p, l := newTestPipeline(t, nil) // no baseline anywhere

	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Bootstrap || !rep.StateAdvanced || sc.srv.totalHits() != 0 {
		t.Fatalf("bootstrap: %+v hits=%d", rep, sc.srv.totalHits())
	}
	if b, err := catalog.Load(l.baselineFile()); err != nil || b.BuildHash() != "NEW" {
		t.Fatalf("baseline not recorded (err=%v)", err)
	}
	// the very same catalog again: now there IS a baseline, and no difference
	rep, err = process(t, p, l, sc.cur)
	if err != nil || rep.Bootstrap || rep.Planned != 0 || !rep.StateAdvanced {
		t.Fatalf("second run: %+v err=%v", rep, err)
	}
}

func TestFullTreatsEverythingAsNew(t *testing.T) {
	sc := newScenario(t)
	sc.srv.set("/rel/"+sc.bFile, true)
	p, l := newTestPipeline(t, func(c *Config) { c.Full = true })

	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Planned != 3 || rep.Downloaded != 3 {
		t.Errorf("-full should fetch all 3 bundles: planned=%d downloaded=%d", rep.Planned, rep.Downloaded)
	}
}

func TestFullIgnoresAnExistingBaseline(t *testing.T) {
	// The use case: the selection was widened, and what it used to skip must
	// be fetched now. A saved baseline must not hide any of it.
	sc := newScenario(t)
	sc.srv.set("/rel/"+sc.bFile, true) // B is unchanged since the baseline...
	p, l := newTestPipeline(t, func(c *Config) { c.Full = true })
	saveBaseline(t, l, sc.old)

	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Planned != 3 || sc.srv.hitCount("/rel/"+sc.bFile) != 1 { // ...and is fetched anyway
		t.Errorf("-full with a baseline: planned=%d, hits on the unchanged bundle=%d", rep.Planned, sc.srv.hitCount("/rel/"+sc.bFile))
	}
	if !rep.StateAdvanced {
		t.Error("the baseline should still move to the new catalog")
	}
}

func TestExplicitBaselineWinsOverMissingState(t *testing.T) {
	sc := newScenario(t)
	old := filepath.Join(t.TempDir(), "old_unpacked.json") // like the legacy unpack_catalog output
	if err := sc.old.Save(old); err != nil {
		t.Fatal(err)
	}
	p, l := newTestPipeline(t, func(c *Config) { c.BaselinePath = old })

	rep, err := process(t, p, l, sc.cur)
	if err != nil || rep.Bootstrap || rep.Planned != 2 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
}

func TestCorruptBaselineIsAnErrorNotASilentFullDownload(t *testing.T) {
	sc := newScenario(t)
	p, l := newTestPipeline(t, nil)
	os.MkdirAll(l.stateDir(), 0o755)
	os.WriteFile(l.baselineFile(), []byte("{ not json"), 0o644)
	if _, err := process(t, p, l, sc.cur); err == nil {
		t.Fatal("a corrupt baseline must stop the run")
	}
	if sc.srv.totalHits() != 0 {
		t.Error("nothing should be downloaded after a baseline error")
	}
}

func TestMaxBundlesIsATrialRun(t *testing.T) {
	sc := newScenario(t)
	p, l := newTestPipeline(t, func(c *Config) { c.MaxBundles = 1 })
	saveBaseline(t, l, sc.old)

	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Planned != 1 || !rep.Truncated {
		t.Errorf("planned=%d truncated=%v", rep.Planned, rep.Truncated)
	}
	if rep.StateAdvanced {
		t.Error("a capped run must not advance the baseline, or the rest would be lost")
	}
	if b, _ := catalog.Load(l.baselineFile()); b.BuildHash() != "OLD" {
		t.Error("baseline changed")
	}
}

func TestBundleFilterIsPermanentAndAdvancesState(t *testing.T) {
	sc := newScenario(t)
	p, l := newTestPipeline(t, func(c *Config) { c.BundleFilter = "GAMMA" }) // case-insensitive
	saveBaseline(t, l, sc.old)

	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Planned != 1 || sc.srv.hitCount("/rel/"+sc.c) != 1 || sc.srv.hitCount("/rel/"+sc.aNew) != 0 {
		t.Errorf("filter did not narrow the plan: planned=%d", rep.Planned)
	}
	if !rep.StateAdvanced {
		t.Error("a filter is the user's standing choice; state should advance")
	}
}

func TestBrokenBundleIsReportedButDoesNotBlockProgress(t *testing.T) {
	sc := newScenario(t)
	sc.srv.garbage["/rel/"+sc.c] = true
	// the garbage the server returns has a different length than the fixture
	sc.cur.Bundles[2].SizeBytes = uint32(len("<html>this is not a bundle</html>"))
	p, l := newTestPipeline(t, nil)
	saveBaseline(t, l, sc.old)

	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatalf("an unextractable bundle is not a download failure: %v", err)
	}
	if len(rep.ExtractFailed) != 1 || rep.ExtractFailed[0].Bundle != sc.c {
		t.Fatalf("ExtractFailed = %+v", rep.ExtractFailed)
	}
	if !rep.StateAdvanced {
		t.Error("a deterministic extraction failure must not freeze the baseline forever")
	}
	out := l.outputDir("s20260917_testrelease")
	if exists(filepath.Join(out, "gamma")) || exists(filepath.Join(out, "gamma.partial")) {
		t.Error("failed extraction must leave no folder behind")
	}
	if !exists(filepath.Join(out, "alpha", "TestTex.png")) {
		t.Error("the healthy bundle should still be extracted")
	}
}

func TestSizeVerification(t *testing.T) {
	sc := newScenario(t)
	sc.cur.Bundles[0].SizeBytes += 5 // catalog disagrees with the CDN about A

	p, l := newTestPipeline(t, nil)
	saveBaseline(t, l, sc.old)
	rep, err := process(t, p, l, sc.cur)
	if err == nil || len(rep.DownloadFailed) != 1 || !strings.Contains(rep.DownloadFailed[0].Error, "does not match") {
		t.Fatalf("expected a size-mismatch failure, got err=%v failed=%+v", err, rep.DownloadFailed)
	}

	p2, l2 := newTestPipeline(t, func(c *Config) { c.VerifySize = false })
	saveBaseline(t, l2, sc.old)
	if rep, err = process(t, p2, l2, sc.cur); err != nil || rep.Downloaded != 2 {
		t.Fatalf("with verification off the run should succeed: err=%v rep=%+v", err, rep)
	}
}

func TestNoExtractAndDeleteBundles(t *testing.T) {
	if _, err := New(Config{Resolver: resolver.Manual("s20260917_x"), NoExtract: true, DeleteBundles: true}, cdn.NewClient(), nil); err == nil {
		t.Error("-no-extract with -delete-bundles must be rejected")
	}

	sc := newScenario(t)
	p, l := newTestPipeline(t, func(c *Config) { c.NoExtract = true })
	saveBaseline(t, l, sc.old)
	rep, err := process(t, p, l, sc.cur)
	if err != nil || rep.Extracted.Bundles != 0 || !exists(filepath.Join(l.bundlesDir(), sc.c)) || exists(l.outputDir("s20260917_testrelease")) {
		t.Fatalf("download-only run misbehaved: err=%v rep=%+v", err, rep.Extracted)
	}

	sc2 := newScenario(t)
	p2, l2 := newTestPipeline(t, func(c *Config) { c.DeleteBundles = true })
	saveBaseline(t, l2, sc2.old)
	if _, err := process(t, p2, l2, sc2.cur); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(l2.bundlesDir(), sc2.c)) {
		t.Error("-delete-bundles should remove the bundle after a successful extraction")
	}
	if !exists(filepath.Join(l2.outputDir("s20260917_testrelease"), "gamma", "TestTex.png")) {
		t.Error("...but keep what was extracted from it")
	}
}

func TestShippedBundles(t *testing.T) {
	srv := newCDN(t)
	data := srv.data
	shipped := `{UnityEngine.AddressableAssets.Addressables.RuntimePath}\s20260917_testrelease\` + bundleFile("fonts", 'f')
	old := mkCat("OLD")
	cur := mkCat("NEW", mkBundle("F", shipped, len(data), 'f'))

	// without a game dir: skipped, and said so
	p, l := newTestPipeline(t, nil)
	saveBaseline(t, l, old)
	rep, err := process(t, p, l, cur)
	if err != nil || rep.SkippedLocal != 1 || rep.Planned != 0 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}

	// with a game dir that has the file: extracted in place, never deleted
	game := t.TempDir()
	dir := filepath.Join(game, "LimbusCompany_Data", "StreamingAssets", "aa", "s20260917_testrelease")
	os.MkdirAll(dir, 0o755)
	local := filepath.Join(dir, bundleFile("fonts", 'f'))
	os.WriteFile(local, data, 0o644)

	p2, l2 := newTestPipeline(t, func(c *Config) { c.GameDir = game; c.DeleteBundles = true })
	saveBaseline(t, l2, old)
	rep, err = process(t, p2, l2, cur)
	if err != nil || rep.SkippedLocal != 0 || rep.Extracted.Bundles != 1 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if !exists(local) {
		t.Fatal("a file inside the game install must never be deleted")
	}
	if srv.totalHits() != 0 || exists(l2.bundlesDir()) {
		t.Error("a shipped bundle needs no download and no cache copy")
	}
}

func TestCancellationStopsWithoutAdvancing(t *testing.T) {
	sc := newScenario(t)
	p, l := newTestPipeline(t, nil)
	saveBaseline(t, l, sc.old)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := p.process(ctx, &Report{StartedAt: time.Now()}, l, "s20260917_testrelease", "H", sc.cur)
	if err == nil {
		t.Fatal("expected a context error")
	}
	if rep.StateAdvanced || exists(l.lastFile()) {
		t.Error("an interrupted run must not advance state")
	}
}

func TestExtractorOptionsAreCheckedBeforeAnyDownload(t *testing.T) {
	cfg := Config{Resolver: resolver.Manual("s20260917_x")}
	cfg.Extract.Only = "bogus"
	if _, err := New(cfg, cdn.NewClient(), nil); err == nil {
		t.Fatal("bad -only must be rejected up front")
	}
	if _, err := New(Config{}, cdn.NewClient(), nil); err == nil {
		t.Fatal("no catalog source must be rejected")
	}
	if _, err := New(Config{Resolver: resolver.Manual("s20260917_x"), GameDir: t.TempDir()}, cdn.NewClient(), nil); err == nil {
		t.Fatal("a -game-dir without StreamingAssets/aa must be rejected")
	}
}

func TestRunSkipsEverythingWhenHashUnchanged(t *testing.T) {
	sc := newScenario(t)
	hashSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/s20260917_testrelease/catalog_S1.hash" {
			w.Write([]byte("SAME-HASH\n"))
			return
		}
		t.Errorf("unexpected request %s: with an unchanged hash only the .hash file may be fetched", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer hashSrv.Close()

	p, l := newTestPipeline(t, func(c *Config) { c.CDNBase = hashSrv.URL + "/" })
	saveBaseline(t, l, sc.old)
	if err := writeJSON(l.lastFile(), State{Label: "prev", Hash: "SAME-HASH"}); err != nil {
		t.Fatal(err)
	}
	rep, err := p.Run(context.Background())
	if err != nil || !rep.UpToDate {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
}

func TestRunReportsAGarbageCatalogCleanly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".hash") {
			w.Write([]byte("H1"))
			return
		}
		w.Write([]byte("<html>maintenance</html>")) // a CDN error page instead of catalog_S1.bin
	}))
	defer srv.Close()
	p, _ := newTestPipeline(t, func(c *Config) { c.CDNBase = srv.URL })
	_, err := p.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "catalog") {
		t.Fatalf("want a readable catalog error (and no panic), got %v", err)
	}
}

func TestRunStopsOnMissingHash(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	p, _ := newTestPipeline(t, func(c *Config) { c.CDNBase = srv.URL })
	if _, err := p.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "catalog_S1.hash") {
		t.Fatalf("got %v", err)
	}
}
