// Package resolver obtains release folder names from manual input, files,
// commands, or a local game installation.
package resolver

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Resolver returns the newest release folder name.
type Resolver interface {
	// Name identifies the resolver in logs.
	Name() string
	// Resolve returns a name that satisfies Validate.
	Resolve(ctx context.Context) (string, error)
}

var versionRe = regexp.MustCompile(`^s\d{8}_[A-Za-z0-9_-]+$`)

// Validate checks that v has the shape of a release folder name. This also
// keeps odd input ("../x", "a/b") out of the URLs and file paths built from
// it.
func Validate(v string) error {
	if !versionRe.MatchString(v) {
		return fmt.Errorf("%q is not a release folder name (expected like s20260910_fDbU_dzxziNuVLKe_Miv)", v)
	}
	return nil
}

func validated(v string) (string, error) {
	v = strings.TrimSpace(v)
	if err := Validate(v); err != nil {
		return "", err
	}
	return v, nil
}

// Manual returns a fixed, user-supplied name (the -version flag).
type Manual string

func (m Manual) Name() string                            { return "manual" }
func (m Manual) Resolve(context.Context) (string, error) { return validated(string(m)) }

// File reads the release name from the first non-empty line of a text file.
type File struct{ Path string }

func (f File) Name() string { return "file " + f.Path }
func (f File) Resolve(context.Context) (string, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return "", err
	}
	line, ok := lastOrFirstLine(data, true)
	if !ok {
		return "", fmt.Errorf("%s is empty", f.Path)
	}
	return validated(line)
}

// Command uses the last non-empty stdout line of a shell command as the
// release name. Stderr is passed through.
type Command struct {
	Cmdline string
	Timeout time.Duration // 0 = 2 minutes
}

func (c Command) Name() string { return "command " + c.Cmdline }
func (c Command) Resolve(ctx context.Context) (string, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", c.Cmdline)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", c.Cmdline)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("running %q: %w", c.Cmdline, err)
	}
	line, ok := lastOrFirstLine(stdout.Bytes(), false)
	if !ok {
		return "", fmt.Errorf("%q printed nothing", c.Cmdline)
	}
	return validated(line)
}

// GameDir looks into a local Steam install for the newest release folder.
type GameDir struct{ Dir string }

func (g GameDir) Name() string { return "game dir " + g.Dir }
func (g GameDir) Resolve(context.Context) (string, error) {
	aa, err := AADir(g.Dir)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(aa)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries { // folder or file: both carry the name
		if versionRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no release folder (sYYYYMMDD_...) in %s", aa)
	}
	sort.Strings(names) // sYYYYMMDD prefix: lexical order is chronological
	return names[len(names)-1], nil
}

// AADir finds the StreamingAssets/aa directory of a game install. dir may be
// the install root, StreamingAssets, or aa itself.
func AADir(dir string) (string, error) {
	cands := []string{
		filepath.Join(dir, "LimbusCompany_Data", "StreamingAssets", "aa"),
		filepath.Join(dir, "StreamingAssets", "aa"),
		filepath.Join(dir, "aa"),
		dir,
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() && (c != dir || hasVersionEntry(c)) {
			return c, nil
		}
	}
	return "", fmt.Errorf("no StreamingAssets/aa directory under %s (looked in: %s)", dir, strings.Join(cands, ", "))
}

func hasVersionEntry(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if versionRe.MatchString(e.Name()) {
			return true
		}
	}
	return false
}

// ErrNotImplemented is returned by resolvers that are only a placeholder.
var ErrNotImplemented = errors.New("not implemented yet")

// Steam is an unimplemented depot-based release resolver.
type Steam struct{}

func (Steam) Name() string { return "steam" }
func (Steam) Resolve(context.Context) (string, error) {
	return "", fmt.Errorf("steam resolver: %w (use -version, -version-file or -version-cmd for now)", ErrNotImplemented)
}

// lastOrFirstLine returns the first (first=true) or last (first=false)
// non-empty trimmed line of data.
func lastOrFirstLine(data []byte, first bool) (string, bool) {
	var found string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		found = line
		if first {
			return found, true
		}
	}
	return found, found != ""
}
