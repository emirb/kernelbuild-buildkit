package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSeed installs a pullSeed that materializes the given files into the
// build dir (a seed tarball's effect) and reports found/err. A nil files map
// with found=true still counts as a pull: the caller's stamp check decides.
func fakeSeed(t *testing.T, found bool, err error, files map[string]string) {
	t.Helper()
	old := pullSeed
	pullSeed = func(context.Context, *seedCfg) (int64, bool, error) {
		if err != nil || !found {
			return 0, found, err
		}
		var n int64
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(buildDir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			n += int64(len(content))
		}
		return n, true, nil
	}
	t.Cleanup(func() { pullSeed = old })
}

// sourceServer serves one tarball whose sha the test pins through SRC_ID and
// SRC_SHA256; returns the Makefile content it carries.
func sourceServer(t *testing.T) (makefile string) {
	t.Helper()
	makefile = "from-tarball:\n"
	tarball, sum := gzTarSHA(t, [3]string{"linux-9.9/Makefile", "f", makefile})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("APPLY_PATCHES", "0")
	t.Setenv("SRC_LOCAL", "")
	t.Setenv("SRC_URL", srv.URL+"/linux-9.9.tar.gz")
	t.Setenv("SRC_ID", sum)
	t.Setenv("SRC_SHA256", sum)
	return makefile
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		return ""
	}
	return string(b)
}

// TestPrepareTreeSeed covers the cold-mount decisions in prepareTree: a seed
// whose stamp matches this build is adopted as-is, a seed with a foreign
// stamp is discarded and the tarball extracted instead, an absent seed falls
// through to extraction, and a transport error is fatal.
func TestPrepareTreeSeed(t *testing.T) {
	seed := &seedCfg{url: "https://h/b", key: "k"}

	t.Run("matching-stamp-is-adopted", func(t *testing.T) {
		build, _, _ := withDirs(t)
		t.Chdir(build)
		sourceServer(t)
		want, err := stampWant()
		if err != nil {
			t.Fatal(err)
		}
		fakeSeed(t, true, nil, map[string]string{"Makefile": "from-seed:\n", ".kbf-stamp.seed": want})
		if err := prepareTree(context.Background(), seed); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, "Makefile"); got != "from-seed:\n" {
			t.Fatalf("Makefile = %q, want the seed's (the tarball must not have been extracted over it)", got)
		}
		if readStamp() != want {
			t.Errorf("stamp = %q, want %q", readStamp(), want)
		}
		if _, err := os.Stat(".kbf-stamp.seed"); !os.IsNotExist(err) {
			t.Error("quarantined seed stamp left behind")
		}
	})

	t.Run("foreign-stamp-is-discarded", func(t *testing.T) {
		build, _, _ := withDirs(t)
		t.Chdir(build)
		makefile := sourceServer(t)
		fakeSeed(t, true, nil, map[string]string{"Makefile": "from-seed:\n", "stale.o": "x", ".kbf-stamp.seed": "src=OTHER patches=none"})
		if err := prepareTree(context.Background(), seed); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, "Makefile"); got != makefile {
			t.Fatalf("Makefile = %q, want the tarball's %q", got, makefile)
		}
		if _, err := os.Stat("stale.o"); !os.IsNotExist(err) {
			t.Error("foreign seed's object survived the discard")
		}
		want, _ := stampWant()
		if readStamp() != want {
			t.Errorf("stamp = %q, want %q", readStamp(), want)
		}
	})

	t.Run("absent-seed-extracts", func(t *testing.T) {
		build, _, _ := withDirs(t)
		t.Chdir(build)
		makefile := sourceServer(t)
		fakeSeed(t, false, nil, nil)
		if err := prepareTree(context.Background(), seed); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, "Makefile"); got != makefile {
			t.Fatalf("Makefile = %q, want the tarball's", got)
		}
	})

	t.Run("transport-error-is-fatal", func(t *testing.T) {
		build, _, _ := withDirs(t)
		t.Chdir(build)
		sourceServer(t)
		fakeSeed(t, false, errors.New("connection reset"), nil)
		err := prepareTree(context.Background(), seed)
		if err == nil || !strings.Contains(err.Error(), "seed pull") {
			t.Fatalf("seed error: %v", err)
		}
	})
}

