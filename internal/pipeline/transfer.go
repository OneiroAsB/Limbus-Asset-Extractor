package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"limbus-asset-extractor/internal/extractor"
)

// transfer streams planned bundles through a bounded download/extraction queue.
// Backpressure limits pending downloads when extraction is slower. Completed
// bundles are journaled before their cached files are deleted.
func (p *Pipeline) transfer(ctx context.Context, work []*Task, l layout, label string, rep *Report) {
	extracting := !p.Cfg.NoExtract
	var mu sync.Mutex // guards rep and the counters below

	var done *doneLog
	if extracting {
		done = loadDoneLog(filepath.Join(l.runDir(label), "done.json"), p.optionsFingerprint())
	}

	// Skip what an earlier run already finished.
	var todo []*Task
	for _, t := range work {
		if done != nil {
			st, ok := done.get(t.File)
			complete := st.Extracted() == 0 || dirExists(filepath.Join(l.outputDir(label), t.Stem))
			var delta *AssetDiff
			if p.Cfg.AssetDiff {
				delta = loadAssetDiff(t, l, label)
				complete = delta != nil
			}
			if ok && complete {
				rep.Resumed++
				rep.addExtract(st)
				if delta != nil {
					rep.AssetDiffs = append(rep.AssetDiffs, delta)
				}
				p.logf("      already extracted in an earlier run: %s", t.Stem)
				if p.Cfg.DeleteBundles && t.Local == "" {
					os.Remove(filepath.Join(l.bundlesDir(), t.File)) // leftover of a run that died before cleaning up
					if t.Previous != nil && t.Previous.Local == "" {
						os.Remove(filepath.Join(l.bundlesDir(), t.Previous.File))
					}
				}
				continue
			}
		}
		todo = append(todo, t)
	}
	nDownload := 0
	for _, t := range todo {
		if t.needsDownload() {
			nDownload++
		}
	}

	var ready chan *Task
	var extractWG sync.WaitGroup
	if extracting {
		ready = make(chan *Task, p.Cfg.ExtractWorkers)
		extracted := 0
		for i := 0; i < p.Cfg.ExtractWorkers; i++ {
			extractWG.Add(1)
			go func() {
				defer extractWG.Done()
				for t := range ready {
					if ctx.Err() != nil {
						continue // interrupted: drain the queue without starting new work
					}
					var st extractor.Stats
					var detail string
					var delta *AssetDiff
					var err error
					if p.Cfg.AssetDiff {
						st, delta, err = p.extractAssetDiff(t, l, label)
					} else {
						st, detail, err = p.extractOne(t, l.outputDir(label))
					}

					mu.Lock()
					extracted++
					n := extracted
					if err != nil {
						rep.ExtractFailed = append(rep.ExtractFailed, Failure{Bundle: t.File, Error: err.Error()})
					} else {
						rep.addExtract(st)
						if delta != nil {
							rep.AssetDiffs = append(rep.AssetDiffs, delta)
						}
					}
					mu.Unlock()

					if err != nil {
						p.logf("      [%d/%d] ERROR %s: %v (bundle kept on disk)", n, len(todo), t.File, err)
					} else {
						p.logf("      [%d/%d] %-34s %d textures, %d texts, %d other objects, %d excluded, %d skipped",
							n, len(todo), t.Stem, st.Textures, st.Texts, st.Dumped, st.Filtered, st.Skipped)
						if delta != nil {
							p.logf("             files: +%d ~%d -%d, unchanged: %d", len(delta.Added), len(delta.Changed), len(delta.Removed), delta.Unchanged)
						}
					}
					if detail != "" {
						p.logf("%s", strings.TrimRight(detail, "\n"))
					}
					if err != nil {
						continue // keep the bundle: it can be inspected, and a fixed extractor can retry it
					}
					// Journal first, delete second: a crash in between only leaves a file behind.
					if jerr := done.record(t.File, st); jerr != nil {
						p.logf("      warning: could not write done.json journal: %v", jerr)
						continue
					}
					if p.Cfg.DeleteBundles && t.Local == "" {
						os.Remove(t.cached)
					}
					if p.Cfg.DeleteBundles && t.Previous != nil && t.Previous.Local == "" {
						os.Remove(t.Previous.cached)
					}
				}
			}()
		}
	}

	jobs := make(chan *Task)
	var downloadWG sync.WaitGroup
	downloaded := 0
	for i := 0; i < p.Cfg.DownloadWorkers; i++ {
		downloadWG.Add(1)
		go func() {
			defer downloadWG.Done()
			for t := range jobs {
				if previous := t.Previous; previous != nil {
					var err error
					if previous.Skip != "" {
						err = fmt.Errorf("old bundle: %s", previous.Skip)
					} else if previous.needsDownload() {
						res, downloadErr := p.fetchBundle(ctx, previous, filepath.Join(l.bundlesDir(), previous.File))
						err = downloadErr
						if err == nil {
							previous.cached = filepath.Join(l.bundlesDir(), previous.File)
							mu.Lock()
							if res.Cached {
								rep.BaselineCached++
							} else {
								rep.BaselineDownloaded++
								rep.BaselineBytes += res.Bytes
							}
							mu.Unlock()
							p.logf("      for comparison: %s (%s)", previous.File, humanBytes(res.Bytes))
						}
					}
					if err != nil {
						mu.Lock()
						rep.DownloadFailed = append(rep.DownloadFailed, Failure{Bundle: previous.File, Error: "baseline: " + err.Error()})
						mu.Unlock()
						p.logf("      ERROR downloading old bundle %s: %v", previous.File, err)
						continue
					}
				}
				if t.needsDownload() {
					dest := filepath.Join(l.bundlesDir(), t.File)
					res, err := p.fetchBundle(ctx, t, dest)

					mu.Lock()
					downloaded++
					n := downloaded
					switch {
					case err != nil:
						if ctx.Err() == nil {
							rep.DownloadFailed = append(rep.DownloadFailed, Failure{Bundle: t.File, Error: err.Error()})
						}
					case res.Cached:
						t.cached = dest
						rep.Cached++
					default:
						t.cached = dest
						rep.Downloaded++
						rep.DownloadedByte += res.Bytes
					}
					mu.Unlock()

					switch {
					case err != nil && ctx.Err() == nil:
						p.logf("      [%d/%d] ERROR downloading %s: %v", n, nDownload, t.File, err)
					case err == nil && res.Cached:
						p.logf("      [%d/%d] cached     %s", n, nDownload, t.File)
					case err == nil:
						p.logf("      [%d/%d] downloaded %s (%s%s)", n, nDownload, t.File, humanBytes(res.Bytes), ifTrue(res.Resumed, ", resumed"))
					}
					if err != nil {
						continue
					}
				}
				if ready != nil { // shipped bundles need no download and go straight on
					select {
					case ready <- t:
					case <-ctx.Done():
					}
				}
			}
		}()
	}

feed:
	for _, t := range todo {
		select {
		case jobs <- t:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	downloadWG.Wait()
	if ready != nil {
		close(ready)
		extractWG.Wait()
	}
	sort.Slice(rep.AssetDiffs, func(i, j int) bool { return rep.AssetDiffs[i].Folder < rep.AssetDiffs[j].Folder })
}

// doneFile is runs/<label>/done.json.
type doneFile struct {
	// Options identifies what was extracted and how. If it differs from the
	// current run's (the selection or -only/-filter changed), the old results
	// no longer apply and nothing is skipped.
	Options string                     `json:"options"`
	Bundles map[string]extractor.Stats `json:"bundles"` // bundle file name -> what came out of it
}

type doneLog struct {
	mu   sync.Mutex
	path string
	data doneFile
}

// loadDoneLog starts a new journal if the file is missing, unreadable,
// or was recorded with different extraction options.
func loadDoneLog(path, options string) *doneLog {
	d := &doneLog{path: path, data: doneFile{Options: options, Bundles: map[string]extractor.Stats{}}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return d
	}
	var f doneFile
	if json.Unmarshal(raw, &f) == nil && f.Options == options && f.Bundles != nil {
		d.data = f
	}
	return d
}

func (d *doneLog) get(file string) (extractor.Stats, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.data.Bundles[file]
	return st, ok
}

// record notes a finished bundle and saves the journal right away.
func (d *doneLog) record(file string, st extractor.Stats) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data.Bundles[file] = st
	return writeJSON(d.path, d.data)
}

// optionsFingerprint identifies the extraction options stored in the resume journal.
func (p *Pipeline) optionsFingerprint() string {
	o := p.Cfg.Extract
	sortedLower := func(in []string) string {
		out := make([]string, 0, len(in))
		for _, s := range in {
			out = append(out, strings.ToLower(strings.TrimSpace(s)))
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	parts := []string{
		"only=" + strings.ToLower(o.Only),
		"filter=" + strings.ToLower(o.Filter),
		"exclude=" + sortedLower(o.ExcludeNames),
		"exclude-text=" + sortedLower(o.ExcludeTextExts),
	}
	if p.Cfg.AssetDiff {
		parts = append(parts, "asset-diff=v1:"+p.assetBaseline)
	}
	return strings.Join(parts, ";")
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
