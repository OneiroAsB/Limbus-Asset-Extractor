package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"limbus-asset-extractor/internal/extractor"
)

// AssetDiff describes differences in extracted content, not Unity container
// metadata. PNGs are re-encoded by the same extractor in both versions, so
// bundle compression, object order and unrelated metadata do not affect them.
type AssetDiff struct {
	Folder    string   `json:"folder"`
	OldBundle string   `json:"old_bundle,omitempty"`
	NewBundle string   `json:"new_bundle"`
	Added     []string `json:"added"`
	Changed   []string `json:"changed"`
	Removed   []string `json:"removed"`
	Unchanged int      `json:"unchanged"`
}

func assetDiffPath(l layout, label, stem string) string {
	return filepath.Join(l.runDir(label), "asset-diff", stem+".json")
}

func beforeDir(l layout, label, stem string) string {
	return filepath.Join(l.root, "before", label, stem)
}

func fileHashes(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("not a regular file: %s", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return out, err
}

// compareAssetDirs prunes unchanged files from after and copies only changed
// and removed originals to before. old remains untouched.
func compareAssetDirs(old, after, before string) (*AssetDiff, error) {
	previous := map[string]string{}
	var err error
	if old != "" {
		previous, err = fileHashes(old)
		if err != nil {
			return nil, err
		}
	}
	current, err := fileHashes(after)
	if err != nil {
		return nil, err
	}
	d := &AssetDiff{Added: []string{}, Changed: []string{}, Removed: []string{}}
	for file, hash := range current {
		if prev, ok := previous[file]; !ok {
			d.Added = append(d.Added, file)
		} else if prev != hash {
			d.Changed = append(d.Changed, file)
		} else {
			d.Unchanged++
			if err := os.Remove(filepath.Join(after, filepath.FromSlash(file))); err != nil {
				return nil, err
			}
		}
	}
	for file := range previous {
		if _, ok := current[file]; !ok {
			d.Removed = append(d.Removed, file)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Changed)
	sort.Strings(d.Removed)
	for _, files := range [][]string{d.Changed, d.Removed} {
		for _, file := range files {
			if err := copyFileAtomic(filepath.Join(old, filepath.FromSlash(file)), filepath.Join(before, filepath.FromSlash(file))); err != nil {
				return nil, err
			}
		}
	}
	return d, nil
}

// publishAssetDir replaces one generated bundle directory. A bundle with no
// differences leaves no output directory, even if it held files on an old run.
func publishAssetDir(src, dst string, count int) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func (p *Pipeline) extractAssetDiff(t *Task, l layout, label string) (extractor.Stats, *AssetDiff, error) {
	tmp, err := os.MkdirTemp(l.runDir(label), ".asset-diff-")
	if err != nil {
		return extractor.Stats{}, nil, err
	}
	defer os.RemoveAll(tmp)
	after, before := filepath.Join(tmp, "after"), filepath.Join(tmp, "before")
	opt := p.Cfg.Extract
	var detail bytes.Buffer
	if p.Cfg.Verbose {
		opt.Log = &detail
		fmt.Fprintf(&detail, "new version %s:\n", t.File)
		defer func() { p.logf("%s", strings.TrimRight(detail.String(), "\n")) }()
	}
	st, err := extractor.ExtractFile(t.path(), after, opt)
	if err != nil {
		return st, nil, err
	}
	if st.Skipped > 0 {
		return st, nil, fmt.Errorf("comparison incomplete: %d objects could not be extracted from the new version", st.Skipped)
	}
	old := ""
	if t.Previous != nil {
		old = filepath.Join(tmp, "old")
		if p.Cfg.Verbose {
			fmt.Fprintf(&detail, "old version %s:\n", t.Previous.File)
		}
		previous, err := extractor.ExtractFile(t.Previous.path(), old, opt)
		if err != nil {
			return st, nil, fmt.Errorf("old bundle: %w", err)
		}
		if previous.Skipped > 0 {
			return st, nil, fmt.Errorf("comparison incomplete: %d objects could not be extracted from the old version", previous.Skipped)
		}
	}
	d, err := compareAssetDirs(old, after, before)
	if err != nil {
		return st, nil, err
	}
	d.Folder, d.NewBundle = t.Stem, t.File
	if t.Previous != nil {
		d.OldBundle = t.Previous.File
	}
	if err := publishAssetDir(after, filepath.Join(l.outputDir(label), t.Stem), len(d.Added)+len(d.Changed)); err != nil {
		return st, nil, err
	}
	if err := publishAssetDir(before, beforeDir(l, label, t.Stem), len(d.Changed)+len(d.Removed)); err != nil {
		return st, nil, err
	}
	if err := writeJSON(assetDiffPath(l, label, t.Stem), d); err != nil {
		return st, nil, err
	}
	return st, d, nil
}

func loadAssetDiff(t *Task, l layout, label string) *AssetDiff {
	raw, err := os.ReadFile(assetDiffPath(l, label, t.Stem))
	if err != nil {
		return nil
	}
	var d AssetDiff
	if json.Unmarshal(raw, &d) != nil || d.NewBundle != t.File {
		return nil
	}
	oldFile := ""
	if t.Previous != nil {
		oldFile = t.Previous.File
	}
	if d.OldBundle != oldFile {
		return nil
	}
	for _, files := range []struct {
		root  string
		names []string
	}{
		{filepath.Join(l.outputDir(label), t.Stem), d.Added},
		{filepath.Join(l.outputDir(label), t.Stem), d.Changed},
		{beforeDir(l, label, t.Stem), d.Changed},
		{beforeDir(l, label, t.Stem), d.Removed},
	} {
		for _, file := range files.names {
			if !fileExists(filepath.Join(files.root, filepath.FromSlash(file))) {
				return nil
			}
		}
	}
	return &d
}
