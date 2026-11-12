package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"limbus-asset-extractor/internal/cdn"
	"limbus-asset-extractor/internal/extractor"
	"limbus-asset-extractor/internal/resolver"
)

// Config is everything a pipeline run needs.
type Config struct {
	// WorkDir holds all state and output. Layout:
	//
	//	state/last.json                    hash + version of the last processed catalog
	//	state/catalog_S1_unpacked.json     that catalog: the baseline the next run diffs against
	//	runs/<label>/                      catalog_S1.bin/.hash, unpacked json, diff.json, report.json
	//	bundles/                           downloaded bundle cache
	//	output/<label>/<bundle>/           extracted assets
	WorkDir string

	// CDNBase is the CDN root, without a trailing slash.
	CDNBase string

	// Catalog source. Set Resolver to fetch catalog_S1.hash/.bin from the CDN
	// for the version it returns, or CatalogPath to use a local catalog_S1.bin
	// (no network access for the catalog, no hash check).
	Resolver    resolver.Resolver
	CatalogPath string
	// Label names this run's folders. Defaults to the resolved version, or,
	// for a local catalog, the release folder most of its bundles live in.
	Label string

	// BaselinePath is a catalog JSON to diff against instead of the saved
	// state (the old unpack_catalog output works). Without it, and without
	// saved state, the first run only records a baseline.
	BaselinePath string
	// Full treats every bundle as new (no baseline needed). Combine with
	// BundleFilter unless you want the whole ~15 GB.
	Full bool
	// AssetDiff compares extracted files with the previous bundle. Output
	// contains only added/changed files; before/ keeps changed/removed originals.
	AssetDiff bool
	// DryRun stops after planning: shows what would be downloaded and
	// changes neither state nor the bundle cache.
	DryRun bool

	// BundleFilter keeps only bundles whose file name contains this text
	// (case-insensitive). It is a permanent choice: bundles it excludes are
	// treated as uninteresting and will not reappear in the next diff.
	BundleFilter string
	// MaxBundles caps how many bundles are processed (0 = no cap). A capped
	// run is a trial: the baseline is NOT advanced, so the next run sees the
	// rest again.
	MaxBundles int

	// Select limits the run to the bundles (and the objects inside them) that
	// matter: see selection.go. nil takes everything. Like BundleFilter it is a
	// standing choice - what it leaves out does not come back in a later diff.
	Select *Selection

	DownloadWorkers int // parallel downloads (default 4)
	ExtractWorkers  int // parallel extractions (default 2; a big bundle can use ~1 GB of RAM)
	VerifySize      bool
	NoExtract       bool // download only
	// DeleteBundles removes each downloaded bundle as soon as it has been
	// extracted successfully. A bundle that fails to extract is kept, for
	// inspection. Bundles that ship with the game are never touched.
	DeleteBundles bool
	GameDir       string

	Extract extractor.Options
	Verbose bool // per-object extractor output
}

func (c *Config) withDefaults() {
	if c.WorkDir == "" {
		c.WorkDir = "work"
	}
	if c.CDNBase == "" {
		c.CDNBase = cdn.DefaultBase
	}
	c.CDNBase = strings.TrimRight(c.CDNBase, "/")
	if c.DownloadWorkers < 1 {
		c.DownloadWorkers = 4
	}
	if c.ExtractWorkers < 1 {
		c.ExtractWorkers = 2
	}
}

func (c *Config) validate() error {
	switch {
	case c.Resolver == nil && c.CatalogPath == "":
		return fmt.Errorf("no catalog source: give a version (-version, -version-file, -version-cmd, -game-dir) or a local catalog (-catalog)")
	case c.NoExtract && c.DeleteBundles:
		return fmt.Errorf("deleting bundles needs extraction: it would delete bundles nothing was extracted from")
	case c.AssetDiff && c.NoExtract:
		return fmt.Errorf("-asset-diff requires extraction; remove -no-extract")
	case c.AssetDiff && c.Full:
		return fmt.Errorf("-asset-diff requires a baseline; remove -full")
	case c.MaxBundles < 0:
		return fmt.Errorf("-max-bundles must be >= 0")
	}
	if c.Label != "" {
		if err := resolver.Validate(c.Label); err != nil {
			return fmt.Errorf("label: %w", err)
		}
	}
	return nil
}

