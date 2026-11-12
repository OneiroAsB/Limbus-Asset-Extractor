package pipeline

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Default bundle and asset selection rules.
//
// Bundle names are matched by filename stem, without the content hash or
// "_assets_all". For example: e20211_assets_all_<hash>.bundle -> "e20211".
// Matching is case-insensitive.

// Include a bundle only if its stem matches AT LEAST ONE of these patterns.
var selectBundles = []string{
	`^p\d+`, // identities: p10216, p11013, ...
	`^e\d+`, // EGO:           e20211, e20510, ...
	`^s8_`,  // season 8 (other seasons such as s4_, s5_, s7_ are excluded)
}

// ...and does NOT match any of these patterns (checked after the include list).
var rejectBundles = []string{
	`\.mp4$`, // video bundles: no files are extracted from them (only Texture2D/TextAsset are supported)
}

// Unwanted objects inside bundles. Skipped during extraction (textures are
// not even decoded). Rules match object names using case-insensitive
// substrings, so "fx" matches both "vfx_hit" and "Hit_FX_01".
var excludeObjectNames = []string{
	"fx", "vfx", "mask", "atlas", "sactx",
}

// Skip TextAssets that would be saved with one of these extensions.
var excludeTextExts = []string{".json"}

// Selection says which bundles the run takes and which objects inside them it
// leaves out.
type Selection struct {
	include         []*regexp.Regexp
	reject          []*regexp.Regexp
	ExcludeNames    []string
	ExcludeTextExts []string
}

// DefaultSelection returns the hardcoded selection above.
func DefaultSelection() *Selection {
	return &Selection{
		include:         mustCompileAll(selectBundles),
		reject:          mustCompileAll(rejectBundles),
		ExcludeNames:    append([]string(nil), excludeObjectNames...),
		ExcludeTextExts: append([]string(nil), excludeTextExts...),
	}
}

func mustCompileAll(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(`(?i)` + p)
	}
	return out
}

// WantsBundle reports whether a bundle with this stem is taken.
func (s *Selection) WantsBundle(stem string) bool {
	if s == nil {
		return true
	}
	ok := false
	for _, re := range s.include {
		if re.MatchString(stem) {
			ok = true
			break
		}
	}
	if !ok {
		return false
	}
	for _, re := range s.reject {
		if re.MatchString(stem) {
			return false
		}
	}
	return true
}

// describe is the one-line description shown in the log.
func (s *Selection) describe() string {
	return fmt.Sprintf("bundles %s; exclude contents: names containing %s, files %s",
		strings.Join(selectBundles, " | "),
		strings.Join(s.ExcludeNames, ", "),
		strings.Join(s.ExcludeTextExts, ", "))
}

// family names the group a stem belongs to, for the "what was left out" summary:
// the part before the first underscore ("s4_240411_enemy_sd" -> "s4").
func family(stem string) string {
	f := stem
	if i := strings.IndexByte(stem, '_'); i > 0 {
		f = stem[:i]
	}
	if f != "" && strings.Trim(f, "0123456789") == "" {
		return "(numeric)"
	}
	return f
}

// summarizeFamilies renders "s4 ×12 (210.0 MiB), s7 ×5 (40.1 MiB), ..." for the
// biggest groups of the given tasks.
func summarizeFamilies(tasks []*Task, top int) string {
	type agg struct {
		name  string
		count int
		bytes int64
	}
	byName := map[string]*agg{}
	for _, t := range tasks {
		f := family(t.Stem)
		a := byName[f]
		if a == nil {
			a = &agg{name: f}
			byName[f] = a
		}
		a.count++
		a.bytes += int64(t.Size)
	}
	all := make([]*agg, 0, len(byName))
	for _, a := range byName {
		all = append(all, a)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].bytes != all[j].bytes {
			return all[i].bytes > all[j].bytes
		}
		return all[i].name < all[j].name
	})
	parts := make([]string, 0, top+1)
	for i, a := range all {
		if i == top {
			parts = append(parts, fmt.Sprintf("and %d more", len(all)-top))
			break
		}
		parts = append(parts, fmt.Sprintf("%s ×%d (%s)", a.name, a.count, humanBytes(a.bytes)))
	}
	return strings.Join(parts, ", ")
}
