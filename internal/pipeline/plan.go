package pipeline

import (
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"limbus-asset-extractor/internal/catalog"
	"limbus-asset-extractor/internal/catalogdiff"
	"limbus-asset-extractor/internal/resolver"
)

// Task is one bundle the run has to bring in and extract.
type Task struct {
	Name   string // catalog bundle_name
	File   string // bundle file name, e.g. 2010121_assets_all_<hash>.bundle
	Stem   string // output folder name: File without the content hash, stable across versions
	Reason string // "added" or "changed"
	Size   uint32 // size promised by the catalog

	URL string // CDN address; "" for bundles that ship with the game
	// Local is where the bundle already is on disk (shipped with the game),
	// or "" when it has to be downloaded.
	Local string
	// Skip, when set, explains why this bundle is left out of the run.
	Skip     string
	Previous *Task // old bundle used by -asset-diff; nil for added bundles

	// cached is the path in the bundle cache once downloaded.
	cached string
}

func (t *Task) path() string {
	if t.Local != "" {
		return t.Local
	}
	return t.cached
}

func (t *Task) needsDownload() bool { return t.Skip == "" && t.Local == "" && t.URL != "" }

type planOptions struct {
	filter string     // lowercase substring of the file name
	sel    *Selection // which bundles are wanted at all; nil = all
	max    int
	aaDir  string // StreamingAssets/aa of a local install, for shipped bundles
}

type plan struct {
	tasks     []*Task // to be processed
	truncated bool    // max cut the list short
	// deselected are bundles the Selection turned down. They are never
	// downloaded; they are kept only to report what was left out.
	deselected []*Task
}

// buildPlan resolves added and changed bundles against the current catalog.
// Filters run before the bundle limit so excluded bundles do not consume it.
func buildPlan(d *catalogdiff.Diff, cur *catalog.Catalog, o planOptions) plan {
	var pl plan
	byName := make(map[string]catalog.Bundle, len(cur.Bundles))
	for _, b := range cur.Bundles {
		byName[b.Name()] = b
	}

	type pick struct {
		b      catalog.Bundle
		reason string
	}
	var picks []pick
	for _, b := range d.Bundles.Added {
		picks = append(picks, pick{b, "added"})
	}
	for _, ch := range d.Bundles.Changed {
		if b, ok := byName[ch.BundleName]; ok {
			picks = append(picks, pick{b, "changed"})
		}
	}
	sort.Slice(picks, func(i, j int) bool { return picks[i].b.Name() < picks[j].b.Name() })

	usedStems := map[string]bool{}
	for _, p := range picks {
		file := bundleFileName(p.b.Path)
		if o.filter != "" && !strings.Contains(strings.ToLower(file), o.filter) {
			continue
		}
		stem, hash := splitBundleFile(file)
		if !o.sel.WantsBundle(stem) {
			pl.deselected = append(pl.deselected, &Task{Name: p.b.Name(), File: file, Stem: stem, Reason: p.reason, Size: p.b.SizeBytes})
			continue
		}
		if usedStems[stem] { // two bundles claiming one stem: disambiguate by hash
			stem += "_" + shortHash(hash, p.b.Hash)
		}
		usedStems[stem] = true

		t := &Task{Name: p.b.Name(), File: file, Stem: stem, Reason: p.reason, Size: p.b.SizeBytes}
		if isRemote(p.b.Path) {
			t.URL = p.b.Path
		} else {
			resolveShipped(t, p.b.Path, o.aaDir)
		}
		pl.tasks = append(pl.tasks, t)
	}

	if o.max > 0 && len(pl.tasks) > o.max {
		pl.tasks, pl.truncated = pl.tasks[:o.max], true
	}
	return pl
}

func isRemote(p string) bool {
	l := strings.ToLower(p)
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://")
}

// resolveShipped handles bundles whose location is a runtime path such as
// "{UnityEngine.AddressableAssets.Addressables.RuntimePath}\<release>\x.bundle":
// they come with the game from Steam and are not on the CDN.
func resolveShipped(t *Task, p, aaDir string) {
	if aaDir == "" {
		t.Skip = "ships with the game; pass -game-dir to include it"
		return
	}
	rel := p
	if strings.HasPrefix(rel, "{") {
		if i := strings.Index(rel, "}"); i >= 0 {
			rel = rel[i+1:]
		}
	}
	rel = strings.TrimLeft(strings.ReplaceAll(rel, `\`, "/"), "/")
	local := filepath.Join(aaDir, filepath.FromSlash(rel))
	// The path comes from a catalog downloaded from the network: never let a
	// ".." in it point outside the game's own folder.
	if inside, err := filepath.Rel(aaDir, local); err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		t.Skip = "path escapes the game folder: " + p
		return
	}
	if !fileExists(local) {
		t.Skip = "ships with the game but is not in " + aaDir
		return
	}
	t.Local = local
}

// bundleFileName returns the last path segment of a URL or runtime path.
func bundleFileName(p string) string {
	if isRemote(p) {
		if u, err := url.Parse(p); err == nil {
			return path.Base(u.Path)
		}
	}
	p = strings.ReplaceAll(p, `\`, "/")
	return p[strings.LastIndex(p, "/")+1:]
}

var bundleFileRe = regexp.MustCompile(`^(.*?)(?:_assets_all)?_([0-9a-f]{32})\.bundle$`)
var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// splitBundleFile turns "2010121_assets_all_<32 hex>.bundle" into ("2010121",
// "<32 hex>"). The hash changes whenever the content does, so folders named
// by the stem line up from one release to the next.
func splitBundleFile(file string) (stem, hash string) {
	if m := bundleFileRe.FindStringSubmatch(file); m != nil {
		stem, hash = m[1], m[2]
	} else {
		stem = strings.TrimSuffix(file, ".bundle")
	}
	stem = unsafeChars.ReplaceAllString(stem, "_")
	if stem == "" || strings.Trim(stem, ".") == "" {
		stem = "bundle"
	}
	return stem, hash
}

func shortHash(fromFile, fromCatalog string) string {
	h := fromFile
	if h == "" {
		h = fromCatalog
	}
	if len(h) > 8 {
		h = h[:8]
	}
	if h == "" {
		h = "dup"
	}
	return h
}

// dominantRelease returns the release folder that most remote bundles of the
// catalog live in, or "" if there is none that looks like one.
func dominantRelease(cat *catalog.Catalog) string {
	count := map[string]int{}
	for _, b := range cat.Bundles {
		if !isRemote(b.Path) {
			continue
		}
		u, err := url.Parse(b.Path)
		if err != nil {
			continue
		}
		if seg := strings.Split(strings.TrimLeft(u.Path, "/"), "/")[0]; resolver.Validate(seg) == nil {
			count[seg]++
		}
	}
	best, n := "", 0
	for seg, c := range count {
		if c > n || (c == n && seg > best) {
			best, n = seg, c
		}
	}
	return best
}
