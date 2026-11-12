// Package catalogdiff compares two decoded Addressables catalogs and reports
// which bundles and keys were added, removed or changed between them.
//
// Bundles are matched by bundle_name (a stable identifier); hash, crc and
// size are the fields that tell you the content behind a name has changed.
// Numeric location ids are deliberately NOT used as identity: they are
// rebuilt on every catalog build and mean nothing across versions.
package catalogdiff

import (
	"fmt"
	"sort"
	"strings"

	"limbus-asset-extractor/internal/catalog"
)

// ValueChange is an old/new pair.
type ValueChange[T any] struct {
	Old T `json:"old"`
	New T `json:"new"`
}

// BundleChange lists which attributes of a same-named bundle differ. Note it
// carries no path: look the bundle up in the NEW catalog to get its URL.
type BundleChange struct {
	BundleName string               `json:"bundle_name"`
	Hash       *ValueChange[string] `json:"hash,omitempty"`
	CRC        *ValueChange[uint32] `json:"crc,omitempty"`
	SizeBytes  *ValueChange[uint32] `json:"size_bytes,omitempty"`
}

// KeyChange is a catalog key whose resolved targets differ.
type KeyChange struct {
	Key string   `json:"key"`
	Old []string `json:"old"`
	New []string `json:"new"`
}

// Diff contains added, removed, and changed catalog bundles and keys.
type Diff struct {
	OldFile string `json:"old_file"`
	NewFile string `json:"new_file"`

	Catalog struct {
		BuildResultHash *ValueChange[string] `json:"build_result_hash,omitempty"`
	} `json:"catalog"`

	Bundles struct {
		Added   []catalog.Bundle `json:"added"`
		Removed []catalog.Bundle `json:"removed"`
		Changed []BundleChange   `json:"changed"`
	} `json:"bundles"`

	Keys struct {
		Added   []string    `json:"added"`
		Removed []string    `json:"removed"`
		Changed []KeyChange `json:"changed"`
	} `json:"keys"`
}

// Compare diffs old against new. oldName/newName are recorded verbatim in
// the report (pass logical names, not absolute paths with usernames in them).
func Compare(old, new *catalog.Catalog, oldName, newName string) *Diff {
	d := &Diff{OldFile: oldName, NewFile: newName}

	if old.BuildHash() != new.BuildHash() {
		d.Catalog.BuildResultHash = &ValueChange[string]{Old: old.BuildHash(), New: new.BuildHash()}
	}

	oldBundles := indexBundles(old)
	newBundles := indexBundles(new)

	for name, b := range newBundles {
		if _, ok := oldBundles[name]; !ok {
			d.Bundles.Added = append(d.Bundles.Added, b)
		}
	}
	for name, b := range oldBundles {
		if _, ok := newBundles[name]; !ok {
			d.Bundles.Removed = append(d.Bundles.Removed, b)
		}
	}

	var common []string
	for name := range oldBundles {
		if _, ok := newBundles[name]; ok {
			common = append(common, name)
		}
	}
	sort.Strings(common)

	for _, name := range common {
		o, n := oldBundles[name], newBundles[name]
		ch := BundleChange{BundleName: name}
		if o.Hash != n.Hash {
			ch.Hash = &ValueChange[string]{Old: o.Hash, New: n.Hash}
		}
		if o.Crc != n.Crc {
			ch.CRC = &ValueChange[uint32]{Old: o.Crc, New: n.Crc}
		}
		if o.SizeBytes != n.SizeBytes {
			ch.SizeBytes = &ValueChange[uint32]{Old: o.SizeBytes, New: n.SizeBytes}
		}
		if ch.Hash != nil || ch.CRC != nil || ch.SizeBytes != nil {
			d.Bundles.Changed = append(d.Bundles.Changed, ch)
		}
	}

	sort.Slice(d.Bundles.Added, func(i, j int) bool { return d.Bundles.Added[i].Name() < d.Bundles.Added[j].Name() })
	sort.Slice(d.Bundles.Removed, func(i, j int) bool { return d.Bundles.Removed[i].Name() < d.Bundles.Removed[j].Name() })

	oldKeys := keyTargets(old)
	newKeys := keyTargets(new)

	for key := range newKeys {
		if _, ok := oldKeys[key]; !ok {
			d.Keys.Added = append(d.Keys.Added, key)
		}
	}
	for key := range oldKeys {
		if _, ok := newKeys[key]; !ok {
			d.Keys.Removed = append(d.Keys.Removed, key)
		}
	}
	for key, oldT := range oldKeys {
		if newT, ok := newKeys[key]; ok && !sameStrings(oldT, newT) {
			d.Keys.Changed = append(d.Keys.Changed, KeyChange{Key: key, Old: oldT, New: newT})
		}
	}
	sort.Strings(d.Keys.Added)
	sort.Strings(d.Keys.Removed)
	sort.Slice(d.Keys.Changed, func(i, j int) bool { return d.Keys.Changed[i].Key < d.Keys.Changed[j].Key })

	return d
}

// Summary formats catalog counts and change totals.
func (d *Diff) Summary(old, new *catalog.Catalog) string {
	var b strings.Builder
	fmt.Fprintf(&b, "build_result_hash: %s -> %s\n", old.BuildHash(), new.BuildHash())
	fmt.Fprintf(&b, "bundles: +%d  -%d  ~%d\n", len(d.Bundles.Added), len(d.Bundles.Removed), len(d.Bundles.Changed))
	fmt.Fprintf(&b, "keys:    +%d  -%d  ~%d\n", len(d.Keys.Added), len(d.Keys.Removed), len(d.Keys.Changed))
	fmt.Fprintf(&b, "old: keys=%d locations=%d bundles=%d\n", old.Stats.Keys, old.Stats.Locations, old.Stats.Bundles)
	fmt.Fprintf(&b, "new: keys=%d locations=%d bundles=%d", new.Stats.Keys, new.Stats.Locations, new.Stats.Bundles)
	return b.String()
}

func indexBundles(c *catalog.Catalog) map[string]catalog.Bundle {
	m := make(map[string]catalog.Bundle, len(c.Bundles))
	for _, b := range c.Bundles {
		m[b.Name()] = b
	}
	return m
}

// locationIdentity is the version-stable identity of a location.
func locationIdentity(loc catalog.Location) string {
	// Bundle locations keep their bundle_name while hash/path change.
	if loc.BundleInfo != nil && loc.BundleInfo.BundleName != nil && *loc.BundleInfo.BundleName != "" {
		return "bundle:" + *loc.BundleInfo.BundleName
	}
	if loc.PrimaryKey != "" {
		return "key:" + loc.PrimaryKey
	}
	return "internal:" + loc.InternalID
}

func keyTargets(c *catalog.Catalog) map[string][]string {
	locs := make(map[int]catalog.Location, len(c.Locations))
	for _, l := range c.Locations {
		locs[l.ID] = l
	}

	out := make(map[string][]string, len(c.Keys))
	for key, ids := range c.Keys {
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			loc, ok := locs[id]
			if !ok {
				// Keep dangling references visible instead of dropping them.
				seen[fmt.Sprintf("<missing-location:%d>", id)] = struct{}{}
				continue
			}
			seen[locationIdentity(loc)] = struct{}{}
		}
		vals := make([]string, 0, len(seen))
		for v := range seen {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		out[key] = vals
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
