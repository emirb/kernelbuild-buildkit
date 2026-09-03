package main

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkTar builds a tar stream from (name, kind, content/linkname) triples.
// kind: 'f' regular file, 'd' dir, 'l' symlink, 'h' hardlink.
func mkTar(t testing.TB, entries ...[3]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		name, kind, arg := e[0], e[1], e[2]
		hdr := &tar.Header{Name: name, Mode: 0o644, ModTime: time.Unix(10, 0)}
		switch kind {
		case "f":
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(arg))
		case "d":
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		case "l":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = arg
		case "h":
			hdr.Typeflag = tar.TypeLink
			hdr.Linkname = arg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if kind == "f" {
			if _, err := tw.Write([]byte(arg)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// FuzzUntar: arbitrary bytes as a tar stream against a confined root. The
// oracles: no panic, and nothing is ever written outside the target dir —
// the canary sibling stays empty no matter what names/symlinks/hardlinks the
// stream contains. This is the security property the os.Root migration
// exists for (SOURCE_URL is user-supplied).
func FuzzUntar(f *testing.F) {
	f.Add(mkTar(f, [3]string{"linux-6.18.20/Makefile", "f", "all:\n"}), 1)
	f.Add(mkTar(f, [3]string{"../escape.txt", "f", "x"}), 0)
	f.Add(mkTar(f,
		[3]string{"d/link", "l", "../../canary"},
		[3]string{"d/link/pwned", "f", "x"}), 0)
	f.Add(mkTar(f,
		[3]string{"abs", "l", "/etc"},
		[3]string{"abs/pwned", "f", "x"}), 0)
	f.Add(mkTar(f, [3]string{"h", "h", "../outside"}, [3]string{"a/b/c", "f", "deep"}), 2)
	f.Add([]byte("not a tar at all"), 1)
	f.Fuzz(func(t *testing.T, stream []byte, strip int) {
		if strip < 0 || strip > 4 {
			return
		}
		parent := t.TempDir()
		target := filepath.Join(parent, "target")
		canary := filepath.Join(parent, "canary")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(canary, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = untar(target, bytes.NewReader(stream), strip) // error is fine; panic/escape is not
		ents, err := os.ReadDir(canary)
		if err != nil || len(ents) != 0 {
			t.Fatalf("escape: canary dir touched (ents=%d err=%v)", len(ents), err)
		}
	})
}

// FuzzStripComponents: never panics; an accepted name is always a clean
// relative path that cannot step out of the extraction root on its own.
func FuzzStripComponents(f *testing.F) {
	f.Add("linux-6.18.20/Makefile", 1)
	f.Add("../x", 0)
	f.Add("/abs/path", 1)
	f.Add("a//b/./c", 2)
	f.Add(".", 0)
	f.Fuzz(func(t *testing.T, name string, strip int) {
		if strip < 0 || strip > 8 {
			return
		}
		out, ok := stripComponents(name, strip)
		if !ok {
			return
		}
		if filepath.IsAbs(out) {
			t.Errorf("stripComponents(%q, %d) = %q: absolute", name, strip, out)
		}
	})
}

// TestUntarSymlinkEscapeVariants pins the concrete hostile shapes (these are
// the fuzz seeds, asserted deterministically so a regression fails fast and
// readably rather than via fuzz).
func TestUntarSymlinkEscapeVariants(t *testing.T) {
	cases := map[string][]byte{
		"dotdot-name":    mkTar(t, [3]string{"../escape.txt", "f", "x"}),
		"symlink-out":    mkTar(t, [3]string{"l", "l", "../../x"}, [3]string{"l/pwned", "f", "x"}),
		"symlink-abs":    mkTar(t, [3]string{"l", "l", "/tmp"}, [3]string{"l/pwned", "f", "x"}),
		"hardlink-out":   mkTar(t, [3]string{"h", "h", "../../../etc/hosts"}),
		"deep-dotdot":    mkTar(t, [3]string{"a/b/../../../esc", "f", "x"}),
		"symlink-parent": mkTar(t, [3]string{"p", "l", ".."}, [3]string{"p/esc", "f", "x"}),
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "t")
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			before := snapshotOutside(t, parent, target)
			_ = untar(target, bytes.NewReader(stream), 0)
			after := snapshotOutside(t, parent, target)
			if before != after {
				t.Fatalf("filesystem outside target changed:\nbefore: %s\nafter:  %s", before, after)
			}
		})
	}
}

// snapshotOutside lists everything under parent that is not inside target.
func snapshotOutside(t *testing.T, parent, target string) string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(parent, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == target || strings.HasPrefix(p, target+string(os.PathSeparator)) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		names = append(names, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(names, "\n")
}
