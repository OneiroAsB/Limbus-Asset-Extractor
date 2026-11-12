package pipeline

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"limbus-asset-extractor/internal/cdn"
	"limbus-asset-extractor/internal/resolver"
)

// redirectTransport sends every request to target, keeping the path, so
// the catalog's real https://download.limbuscompanycdn.org/... URLs land on a
// local test server.
type redirectTransport struct{ target *url.URL }

func (rt redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// TestRealCatalogsEndToEnd runs the complete Run() flow, including decoding a
// real catalog_S1.bin and diffing it against an older unpacked catalog, with
// the CDN replaced by a local server (bundles are the tiny test fixture).
//
//	LIMBUS_NEW_BIN=/path/catalog_S1.bin LIMBUS_OLD_JSON=/path/old_unpacked.json \
//	  go test ./internal/pipeline -run RealCatalogs -v
func TestRealCatalogsEndToEnd(t *testing.T) {
	newBin, oldJSON := os.Getenv("LIMBUS_NEW_BIN"), os.Getenv("LIMBUS_OLD_JSON")
	if newBin == "" || oldJSON == "" {
		t.Skip("set LIMBUS_NEW_BIN and LIMBUS_OLD_JSON to run against real catalogs")
	}
	binData, err := os.ReadFile(newBin)
	if err != nil {
		t.Fatal(err)
	}
	fixture := fixtureBundle(t)

	var hashReqs, binReqs, bundleReqs int32
	const version = "s20260917_XozhYgm77guBS5Wgmims"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+version+"/catalog_S1.hash":
			atomic.AddInt32(&hashReqs, 1)
			w.Write([]byte("E2E-HASH-1\n"))
		case r.URL.Path == "/"+version+"/catalog_S1.bin":
			atomic.AddInt32(&binReqs, 1)
			w.Write(binData)
		case strings.HasSuffix(r.URL.Path, ".bundle"):
			if r.Header.Get("X-Requested-With") == "" || !strings.HasPrefix(r.Header.Get("User-Agent"), "UnityPlayer/") {
				t.Errorf("game headers missing on %s", r.URL.Path)
			}
			atomic.AddInt32(&bundleReqs, 1)
			w.Write(fixture)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)

	client := cdn.NewClient()
	client.HTTP = &http.Client{Transport: redirectTransport{target}}
	client.Backoff = time.Millisecond

	cfg := Config{
		WorkDir:         t.TempDir(),
		BaselinePath:    oldJSON,
		Resolver:        resolver.Manual(version),
		DownloadWorkers: 8,
		ExtractWorkers:  4,
		VerifySize:      false, // the fixture is not as big as the real bundles
		DeleteBundles:   true,  // as the CLI does by default
	}
	p, err := New(cfg, client, log.New(testWriter{t}, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	rep, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", rep.Summary())

	if rep.Label != version {
		t.Errorf("label = %q", rep.Label)
	}
	if rep.Diff.BundlesAdded+rep.Diff.BundlesChanged != rep.Planned+rep.SkippedLocal {
		t.Errorf("added(%d)+changed(%d) != planned(%d)+skipped(%d)", rep.Diff.BundlesAdded, rep.Diff.BundlesChanged, rep.Planned, rep.SkippedLocal)
	}
	if int(bundleReqs) != rep.Planned || rep.Downloaded != rep.Planned {
		t.Errorf("bundle requests=%d downloaded=%d planned=%d", bundleReqs, rep.Downloaded, rep.Planned)
	}
	if len(rep.DownloadFailed)+len(rep.ExtractFailed) != 0 {
		t.Errorf("failures: %v %v", rep.DownloadFailed, rep.ExtractFailed)
	}
	if rep.Extracted.Bundles != rep.Planned {
		t.Errorf("extracted %d bundles, planned %d", rep.Extracted.Bundles, rep.Planned)
	}
	if !rep.StateAdvanced {
		t.Error("state should have advanced")
	}

	l := layout{cfg.WorkDir}
	if left := bundleFilesIn(l.bundlesDir()); len(left) != 0 {
		t.Errorf("%d bundles still on disk after the run, e.g. %s", len(left), left[0])
	}
	dirs, _ := os.ReadDir(l.outputDir(version))
	if len(dirs) != rep.Planned {
		t.Errorf("output has %d bundle folders, want %d", len(dirs), rep.Planned)
	}
	for _, d := range dirs {
		if strings.Contains(d.Name(), ".partial") {
			t.Errorf("leftover %s", d.Name())
		}
	}

	// Second run: the hash is unchanged, so only the .hash file is fetched.
	before := atomic.LoadInt32(&bundleReqs)
	rep2, err := p.Run(context.Background())
	if err != nil || !rep2.UpToDate {
		t.Fatalf("second run: %+v err=%v", rep2, err)
	}
	if hashReqs != 2 || binReqs != 1 || atomic.LoadInt32(&bundleReqs) != before {
		t.Errorf("second run should only re-check the hash: hash=%d bin=%d bundles %d->%d", hashReqs, binReqs, before, bundleReqs)
	}

	// Third run: the CDN publishes a new hash for the same catalog content.
	// -baseline was only for bootstrapping; dropped, as a regular run would,
	// the saved state is the baseline, so the diff is empty.
	p.Cfg.BaselinePath = ""
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".hash") {
			w.Write([]byte("E2E-HASH-2"))
			return
		}
		w.Write(binData)
	})
	rep3, err := p.Run(context.Background())
	if err != nil || rep3.UpToDate || rep3.Planned != 0 || !rep3.StateAdvanced {
		t.Fatalf("third run: %+v err=%v", rep3, err)
	}
	if st, _ := loadState(l); st == nil || st.Hash != "E2E-HASH-2" {
		t.Errorf("state hash not updated: %+v", st)
	}
}
