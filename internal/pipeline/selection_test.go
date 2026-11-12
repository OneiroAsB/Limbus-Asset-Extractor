package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"limbus-asset-extractor/internal/catalog"
	"limbus-asset-extractor/internal/resolver"
)

// Real bundle stems from an actual update (the run log), grouped by whether
// they are wanted.
var (
	wantedStems = []string{
		"e20211", "e20510", "e20610", // EGO
		"p10216", "p11013", "p10815", "p10416", "p10615", "p10315", "p10116", "p10917", "p10614", "p11216", // identities
		"s8_a1c10p3_produce", "s8_a1c10p1_rpgplayer", "s8_a1c10p2_enemy_sd", "s8_a1c10p3_story_cg", "S8_UPPER_CASE_TOO",
	}
	unwantedStems = []string{
		"s4_240411_enemy_sd", "s5_250220_railwayselectbuff", "s7_260716_lobbybgm", "s7_a1c9p3_common", "s6_250925_produce", "s2_230601_story_bg",
		"storypsb_s1_0", "ui_formationuipanel", "static_s1_0", "shader", "shaderv2", "uispritecontainers", "skinpreview-archive",
		"2050121", "vfx__unitybuiltinassets", "a1d6854c8735f72d1361b49be9648465_monoscripts",
		"produce_s1s2_x", // starts with "p", but not "p<digits>"
		"s80_something",  // "s80_" is not "s8_"
		"escape_room",    // starts with "e", but not "e<digits>"
		"skillpreviewvideo_assets_skillpreviewvideo_21210.mp4",
		"s8_a1c10p3_video_assets_egovideo_20510.mp4", // s8, but a video bundle
		"s8_a1c10p3_video_assets_personalityvideo_10315.mp4",
	}
)

func TestDefaultSelectionBundles(t *testing.T) {
	sel := DefaultSelection()
	for _, s := range wantedStems {
		if !sel.WantsBundle(s) {
			t.Errorf("%q should be taken", s)
		}
	}
	for _, s := range unwantedStems {
		if sel.WantsBundle(s) {
			t.Errorf("%q should be left out", s)
		}
	}
	var none *Selection
	if !none.WantsBundle("anything_at_all") {
		t.Error("a nil selection must take everything")
	}
}

func TestDefaultSelectionObjectRules(t *testing.T) {
	sel := DefaultSelection()
	for _, want := range []string{"fx", "vfx", "mask", "atlas", "sactx"} {
		found := false
		for _, n := range sel.ExcludeNames {
			found = found || n == want
		}
		if !found {
			t.Errorf("object rule %q missing from the default selection", want)
		}
	}
	if len(sel.ExcludeTextExts) != 1 || sel.ExcludeTextExts[0] != ".json" {
		t.Errorf("text extensions = %v", sel.ExcludeTextExts)
	}
	// DefaultSelection hands out copies: editing one must not change the next.
	sel.ExcludeNames[0] = "changed"
	if DefaultSelection().ExcludeNames[0] == "changed" {
		t.Error("DefaultSelection shares its slices with the package variables")
	}
}

func TestPlanAppliesSelectionBeforeTheCap(t *testing.T) {
	// Sorted by catalog bundle name, the unwanted bundle comes first: if the
	// cap were applied before the selection it would use up the only slot.
	srvURL := "https://h/r/"
	added := []catalog.Bundle{
		mkBundle("a-unwanted", srvURL+bundleFile("s4_240411_enemy_sd", 'a'), 100, 'a'),
		mkBundle("b-wanted", srvURL+bundleFile("p10216", 'b'), 200, 'b'),
		mkBundle("c-wanted", srvURL+bundleFile("e20211", 'c'), 300, 'c'),
		mkBundle("d-video", srvURL+"s8_x_video_assets_egovideo_1.mp4_"+hex32('d')+".bundle", 400, 'd'),
	}
	pl := buildPlan(diffOf(added), mkCat("N"), planOptions{sel: DefaultSelection(), max: 1})
	if len(pl.tasks) != 1 || pl.tasks[0].Name != "b-wanted" || !pl.truncated {
		t.Fatalf("tasks=%v truncated=%v", names(pl.tasks), pl.truncated)
	}
	if len(pl.deselected) != 2 || pl.deselected[0].Name != "a-unwanted" || pl.deselected[1].Name != "d-video" {
		t.Errorf("deselected = %v", names(pl.deselected))
	}
	if pl.deselected[0].Size != 100 {
		t.Error("deselected bundles keep their size, for the report")
	}

	// no cap: the unwanted ones never reach the task list
	pl = buildPlan(diffOf(added), mkCat("N"), planOptions{sel: DefaultSelection()})
	if len(pl.tasks) != 2 || pl.truncated {
		t.Errorf("tasks=%v truncated=%v", names(pl.tasks), pl.truncated)
	}
	// the bundle-name filter and the selection both have to agree
	pl = buildPlan(diffOf(added), mkCat("N"), planOptions{sel: DefaultSelection(), filter: "e20211"})
	if len(pl.tasks) != 1 || pl.tasks[0].Name != "c-wanted" {
		t.Errorf("filter+selection: %v", names(pl.tasks))
	}
}

