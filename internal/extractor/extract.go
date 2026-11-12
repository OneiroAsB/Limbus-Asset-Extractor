// Package extractor pulls assets out of a Unity "UnityFS" asset bundle:
// Texture2D as PNG, TextAsset as its original text/JSON/bytes, and (on
// request) any other object type as a generic JSON dump of whatever its
// embedded TypeTree describes.
//
// Supported: UnityFS bundles compressed with LZ4/LZ4HC or uncompressed
// blocks; Texture2D formats: Alpha8, R8, RG16, R16, RGB24, RGBA32, ARGB32,
// BGRA32, RGB565, ARGB4444, RGBA4444, DXT1/DXT1Crunched, DXT3,
// DXT5/DXT5Crunched, BC4, BC5, BC7; TextAsset (any content); any other
// class, generically, when its TypeTree is embedded in the file.
//
// Not supported (reported per-object, not fatal): LZMA-compressed bundles,
// mobile texture formats (ETC/ETC2/EAC/PVRTC/ASTC), HDR texture formats
// (BC6H/half/float), Crunch-*compressed* pixel data specifically, and
// generic dumping of a class whose TypeTree isn't embedded in this file.
package extractor

import (
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	classTexture2D = 28
	classTextAsset = 49
)

// DefaultOnly is what gets extracted when Options.Only is empty.
const DefaultOnly = "texture,text"

// Options controls what Extract pulls out of a bundle.
type Options struct {
	// Only is a comma-separated list: "texture", "text", "any" (dump every
	// other class generically as JSON) and/or numeric ClassIDs such as "114"
	// (MonoBehaviour). Empty means DefaultOnly.
	Only string
	// Filter keeps only objects whose name contains this text
	// (case-insensitive). Empty keeps everything.
	Filter string
	// ExcludeNames drops every object whose name contains any of these
	// (case-insensitive substrings). The check runs BEFORE a texture is
	// decoded, so excluded textures cost almost nothing.
	ExcludeNames []string
	// ExcludeTextExts drops TextAssets that would be written with one of these
	// extensions, e.g. ".json". The extension is the one in the asset's name,
	// or - if it has none - the one guessed from its content.
	ExcludeTextExts []string
	// Log receives one line per extracted/skipped object. nil = silent.
	Log io.Writer
}

// Stats counts what an extraction did.
type Stats struct {
	Textures int `json:"textures"`
	Texts    int `json:"texts"`
	Dumped   int `json:"dumped"`   // other classes, as JSON
	Skipped  int `json:"skipped"`  // objects that were selected but could not be extracted
	Filtered int `json:"filtered"` // objects left out on purpose by ExcludeNames / ExcludeTextExts
}

// Extracted is the number of files written.
func (s Stats) Extracted() int { return s.Textures + s.Texts + s.Dumped }

// Add accumulates o into s.
func (s *Stats) Add(o Stats) {
	s.Textures += o.Textures
	s.Texts += o.Texts
	s.Dumped += o.Dumped
	s.Skipped += o.Skipped
	s.Filtered += o.Filtered
}

// selection describes what Options.Only asked for.
type selection struct {
	texture, text, any bool
	classIDs           map[int32]bool // explicit numeric classIDs (dumped generically unless 28/49)
}

func parseSelection(spec string) (selection, error) {
	sel := selection{classIDs: map[int32]bool{}}
	if strings.TrimSpace(spec) == "" {
		spec = DefaultOnly
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		switch part {
		case "":
		case "texture", "textures", "png":
			sel.texture = true
		case "text", "textasset", "textassets":
			sel.text = true
		case "any", "all", "other", "others":
			sel.any = true
		default:
			id, err := strconv.Atoi(part)
			if err != nil {
				return sel, fmt.Errorf("unknown -only value %q (expected texture, text, any, or a numeric ClassID)", part)
			}
			sel.classIDs[int32(id)] = true
		}
	}
	return sel, nil
}

func (s selection) wants(classID int32) bool {
	switch classID {
	case classTexture2D:
		return s.texture || s.classIDs[classID]
	case classTextAsset:
		return s.text || s.classIDs[classID]
	default:
		return s.any || s.classIDs[classID]
	}
}

// excludes is Options.ExcludeNames / ExcludeTextExts, normalised once.
type excludes struct {
	names    []string        // lower-case
	textExts map[string]bool // lower-case, with the leading dot
}

func newExcludes(opt Options) excludes {
	ex := excludes{textExts: map[string]bool{}}
	for _, n := range opt.ExcludeNames {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			ex.names = append(ex.names, n)
		}
	}
	for _, e := range opt.ExcludeTextExts {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			ex.textExts["."+strings.TrimPrefix(e, ".")] = true
		}
	}
	return ex
}