// TestPrepareTreeWarmAndStale: a warm tree with a matching stamp is reused
// without touching the source at all, and a mount holding stale objects but
// no Makefile (an extract killed midway) is cleared before hydrating.
func TestPrepareTreeWarmAndStale(t *testing.T) {
	t.Run("warm-tree-needs-no-source", func(t *testing.T) {
		build, _, _ := withDirs(t)
		t.Chdir(build)
		t.Setenv("APPLY_PATCHES", "0")
		t.Setenv("SRC_LOCAL", "")
		t.Setenv("SRC_URL", "https://unreachable.invalid/linux.tar.gz")
		t.Setenv("SRC_ID", "warm")
		t.Setenv("SRC_SHA256", "")
		want, _ := stampWant()
		if err := os.WriteFile("Makefile", []byte("warm:\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(".kbf-stamp", []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := prepareTree(context.Background(), nil); err != nil {
			t.Fatalf("warm tree reached for the source: %v", err)
		}
		if readFile(t, "Makefile") != "warm:\n" {
			t.Error("warm tree was replaced")
		}
	})

	t.Run("stale-objects-cleared-before-extract", func(t *testing.T) {
		build, _, _ := withDirs(t)
		t.Chdir(build)
		makefile := sourceServer(t)
		if err := os.WriteFile("stale.o", []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := prepareTree(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat("stale.o"); !os.IsNotExist(err) {
			t.Error("stale object survived into the fresh tree")
		}
		if readFile(t, "Makefile") != makefile {
			t.Error("tree not extracted")
		}
	})
}

// loggingMake puts a `make` on PATH that records its argv, one line per
// invocation, and creates .config when asked for a defconfig-style target.
func loggingMake(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "make.log")
	script := "#!/bin/sh\necho \"$*\" >> \"$FAKE_MAKE_LOG\"\n" +
		"if [ -n \"$FAKE_MAKE_FAIL\" ]; then exit 1; fi\n" +
		"case \"$1\" in olddefconfig) : ;; *defconfig|tinyconfig) printf 'CONFIG_BASE=y\\n' > .config ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "make"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_MAKE_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// TestConfigureBaseMake: with BASE_MAKE the tree's own config targets run
// first (with ARCH/CROSS_COMPILE), the context config is appended as a
// fragment, olddefconfig follows, and an identical input then skips all of it.
func TestConfigureBaseMake(t *testing.T) {
	logPath := loggingMake(t)
	build, _, _ := withDirs(t)
	t.Chdir(build)
	t.Setenv("BASE_MAKE", "x86_64_defconfig kvm_guest.config")
	t.Setenv("KARCH", "x86_64")
	t.Setenv("CROSS_COMPILE", "x86_64-linux-gnu-")
	withConfig(t, "CONFIG_A=y\n")

	if err := configure(); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, ".config"), "CONFIG_BASE=y\n\nCONFIG_A=y\n"; got != want {
		t.Errorf(".config = %q, want base config plus fragment %q", got, want)
	}
	calls := strings.Split(strings.TrimSpace(readFile(t, logPath)), "\n")
	if len(calls) != 2 {
		t.Fatalf("make calls = %q, want base-make then olddefconfig", calls)
	}
	if !strings.HasPrefix(calls[0], "x86_64_defconfig kvm_guest.config") || !strings.Contains(calls[0], "ARCH=x86_64") || !strings.Contains(calls[0], "CROSS_COMPILE=x86_64-linux-gnu-") {
		t.Errorf("base-make call = %q", calls[0])
	}
	if !strings.HasPrefix(calls[1], "olddefconfig") || !strings.Contains(calls[1], "ARCH=x86_64") {
		t.Errorf("olddefconfig call = %q", calls[1])
	}

	// Same fragment, same base: nothing runs.
	if err := configure(); err != nil {
		t.Fatal(err)
	}
	if again := strings.Split(strings.TrimSpace(readFile(t, logPath)), "\n"); len(again) != 2 {
		t.Errorf("unchanged config re-ran make: %q", again)
	}

	// Same fragment over a different base is a different config: it re-runs.
	t.Setenv("BASE_MAKE", "x86_64_defconfig")
	if err := configure(); err != nil {
		t.Fatal(err)
	}
	if again := strings.Split(strings.TrimSpace(readFile(t, logPath)), "\n"); len(again) != 4 {
		t.Errorf("base change did not re-run make: %q", again)
	}

	// A failing base target is reported as such, and the skip marker is not
	// left pointing at the new input.
	t.Setenv("FAKE_MAKE_FAIL", "1")
	withConfig(t, "CONFIG_B=y\n")
	err := configure()
	if err == nil || !strings.Contains(err.Error(), "base-make") {
		t.Fatalf("base-make failure: %v", err)
	}
	if _, err := os.Stat(".kbf-config-src"); !os.IsNotExist(err) {
		t.Error("skip marker survived a failed configure")
	}
}

// TestConfigureEmptyFragmentOverBase: an empty context config over BASE_MAKE
// appends nothing, so .config is exactly what the base target produced.
func TestConfigureEmptyFragmentOverBase(t *testing.T) {
	loggingMake(t)
	build, _, _ := withDirs(t)
	t.Chdir(build)
	t.Setenv("BASE_MAKE", "tinyconfig")
	t.Setenv("KARCH", "")
	t.Setenv("CROSS_COMPILE", "")
	withConfig(t, "# nothing\n\n")
	if err := configure(); err != nil {
		t.Fatal(err)
	}
	// "# nothing" is not blank, so it is appended; only whitespace-only input skips.
	withConfig(t, "  \n")
	t.Setenv("BASE_MAKE", "defconfig")
	if err := configure(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, ".config"); got != "CONFIG_BASE=y\n" {
		t.Errorf(".config = %q, want the bare base config", got)
	}
}
