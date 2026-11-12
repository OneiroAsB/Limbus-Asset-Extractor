// Command limbus-extractor downloads and extracts Limbus Company Addressables assets.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"limbus-asset-extractor/internal/catalog"
	"limbus-asset-extractor/internal/catalogdiff"
	"limbus-asset-extractor/internal/cdn"
	"limbus-asset-extractor/internal/extractor"
	"limbus-asset-extractor/internal/pipeline"
	"limbus-asset-extractor/internal/resolver"
)

const usage = `Limbus Asset Extractor - download and extract Limbus Company assets with one command

Usage:
  limbus-extractor [run] [flags]        run the entire pipeline (run is the default command)
  limbus-extractor unpack <catalog_S1.bin> [-o out.json]
  limbus-extractor diff [-out diff.json] <old.json> <new.json>
  limbus-extractor extract [-list] [-only ..] [-filter ..] [-select] <bundle> [directory]
  limbus-extractor help

Pipeline (run):
  version -> catalog_S1.hash (changed?) -> catalog_S1.bin -> decode
  -> diff against the previous catalog -> download new/changed bundles -> extract

Examples:
  limbus-extractor run -version s20260910_fDbU_dzxziNuVLKe_Miv
  limbus-extractor run -game-dir "C:\Program Files (x86)\Steam\steamapps\common\Limbus Company"
  limbus-extractor run -catalog catalog_S1.bin -dry-run
  limbus-extractor run -version-cmd "./steam-depot-watcher"

Flag details: limbus-extractor run -h`

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "unpack":
		err = cmdUnpack(args)
	case "diff":
		err = cmdDiff(args)
	case "extract":
		err = cmdExtract(args)
	case "help", "-h", "--help":
		fmt.Println(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", cmd, usage)
		os.Exit(2)
	}
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
	case errors.Is(err, errUsage):
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("invalid arguments")

func usageErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, a...))
}