// byName returns the first excluded substring found in name.
func (e excludes) byName(name string) (rule string, hit bool) {
	lower := strings.ToLower(name)
	for _, n := range e.names {
		if strings.Contains(lower, n) {
			return n, true
		}
	}
	return "", false
}

// ValidateOptions reports a problem with opt without touching any bundle, so
// callers can fail before spending time on downloads.
func ValidateOptions(opt Options) error {
	_, err := parseSelection(opt.Only)
	return err
}

// fallbackVersion is used whenever a SerializedFile's own embedded Unity
// version string is blank or "0.0.0". It is process-wide; set it once at
// startup via SetFallbackUnityVersion, before any concurrent extraction.
var fallbackVersion [4]int

// SetFallbackUnityVersion sets the engine version assumed when a bundle's
// own embedded version is blank/"0.0.0" (common with Addressables' "Strip
// Unity Version" option). It is only a last resort, used when the bundle has
// no embedded TypeTree either. Call it once, before extracting.
func SetFallbackUnityVersion(v string) { fallbackVersion = parseUnityVersion(v) }

// ExtractFile reads a bundle from disk and extracts it into outDir.
func ExtractFile(path, outDir string, opt Options) (Stats, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Stats{}, fmt.Errorf("reading bundle: %w", err)
	}
	return Extract(raw, outDir, opt)
}

// Extract extracts the contents of a UnityFS bundle held in memory into
// outDir (created if needed). A malformed bundle yields an error rather than
// a panic. Per-object problems are not errors: they are counted in
// Stats.Skipped and described on opt.Log.
func Extract(raw []byte, outDir string, opt Options) (st Stats, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic while extracting: %v", r)
		}
	}()

	sel, err := parseSelection(opt.Only)
	if err != nil {
		return st, err
	}
	entries, err := ParseBundle(raw)
	if err != nil {
		return st, fmt.Errorf("parsing bundle container: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return st, fmt.Errorf("creating output dir: %w", err)
	}
	w := opt.Log
	if w == nil {
		w = io.Discard
	}
	return runExtract(entries, sel, strings.ToLower(opt.Filter), newExcludes(opt), outDir, w), nil
}

// ListFile is List for a bundle on disk.
func ListFile(path string, w io.Writer) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading bundle: %w", err)
	}
	return List(raw, w)
}

// List writes per-class object counts and sample names without extracting files.
func List(raw []byte, w io.Writer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic while listing: %v", r)
		}
	}()
	entries, err := ParseBundle(raw)
	if err != nil {
		return fmt.Errorf("parsing bundle container: %w", err)
	}
	fmt.Fprintf(w, "bundle contains %d entr%s\n", len(entries), plural(len(entries), "y", "ies"))
	runList(entries, w)
	return nil
}

func sortedEntryNames(entries map[string][]byte) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func runList(entries map[string][]byte, w io.Writer) {
	// Deterministic order, so repeated listings are diffable.
	for _, name := range sortedEntryNames(entries) {
		sf, perr := safeParseSerializedFile(entries[name])
		if perr != nil {
			continue
		}
		fmt.Fprintf(w, "\n%s: SerializedFile (Unity %v), %d objects\n", name, sf.UnityVersion, len(sf.Objects))

		type classInfo struct {
			count   int
			samples []string
		}
		byClass := map[int32]*classInfo{}
		for _, obj := range sf.Objects {
			ci := byClass[obj.ClassID]
			if ci == nil {
				ci = &classInfo{}
				byClass[obj.ClassID] = ci
			}
			ci.count++
			if len(ci.samples) < 5 {
				if n := bestEffortName(sf, obj); n != "" {
					ci.samples = append(ci.samples, n)
				}
			}
		}
		var ids []int32
		for id := range byClass {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			ci := byClass[id]
			line := fmt.Sprintf("  %-20s (ClassID %4d): %5d object%s", classIDName(id), id, ci.count, plural(ci.count, "", "s"))
			if len(ci.samples) > 0 {
				line += "   e.g. " + strings.Join(ci.samples, ", ")
			}
			fmt.Fprintln(w, line)
		}
	}
}