type layout struct{ root string }

func (l layout) stateDir() string              { return filepath.Join(l.root, "state") }
func (l layout) lastFile() string              { return filepath.Join(l.stateDir(), "last.json") }
func (l layout) baselineFile() string          { return filepath.Join(l.stateDir(), "catalog_S1_unpacked.json") }
func (l layout) runDir(label string) string    { return filepath.Join(l.root, "runs", label) }
func (l layout) bundlesDir() string            { return filepath.Join(l.root, "bundles") }
func (l layout) outputDir(label string) string { return filepath.Join(l.root, "output", label) }

// State records the last fully processed catalog.
type State struct {
	Label       string    `json:"label"`
	Hash        string    `json:"catalog_hash"` // contents of catalog_S1.hash; "" if the catalog was supplied locally
	BuildHash   string    `json:"build_result_hash"`
	ProcessedAt time.Time `json:"processed_at"`
}

// loadState returns nil (and no error) when there is no saved state.
func loadState(l layout) (*State, error) {
	data, err := os.ReadFile(l.lastFile())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s is corrupt: %w", l.lastFile(), err)
	}
	return &s, nil
}

// writeFileAtomic writes a temporary file and renames it over the destination.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func copyFileAtomic(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, data)
}

// Failure is one bundle that could not be downloaded or extracted.
type Failure struct {
	Bundle string `json:"bundle"`
	Error  string `json:"error"`
}

// PlanEntry is one bundle of the run's plan, as recorded in the report.
type PlanEntry struct {
	Bundle string `json:"bundle"`        // file name
	Folder string `json:"output_folder"` // output/<label>/<folder>
	Reason string `json:"reason"`        // added | changed
	Size   uint32 `json:"size_bytes"`
}

