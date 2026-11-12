// Package pipeline strings the individual modules into one run:
//
//	resolve version -> catalog_S1.hash (changed?) -> catalog_S1.bin
//	  -> unpack -> diff against the previous catalog -> download new and
//	  changed bundles -> extract them -> remember this catalog as the baseline
package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"limbus-asset-extractor/internal/catalog"
	"limbus-asset-extractor/internal/catalogdiff"
	"limbus-asset-extractor/internal/cdn"
	"limbus-asset-extractor/internal/extractor"
	"limbus-asset-extractor/internal/resolver"
)

// Pipeline runs the whole flow. Use New.
type Pipeline struct {
	Cfg    Config
	Client *cdn.Client
	Log    *log.Logger

	aaDir         string // StreamingAssets/aa of the local install, if any
	assetBaseline string // identifies the comparison used by the resume journal
}

// New checks cfg and returns a ready pipeline. All configuration mistakes
// that can be found without the network are reported here, before anything
// is downloaded.
func New(cfg Config, client *cdn.Client, logger *log.Logger) (*Pipeline, error) {
	cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := extractor.ValidateOptions(cfg.Extract); err != nil {
		return nil, err
	}
	if cfg.Select != nil {
		cfg.Extract.ExcludeNames = append(append([]string(nil), cfg.Extract.ExcludeNames...), cfg.Select.ExcludeNames...)
		cfg.Extract.ExcludeTextExts = append(append([]string(nil), cfg.Extract.ExcludeTextExts...), cfg.Select.ExcludeTextExts...)
	}
	p := &Pipeline{Cfg: cfg, Client: client, Log: logger}
	if cfg.GameDir != "" {
		aa, err := resolver.AADir(cfg.GameDir)
		if err != nil {
			return nil, err
		}
		p.aaDir = aa
	}
	return p, nil
}

func (p *Pipeline) logf(format string, a ...any) {
	if p.Log != nil {
		p.Log.Printf(format, a...)
	}
}

// Run executes the pipeline and returns a non-nil report. Download failures
// and incomplete asset comparisons prevent baseline advancement. Ordinary
// extraction failures are recorded without failing the run.
func (p *Pipeline) Run(ctx context.Context) (*Report, error) {
	rep := &Report{StartedAt: time.Now()}
	l := layout{p.Cfg.WorkDir}

	var label, hash, binPath string
	if p.Cfg.CatalogPath != "" {
		label, binPath = p.Cfg.Label, p.Cfg.CatalogPath
		p.logf("[1/5] Catalog: local file %s", binPath)
	} else {
		version, err := p.Cfg.Resolver.Resolve(ctx)
		if err != nil {
			return rep, fmt.Errorf("resolving version (%s): %w", p.Cfg.Resolver.Name(), err)
		}
		label = p.Cfg.Label
		if label == "" {
			label = version
		}
		p.logf("[1/5] Version: %s (source: %s)", version, p.Cfg.Resolver.Name())

		base := p.Cfg.CDNBase + "/" + version
		hash, err = p.Client.GetText(ctx, base+"/catalog_S1.hash")
		if err != nil {
			return rep, fmt.Errorf("catalog_S1.hash: %w", err)
		}
		rep.CatalogHash = hash

		st, err := loadState(l)
		if err != nil {
			return rep, err
		}
		if st != nil {
			rep.PreviousHash = st.Hash
			if st.Hash != "" && st.Hash == hash && fileExists(l.baselineFile()) && !p.Cfg.Full && p.Cfg.BaselinePath == "" {
				p.logf("      hash %s has not changed since %s; nothing to do", hash, st.ProcessedAt.Format("2006-01-02 15:04"))
				rep.UpToDate = true
				rep.FinishedAt = time.Now()
				return rep, nil
			}
			p.logf("      hash changed: %s -> %s", orDash(st.Hash), hash)
		} else {
			p.logf("      hash: %s (no saved state)", hash)
		}

		runDir := l.runDir(label)
		binPath = filepath.Join(runDir, "catalog_S1.bin")
		p.logf("      downloading catalog_S1.bin")
		if _, err := p.Client.Download(ctx, base+"/catalog_S1.bin", binPath, 0); err != nil {
			return rep, fmt.Errorf("catalog_S1.bin: %w", err)
		}
		if err := writeFileAtomic(filepath.Join(runDir, "catalog_S1.hash"), []byte(hash+"\n")); err != nil {
			return rep, err
		}
	}

	cat, err := catalog.DecodeFile(binPath)
	if err != nil {
		return rep, fmt.Errorf("decoding catalog %s: %w", binPath, err)
	}
	if label == "" {
		if label = dominantRelease(cat); label == "" {
			label = "local-" + time.Now().Format("20060102-150405")
		}
	}
	return p.process(ctx, rep, l, label, hash, cat)
}