// parseInterspersed is flag.Parse that also accepts flags after positional
// arguments ("unpack catalog.bin -o out.json").
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var cfg pipeline.Config
	var headers multiFlag

	// where the version / catalog comes from
	version := fs.String("version", "", "release folder name, e.g. s20260910_fDbU_dzxziNuVLKe_Miv (manual)")
	versionFile := fs.String("version-file", "", "read the release name from a text file (which a Steam module can update)")
	versionCmd := fs.String("version-cmd", "", "run a command; its last non-empty stdout line is the release name")
	fs.StringVar(&cfg.GameDir, "game-dir", "", "game directory: detect the latest release in StreamingAssets/aa and include bundles shipped with the game")
	fs.StringVar(&cfg.CatalogPath, "catalog", "", "use a local catalog_S1.bin instead of downloading it from the CDN (no hash check)")

	fs.StringVar(&cfg.WorkDir, "work", "work", "working directory (state, bundle cache, output)")
	fs.StringVar(&cfg.CDNBase, "cdn", cdn.DefaultBase, "CDN base URL")

	// what to do
	fs.StringVar(&cfg.BaselinePath, "baseline", "", "compare against this unpacked.json instead of the saved previous catalog")
	fs.BoolVar(&cfg.Full, "full", false, "treat all bundles as new (no baseline). About 15 GB; consider using -bundle-filter")
	fs.BoolVar(&cfg.AssetDiff, "asset-diff", false, "compare files inside bundles: keep new/changed files and save originals in before (requires a baseline and downloads old bundles too)")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "show the download plan and stop without updating state")
	fs.StringVar(&cfg.BundleFilter, "bundle-filter", "", "include only bundles whose filenames contain this text. Permanent selection: excluded bundles will not reappear in the next diff")
	fs.IntVar(&cfg.MaxBundles, "max-bundles", 0, "process at most N bundles (trial run: the baseline is not advanced)")
	fs.BoolVar(&cfg.NoExtract, "no-extract", false, "download only, without extracting")
	keepBundles := fs.Bool("keep-bundles", false, "keep downloaded bundles after extraction (by default each bundle is deleted immediately after extraction)")
	fs.Bool("delete-bundles", true, "deprecated and ignored: bundles are now deleted by default")
	noSelect := fs.Bool("no-select", false, "disable internal/pipeline/selection.go rules and include ALL bundles and their contents")
	noVerify := fs.Bool("no-verify-size", false, "skip download size verification against the catalog (also disables resuming partial downloads)")

	// extractor
	fs.StringVar(&cfg.Extract.Only, "only", extractor.DefaultOnly, `objects to extract: texture, text, any (other classes as JSON), and/or numeric ClassIDs (114 = MonoBehaviour)`)
	fs.StringVar(&cfg.Extract.Filter, "filter", "", "extract only objects whose names contain this text")
	fallbackUnity := fs.String("fallback-unity", "2022.3.22f1", "Unity version to assume when the embedded version is blank or 0.0.0 (fallback when TypeTree is also unavailable)")
	fs.BoolVar(&cfg.Verbose, "v", false, "verbose extractor output for each object")

	// network
	fs.IntVar(&cfg.DownloadWorkers, "download-workers", 4, "concurrent downloads")
	fs.IntVar(&cfg.ExtractWorkers, "extract-workers", 2, "concurrent extractions (a large bundle can require about 1 GB of RAM)")
	retries := fs.Int("retries", 3, "retries after a network error")
	userAgent := fs.String("user-agent", "", "User-Agent (defaults to the game client value)")
	clientVersion := fs.String("client-version", cdn.DefaultUnityVersion, "client Unity version for X-Unity-Version (and User-Agent unless overridden)")
	requestedWith := fs.String("requested-with", cdn.DefaultRequestedWith, "X-Requested-With header value")
	fs.Var(&headers, "header", `add or override an HTTP header: "Name: value" (repeatable)`)

	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: limbus-extractor run [flags]\n\nSpecify a version source: -version, -version-file, -version-cmd, -game-dir, or a local -catalog.\n\n")
		fs.PrintDefaults()
	}
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected arguments: %s", strings.Join(pos, " "))
	}
	cfg.VerifySize = !*noVerify
	cfg.DeleteBundles = deleteAfterExtract(*keepBundles, cfg.NoExtract)
	if !*noSelect {
		cfg.Select = pipeline.DefaultSelection()
	}

	switch {
	case cfg.CatalogPath != "":
		if *version != "" {
			cfg.Label = *version // names the run folders; the catalog itself is local
		}
	case *version != "":
		if err := resolver.Validate(*version); err != nil {
			return usageErr("-version: %v", err)
		}
		cfg.Resolver = resolver.Manual(*version)
	case *versionFile != "":
		cfg.Resolver = resolver.File{Path: *versionFile}
	case *versionCmd != "":
		cfg.Resolver = resolver.Command{Cmdline: *versionCmd}
	case cfg.GameDir != "":
		cfg.Resolver = resolver.GameDir{Dir: cfg.GameDir}
	default:
		return usageErr("no version source specified (-version, -version-file, -version-cmd, -game-dir, or -catalog); see limbus-extractor run -h")
	}

	client := cdn.NewClient()
	client.Retries = *retries
	client.Header.Set("X-Unity-Version", *clientVersion)
	client.Header.Set("User-Agent", fmt.Sprintf("UnityPlayer/%s (UnityWebRequest/1.0, libcurl/2.0)", *clientVersion))
	if *userAgent != "" {
		client.Header.Set("User-Agent", *userAgent)
	}
	client.Header.Set("X-Requested-With", *requestedWith)
	for _, h := range headers {
		name, value, ok := strings.Cut(h, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return usageErr(`-header %q: expected "Name: value"`, h)
		}
		client.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	client.HTTP.Transport.(*http.Transport).MaxIdleConnsPerHost = cfg.DownloadWorkers + 2

	extractor.SetFallbackUnityVersion(*fallbackUnity)

	logger := log.New(os.Stderr, "", log.Ltime)
	p, err := pipeline.New(cfg, client, logger)
	if err != nil {
		return usageErr("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rep, runErr := p.Run(ctx)
	if rep.UpToDate || rep.Label != "" { // an early failure has nothing worth summarising
		fmt.Println()
		fmt.Print(rep.Summary())
	}
	return runErr
}

func cmdUnpack(args []string) error {
	fs := flag.NewFlagSet("unpack", flag.ContinueOnError)
	out := fs.String("o", "", "output JSON path (defaults to <name>_unpacked.json next to the .bin file)")
	fs.StringVar(out, "output", "", "alias for -o")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("usage: limbus-extractor unpack <catalog_S1.bin> [-o output.json]")
	}
	in := pos[0]
	if *out == "" {
		*out = strings.TrimSuffix(in, ".bin") + "_unpacked.json"
	}
	cat, err := catalog.DecodeFile(in)
	if err != nil {
		return err
	}
	if err := cat.Save(*out); err != nil {
		return err
	}
	fmt.Printf("Unpacked %d locations (%d bundles) across %d keys -> %s\n", cat.Stats.Locations, cat.Stats.Bundles, cat.Stats.Keys, *out)
	return nil
}

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	out := fs.String("out", "", "write the full diff to this JSON file")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageErr("usage: limbus-extractor diff [-out diff.json] <old.json> <new.json>")
	}
	old, err := catalog.Load(pos[0])
	if err != nil {
		return err
	}
	cur, err := catalog.Load(pos[1])
	if err != nil {
		return err
	}
	if old.Meta.Version != cur.Meta.Version {
		fmt.Fprintf(os.Stderr, "warning: catalog format versions differ: %d -> %d\n", old.Meta.Version, cur.Meta.Version)
	}
	d := catalogdiff.Compare(old, cur, pos[0], pos[1])
	fmt.Println("=== Catalog diff ===")
	fmt.Printf("OLD: %s\nNEW: %s\n", pos[0], pos[1])
	fmt.Println(d.Summary(old, cur))
	if *out != "" {
		if err := writeJSONFile(*out, d); err != nil {
			return err
		}
		fmt.Printf("\nFull diff written to: %s\n", *out)
	}
	return nil
}

