package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"
)

// TestUntarHardlink: kernel tarballs contain legitimate in-tree hardlinks; a
// silently skipped TypeLink branch must fail here, not in a 250s build.
func TestUntarHardlink(t *testing.T) {
	dir := t.TempDir()
	stream := mkTar(t,
		[3]string{"a.txt", "f", "payload"},
		[3]string{"b.txt", "h", "a.txt"},
	)
	if err := untar(dir, bytes.NewReader(stream), 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "b.txt"))
	if err != nil || string(b) != "payload" {
		t.Fatalf("hardlink content = %q, %v", b, err)
	}
	sa, _ := os.Stat(filepath.Join(dir, "a.txt"))
	sb, _ := os.Stat(filepath.Join(dir, "b.txt"))
	if !os.SameFile(sa, sb) {
		t.Error("b.txt is not a hardlink of a.txt")
	}
}

func TestUntarSymlinkThenFileReplaces(t *testing.T) {
	// A symlink entry followed by a regular file at the same path must
	// REPLACE the link (tar semantics), not write through it (an O_TRUNC
	// open would follow the link and overwrite its target).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "victim"), []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := mkTar(t,
		[3]string{"lnk", "l", "victim"},
		[3]string{"lnk", "f", "overwrite"},
	)
	if err := untar(dir, bytes.NewReader(stream), 0); err != nil {
		t.Fatal(err)
	}
	v, _ := os.ReadFile(filepath.Join(dir, "victim"))
	if string(v) != "untouched" {
		t.Fatalf("write went THROUGH the symlink: victim = %q", v)
	}
	l, _ := os.ReadFile(filepath.Join(dir, "lnk"))
	if string(l) != "overwrite" {
		t.Fatalf("lnk = %q, want the regular file", l)
	}
}

func TestUntarGz(t *testing.T) {
	mt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	writeHdr := func(hdr *tar.Header, body []byte) {
		t.Helper()
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if body != nil {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeHdr(&tar.Header{Name: "linux-6.18.20/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mt}, nil)
	writeHdr(&tar.Header{Name: "linux-6.18.20/Makefile", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5, ModTime: mt}, []byte("hello"))
	writeHdr(&tar.Header{Name: "linux-6.18.20/scripts/run.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: 3, ModTime: mt}, []byte("#!x"))
	writeHdr(&tar.Header{Name: "linux-6.18.20/link", Typeflag: tar.TypeSymlink, Linkname: "Makefile", ModTime: mt}, nil)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	zr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := untar(dir, zr, 1); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil || string(b) != "hello" {
		t.Errorf("Makefile content = %q, %v", b, err)
	}
	st, err := os.Stat(filepath.Join(dir, "scripts/run.sh"))
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode = %v, %v (want 0755)", st.Mode(), err)
	}
	if st, err := os.Stat(filepath.Join(dir, "Makefile")); err != nil || !st.ModTime().Equal(mt) {
		t.Errorf("Makefile mtime = %v, %v (want %v)", st.ModTime(), err, mt)
	}
	if ln, err := os.Readlink(filepath.Join(dir, "link")); err != nil || ln != "Makefile" {
		t.Errorf("symlink = %q, %v", ln, err)
	}
}

// TestUntarRejectsEscape asserts the two classic tar-slip attacks are dead
// under os.Root: a deep ../ traversal name, and writing THROUGH a symlink
// member that points outside the tree. Both matter once SOURCE_URL (hence the
// tarball) is user-supplied.
func TestUntarRejectsEscape(t *testing.T) {
	outside := t.TempDir()

	// (a) name traversal surviving Clean+strip: a/../../../evil -> ../../evil
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "a/../../../evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := untar(t.TempDir(), &buf, 1); err == nil {
		t.Error("../ traversal entry was accepted")
	}

	// (b) symlink slip: top/d -> <outside>, then top/d/pwned written through it
	buf.Reset()
	tw = tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "top/d", Typeflag: tar.TypeSymlink, Linkname: outside}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "top/d/pwned", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	err := untar(t.TempDir(), &buf, 1)
	if _, statErr := os.Stat(filepath.Join(outside, "pwned")); statErr == nil {
		t.Fatal("symlink tar-slip escaped the tree")
	}
	if err == nil {
		t.Error("symlink-slip member did not error")
	}
}
