package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"limbus-asset-extractor/internal/catalog"
	"limbus-asset-extractor/internal/extractor"
)

const testLabel = "s20260917_testrelease"

func bundleFilesIn(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".bundle") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestStreamExtractsAndDeletesWhileDownloadsAreStillRunning(t *testing.T) {
	sc := newScenario(t) // A (changed) and C (added)
	p, l := newTestPipeline(t, func(c *Config) { c.DeleteBundles = true; c.DownloadWorkers = 2 })
	saveBaseline(t, l, sc.old)

	// C is held back until A has been extracted AND removed. With the old
	// "download everything, then extract" flow that can never happen, and the
	// gate would give up after 5 seconds.
	var sawIt atomic.Bool
	sc.srv.setGate(func(path string) {
		if path != "/rel/"+sc.c {
			return
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if exists(filepath.Join(l.outputDir(testLabel), "alpha", "TestTex.png")) && !exists(filepath.Join(l.bundlesDir(), sc.aNew)) {
				sawIt.Store(true)
				return
			}
		}
	})

	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if !sawIt.Load() {
		t.Fatal("A was not extracted and deleted while C was still downloading: the stages are not overlapping")
	}
	if rep.Extracted.Bundles != 2 || len(rep.ExtractFailed) != 0 {
		t.Errorf("extracted=%+v failed=%v", rep.Extracted, rep.ExtractFailed)
	}
	if left := bundleFilesIn(l.bundlesDir()); len(left) != 0 {
		t.Errorf("bundles left on disk after the run: %v", left)
	}
}

func TestKeepModeLeavesBundlesOnDisk(t *testing.T) {
	sc := newScenario(t)
	p, l := newTestPipeline(t, nil) // DeleteBundles is off
	saveBaseline(t, l, sc.old)
	if _, err := process(t, p, l, sc.cur); err != nil {
		t.Fatal(err)
	}
	if got := bundleFilesIn(l.bundlesDir()); len(got) != 2 {
		t.Errorf("keep mode should leave both bundles, found %v", got)
	}
}

func TestFailedExtractionKeepsTheBundleAndIsNotJournaled(t *testing.T) {
	sc := newScenario(t)
	sc.srv.garbage["/rel/"+sc.c] = true
	sc.cur.Bundles[2].SizeBytes = uint32(len("<html>this is not a bundle</html>"))
	p, l := newTestPipeline(t, func(c *Config) { c.DeleteBundles = true })
	saveBaseline(t, l, sc.old)

	rep, err := process(t, p, l, sc.cur)
	if err != nil || len(rep.ExtractFailed) != 1 {
		t.Fatalf("err=%v failed=%v", err, rep.ExtractFailed)
	}
	left := bundleFilesIn(l.bundlesDir())
	if len(left) != 1 || left[0] != sc.c {
		t.Errorf("only the bundle that failed to extract should stay on disk, found %v", left)
	}
	d := loadDoneLog(filepath.Join(l.runDir(testLabel), "done.json"), p.optionsFingerprint())
	if _, ok := d.get(sc.c); ok {
		t.Error("a failed bundle must not be journaled as done")
	}
	if _, ok := d.get(sc.aNew); !ok {
		t.Error("the bundle that did extract should be journaled")
	}
}

// interrupted runs C into a 404 so that A is finished and C is not.
func interruptedRun(t *testing.T, sc *scenario, mutate func(*Config)) (*Pipeline, layout) {
	t.Helper()
	sc.srv.set("/rel/"+sc.c, false)
	p, l := newTestPipeline(t, mutate)
	saveBaseline(t, l, sc.old)
	if _, err := process(t, p, l, sc.cur); err == nil {
		t.Fatal("the first run was supposed to fail on C")
	}
	sc.srv.set("/rel/"+sc.c, true)
	return p, l
}

func TestResumeNeedsNeitherCacheNorRedownload(t *testing.T) {
	sc := newScenario(t)
	p, l := interruptedRun(t, sc, func(c *Config) { c.DeleteBundles = true })
	if got := bundleFilesIn(l.bundlesDir()); len(got) != 0 {
		t.Fatalf("A was deleted after extraction, but %v is on disk", got)
	}
	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if n := sc.srv.hitCount("/rel/" + sc.aNew); n != 1 {
		t.Errorf("A was fetched %d times; the journal should have spared the second download", n)
	}
	if rep.Resumed != 1 || rep.Downloaded != 1 || rep.Extracted.Bundles != 2 || !rep.StateAdvanced {
		t.Errorf("resumed=%d downloaded=%d extracted=%+v advanced=%v", rep.Resumed, rep.Downloaded, rep.Extracted, rep.StateAdvanced)
	}
	for _, stem := range []string{"alpha", "gamma"} {
		if !exists(filepath.Join(l.outputDir(testLabel), stem, "TestTex.png")) {
			t.Errorf("output for %s missing", stem)
		}
	}
}

