package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type tarEntry struct {
	name, link, content string
	mode                os.FileMode
	typ                 byte
	mtime               time.Time
}

func buildTar(tb testing.TB, entries []tarEntry) *bytes.Buffer {
	tb.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: int64(e.mode), ModTime: e.mtime, Typeflag: e.typ, Linkname: e.link}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			tb.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := io.WriteString(tw, e.content); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		tb.Fatal(err)
	}
	return &buf
}

// The parallel pool must be observationally identical to the old serial loop:
// contents, modes, mtimes, symlinks, hardlinks, same-path-replaces, and the
// stamp quarantine all land exactly as before.
func TestUntarParallelMatchesSerial(t *testing.T) {
	base := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	old := untarInlineLimit
	untarInlineLimit = 1 << 10 // exercise the inline large-file path
	defer func() { untarInlineLimit = old }()

	entries := []tarEntry{
		{name: "dir/", typ: tar.TypeDir, mode: 0o750, mtime: base},
	}
	for i := range 400 {
		entries = append(entries, tarEntry{
			name: fmt.Sprintf("dir/sub%02d/f%04d.o", i%7, i), typ: tar.TypeReg,
			content: strings.Repeat(fmt.Sprintf("obj%d ", i), 10+i%50),
			mode:    0o644, mtime: base.Add(time.Duration(i) * time.Second),
		})
	}
	entries = append(entries,
		// Large file: above the (lowered) inline limit, streamed behind a barrier.
		tarEntry{name: "vmlinux", typ: tar.TypeReg, content: strings.Repeat("BIG", 1000), mode: 0o755, mtime: base},
		// Same path twice: tar semantics say the later entry wins.
		tarEntry{name: "dir/dup.o", typ: tar.TypeReg, content: "first", mode: 0o644, mtime: base},
		tarEntry{name: "dir/dup.o", typ: tar.TypeReg, content: "second", mode: 0o600, mtime: base.Add(time.Hour)},
		// Hardlink to a file that was dispatched to the pool just before.
		tarEntry{name: "dir/linked.o", typ: tar.TypeReg, content: "linked-content", mode: 0o644, mtime: base},
		tarEntry{name: "dir/hardlink.o", typ: tar.TypeLink, link: "dir/linked.o"},
		// Symlink after queued writes.
		tarEntry{name: "dir/symlink", typ: tar.TypeSymlink, link: "sub00/f0000.o"},
		// The seed stamp must arrive quarantined.
		tarEntry{name: ".kbf-stamp", typ: tar.TypeReg, content: "src=x patches=none", mode: 0o644, mtime: base},
	)

	dir := t.TempDir()
	if err := untarSeed(dir, buildTar(t, entries)); err != nil {
		t.Fatal(err)
	}

	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return string(b)
	}
	for i := range 400 {
		p := fmt.Sprintf("dir/sub%02d/f%04d.o", i%7, i)
		if got, want := read(p), strings.Repeat(fmt.Sprintf("obj%d ", i), 10+i%50); got != want {
			t.Fatalf("%s content mismatch", p)
		}
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil || fi.Mode().Perm() != 0o644 || !fi.ModTime().Equal(base.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("%s meta: %v %v %v", p, fi.Mode(), fi.ModTime(), err)
		}
	}
	if read("vmlinux") != strings.Repeat("BIG", 1000) {
		t.Fatal("large inline file corrupted")
	}
	if got := read("dir/dup.o"); got != "second" {
		t.Fatalf("dup: later entry must win, got %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "dir", "dup.o")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("dup mode: %v", fi.Mode())
	}
	if read("dir/hardlink.o") != "linked-content" {
		t.Fatal("hardlink target content wrong (barrier failed?)")
	}
	if target, err := os.Readlink(filepath.Join(dir, "dir", "symlink")); err != nil || target != "sub00/f0000.o" {
		t.Fatalf("symlink: %q %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".kbf-stamp")); !os.IsNotExist(err) {
		t.Fatal("stamp not quarantined")
	}
	if read(".kbf-stamp.seed") != "src=x patches=none" {
		t.Fatal("quarantined stamp content wrong")
	}
}

// A worker-side write failure must surface as the untar error and must not
// hang the reader (the pool keeps draining after failure).
func TestUntarParallelErrorPropagates(t *testing.T) {
	entries := []tarEntry{
		{name: "blocker", typ: tar.TypeReg, content: "a file where a dir must go", mode: 0o644, mtime: time.Unix(0, 0)},
	}
	for i := range 100 {
		entries = append(entries, tarEntry{name: fmt.Sprintf("blocker/child%d.o", i), typ: tar.TypeReg, content: "x", mode: 0o644, mtime: time.Unix(0, 0)})
	}
	err := untar(t.TempDir(), buildTar(t, entries), 0)
	if err == nil {
		t.Fatal("expected an error from writes under a file")
	}
}

// The hydrate hot path: many small object files. Run with GOMAXPROCS=4 to
// mirror the standard-4 container.
func BenchmarkUntarSmallFiles(b *testing.B) {
	entries := make([]tarEntry, 0, 20000)
	content := strings.Repeat("x", 24<<10)
	for i := range 20000 {
		entries = append(entries, tarEntry{
			name: fmt.Sprintf("d%02d/f%05d.o", i%40, i), typ: tar.TypeReg,
			content: content, mode: 0o644, mtime: time.Unix(int64(i), 0),
		})
	}
	stream := buildTar(b, entries).Bytes()
	b.SetBytes(int64(len(stream)))
	b.ResetTimer()
	for b.Loop() {
		if err := untar(b.TempDir(), bytes.NewReader(stream), 0); err != nil {
			b.Fatal(err)
		}
	}
}