func names(ts []*Task) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func TestSelectionEndToEnd(t *testing.T) {
	files := map[string]byte{ // stem -> hash char
		"e20211": 'a', "p10216": 'b', "s4_240411_enemy_sd": 'c', "static_s1_0": 'd', "s8_x_video_assets_egovideo_1.mp4": 'e', "storypsb_s1_0": 'f',
	}
	wanted := map[string]bool{"e20211": true, "p10216": true}

	var good []string
	var bundles []catalog.Bundle
	srvPlaceholder := newCDN(t) // only to learn the fixture size and URL base
	for stem, c := range files {
		file := bundleFile(stem, c)
		if strings.Contains(stem, ".mp4") {
			file = stem + "_" + hex32(c) + ".bundle"
		}
		if wanted[stem] {
			good = append(good, "/rel/"+file)
		}
		bundles = append(bundles, mkBundle("name-"+stem, srvPlaceholder.url(file), len(srvPlaceholder.data), c))
	}
	srv := srvPlaceholder
	for _, g := range good {
		srv.set(g, true)
	}
	old := mkCat("OLD")
	cur := mkCat("NEW", bundles...)

	p, l := newTestPipeline(t, func(c *Config) { c.Select = DefaultSelection() })
	saveBaseline(t, l, old)
	rep, err := process(t, p, l, cur)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Planned != 2 || rep.Downloaded != 2 {
		t.Errorf("planned=%d downloaded=%d, want 2/2", rep.Planned, rep.Downloaded)
	}
	if rep.NotSelected != 4 || rep.NotSelectedBytes != 4*int64(len(srv.data)) || len(rep.NotSelectedList) != 4 {
		t.Errorf("not selected: %d bundles, %d bytes, %d listed", rep.NotSelected, rep.NotSelectedBytes, len(rep.NotSelectedList))
	}
	// the point of the feature: unwanted bundles are never requested at all
	for stem, c := range files {
		file := bundleFile(stem, c)
		if strings.Contains(stem, ".mp4") {
			file = stem + "_" + hex32(c) + ".bundle"
		}
		if got, want := srv.hitCount("/rel/"+file), map[bool]int{true: 1, false: 0}[wanted[stem]]; got != want {
			t.Errorf("%s: requested %d time(s), want %d", stem, got, want)
		}
	}
	for stem := range wanted {
		if !exists(filepath.Join(l.outputDir("s20260917_testrelease"), stem, "TestTex.png")) {
			t.Errorf("output for %s missing", stem)
		}
	}
	if !rep.StateAdvanced {
		t.Error("the selection is a standing choice: the baseline must advance")
	}
	// ...which means a later run without the selection finds nothing left to do
	p2, _ := newTestPipelineIn(t, l, nil)
	rep2, err := process(t, p2, l, cur)
	if err != nil || rep2.Planned != 0 {
		t.Errorf("left-out bundles must not come back: planned=%d err=%v", rep2.Planned, err)
	}
}

func TestSelectionObjectRulesReachTheExtractor(t *testing.T) {
	sc := newScenario(t)
	// every texture in the fixture bundle is called "TestTex"
	p, l := newTestPipeline(t, func(c *Config) {
		sel := DefaultSelection()
		sel.ExcludeNames = []string{"TESTTEX"}
		c.Select = sel
		// the scenario's bundles are named alpha/gamma: let them through
		sel.include = mustCompileAll([]string{`.`})
	})
	saveBaseline(t, l, sc.old)
	rep, err := process(t, p, l, sc.cur)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Downloaded != 2 || rep.Extracted.Filtered != 2 || rep.Extracted.Textures != 0 {
		t.Errorf("downloaded=%d filtered=%d textures=%d", rep.Downloaded, rep.Extracted.Filtered, rep.Extracted.Textures)
	}
	if exists(l.outputDir("s20260917_testrelease")) {
		if entries, _ := os.ReadDir(l.outputDir("s20260917_testrelease")); len(entries) != 0 {
			t.Errorf("bundles with nothing left must leave no folder: %v", entries)
		}
	}
	if rep.Extracted.Empty != 2 {
		t.Errorf("empty bundles = %d", rep.Extracted.Empty)
	}
}

func TestNewMergesSelectionIntoExtractOptionsWithoutAliasing(t *testing.T) {
	sel := DefaultSelection()
	cfg := Config{Resolver: resolver.Manual("s20260917_x"), Select: sel}
	cfg.Extract.ExcludeNames = []string{"own"}
	p, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(p.Cfg.Extract.ExcludeNames, ",")
	if !strings.Contains(got, "own") || !strings.Contains(got, "sactx") {
		t.Errorf("extractor excludes = %q", got)
	}
	if len(cfg.Extract.ExcludeNames) != 1 {
		t.Error("New modified the caller's slice")
	}
	if len(p.Cfg.Extract.ExcludeTextExts) != 1 || p.Cfg.Extract.ExcludeTextExts[0] != ".json" {
		t.Errorf("text exts = %v", p.Cfg.Extract.ExcludeTextExts)
	}
}

func TestFamilyAndSummary(t *testing.T) {
	for in, want := range map[string]string{
		"s4_240411_enemy_sd": "s4", "static_s1_0": "static", "storypsb_s1_0": "storypsb",
		"2050121": "(numeric)", "shader": "shader", "": "",
	} {
		if got := family(in); got != want {
			t.Errorf("family(%q) = %q, want %q", in, got, want)
		}
	}
	tasks := []*Task{
		{Stem: "s4_a", Size: 3 << 20}, {Stem: "s4_b", Size: 2 << 20},
		{Stem: "static_s1_0", Size: 9 << 20}, {Stem: "shader", Size: 1 << 20},
	}
	s := summarizeFamilies(tasks, 2)
	if !strings.HasPrefix(s, "static ×1 (9.0 MiB), s4 ×2 (5.0 MiB)") || !strings.Contains(s, "and 1 more") {
		t.Errorf("summary = %q", s)
	}
}