func (p *Pipeline) process(ctx context.Context, rep *Report, l layout, label, hash string, cat *catalog.Catalog) (*Report, error) {
	rep.Label, rep.CatalogHash = label, hash
	defer func() { rep.FinishedAt = time.Now() }()

	runDir := l.runDir(label)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return rep, err
	}
	finish := func(err error) (*Report, error) {
		if werr := writeJSON(filepath.Join(runDir, "report.json"), rep); werr != nil && err == nil {
			err = werr
		}
		return rep, err
	}

	unpacked := filepath.Join(runDir, "catalog_S1_unpacked.json")
	if err := cat.Save(unpacked); err != nil {
		return finish(err)
	}
	p.logf("[2/5] Catalog decoded: %d bundles, %d locations, %d keys", cat.Stats.Bundles, cat.Stats.Locations, cat.Stats.Keys)

	var old *catalog.Catalog
	var oldName string
	if p.Cfg.Full {
		// -full ignores any baseline, saved or given: every bundle counts as new.
		old, oldName = &catalog.Catalog{Keys: map[string][]int{}}, "(empty, -full)"
	} else {
		var err error
		if old, oldName, err = p.loadBaseline(l); err != nil {
			return finish(err)
		}
		if old == nil {
			if p.Cfg.AssetDiff {
				return finish(fmt.Errorf("-asset-diff: a previous catalog is required; use -baseline old_unpacked.json"))
			}
			rep.Bootstrap = true
			if !p.Cfg.DryRun {
				if err := p.advanceState(l, label, hash, cat, unpacked); err != nil {
					return finish(err)
				}
				rep.StateAdvanced = true
			}
			p.logf("[3/5] No previous catalog to compare against. Recording the current catalog as the baseline.")
			p.logf("      To extract assets now, use -full [-bundle-filter text] or -baseline old_unpacked.json")
			return finish(nil)
		}
	}

	d := catalogdiff.Compare(old, cat, oldName, filepath.ToSlash(filepath.Join("runs", label, "catalog_S1_unpacked.json")))
	rep.Diff.BundlesAdded, rep.Diff.BundlesRemoved, rep.Diff.BundlesChanged = len(d.Bundles.Added), len(d.Bundles.Removed), len(d.Bundles.Changed)
	rep.Diff.KeysAdded, rep.Diff.KeysRemoved, rep.Diff.KeysChanged = len(d.Keys.Added), len(d.Keys.Removed), len(d.Keys.Changed)
	if err := writeJSON(filepath.Join(runDir, "diff.json"), d); err != nil {
		return finish(err)
	}
	p.logf("[3/5] Diff against %s: bundles +%d -%d ~%d, keys +%d -%d ~%d", oldName,
		rep.Diff.BundlesAdded, rep.Diff.BundlesRemoved, rep.Diff.BundlesChanged,
		rep.Diff.KeysAdded, rep.Diff.KeysRemoved, rep.Diff.KeysChanged)

	pl := buildPlan(d, cat, planOptions{filter: strings.ToLower(p.Cfg.BundleFilter), sel: p.Cfg.Select, max: p.Cfg.MaxBundles, aaDir: p.aaDir})
	if p.Cfg.AssetDiff {
		data, _ := json.Marshal(old.Bundles)
		p.assetBaseline = fmt.Sprintf("%x", sha256.Sum256(data))
		oldByName := make(map[string]catalog.Bundle, len(old.Bundles))
		for _, b := range old.Bundles {
			oldByName[b.Name()] = b
		}
		for _, t := range pl.tasks {
			if t.Reason != "changed" {
				continue
			}
			b, ok := oldByName[t.Name]
			if !ok {
				return finish(fmt.Errorf("-asset-diff: baseline does not contain %s", t.Name))
			}
			previousDiff := &catalogdiff.Diff{}
			previousDiff.Bundles.Added = []catalog.Bundle{b}
			previous := buildPlan(previousDiff, old, planOptions{aaDir: p.aaDir})
			t.Previous = previous.tasks[0]
		}
	}
	tasks, truncated := pl.tasks, pl.truncated
	rep.Truncated = truncated
	for _, t := range pl.deselected {
		rep.NotSelected++
		rep.NotSelectedBytes += int64(t.Size)
		rep.NotSelectedList = append(rep.NotSelectedList, PlanEntry{Bundle: t.File, Folder: t.Stem, Reason: t.Reason, Size: t.Size})
	}
	var work []*Task
	for _, t := range tasks {
		if t.Skip != "" {
			rep.SkippedLocal++
			continue
		}
		work = append(work, t)
		rep.Plan = append(rep.Plan, PlanEntry{Bundle: t.File, Folder: t.Stem, Reason: t.Reason, Size: t.Size})
		if t.needsDownload() {
			rep.PlannedBytes += int64(t.Size)
		}
		if t.Previous != nil && t.Previous.needsDownload() {
			rep.PlannedBytes += int64(t.Previous.Size)
		}
	}
	rep.Planned = len(work)
	p.logf("[4/5] Plan: %d bundle(s), %s%s", rep.Planned, humanBytes(rep.PlannedBytes), planSuffix(rep, p.Cfg))
	if p.Cfg.Select != nil {
		p.logf("      selection: %s", p.Cfg.Select.describe())
		if rep.NotSelected > 0 {
			p.logf("      excluded from download: %d bundle(s), %s - %s", rep.NotSelected, humanBytes(rep.NotSelectedBytes), summarizeFamilies(pl.deselected, 8))
		}
	}
	if p.Cfg.DryRun {
		rep.DryRun = true
		biggest := append([]*Task(nil), work...)
		sort.SliceStable(biggest, func(i, j int) bool { return biggest[i].Size > biggest[j].Size })
		show := min(10, len(biggest))
		if p.Cfg.Verbose {
			show = len(biggest)
		}
		for _, t := range biggest[:show] {
			p.logf("      %-8s %-40s %10s", t.Reason, t.Stem, humanBytes(int64(t.Size)))
		}
		if show < len(biggest) {
			p.logf("      ... and %d more (largest shown; see %s for the full list, or use -v)",
				len(biggest)-show, filepath.Join(runDir, "report.json"))
		}
		if p.Cfg.Verbose {
			for _, t := range pl.deselected {
				p.logf("      excluded %-40s %10s", t.Stem, humanBytes(int64(t.Size)))
			}
		}
		return finish(nil)
	}

	if p.Cfg.NoExtract {
		p.logf("[5/5] Downloading (%d workers); extraction disabled (-no-extract)", p.Cfg.DownloadWorkers)
	} else {
		p.logf("[5/5] Streaming downloads and extraction (%d + %d workers): each downloaded bundle is extracted immediately%s -> %s",
			p.Cfg.DownloadWorkers, p.Cfg.ExtractWorkers, ifTrue(p.Cfg.DeleteBundles, " and deleted"), l.outputDir(label))
	}
	p.transfer(ctx, work, l, label, rep)
	if err := ctx.Err(); err != nil {
		rep.note("interrupted by user")
		return finish(err)
	}

	switch {
	case len(rep.DownloadFailed) > 0:
		rep.note("some downloads failed; the baseline was not advanced")
	case p.Cfg.AssetDiff && len(rep.ExtractFailed) > 0:
		rep.note("file comparison incomplete; the baseline was not advanced")
	case truncated:
		rep.note("-max-bundles applied; the baseline was not advanced, so remaining bundles will appear in the next run")
	default:
		if err := p.advanceState(l, label, hash, cat, unpacked); err != nil {
			return finish(err)
		}
		rep.StateAdvanced = true
	}

	if len(rep.DownloadFailed) > 0 {
		return finish(fmt.Errorf("%d of %d bundles could not be downloaded (details: %s)", len(rep.DownloadFailed), rep.Planned, filepath.Join(runDir, "report.json")))
	}
	if p.Cfg.AssetDiff && len(rep.ExtractFailed) > 0 {
		return finish(fmt.Errorf("%d bundles could not be compared (see report.json)", len(rep.ExtractFailed)))
	}
	return finish(nil)
}

