package extractor

import "regexp"

// parseUnityVersion turns "2022.3.22f1" into [2022, 3, 22, 0]. Non-numeric
// build-type letters (a/b/f/p/c...) are ignored, matching what the version
// gates in Texture2D parsing actually need (major/minor/patch numbers).
func parseUnityVersion(s string) [4]int {
	var out [4]int
	if s == "" {
		return out
	}
	idx := 0
	cur := 0
	haveDigit := false
	flush := func() {
		if idx < len(out) {
			if haveDigit {
				out[idx] = cur
			}
			idx++
		}
		cur = 0
		haveDigit = false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			cur = cur*10 + int(c-'0')
			haveDigit = true
		case c == '.':
			flush()
		default:
			// First non-digit, non-dot character (e.g. 'f' in "22.3.22f1")
			// ends the numeric version part entirely.
			flush()
			return out
		}
	}
	flush()
	return out
}

var invalidFilenameChars = regexp.MustCompile(`[\\/*?:"<>|]`)

// sanitizeFilename replaces path separators and reserved filename punctuation.
func sanitizeFilename(name string) string {
	if name == "" {
		return name
	}
	return invalidFilenameChars.ReplaceAllString(name, "_")
}