// Report summarises a run; it is written to runs/<label>/report.json.
type Report struct {
	Label        string    `json:"label"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	CatalogHash  string    `json:"catalog_hash,omitempty"`
	PreviousHash string    `json:"previous_hash,omitempty"`

	UpToDate      bool `json:"up_to_date"` // hash unchanged: nothing to do
	Bootstrap     bool `json:"bootstrap"`  // first run: baseline recorded, nothing downloaded
	DryRun        bool `json:"dry_run"`
	StateAdvanced bool `json:"state_advanced"`

	Diff struct {
		BundlesAdded   int `json:"bundles_added"`
		BundlesRemoved int `json:"bundles_removed"`
		BundlesChanged int `json:"bundles_changed"`
		KeysAdded      int `json:"keys_added"`
		KeysRemoved    int `json:"keys_removed"`
		KeysChanged    int `json:"keys_changed"`
	} `json:"diff"`

	Plan []PlanEntry `json:"plan,omitempty"`

	NotSelected      int         `json:"not_selected_bundles"`
	NotSelectedBytes int64       `json:"not_selected_bytes"`
	NotSelectedList  []PlanEntry `json:"not_selected,omitempty"`

	Planned        int   `json:"planned_bundles"`
	PlannedBytes   int64 `json:"planned_bytes"`
	Truncated      bool  `json:"truncated_by_max_bundles"`
	SkippedLocal   int   `json:"skipped_local_bundles"`
	Downloaded     int   `json:"downloaded"`
	Cached         int   `json:"already_cached"`
	DownloadedByte int64 `json:"downloaded_bytes"`
	// Resumed counts bundles that an earlier, interrupted run of this release
	// had already extracted: they were neither downloaded nor extracted again.
	Resumed int `json:"resumed_bundles"`

	DownloadFailed     []Failure    `json:"download_failed,omitempty"`
	BaselineDownloaded int          `json:"baseline_downloaded,omitempty"`
	BaselineCached     int          `json:"baseline_cached,omitempty"`
	BaselineBytes      int64        `json:"baseline_downloaded_bytes,omitempty"`
	AssetDiffs         []*AssetDiff `json:"asset_diffs,omitempty"`

	Extracted struct {
		Bundles  int `json:"bundles"`
		Empty    int `json:"bundles_with_nothing_to_extract"`
		Textures int `json:"textures"`
		Texts    int `json:"text_assets"`
		Dumped   int `json:"other_objects"`
		Skipped  int `json:"objects_skipped"`
		Filtered int `json:"objects_excluded"`
	} `json:"extracted"`
	ExtractFailed []Failure `json:"extract_failed,omitempty"`

	Notes []string `json:"notes,omitempty"`
}

func (r *Report) note(format string, a ...any) { r.Notes = append(r.Notes, fmt.Sprintf(format, a...)) }

// OK reports whether the run completed without download failures.
func (r *Report) OK() bool { return len(r.DownloadFailed) == 0 }

// Summary renders the final human-readable overview.
func (r *Report) Summary() string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	switch {
	case r.UpToDate:
		line("Catalog unchanged (hash matches); nothing to do.")
		return b.String()
	case r.Bootstrap:
		line("First run: baseline saved, no bundles downloaded.")
		return b.String()
	}
	line("Version:     %s", r.Label)
	line("Bundle diff: +%d  -%d  ~%d", r.Diff.BundlesAdded, r.Diff.BundlesRemoved, r.Diff.BundlesChanged)
	line("Planned:     %d (%s)", r.Planned, humanBytes(r.PlannedBytes))
	if r.NotSelected > 0 {
		line("Excluded by selection: %d bundle(s), %s; not downloaded", r.NotSelected, humanBytes(r.NotSelectedBytes))
	}
	if r.SkippedLocal > 0 {
		line("Skipped:     %d bundle(s) shipped with the game (requires -game-dir)", r.SkippedLocal)
	}
	if r.DryRun {
		line("Dry-run: no bundles downloaded; state unchanged.")
		return b.String()
	}
	line("Downloaded:  %d (%s), cached: %d", r.Downloaded, humanBytes(r.DownloadedByte), r.Cached)
	if r.BaselineDownloaded+r.BaselineCached > 0 {
		line("For comparison: %d old bundle(s) (%s), cached: %d", r.BaselineDownloaded, humanBytes(r.BaselineBytes), r.BaselineCached)
	}
	for _, d := range r.AssetDiffs {
		line("Files %-24s +%d ~%d -%d, unchanged: %d", d.Folder, len(d.Added), len(d.Changed), len(d.Removed), d.Unchanged)
	}
	if r.Resumed > 0 {
		line("Resumed:     %d bundle(s) already extracted in an interrupted run", r.Resumed)
	}
	if len(r.DownloadFailed) > 0 {
		line("Download failures: %d (see report.json)", len(r.DownloadFailed))
	}
	if r.Extracted.Bundles+len(r.ExtractFailed) > 0 {
		line("Extracted:   %d bundle(s): %d textures, %d texts, %d other objects; skipped: %d; excluded by selection: %d",
			r.Extracted.Bundles, r.Extracted.Textures, r.Extracted.Texts, r.Extracted.Dumped, r.Extracted.Skipped, r.Extracted.Filtered)
	}
	if len(r.ExtractFailed) > 0 {
		line("Extraction failures: %d bundle(s) (see report.json)", len(r.ExtractFailed))
	}
	if r.StateAdvanced {
		line("Baseline advanced.")
	} else {
		line("Baseline NOT advanced; the next run will retry this work.")
	}
	for _, n := range r.Notes {
		line("Note: %s", n)
	}
	return b.String()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// addExtract folds one bundle's extraction result into the report.
func (r *Report) addExtract(st extractor.Stats) {
	r.Extracted.Textures += st.Textures
	r.Extracted.Texts += st.Texts
	r.Extracted.Dumped += st.Dumped
	r.Extracted.Skipped += st.Skipped
	r.Extracted.Filtered += st.Filtered
	if st.Extracted() > 0 {
		r.Extracted.Bundles++
	} else {
		r.Extracted.Empty++
	}
}