// bestEffortName tries to read an object's m_Name generically, for list
// display purposes only; returns "" on any failure rather than erroring.
func bestEffortName(sf *SerializedFile, obj objectEntry) (name string) {
	defer func() { recover() }()
	if int(obj.TypeID) < 0 || int(obj.TypeID) >= len(sf.Types) {
		return ""
	}
	nodes := sf.Types[obj.TypeID].Nodes
	if len(nodes) == 0 || obj.ByteStart < 0 || obj.ByteStart >= int64(len(sf.Data)) {
		return ""
	}
	r := NewReader(sf.Data, !sf.LittleEndian)
	r.SeekTo(int(obj.ByteStart))
	fields, err := readObjectFields(nodes, r)
	if err != nil {
		return ""
	}
	n, _ := fields["m_Name"].(string)
	return n
}

func runExtract(entries map[string][]byte, sel selection, filter string, ex excludes, outDir string, w io.Writer) Stats {
	usedNames := map[string]bool{}
	var st Stats

	// Deterministic order: when names collide, the "_<pathID>" suffix lands
	// on the same file on every run.
	for _, name := range sortedEntryNames(entries) {
		sf, perr := safeParseSerializedFile(entries[name])
		if perr != nil {
			continue
		}
		fmt.Fprintf(w, "%s: SerializedFile (Unity %v)\n", name, sf.UnityVersion)

		for _, obj := range sf.Objects {
			if !sel.wants(obj.ClassID) {
				continue
			}

			switch obj.ClassID {
			case classTexture2D:
				tex, terr := safeReadTexture2D(sf, obj, entries)
				if terr != nil {
					fmt.Fprintf(w, "  [skip] pathID %d: %v\n", obj.PathID, terr)
					st.Skipped++
					continue
				}
				if filter != "" && !strings.Contains(strings.ToLower(tex.Name), filter) {
					continue
				}
				if rule, hit := ex.byName(tex.Name); hit {
					fmt.Fprintf(w, "  [excluded] %s (name contains %q)\n", tex.Name, rule)
					st.Filtered++
					continue
				}
				// Dynamic fonts can ship an empty Texture2D placeholder; its
				// pixels are filled at runtime. It has no image to export.
				if tex.Width == 0 && tex.Height == 0 && len(tex.ImageData) == 0 {
					fmt.Fprintf(w, "  [empty] %s (0x0, no pixels)\n", tex.Name)
					st.Filtered++
					continue
				}
				if err := saveTexturePNG(tex, outDir, usedNames, w); err != nil {
					fmt.Fprintf(w, "  [skip] %q: %v\n", tex.Name, err)
					st.Skipped++
					continue
				}
				st.Textures++

			case classTextAsset:
				txt, terr := safeReadTextAsset(sf, obj)
				if terr != nil {
					fmt.Fprintf(w, "  [skip] pathID %d: %v\n", obj.PathID, terr)
					st.Skipped++
					continue
				}
				if filter != "" && !strings.Contains(strings.ToLower(txt.Name), filter) {
					continue
				}
				if rule, hit := ex.byName(txt.Name); hit {
					fmt.Fprintf(w, "  [excluded] %s (name contains %q)\n", txt.Name, rule)
					st.Filtered++
					continue
				}
				if ext := textAssetFileName(txt); ex.textExts[strings.ToLower(filepath.Ext(ext))] {
					fmt.Fprintf(w, "  [excluded] %s (%s file)\n", txt.Name, strings.ToLower(filepath.Ext(ext)))
					st.Filtered++
					continue
				}
				if err := saveTextAsset(txt, outDir, usedNames, w); err != nil {
					fmt.Fprintf(w, "  [skip] %q: %v\n", txt.Name, err)
					st.Skipped++
					continue
				}
				st.Texts++

			default:
				objName, js, derr := safeReadGenericObjectJSON(sf, obj)
				if derr != nil {
					fmt.Fprintf(w, "  [skip] %s pathID %d: %v\n", classIDName(obj.ClassID), obj.PathID, derr)
					st.Skipped++
					continue
				}
				if filter != "" && !strings.Contains(strings.ToLower(objName), filter) {
					continue
				}
				if rule, hit := ex.byName(objName); hit {
					fmt.Fprintf(w, "  [excluded] %s (name contains %q)\n", objName, rule)
					st.Filtered++
					continue
				}
				if err := saveGenericJSON(objName, obj, js, outDir, usedNames, w); err != nil {
					fmt.Fprintf(w, "  [skip] %q: %v\n", objName, err)
					st.Skipped++
					continue
				}
				st.Dumped++
			}
		}
	}

	fmt.Fprintf(w, "done: %d texture%s, %d text asset%s, %d other object%s dumped, %d skipped, %d excluded\n",
		st.Textures, plural(st.Textures, "", "s"), st.Texts, plural(st.Texts, "", "s"),
		st.Dumped, plural(st.Dumped, "", "s"), st.Skipped, st.Filtered)
	return st
}

