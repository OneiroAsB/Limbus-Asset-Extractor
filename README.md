# Limbus Asset Extractor

A single binary for downloading and extracting Limbus Company assets from the game's CDN:

```text
release version -> catalog_S1.hash -> (changed?) -> catalog_S1.bin -> decode catalog
                                                                        |
  baseline <- saved after processing <- extract <- download <- diff against previous catalog
```

**Limbus Asset Extractor** (`limbus-extractor`) uses only the Go standard library and has no external runtime dependencies.

I started this project out of curiosity and a need to automate downloading some assets from Limbus Company. From the beginning, I also wanted to release it publicly as a complete, standalone program that anyone could use for their own tasks. Keeping roughly 15 GB of game files on a server just for my small task felt excessive, so I decided to fetch the resources from the CDN used by the game instead.

As the project grew, I began to realize that I was struggling to manage it on my own. I eventually stopped developing it before it became the program I had originally envisioned. I'm releasing what I have, with its limitations documented below, in case someone finds it interesting and wants to explore it, adapt it, or take it further. Please bear with the unusual implementation choices and rough edges. I'd be grateful to anyone who takes the time to point out issues, share ideas, or help improve the project.

## Quick start

For a binary release, extract the archive for your platform into a separate directory.
It contains `limbus-extractor` (`limbus-extractor.exe` on Windows) and a short `README.txt`;
the source code and Go toolchain are not needed to run it.

When building from source, `build.sh` writes platform-specific binaries to `dist/`.
Rename the selected binary to the short name above to use the following commands.