func TestResumeIsDroppedWhenExtractionSettingsChange(t *testing.T) {
	sc := newScenario(t)
	_, l := interruptedRun(t, sc, func(c *Config) { c.DeleteBundles = true })

	// same work directory, but now something else is excluded: what A produced
	// back then is not what this run would produce.
	p2, _ := newTestPipelineIn(t, l, func(c *Config) {
		c.DeleteBundles = true
		c.Extract = extractor.Options{ExcludeNames: []string{"somethingelse"}}
	})
	rep, err := process(t, p2, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Resumed != 0 || sc.srv.hitCount("/rel/"+sc.aNew) != 2 {
		t.Errorf("resumed=%d, A fetched %d times: changed settings must redo A", rep.Resumed, sc.srv.hitCount("/rel/"+sc.aNew))
	}
}

func TestResumeRequiresTheOutputToStillBeThere(t *testing.T) {
	sc := newScenario(t)
	p, l := interruptedRun(t, sc, func(c *Config) { c.DeleteBundles = true })
	if err := os.RemoveAll(filepath.Join(l.outputDir(testLabel), "alpha")); err != nil {
		t.Fatal(err)
	}
	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Resumed != 0 || sc.srv.hitCount("/rel/"+sc.aNew) != 2 {
		t.Errorf("the user deleted A's output: A must be redone (resumed=%d, fetched %d)", rep.Resumed, sc.srv.hitCount("/rel/"+sc.aNew))
	}
	if !exists(filepath.Join(l.outputDir(testLabel), "alpha", "TestTex.png")) {
		t.Error("A's output was not restored")
	}
}

func TestResumeOfABundleThatYieldedNothing(t *testing.T) {
	// Everything in the bundle is excluded: no output folder exists, and none
	// is needed to count the bundle as done.
	sc := newScenario(t)
	cfg := func(c *Config) {
		c.DeleteBundles = true
		c.Extract = extractor.Options{ExcludeNames: []string{"testtex"}}
	}
	p, l := interruptedRun(t, sc, cfg)
	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Resumed != 1 || sc.srv.hitCount("/rel/"+sc.aNew) != 1 {
		t.Errorf("resumed=%d, A fetched %d times", rep.Resumed, sc.srv.hitCount("/rel/"+sc.aNew))
	}
	if rep.Extracted.Empty != 2 || rep.Extracted.Filtered != 2 {
		t.Errorf("both bundles are empty after filtering: %+v", rep.Extracted)
	}
}

func TestResumeCleansUpABundleLeftOverByACrash(t *testing.T) {
	sc := newScenario(t)
	// first run keeps bundles (as if it died between "journal" and "delete")
	_, l := interruptedRun(t, sc, nil)
	if got := bundleFilesIn(l.bundlesDir()); len(got) != 1 || got[0] != sc.aNew {
		t.Fatalf("setup: expected A to be left on disk, got %v", got)
	}
	p2, _ := newTestPipelineIn(t, l, func(c *Config) { c.DeleteBundles = true })
	if _, err := process(t, p2, l, sc.cur); err != nil {
		t.Fatal(err)
	}
	if got := bundleFilesIn(l.bundlesDir()); len(got) != 0 {
		t.Errorf("the leftover bundle should have been removed on resume, found %v", got)
	}
}

func TestDoneLogRoundTripAndBadFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "done.json")
	d := loadDoneLog(path, "opts-1")
	if _, ok := d.get("x.bundle"); ok {
		t.Fatal("a missing journal must start empty")
	}
	if err := d.record("x.bundle", extractor.Stats{Textures: 3, Filtered: 1}); err != nil {
		t.Fatal(err)
	}
	if st, ok := loadDoneLog(path, "opts-1").get("x.bundle"); !ok || st.Textures != 3 || st.Filtered != 1 {
		t.Errorf("not read back: %+v %v", st, ok)
	}
	if _, ok := loadDoneLog(path, "opts-2").get("x.bundle"); ok {
		t.Error("a journal written under other settings must be ignored")
	}
	os.WriteFile(path, []byte("{ not json"), 0o644)
	if _, ok := loadDoneLog(path, "opts-1").get("x.bundle"); ok {
		t.Error("a corrupt journal must be ignored, not trusted")
	}
}

func TestOptionsFingerprintIgnoresOrderAndCase(t *testing.T) {
	mk := func(names ...string) string {
		p := &Pipeline{}
		p.Cfg.Extract.ExcludeNames = names
		return p.optionsFingerprint()
	}
	if mk("FX", "mask") != mk("mask", "fx") {
		t.Error("order and case of exclusions must not change the fingerprint")
	}
	if mk("fx") == mk("fx", "mask") {
		t.Error("different exclusions must change it")
	}
}

func TestManyBundlesStreamWithBoundedDiskUse(t *testing.T) {
	const n = 60
	srv := newCDN(t)
	size := len(srv.data)
	var bundles []catalog.Bundle
	for i := 0; i < n; i++ {
		file := fmt.Sprintf("gen%03d_assets_all_%032x.bundle", i, i+1)
		srv.set("/rel/"+file, true)
		bundles = append(bundles, mkBundle(fmt.Sprintf("n%03d", i), srv.url(file), size, byte(i)))
	}
	p, l := newTestPipeline(t, func(c *Config) {
		c.DeleteBundles = true
		c.DownloadWorkers, c.ExtractWorkers = 8, 1 // downloads can far outrun extraction
	})
	saveBaseline(t, l, mkCat("OLD"))

	var peak int32
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if c := int32(len(bundleFilesIn(l.bundlesDir()))); c > atomic.LoadInt32(&peak) {
				atomic.StoreInt32(&peak, c)
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()

	rep, err := process(t, p, l, mkCat("NEW", bundles...))
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Downloaded != n || rep.Extracted.Bundles != n || len(rep.ExtractFailed) != 0 {
		t.Fatalf("downloaded=%d extracted=%d failed=%v", rep.Downloaded, rep.Extracted.Bundles, rep.ExtractFailed)
	}
	if left := bundleFilesIn(l.bundlesDir()); len(left) != 0 {
		t.Errorf("%d bundles left on disk", len(left))
	}
	// 8 downloaders each holding one finished bundle + 1 queue slot + 1 being
	// extracted = 10, whatever the size of the update. A download-everything-
	// first flow would peak at 60.
	if got := atomic.LoadInt32(&peak); got > 11 {
		t.Errorf("peak of %d bundles on disk at once: the queue is not bounding disk use", got)
	}
	t.Logf("peak bundles on disk at once: %d of %d", atomic.LoadInt32(&peak), n)
}