func cmdExtract(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	var opt extractor.Options
	fs.StringVar(&opt.Only, "only", extractor.DefaultOnly, `objects to extract: texture, text, any, and/or numeric ClassIDs`)
	fs.StringVar(&opt.Filter, "filter", "", "include only objects whose names contain this text (case-insensitive)")
	exclude := fs.String("exclude", "", "exclude objects whose names contain any of these comma-separated substrings (case-insensitive)")
	excludeExt := fs.String("exclude-ext", "", "exclude TextAssets with these comma-separated extensions, e.g. .json")
	useSelect := fs.Bool("select", false, "apply the content selection rules from selection.go (fx, vfx, mask, atlas, sactx, .json)")
	list := fs.Bool("list", false, "list bundle contents without extracting anything")
	unity := fs.String("unity-version", "2022.3.22f1", "Unity version to assume when the embedded version is blank or 0.0.0")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usageErr("usage: limbus-extractor extract [-list] [-only texture,text] [-filter text] [-unity-version X.Y.Zf1] <bundle> [directory]")
	}
	outDir := "output"
	if len(pos) == 2 {
		outDir = pos[1]
	}
	extractor.SetFallbackUnityVersion(*unity)
	opt.ExcludeNames = splitList(*exclude)
	opt.ExcludeTextExts = splitList(*excludeExt)
	if *useSelect {
		sel := pipeline.DefaultSelection()
		opt.ExcludeNames = append(opt.ExcludeNames, sel.ExcludeNames...)
		opt.ExcludeTextExts = append(opt.ExcludeTextExts, sel.ExcludeTextExts...)
	}

	if *list {
		return extractor.ListFile(pos[0], os.Stdout)
	}
	opt.Log = os.Stdout
	_, err = extractor.ExtractFile(pos[0], outDir, opt)
	return err
}

func deleteAfterExtract(keep, noExtract bool) bool { return !keep && !noExtract }

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