First, find the release name using the [SteamDB instructions below](#finding-the-release-name-on-steamdb),
or use `-game-dir` with an existing game installation. Replace the example release names with the ones you need.

```bash
# Linux
./limbus-extractor run -version s20260910_fDbU_dzxziNuVLKe_Miv
# Windows
.\limbus-extractor.exe run -version s20260910_fDbU_dzxziNuVLKe_Miv
```

**The first run does not download bundles:** with nothing to compare against, it only records the current catalog as the baseline.
To compare against an existing catalog immediately, pass it once:

```bash
./limbus-extractor run -version s20260917_XozhYgm77guBS5Wgmims -baseline catalog_S1_unpacked.json -dry-run   # preview the plan
./limbus-extractor run -version s20260917_XozhYgm77guBS5Wgmims -baseline catalog_S1_unpacked.json             # execute
```

The `catalog_S1_unpacked.json` produced by the old `unpack_catalog` tool works as-is (the output format is byte-for-byte compatible).
Use `-baseline` for a single run, then remove it from regular runs. Otherwise, every diff will be against that same file; the pipeline warns about this.

## What `run` does

| Step | What happens |
|---|---|
| 1. Version | Read the release name (`s20260910_...`) from `-version`, a file, a command, or the game directory (see below) |
| 2. Hash | `GET <cdn>/<version>/catalog_S1.hash`. Exit if it matches the saved hash |
| 3. Catalog | `GET .../catalog_S1.bin`, then decode it to JSON |
| 4. Diff | Compare against the previous catalog: added and changed bundles go into `diff.json` |
| 5. Download and extract | **Streaming pipeline:** download only added and changed bundles **that pass selection** (see "Selection"). URLs come from the new catalog. Downloads run concurrently, with retries, partial download resumption, and size verification. Each downloaded bundle is immediately extracted into `output/<version>/<bundle_name>/` (textures to PNG; TextAssets as-is, except excluded content), then **deleted immediately** |
| Final step | Save the catalog as the next baseline |

Requests use the game's headers (`User-Agent: UnityPlayer/6000.3.12f1 ...`, `X-Unity-Version`, `X-Requested-With`), without compression, like `curl`.

### Contents of `work/`

```text
work/
├── state/
│   ├── last.json                      hash and version of the last processed catalog
│   └── catalog_S1_unpacked.json        that catalog: the baseline for the next diff
├── runs/<version>/
│   ├── catalog_S1.bin / .hash
│   ├── catalog_S1_unpacked.json
│   ├── diff.json
│   ├── done.json                      journal of extracted bundles, for resuming interrupted runs
│   └── report.json                    downloads, extraction results, failures, and the full plan
├── bundles/                           downloaded bundles; usually empty after extraction
└── output/<version>/<bundle>/...       extracted assets
```

Bundle directories in `output/` omit the content hash (`static_s1_0` instead of `static_s1_0_assets_all_<hash>`).
The same bundles therefore have the same directory names across releases, making their contents easier to compare.

## Selection: what is included and excluded

By default, only selected bundles are processed. The rules are defined in one file,
[`internal/pipeline/selection.go`](internal/pipeline/selection.go). Rebuild after editing it: `./build.sh`.

| Content | Rule | When checked |
|---|---|---|
| Bundles: **included** | `p<digits>` (identities), `e<digits>` (EGO), `s8_...` | Before downloading, by name |
| Bundles: excluded | `....mp4` (videos: no supported assets are extracted from them) | Before downloading |
| Objects inside bundles: excluded | Names containing `fx`, `vfx`, `mask`, `atlas`, or `sactx` (case-insensitive) | During extraction, **before decoding** |
| Files inside bundles: excluded | `.json` | During extraction |

Everything else (`s4_`, `s5_`, `s7_`, `storypsb`, `ui_formation`, `static_s1_0`, `shader`, etc.) is not downloaded.

- **Bundle selection happens before downloading**, so unwanted bundles consume no bandwidth or disk space.
- **Bundle contents cannot be reliably determined in advance:** the catalog does not describe them reliably enough. Objects inside a bundle are therefore filtered only after downloading it. This saves disk space and extraction time: excluded textures are neither decoded nor encoded as PNG, which usually accounts for most of the extraction time. It does not save download bandwidth.
- The report (`report.json`) and logs show how many bundles were excluded, grouped by family (`s4 ×4 (100 MiB), s7 ×4 ...`). With `-dry-run -v`, each excluded bundle is listed individually. Excluded objects appear in the extractor log (`-v`) as `[excluded] name (name contains "fx")`.
- **Selection is a permanent choice:** excluded bundles will not reappear in later diffs. If you expand the selection and want previously excluded content, use `-full`. It ignores the baseline and applies selection to the entire catalog.
- `-no-select` disables all selection rules, including bundle and content filters.
- To check content selection on an existing bundle: `limbus-extractor extract -select bundle.bundle directory`.

## Keep only new and changed files inside bundles

A normal `run` compares **whole bundles**. For example, `p10315` may appear in both catalogs but receive a new hash after being rebuilt. A normal run then downloads and extracts the entire new bundle, even if the images you want have not changed.

The `-asset-diff` mode compares the **contents of extracted files** between two versions:

- New bundle (`p11016`, absent from the baseline): keep all selected files.
- Changed bundle (`p10315`): keep only new and changed files in `output/`.
- Remove identical files from the output. If everything matches, no bundle directory remains in `output/`.
- Save previous versions of changed files, and files that were removed, in `before/`.
- Write `added`, `changed`, `removed`, and the `unchanged` count to `runs/<version>/asset-diff/<bundle>.json` and `report.json`, with a short summary in the console.

Files are compared by name and the SHA-256 of their contents. Both versions' PNG files are produced by the same extractor, so changes to bundle compression, packaging, or Unity metadata alone do not count as image changes. Renaming a file appears as removal of the old name and addition of the new name. Normal selection filters apply; excluded objects are not compared. Bundles removed entirely from the catalog appear in the main `diff.json`; this mode does not download or extract their old contents.
Empty `0×0` textures without pixels, such as dynamic font placeholders, are treated as empty objects and do not interrupt comparison.

Example using two saved catalogs (PowerShell, from the project directory):

```powershell
.\dist\limbus-extractor-windows-amd64.exe run `
  -version s20261015_SdNeUL1o5O3ADlYd_KLZ `
  -baseline ".\work\runs\s20261008_7a56otqmJGP_DMSmYEth\catalog_S1_unpacked.json" `
  -asset-diff -work work-diff
```

Example output layout:

```text
work-diff/output/<new_version>/<bundle>/...   new and changed files: after
work-diff/before/<new_version>/<bundle>/...   changed and removed originals: before
work-diff/runs/<new_version>/asset-diff/<bundle>.json
```

Add `-bundle-filter p10315` to process only that bundle. Use `-dry-run` to preview the plan without downloading bundles. File differences can only be determined after extraction.

The baseline advances after a successful comparison. For subsequent updates, use `run -version <next_release> -asset-diff -work work-diff` without keeping `-baseline` in the command. An explicit `-baseline` lets you compare the same two releases again, even if the newer release has already been processed. Use separate `-work` directories for normal runs and file comparisons.

**Bandwidth:** changed bundles require both versions to be downloaded unless they are cached. The catalog does not contain individual images and cannot be used to download just one image. New bundles require only the new version. Both bundle versions are deleted after a successful comparison unless `-keep-bundles` is set. Old versions must still be available at the URLs in the baseline, or in the local cache. If an old version is unavailable or extraction is incomplete, comparison fails and the baseline is not advanced. `-full` and `-no-extract` are incompatible with `-asset-diff`. Without a previous catalog, this mode exits with instructions.

## Version sources

The `-version` flag takes the game's release folder name, such as `s20260910_fDbU_dzxziNuVLKe_Miv`.
The Steam App ID, Depot ID, Build ID, and numeric Manifest ID are different identifiers and cannot be used as `-version`.

### Finding the release name on SteamDB

You can look up the name without downloading the game:

1. Open [Limbus Company's Depots page](https://steamdb.info/app/1973530/depots/) (App ID `1973530`).
2. Select the [Windows depot `1973531`](https://steamdb.info/depot/1973531/).
3. In **Files**, search for `StreamingAssets/aa/` and find a path containing `sYYYYMMDD_...`.
   Copy that entire folder name, including everything after the date. For example, from
   `LimbusCompany_Data/StreamingAssets/aa/s20260910_fDbU_dzxziNuVLKe_Miv/...`, copy only
   `s20260910_fDbU_dzxziNuVLKe_Miv`.
4. Pass the copied name to `-version`. The names in this README are examples, not a promise that they are the latest release.

For a specific update or an older release, open the depot's [Manifests tab](https://steamdb.info/depot/1973531/manifests/)
and click the Manifest ID for the update you need. Use the `public` branch if filtering by branch.
In the change view, search for `StreamingAssets/aa/` or `s202`:
an **added** path identifies the new release folder, while a **removed** path can identify the previous one.
“Removed” describes the change in the Steam depot; it does not tell you whether the old resources are still available on the CDN.
If the old name is not shown, inspect an earlier manifest's changes or file listing.
Some Steam updates keep the same release folder name, so you may need to look further back to find a different release.
SteamDB may require sign-in to view older entries.
These instructions are for manual browsing. SteamDB's [FAQ](https://steamdb.info/faq/)
prohibits automated scraping/crawling and auto-refreshing; this program does not scrape SteamDB.

If the game is already installed, you can also inspect `LimbusCompany_Data/StreamingAssets/aa/` locally,
or let the program find the release name:

```powershell
.\limbus-extractor.exe run -game-dir "C:\Program Files (x86)\Steam\steamapps\common\Limbus Company"
```

Use your actual installation path. To download all selected bundles immediately, add `-full`;
otherwise, the first normal run only saves the baseline. For comparisons, use the old release's catalog
as the baseline and the new release name as `-version`; a folder name alone is not a baseline catalog.

### Other version sources

Version detection uses the `resolver.Resolver` interface (`internal/resolver`). Available sources:

| Flag | Source |
|---|---|
| `-version s2026...` | Manual release name |
| `-version-file version.txt` | First non-empty line of a file; another module can update this file |
| `-version-cmd "<command>"` | Last non-empty line of the command's stdout |
| `-game-dir "<game_directory>"` | Newest `sYYYYMMDD_*` entry in `LimbusCompany_Data/StreamingAssets/aa` |
| `-catalog catalog_S1.bin` | Use a local catalog; no version is required and no hash check is performed |

A **Steam depot/manifest module** can be integrated without changing the rest of the pipeline:

1. *Without Go:* write a separate program that prints the release name, then use `-version-cmd "./steam-depot-watcher"`.
2. *In Go:* implement `Resolve()` in the `resolver.Steam` placeholder and select it in `cmd/limbus-extractor/main.go`.

Release names are validated (`s`, eight digits, `_`, then letters/digits, underscores, or hyphens), keeping invalid input out of URLs and paths.

### Optional manifest lookup with DepotDownloader

SteamDB's [FAQ](https://steamdb.info/faq/) recommends [DepotDownloader](https://github.com/SteamRE/DepotDownloader)
for downloading depots and manifests directly from Steam. This is separate from scraping the SteamDB website.
DepotDownloader supports `-manifest-only` to download a readable file listing without downloading the game files:

```powershell
.\DepotDownloader.exe -app 1973530 -depot 1973531 -manifest-only -username YOUR_STEAM_USERNAME
```

Replace the username; the tool prompts for the password. With no `-manifest`, it selects the current manifest for the branch.
Search the resulting listing for `LimbusCompany_Data/StreamingAssets/aa/` and copy the `sYYYYMMDD_...` folder name.
Access requires the appropriate game license, and some old manifests may be unavailable.
This external workflow is not integrated into Limbus Asset Extractor and has not been verified here with a Steam account.

## Disk usage

- **Each downloaded bundle is deleted immediately after successful extraction.** Downloading and extraction overlap, so only a few bundles are held on disk at once, roughly `-download-workers` + `-extract-workers` + 1, regardless of update size. The queue between the stages is deliberately short: when extraction falls behind, downloads wait instead of accumulating files.
- Use `-keep-bundles` to retain bundles. `-no-extract` also keeps them, since downloading them is the purpose of that mode.
- **A bundle that fails extraction is kept on disk** for inspection and another attempt after fixing the extractor.
- **Interrupted runs resume instead of starting over.** Extracted bundles are recorded in `runs/<version>/done.json`; subsequent runs skip downloading and extracting them again. The journal is ignored if extraction settings change (selection, `-only`, or `-filter`), or if the output directory was deleted. Those bundles are then processed again.
- The remaining files are the extracted assets in `output/`. They may take more space than the bundles because PNG files can be larger than compressed textures.

## Scheduled runs on a VPS

```cron
*/30 * * * * cd /opt/limbus && ./limbus-extractor run -version-cmd "./steam-depot-watcher" >> limbus-extractor.log 2>&1
```

If the hash is unchanged, the run takes a fraction of a second and leaves the saved state alone. Exit codes: `0` for success, `1` for runtime errors (such as failed downloads), and `2` for invalid arguments.
`Ctrl+C` / `SIGTERM` shuts down cleanly: incomplete downloads remain as `.part` files for resumption, while extracted bundles are skipped on the next run (see "Disk usage").

## When the baseline advances

The pipeline advances the baseline according to the following rules:

| Situation | Baseline |
|---|---|
| All bundles downloaded, including cache hits | Advances |
| Some bundle downloads failed | **Does not advance:** the next run continues using the cache |
| Bundle excluded by selection (`selection.go`) | Advances: selection is a permanent choice |
| Bundle downloaded but extraction failed (e.g. LZMA) | Advances: extraction failures are recorded in `report.json` under `extract_failed`, allowing the pipeline to keep progressing |
| `-max-bundles N` (trial run) | **Does not advance:** remaining bundles appear in the next run |
| `-bundle-filter` | Advances: a permanent choice to include only matching bundles |
| `-dry-run` | No baseline change |

## Common scenarios

```bash
# Preview downloads: the 10 largest bundles, plus the full list in report.json.
./limbus-extractor run -version ... -dry-run

# Download only, without extraction.
./limbus-extractor run -version ... -no-extract

# Process all selected bundles in the current catalog, ignoring the baseline.
./limbus-extractor run -version ... -full

# Extract everything without selection (about 15 GB), or select specific bundles.
./limbus-extractor run -version ... -full -no-select
./limbus-extractor run -version ... -full -no-select -bundle-filter static_s1

# Also extract MonoBehaviour (ClassID 114) as JSON.
./limbus-extractor run -version ... -only texture,text,114

# Include bundles shipped with the game rather than the CDN (three in the sample catalog).
./limbus-extractor run -version ... -game-dir "C:\...\Limbus Company"
```

## Other commands (the original tools, with the same flags)

```bash
limbus-extractor unpack catalog_S1.bin [-o out.json]          # previously unpack_catalog
limbus-extractor diff [-out diff.json] old.json new.json      # previously catalog-diff
limbus-extractor extract [-list] [-only ...] [-filter ...] [-select] [-exclude ...] [-exclude-ext ...] [-unity-version ...] bundle [directory]   # previously unity-extractor
```

## `run` flags

For the complete list, run `limbus-extractor run -h`. Main options:

| Flag | Default | Meaning |
|---|---|---|
| `-work` | `work` | Working directory |
| `-cdn` | `https://download.limbuscompanycdn.org` | CDN base URL |
| `-baseline` | - | Compare against this JSON instead of the saved catalog |
| `-full` | off | Treat all bundles as new, ignoring the baseline; selection still applies |
| `-asset-diff` | off | Compare extracted files against the previous version: new/changed files in `output/`, originals in `before/` |
| `-no-select` | off | Disable `selection.go` rules: include all bundles and contents |
| `-dry-run` | off | Show the plan and stop |
| `-bundle-filter` | - | Include only bundles whose filenames contain this text |
| `-max-bundles` | 0 | Limit the number of bundles (trial run) |
| `-no-extract` / `-keep-bundles` | off | Download without extracting / keep bundles after extraction |
| `-only` / `-filter` | `texture,text` / - | Objects to extract from bundles |
| `-download-workers` / `-extract-workers` | 4 / 2 | Concurrency |
| `-retries` | 3 | Retries after a network error |
| `-no-verify-size` | off | Skip download size verification against the catalog; also disables resuming partial downloads |
| `-user-agent`, `-client-version`, `-requested-with`, `-header "Name: value"` | Game client values | Request headers |
| `-v` | off | Verbose extractor output |

> **About `X-Requested-With`:** the value `this_is_header_value` was copied verbatim from the provided curl example.
> If that was a placeholder, supply the actual value with `-requested-with "..."` or `-header "X-Requested-With: ..."`.

## Project structure

```text
cmd/limbus-extractor/      CLI (run, unpack, diff, extract)
internal/pipeline/        orchestration: planning, downloads, extraction, state, reporting
internal/resolver/        release version sources (interface, implementations, Steam placeholder)
internal/cdn/             HTTP client: game headers, retries, resumable downloads, size verification
internal/catalog/         Addressables catalog_S1.bin decoder (v2/v3) -> Go structures/JSON
internal/catalogdiff/     comparison of two catalogs
internal/extractor/       UnityFS bundles, Texture2D -> PNG, TextAsset, generic JSON dumps
tools/package-release/   developer tool for packaging prebuilt binaries
dist/                    local builds
dist/releases/           standalone release ZIPs and SHA256SUMS
```

## Build and test

```bash
./build.sh                       # dist/: linux amd64/arm64 and windows amd64
./build.sh --release             # also package standalone archives in dist/releases/
go test ./... -race              # all tests, no external network required
```

PowerShell, without `build.sh`:

```powershell
New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED='0'
go build -trimpath -ldflags "-s -w" -o .\dist\limbus-extractor-windows-amd64.exe ./cmd/limbus-extractor
```

To package existing builds without rebuilding:

```bash
go run ./tools/package-release                           # all three platforms
go run ./tools/package-release -platform windows-amd64   # Windows only
```

Each ZIP contains only the executable under its short name and `README.txt`.
Linux executables retain their execute permissions in the archive. `SHA256SUMS`
contains the SHA-256 checksums of the available release archives.

Keep source files in the Git repository. Upload the ZIPs and `SHA256SUMS` as release
assets; `dist/` and the runtime `work/` directory are excluded from Git.
The program creates `work/` relative to the directory from which it is launched,
or at the location specified with `-work`.

Tests using **your real data** are skipped until the required environment variables are set:

```bash
# Catalog decoder: byte-for-byte comparison against an existing unpacked.json.
LIMBUS_SAMPLE_BIN=catalog_S1.bin LIMBUS_SAMPLE_JSON=catalog_S1_unpacked.json go test ./internal/catalog -run RealCatalog -v
# Diff: comparison against an existing diff.json.
LIMBUS_OLD_JSON=old_unpacked.json LIMBUS_NEW_JSON=new_unpacked.json LIMBUS_DIFF_JSON=diff.json go test ./internal/catalogdiff -run RealCatalogs -v
# Entire pipeline with a real catalog and a simulated CDN.
LIMBUS_NEW_BIN=catalog_S1.bin LIMBUS_OLD_JSON=old_unpacked.json go test ./internal/pipeline -run RealCatalogs -v
```

Regenerate the test bundle `internal/extractor/testdata/rgba32.bundle` with:
`WRITE_TESTDATA=1 go test ./internal/extractor -run TestWriteTestdata`.

## Limitations

### What the project cannot do

- **Automatically identify the images I actually want.** The catalog is useful for locating bundles, but it does not provide a reliable way for this tool to decide which images inside them are relevant to my task. Selection relies on hardcoded bundle patterns and object-name filters. These rules can include unwanted files or exclude useful ones, and changing the default rules requires rebuilding the program.
- **Download only a specific image from a remote bundle.** A selected bundle is downloaded as a whole before its contents can be filtered. Avoiding a full game installation still means downloading some resources I do not need.
- **Decide whether an image change matters.** Normal runs detect changes at the bundle level. `-asset-diff` goes further by comparing extracted files by name and SHA-256, so a bundle rebuild alone does not make identical extracted images appear changed. However, even a one-pixel change still counts as a change. There is no visual similarity threshold or automatic matching of renamed images.
- **Discover new releases directly from Steam.** There is no working built-in depot/manifest resolver. I chose to leave Steam sign-in out of this tool because I did not want my automation to handle account credentials or authentication sessions, whether mine or anyone else's. Release names must come from manual input, a file, an external command, or an installed game, as described in [Version sources](#version-sources).
- **Guarantee access to old resources.** Comparing changed bundles with `-asset-diff` requires both versions. If an old bundle is no longer available on the CDN and is not cached locally, that comparison cannot complete. Bundles shipped only with the game also require local game files via `-game-dir`.
- **Extract every Unity asset format.** LZMA bundles, mobile texture formats (ETC/ASTC/PVRTC), HDR (BC6H), and Crunch-compressed pixel data are unsupported. Generic JSON dumps of other object types require an embedded TypeTree. Unsupported formats are reported; an unsupported bundle can fail extraction, while unsupported objects are skipped.
- **Memory:** each bundle is read entirely into memory. The largest bundle in the sample catalog is about 457 MB, and extraction can require about 1 GB of RAM per concurrent worker. Use `-extract-workers 1` on memory-constrained systems.
- Downloaded file sizes are checked against the catalog's `bundle_size`. If that size does not match what the CDN serves, the run reports `size mismatch`. Use `-no-verify-size` to disable verification.

### Plans I did not finish

I wanted the process to work unattended, including discovering release names from Steam depot manifests. I considered that integration, but it is not included in the published tool; the `resolver.Steam` type is only a placeholder. The scheduling example above therefore needs an external version source that you provide yourself.

The [DepotDownloader](https://github.com/SteamRE/DepotDownloader)-based approach I considered for Limbus Company's manifests required a real Steam account with access to the game, rather than anonymous access. Adding Steam authentication would have meant handling my own login and session data. Since I also intended to release the program for anyone to use, I had to consider what it would mean to ask other people to sign in as well. An unfamiliar program asking for a Steam username and password could easily look like a phishing tool, and mistakes in authentication or session handling could put an account at risk. I did not want to take on that work or handle anyone's sensitive account data, including my own, so I left account-based release discovery out. Anyone who wants to use an external tool for that step can still supply the resulting release name separately.

I also wanted comparisons to surface the image changes I cared about without so much manual review. Exact file comparison is implemented, but a comparison that can tolerate insignificant visual changes and more reliably select relevant images was not completed. The current name-based filters remain tailored to my original task.

### Why I stopped working on it

What started as a small automation task grew into maintaining catalog decoding, bundle extraction, selection rules, and comparisons between releases. Each step exposed another mismatch between downloading changed files and getting only the assets I actually wanted. I kept adding rules and extra processing, but the result was becoming harder for me to understand and maintain on my own. Much of the code was written with AI assistance, and I reached a point where I no longer had a clear enough understanding of all of it to feel comfortable continuing. I had started out of curiosity, and eventually lost the motivation to keep expanding it. I am leaving the code here for anyone who finds the problem interesting and wants to explore a different approach or continue the work.

Making the workflow fully unattended and ready for others to use would also have brought account authentication into a project I was already struggling to maintain. I did not want to manage Steam login data for myself or anyone else. I decided to publish the existing implementation with its limitations explained, leaving the broader idea unfinished.

## Acknowledgements

Thanks to the authors of [AddressablesToolsPy](https://github.com/anosu/AddressablesToolsPy) and nesrak1's [AddressablesTools](https://github.com/nesrak1/AddressablesTools) for the catalog decoding logic that this project's Go implementation is based on. Thanks also to [AssetStudio](https://github.com/Perfare/AssetStudio) and [UnityPy](https://github.com/K0lb3/UnityPy) for their work on Unity assets, used as references for the extractor, and to Microsoft's [DirectXTex](https://github.com/microsoft/DirectXTex) for the BC7 lookup tables. Thanks to [mitmproxy](https://github.com/mitmproxy/mitmproxy), which I used to inspect the game's network traffic and find the URLs needed for this project, and to [SteamDB](https://steamdb.info/), which I frequently used to look up depots, manifests, and release changes. This project builds on that work, and I appreciate the time and effort their authors have shared with the community.
