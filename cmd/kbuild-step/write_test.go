package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("stream cut") }

func openRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

// TestWriteEntryErrors: every failure on the way to a written file is
// reported, never swallowed: a parent that is a regular file, a target that
// is a non-empty directory, and a body that fails mid-stream.
func TestWriteEntryErrors(t *testing.T) {
	hdr := &tar.Header{Mode: 0o644, ModTime: time.Unix(1700000000, 0)}

	t.Run("parent-is-a-file", func(t *testing.T) {
		root, dir := openRoot(t)
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeEntry(root, "f/child", strings.NewReader("y"), hdr); err == nil {
			t.Fatal("wrote through a regular file as a directory")
		}
	})
	t.Run("target-is-a-populated-directory", func(t *testing.T) {
		root, dir := openRoot(t)
		if err := os.MkdirAll(filepath.Join(dir, "d", "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeEntry(root, "d", strings.NewReader("y"), hdr); err == nil {
			t.Fatal("replaced a populated directory with a file")
		}
		if _, err := os.Stat(filepath.Join(dir, "d", "inner")); err != nil {
			t.Error("the directory's contents were removed")
		}
	})
	t.Run("body-fails-mid-stream", func(t *testing.T) {
		root, dir := openRoot(t)
		err := writeEntry(root, "a/b.txt", failingReader{}, hdr)
		if err == nil || !strings.Contains(err.Error(), "stream cut") {
			t.Fatalf("read error not propagated: %v", err)
		}
		// The partial file exists (tar semantics leave it to the caller's
		// nuke) but must not claim the header's mode or mtime as if complete.
		if fi, err := os.Stat(filepath.Join(dir, "a", "b.txt")); err == nil && fi.ModTime().Equal(hdr.ModTime) {
			t.Error("mtime restored on a partial file")
		}
	})
	t.Run("success-restores-mode-and-mtime", func(t *testing.T) {
		root, dir := openRoot(t)
		exec := &tar.Header{Mode: 0o755, ModTime: time.Unix(1700000000, 0)}
		if err := writeEntry(root, "bin/tool", bytes.NewReader([]byte("#!/bin/sh\n")), exec); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(dir, "bin", "tool"))
		if err != nil || fi.Mode().Perm() != 0o755 || !fi.ModTime().Equal(exec.ModTime) {
			t.Errorf("mode/mtime = %v %v, %v", fi.Mode(), fi.ModTime(), err)
		}
	})
}

// TestWriteFileBytesErrors: the pooled writer has the same contract.
func TestWriteFileBytesErrors(t *testing.T) {
	mtime := time.Unix(1700000000, 0)
	t.Run("parent-is-a-file", func(t *testing.T) {
		root, dir := openRoot(t)
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeFileBytes(root, "f/child", 0o644, mtime, []byte("y")); err == nil {
			t.Fatal("wrote through a regular file as a directory")
		}
	})
	t.Run("target-is-a-populated-directory", func(t *testing.T) {
		root, dir := openRoot(t)
		if err := os.MkdirAll(filepath.Join(dir, "d", "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeFileBytes(root, "d", 0o644, mtime, []byte("y")); err == nil {
			t.Fatal("replaced a populated directory with a file")
		}
	})
	t.Run("replaces-a-symlink-instead-of-following-it", func(t *testing.T) {
		root, dir := openRoot(t)
		if err := os.WriteFile(filepath.Join(dir, "victim"), []byte("untouched"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("victim", filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		if err := writeFileBytes(root, "link", 0o644, mtime, []byte("payload")); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "victim")); string(b) != "untouched" {
			t.Fatalf("wrote through the symlink: victim = %q", b)
		}
		if fi, err := os.Lstat(filepath.Join(dir, "link")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Error("link was not replaced by a regular file")
		}
	})
}
