// Command package-release packages prebuilt binaries for distribution.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type target struct {
	platform string
	ext      string
}

var targets = []target{
	{"linux-amd64", ""},
	{"linux-arm64", ""},
	{"windows-amd64", ".exe"},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	platform := flag.String("platform", "all", "all, linux-amd64, linux-arm64, or windows-amd64")
	dist := flag.String("dist", "dist", "directory containing the compiled binaries")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flag.Args(), " "))
	}
	var selected []target
	for _, t := range targets {
		if *platform == "all" || *platform == t.platform {
			selected = append(selected, t)
			path := filepath.Join(*dist, "limbus-extractor-"+t.platform+t.ext)
			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("build the binaries first: %w", err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("not a regular binary file: %s", path)
			}
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("unsupported platform %q", *platform)
	}
	out := filepath.Join(*dist, "releases")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, t := range selected {
		path := filepath.Join(out, "limbus-extractor-"+t.platform+".zip")
		if err := pack(*dist, path, t); err != nil {
			return err
		}
		fmt.Println(path)
	}
	var checksums strings.Builder
	for _, t := range targets {
		name := "limbus-extractor-" + t.platform + ".zip"
		f, err := os.Open(filepath.Join(out, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		h := sha256.New()
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Fprintf(&checksums, "%x  %s\n", h.Sum(nil), name)
	}
	return os.WriteFile(filepath.Join(out, "SHA256SUMS"), []byte(checksums.String()), 0o644)
}

func pack(dist, dest string, t target) error {
	f, err := os.CreateTemp(filepath.Dir(dest), ".release-*.zip")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	zw := zip.NewWriter(f)
	add := func(name string, mode os.FileMode, src io.Reader) error {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now().UTC()}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, src)
		return err
	}
	writeErr := func() error {
		binary, err := os.Open(filepath.Join(dist, "limbus-extractor-"+t.platform+t.ext))
		if err != nil {
			return err
		}
		defer binary.Close()
		if err := add("limbus-extractor"+t.ext, 0o755, binary); err != nil {
			return err
		}
		command := "./limbus-extractor"
		if t.ext != "" {
			command = `.\limbus-extractor.exe`
		}
		readme := fmt.Sprintf(`Limbus Asset Extractor

Extract this archive into its own directory and open a terminal there.
Source code, Go, Python, Docker, and an installed game are not required.

Show help:
  %s help

Run with a known release name:
  %s run -version sYYYYMMDD_RELEASE_NAME

Replace sYYYYMMDD_RELEASE_NAME with the actual release name.
Find it on SteamDB: https://steamdb.info/depot/1973531/
In Files, find a path under LimbusCompany_Data/StreamingAssets/aa/
and copy the complete sYYYYMMDD_... folder name, not the numeric Manifest ID.
For older releases: https://steamdb.info/depot/1973531/manifests/
Added paths can show the new release; removed paths can show the previous one.

The first normal run records a baseline without downloading bundles.
Later runs compare against it. Use -full to extract selected bundles now.
Selection defaults to p<digits>, e<digits>, and s8_ bundles.

Show all run options:
  %s run -h

State, cache, and extracted files go into work/ in your current directory.
Use -work PATH to choose another location. Keep that directory between updates.
`, command, command, command)
		return add("README.txt", 0o644, strings.NewReader(readme))
	}()
	zipErr := zw.Close()
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if zipErr != nil {
		return zipErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, dest)
}