func planSuffix(rep *Report, cfg Config) string {
	var parts []string
	if rep.SkippedLocal > 0 {
		parts = append(parts, fmt.Sprintf("%d shipped with the game; skipped", rep.SkippedLocal))
	}
	if rep.Truncated {
		parts = append(parts, fmt.Sprintf("limited by -max-bundles=%d", cfg.MaxBundles))
	}
	if cfg.BundleFilter != "" {
		parts = append(parts, fmt.Sprintf("filter %q", cfg.BundleFilter))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// loadBaseline returns the catalog to diff against: -baseline if given,
// otherwise the one saved by the last successful run. (nil, "", nil) means
// there is none.
func (p *Pipeline) loadBaseline(l layout) (*catalog.Catalog, string, error) {
	path, name := p.Cfg.BaselinePath, ""
	if path != "" {
		name = filepath.Base(path)
		if fileExists(l.baselineFile()) {
			p.logf("      warning: -baseline %s overrides the saved state. "+
				"Use it for a one-off comparison; remove it from regular runs, or every diff will be against that file", path)
		}
	} else if fileExists(l.baselineFile()) {
		path, name = l.baselineFile(), "state/catalog_S1_unpacked.json"
	} else {
		return nil, "", nil
	}
	c, err := catalog.Load(path)
	if err != nil {
		return nil, "", fmt.Errorf("baseline: %w", err)
	}
	return c, name, nil
}

// advanceState replaces the baseline catalog, then updates last.json.
// The two files are written separately.
func (p *Pipeline) advanceState(l layout, label, hash string, cat *catalog.Catalog, unpacked string) error {
	if err := copyFileAtomic(unpacked, l.baselineFile()); err != nil {
		return err
	}
	return writeJSON(l.lastFile(), State{Label: label, Hash: hash, BuildHash: cat.BuildHash(), ProcessedAt: time.Now().UTC()})
}

// fetchBundle downloads one bundle. Without size verification there is no
// way to tell a finished file from a stale one by size, so an existing file is
// trusted (downloads are only renamed into place when complete).
func (p *Pipeline) fetchBundle(ctx context.Context, t *Task, dest string) (cdn.Result, error) {
	if p.Cfg.VerifySize {
		return p.Client.Download(ctx, t.URL, dest, int64(t.Size))
	}
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		return cdn.Result{Bytes: fi.Size(), Cached: true}, nil
	}
	return p.Client.Download(ctx, t.URL, dest, 0)
}

// extractOne unpacks a bundle into output/<label>/<stem>. It writes to a
// ".partial" sibling first and renames, so an interrupted run never leaves a
// half-filled folder looking finished. Bundles with nothing to extract leave
// no folder behind.
func (p *Pipeline) extractOne(t *Task, outRoot string) (st extractor.Stats, detail string, err error) {
	final := filepath.Join(outRoot, t.Stem)
	tmp := final + ".partial"
	os.RemoveAll(tmp)

	opt := p.Cfg.Extract
	var buf bytes.Buffer
	if p.Cfg.Verbose {
		opt.Log = &buf
	}
	st, err = extractor.ExtractFile(t.path(), tmp, opt)
	if p.Cfg.Verbose {
		detail = buf.String() // one block per bundle, so parallel workers don't interleave
	}
	if err != nil {
		os.RemoveAll(tmp)
		return st, detail, err
	}
	os.RemoveAll(final) // replace what an earlier run of this release left
	if st.Extracted() == 0 {
		os.RemoveAll(tmp)
		return st, detail, nil
	}
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return st, detail, err
	}
	return st, detail, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ifTrue(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}
