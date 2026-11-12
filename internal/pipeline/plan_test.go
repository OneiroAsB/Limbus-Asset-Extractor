package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"limbus-asset-extractor/internal/catalog"
	"limbus-asset-extractor/internal/catalogdiff"
)

func TestSplitBundleFile(t *testing.T) {
	h := hex32('a')
	for file, want := range map[string][2]string{
		"2010121_assets_all_" + h + ".bundle":        {"2010121", h},
		"static_s1_0_assets_all_" + h + ".bundle":    {"static_s1_0", h},
		"addressablefonts_" + h + ".bundle":          {"addressablefonts", h}, // the 222 bundles without _assets_all
		"weird name (1)_assets_all_" + h + ".bundle": {"weird_name__1_", h},
		"no_hash_here.bundle":                        {"no_hash_here", ""},
		"..bundle":                                   {"bundle", ""},
		"_assets_all_" + h + ".bundle":               {"bundle", h},
	} {
		stem, hash := splitBundleFile(file)
		if stem != want[0] || hash != want[1] {
			t.Errorf("splitBundleFile(%q) = (%q, %q), want (%q, %q)", file, stem, hash, want[0], want[1])
		}
	}
}

func TestBundleFileName(t *testing.T) {
	for in, want := range map[string]string{
		"https://download.limbuscompanycdn.org/s20260917_X/foo_assets_all_" + hex32('a') + ".bundle?x=1": "foo_assets_all_" + hex32('a') + ".bundle",
		`{UnityEngine.AddressableAssets.Addressables.RuntimePath}\s20260917_X\fonts.bundle`:              "fonts.bundle",
		"plain.bundle": "plain.bundle",
	} {
		if got := bundleFileName(in); got != want {
			t.Errorf("bundleFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// planTasks is buildPlan reduced to what most of these tests look at.
func planTasks(d *catalogdiff.Diff, cur *catalog.Catalog, o planOptions) ([]*Task, bool) {
	pl := buildPlan(d, cur, o)
	return pl.tasks, pl.truncated
}

func diffOf(added []catalog.Bundle, changed ...string) *catalogdiff.Diff {
	d := &catalogdiff.Diff{}
	d.Bundles.Added = added
	for _, n := range changed {
		d.Bundles.Changed = append(d.Bundles.Changed, catalogdiff.BundleChange{BundleName: n})
	}
	return d
}

func TestBuildPlanResolvesChangedAgainstNewCatalogAndSorts(t *testing.T) {
	cur := mkCat("N",
		mkBundle("zzz", "https://h/r/"+bundleFile("zeta", 'z'), 10, 'z'),
		mkBundle("mmm", "https://h/r/"+bundleFile("mu", 'm'), 20, 'm'),
	)
	added := []catalog.Bundle{mkBundle("aaa", "https://h/r/"+bundleFile("alpha", 'a'), 30, 'a')}
	tasks, trunc := planTasks(diffOf(added, "zzz", "mmm", "gone"), cur, planOptions{})
	if trunc || len(tasks) != 3 { // "gone" is not in the new catalog: ignored
		t.Fatalf("got %d tasks (truncated=%v)", len(tasks), trunc)
	}
	order := []string{tasks[0].Name, tasks[1].Name, tasks[2].Name}
	if order[0] != "aaa" || order[1] != "mmm" || order[2] != "zzz" {
		t.Errorf("tasks not sorted by bundle name: %v", order)
	}
	if tasks[0].Reason != "added" || tasks[1].Reason != "changed" {
		t.Errorf("reasons: %s, %s", tasks[0].Reason, tasks[1].Reason)
	}
	if tasks[1].URL != "https://h/r/"+bundleFile("mu", 'm') || tasks[1].Size != 20 {
		t.Errorf("changed bundle must take URL and size from the new catalog: %+v", tasks[1])
	}
}

func TestBuildPlanFilterAndMax(t *testing.T) {
	cur := mkCat("N")
	var added []catalog.Bundle
	for i, stem := range []string{"story_1", "story_2", "icons", "story_3"} {
		added = append(added, mkBundle(stem, "https://h/r/"+bundleFile(stem, byte('a'+i)), 1, byte('a'+i)))
	}
	tasks, _ := planTasks(diffOf(added), cur, planOptions{filter: "story"})
	if len(tasks) != 3 {
		t.Fatalf("filter kept %d, want 3", len(tasks))
	}
	tasks, trunc := planTasks(diffOf(added), cur, planOptions{filter: "story", max: 2})
	if len(tasks) != 2 || !trunc {
		t.Fatalf("max: %d tasks truncated=%v", len(tasks), trunc)
	}
	if _, trunc = planTasks(diffOf(added), cur, planOptions{max: 4}); trunc {
		t.Error("max equal to the size of the plan is not a truncation")
	}
}

func TestBuildPlanDisambiguatesStemCollisions(t *testing.T) {
	// "foo_assets_all_<h>" and "foo_<h2>" both reduce to the stem "foo".
	a := mkBundle("a", "https://h/r/"+bundleFile("foo", '1'), 1, '1')
	b := mkBundle("b", "https://h/r/foo_"+hex32('2')+".bundle", 1, '2')
	tasks, _ := planTasks(diffOf([]catalog.Bundle{a, b}), mkCat("N"), planOptions{})
	if tasks[0].Stem == tasks[1].Stem {
		t.Fatalf("both bundles would write into %q", tasks[0].Stem)
	}
	if tasks[0].Stem != "foo" || tasks[1].Stem != "foo_22222222" {
		t.Errorf("stems = %q, %q", tasks[0].Stem, tasks[1].Stem)
	}
}

func TestShippedBundleResolution(t *testing.T) {
	rt := `{UnityEngine.AddressableAssets.Addressables.RuntimePath}\s20260917_X\` + bundleFile("fonts", 'f')
	added := []catalog.Bundle{mkBundle("F", rt, 1, 'f')}

	tasks, _ := planTasks(diffOf(added), mkCat("N"), planOptions{})
	if tasks[0].Skip == "" || tasks[0].URL != "" || tasks[0].needsDownload() {
		t.Errorf("without a game dir the shipped bundle must be skipped: %+v", tasks[0])
	}

	aa := t.TempDir()
	tasks, _ = planTasks(diffOf(added), mkCat("N"), planOptions{aaDir: aa})
	if tasks[0].Skip == "" {
		t.Error("a shipped bundle missing from the install must be skipped, not fail the run")
	}

	dir := filepath.Join(aa, "s20260917_X")
	os.MkdirAll(dir, 0o755)
	want := filepath.Join(dir, bundleFile("fonts", 'f'))
	os.WriteFile(want, []byte("x"), 0o644)
	tasks, _ = planTasks(diffOf(added), mkCat("N"), planOptions{aaDir: aa})
	if tasks[0].Skip != "" || tasks[0].Local != want || tasks[0].needsDownload() {
		t.Errorf("expected local %s, got %+v", want, tasks[0])
	}
}

func TestShippedBundleCannotEscapeTheGameDir(t *testing.T) {
	aa := t.TempDir()
	outside := filepath.Join(filepath.Dir(aa), "secret.bundle")
	os.WriteFile(outside, []byte("x"), 0o644)
	defer os.Remove(outside)

	evil := `{X}\..\secret.bundle`
	tasks, _ := planTasks(diffOf([]catalog.Bundle{mkBundle("E", evil, 1, 'e')}), mkCat("N"), planOptions{aaDir: aa})
	if tasks[0].Local != "" {
		t.Fatalf("a catalog path with .. must not resolve outside aa: %s", tasks[0].Local)
	}
}

func TestDominantRelease(t *testing.T) {
	cat := mkCat("N",
		mkBundle("1", "https://h/s20260917_Aaaa/"+bundleFile("x", '1'), 1, '1'),
		mkBundle("2", "https://h/s20260917_Aaaa/"+bundleFile("y", '2'), 1, '2'),
		mkBundle("3", "https://h/s20260806_Bbbb/"+bundleFile("z", '3'), 1, '3'),
		mkBundle("4", `{UnityEngine.AddressableAssets.Addressables.RuntimePath}\s20250101_Old\f.bundle`, 1, '4'),
	)
	if got := dominantRelease(cat); got != "s20260917_Aaaa" {
		t.Errorf("got %q", got)
	}
	if got := dominantRelease(mkCat("N")); got != "" {
		t.Errorf("empty catalog: got %q", got)
	}
}