func safeParseSerializedFile(data []byte) (sf *SerializedFile, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return parseSerializedFile(data, fallbackVersion)
}

func safeReadTexture2D(sf *SerializedFile, obj objectEntry, resources map[string][]byte) (tex *Texture2DInfo, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return readTexture2D(sf, obj, resources)
}

func safeReadTextAsset(sf *SerializedFile, obj objectEntry) (txt *TextAssetInfo, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return readTextAsset(sf, obj)
}

func safeReadGenericObjectJSON(sf *SerializedFile, obj objectEntry) (name string, js []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return readGenericObjectJSON(sf, obj)
}

func saveTexturePNG(tex *Texture2DInfo, outDir string, usedNames map[string]bool, w io.Writer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic decoding %s: %v", textureFormatName(tex.Format), r)
		}
	}()

	img, derr := decodeTexture(tex.Format, tex.ImageData, int(tex.Width), int(tex.Height))
	if derr != nil {
		return fmt.Errorf("%s: %w", textureFormatName(tex.Format), derr)
	}

	rawName := tex.Name
	if rawName == "" {
		rawName = fmt.Sprintf("texture_%d", tex.PathID)
	}
	clean := sanitizeFilename(rawName)
	fname := clean + ".png"
	if usedNames[fname] {
		fname = fmt.Sprintf("%s_%d.png", clean, tex.PathID)
	}
	usedNames[fname] = true

	out, cerr := os.Create(filepath.Join(outDir, fname))
	if cerr != nil {
		return cerr
	}
	defer out.Close()

	if perr := png.Encode(out, img); perr != nil {
		return perr
	}
	fmt.Fprintf(w, "  %s (%dx%d, %s) -> %s\n", tex.Name, tex.Width, tex.Height, textureFormatName(tex.Format), fname)
	return nil
}

// saveTextAsset writes a TextAsset's script content out as-is (whatever
// bytes it contains - text, JSON, CSV, or arbitrary binary). If the asset's
// own name doesn't already carry a file extension, one is guessed from the
// content (.json for JSON-looking text, .txt otherwise).
func saveTextAsset(txt *TextAssetInfo, outDir string, usedNames map[string]bool, w io.Writer) error {
	fname := uniqueName(textAssetFileName(txt), txt.PathID, usedNames)

	if err := os.WriteFile(filepath.Join(outDir, fname), []byte(txt.Script), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(w, "  %s (%d bytes) -> %s\n", txt.Name, len(txt.Script), fname)
	return nil
}

// saveGenericJSON writes a generically-dumped object's fields to disk. The
// ClassID is baked into the filename since names collide freely across
// unrelated classes (a Material and a GameObject can share a name).
func saveGenericJSON(name string, obj objectEntry, js []byte, outDir string, usedNames map[string]bool, w io.Writer) error {
	rawName := name
	if rawName == "" {
		rawName = fmt.Sprintf("object_%d", obj.PathID)
	}
	clean := fmt.Sprintf("%s.%s.json", sanitizeFilename(rawName), sanitizeFilename(classIDName(obj.ClassID)))
	fname := uniqueName(clean, obj.PathID, usedNames)

	if err := os.WriteFile(filepath.Join(outDir, fname), js, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(w, "  [%s] %s (%d bytes) -> %s\n", classIDName(obj.ClassID), name, len(js), fname)
	return nil
}

// textAssetFileName is the file name a TextAsset is written under (before
// de-duplication): its own name, plus a guessed extension if it has none.
func textAssetFileName(txt *TextAssetInfo) string {
	rawName := txt.Name
	if rawName == "" {
		rawName = fmt.Sprintf("textasset_%d", txt.PathID)
	}
	clean := sanitizeFilename(rawName)
	if filepath.Ext(clean) == "" {
		clean += guessTextExtension(txt.Script)
	}
	return clean
}

func uniqueName(fname string, pathID int64, usedNames map[string]bool) string {
	if !usedNames[fname] {
		usedNames[fname] = true
		return fname
	}
	ext := filepath.Ext(fname)
	base := strings.TrimSuffix(fname, ext)
	deduped := fmt.Sprintf("%s_%d%s", base, pathID, ext)
	usedNames[deduped] = true
	return deduped
}

func guessTextExtension(content string) string {
	trimmed := strings.TrimSpace(strings.TrimPrefix(content, "\ufeff"))
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return ".json"
	}
	return ".txt"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
