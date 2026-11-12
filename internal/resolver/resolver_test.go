package resolver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const sample = "s20260910_fDbU_dzxziNuVLKe_Miv" // the example from the game's own folder

func TestValidate(t *testing.T) {
	for _, ok := range []string{
		sample,
		"s20260806_ubyNQbVU6Gsz_QkytBRo",
		"s20260917_XozhYgm77guBS5Wgmims",
	} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"", "s2026_x", "20260910_abc", "S20260910_abc", "s20260910_", "s20260910",
		"s20260910_a/b", "s20260910_../x", `s20260910_a\b`, "s20260910_a b", "../s20260910_abc",
		"s20260910_abc\n", // trailing newline must not sneak through
	} {
		if err := Validate(bad); err == nil {
			t.Errorf("Validate(%q) = nil, want an error", bad)
		}
	}
}

func TestManual(t *testing.T) {
	if v, err := Manual("  " + sample + " ").Resolve(context.Background()); err != nil || v != sample {
		t.Fatalf("got %q, %v", v, err)
	}
	if _, err := Manual("nope").Resolve(context.Background()); err == nil {
		t.Fatal("expected a validation error")
	}
}

func TestFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "version.txt")
	os.WriteFile(p, []byte("\r\n\n  "+sample+"  \r\nsecond line ignored\n"), 0o644)
	if v, err := (File{p}).Resolve(context.Background()); err != nil || v != sample {
		t.Fatalf("got %q, %v", v, err)
	}
	os.WriteFile(p, []byte("\n  \n"), 0o644)
	if _, err := (File{p}).Resolve(context.Background()); err == nil {
		t.Fatal("empty file should fail")
	}
	if _, err := (File{filepath.Join(t.TempDir(), "missing")}).Resolve(context.Background()); err == nil {
		t.Fatal("missing file should fail")
	}
}

func TestCommandTakesLastLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	// Only the last non-empty stdout line should be treated as the version.
	v, err := Command{Cmdline: "echo checking depot...; echo " + sample}.Resolve(context.Background())
	if err != nil || v != sample {
		t.Fatalf("got %q, %v", v, err)
	}
	if _, err := (Command{Cmdline: "exit 3"}).Resolve(context.Background()); err == nil {
		t.Error("non-zero exit should fail")
	}
	if _, err := (Command{Cmdline: "true"}).Resolve(context.Background()); err == nil {
		t.Error("silent command should fail")
	}
	if _, err := (Command{Cmdline: "echo not-a-version"}).Resolve(context.Background()); err == nil {
		t.Error("garbage output should fail validation")
	}
}

func makeGame(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	aa := filepath.Join(root, "LimbusCompany_Data", "StreamingAssets", "aa")
	if err := os.MkdirAll(aa, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(aa, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(aa, "catalog.json"), []byte("{}"), 0o644) // noise
	os.MkdirAll(filepath.Join(aa, "StandaloneWindows64"), 0o755)         // noise
	return root
}

func TestGameDirPicksNewest(t *testing.T) {
	root := makeGame(t, "s20260806_ubyNQbVU6Gsz_QkytBRo", sample, "s20250101_old_one")
	v, err := GameDir{root}.Resolve(context.Background())
	if err != nil || v != sample {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestGameDirAcceptsDeeperRoots(t *testing.T) {
	root := makeGame(t, sample)
	for _, dir := range []string{
		root,
		filepath.Join(root, "LimbusCompany_Data", "StreamingAssets"),
		filepath.Join(root, "LimbusCompany_Data", "StreamingAssets", "aa"),
	} {
		got, err := (GameDir{dir}).Resolve(context.Background())
		if err != nil || got != sample {
			t.Errorf("dir %s: got %q, %v", dir, got, err)
		}
	}
}

func TestGameDirAcceptsFilesNotJustFolders(t *testing.T) {
	root := t.TempDir()
	aa := filepath.Join(root, "LimbusCompany_Data", "StreamingAssets", "aa")
	os.MkdirAll(aa, 0o755)
	os.WriteFile(filepath.Join(aa, sample), nil, 0o644)
	if v, err := (GameDir{root}).Resolve(context.Background()); err != nil || v != sample {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestGameDirErrors(t *testing.T) {
	if _, err := (GameDir{t.TempDir()}).Resolve(context.Background()); err == nil {
		t.Error("empty dir should fail")
	}
	if _, err := (GameDir{makeGame(t)}).Resolve(context.Background()); err == nil {
		t.Error("aa without any release folder should fail")
	}
}

func TestAADir(t *testing.T) {
	root := makeGame(t, sample)
	got, err := AADir(root)
	if err != nil || filepath.Base(got) != "aa" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSteamStubIsHonestAboutBeingAStub(t *testing.T) {
	_, err := Steam{}.Resolve(context.Background())
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("want ErrNotImplemented, got %v", err)
	}
}
