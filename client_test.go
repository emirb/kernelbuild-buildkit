package kbuild

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCacheEntry(t *testing.T) {
	e, err := ParseCacheEntry("type=registry,ref=ghcr.io/org/cache:main", "", "")
	if err != nil || e.Type != "registry" || e.Attrs["ref"] != "ghcr.io/org/cache:main" {
		t.Errorf("registry entry = %+v, %v", e, err)
	}
	e, err = ParseCacheEntry("type=s3,bucket=b,endpoint_url=https://x,region=auto", "AK", "SK")
	if err != nil || e.Attrs["access_key_id"] != "AK" || e.Attrs["secret_access_key"] != "SK" {
		t.Errorf("s3 creds not injected: %+v, %v", e, err)
	}
	e, err = ParseCacheEntry("type=s3,bucket=b,access_key_id=EXPLICIT", "AK", "SK")
	if err != nil || e.Attrs["access_key_id"] != "EXPLICIT" {
		t.Errorf("explicit creds must win: %+v, %v", e, err)
	}
	for _, bad := range []string{"ref=x", "type=s3,malformed", ""} {
		if _, err := ParseCacheEntry(bad, "", ""); err == nil {
			t.Errorf("ParseCacheEntry(%q) accepted", bad)
		}
	}
}

func TestS3CacheURL(t *testing.T) {
	spec, err := S3CacheURL("https://acct.r2.cloudflarestorage.com/bucket", "")
	if err != nil || !strings.Contains(spec, "type=s3,bucket=bucket") ||
		!strings.Contains(spec, "endpoint_url=https://acct.r2.cloudflarestorage.com") {
		t.Errorf("spec = %q, %v", spec, err)
	}
	for _, bad := range []string{"http://h/b", "https://hostonly", "https://h/b/extra"} {
		if _, err := S3CacheURL(bad, ""); err == nil {
			t.Errorf("S3CacheURL(%q) accepted", bad)
		}
	}
}

func TestVertexLabel(t *testing.T) {
	if got := VertexLabel("short"); got != "short" {
		t.Errorf("short = %q", got)
	}
	multi := VertexLabel("line1\nline2")
	if strings.Contains(multi, "\n") || !strings.Contains(multi, "line1") {
		t.Errorf("multiline not compressed: %q", multi)
	}
	if long := VertexLabel(strings.Repeat("x", 300)); len(long) > 110 {
		t.Errorf("long label not truncated: %d chars", len(long))
	}
}

// minimalELF returns the smallest header debug/elf will parse, for the given
// machine — enough for stageHelper's platform gate.
func minimalELF(t *testing.T, machine elf.Machine) []byte {
	t.Helper()
	var b bytes.Buffer
	b.Write([]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), 1, 0})
	b.Write(make([]byte, 8)) // padding
	le := binary.LittleEndian
	var h [48]byte
	le.PutUint16(h[0:], uint16(elf.ET_EXEC))
	le.PutUint16(h[2:], uint16(machine))
	le.PutUint32(h[4:], 1)   // version
	le.PutUint64(h[8:], 0)   // entry
	le.PutUint64(h[16:], 0)  // phoff
	le.PutUint64(h[24:], 0)  // shoff
	le.PutUint32(h[32:], 0)  // flags
	le.PutUint16(h[36:], 64) // ehsize
	le.PutUint16(h[38:], 0)  // phentsize
	le.PutUint16(h[40:], 0)  // phnum
	le.PutUint16(h[42:], 0)  // shentsize
	le.PutUint16(h[44:], 0)  // shnum
	le.PutUint16(h[46:], 0)  // shstrndx
	b.Write(h[:])
	if _, err := elf.NewFile(bytes.NewReader(b.Bytes())); err != nil {
		t.Fatalf("test ELF does not parse: %v", err)
	}
	return b.Bytes()
}

func TestStageHelper(t *testing.T) {
	write := func(t *testing.T, content []byte) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "kbuild-step")
		if err := os.WriteFile(p, content, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("missing", func(t *testing.T) {
		if _, err := stageHelper(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatal("want error for missing helper")
		}
	})
	t.Run("not-elf", func(t *testing.T) {
		_, err := stageHelper(write(t, []byte("#!/bin/sh\necho no\n")))
		if err == nil || !strings.Contains(err.Error(), "linux/amd64") {
			t.Fatalf("want linux/amd64 rejection, got %v", err)
		}
	})
	t.Run("arm64-elf", func(t *testing.T) {
		if _, err := stageHelper(write(t, minimalELF(t, elf.EM_AARCH64))); err == nil {
			t.Fatal("want rejection of an arm64 ELF")
		}
	})
	t.Run("amd64-elf", func(t *testing.T) {
		src := write(t, minimalELF(t, elf.EM_X86_64))
		dir, err := stageHelper(src)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		staged, err := os.ReadDir(dir)
		if err != nil || len(staged) != 1 || staged[0].Name() != "kbuild-step" {
			t.Fatalf("staged dir wrong: %v %v", staged, err)
		}
		info, err := os.Stat(filepath.Join(dir, "kbuild-step"))
		if err != nil || info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("staged helper not executable: %v %v", info, err)
		}
	})
}

func TestBuildFailsFastWithoutDaemon(t *testing.T) {
	// Spec-level errors must surface BEFORE any daemon dial: they are
	// decidable locally, and the reorder makes them testable.
	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "kernel.config"), []byte("# defaults\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := DefaultSpec()
	spec.SeedPush = true
	_, err := Build(context.Background(), spec, BuildConfig{Addr: "tcp://127.0.0.1:1", ContextDir: ctxDir})
	if err == nil || !strings.Contains(err.Error(), "SeedPush requires SeedURL") {
		t.Fatalf("SeedPush without SeedURL: %v", err)
	}

	// HelperRef mode must NOT demand a local helper binary: with a bogus
	// helper path the failure has to be the (unreachable) daemon, never
	// "kbuild-step helper not found".
	spec = DefaultSpec()
	spec.HelperRef = "ghcr.io/example/frontend:v1"
	_, err = Build(context.Background(), spec, BuildConfig{
		Addr:       "tcp://127.0.0.1:1",
		ContextDir: ctxDir,
		HelperBin:  "/nonexistent/kbuild-step",
	})
	if err == nil {
		t.Fatal("expected a connect error")
	}
	if strings.Contains(err.Error(), "helper") {
		t.Fatalf("HelperRef mode still staged the local helper: %v", err)
	}

	// And without HelperRef the same bogus path must fail at staging, before
	// the dial.
	spec = DefaultSpec()
	_, err = Build(context.Background(), spec, BuildConfig{
		Addr:       "tcp://127.0.0.1:1",
		ContextDir: ctxDir,
		HelperBin:  "/nonexistent/kbuild-step",
	})
	if err == nil || !strings.Contains(err.Error(), "kbuild-step helper not found") {
		t.Fatalf("local mode with missing helper: %v", err)
	}
}

func TestParseCacheEntryCommaInValue(t *testing.T) {
	// A quoted attr value containing a comma must survive (naive Split broke
	// this; the fix uses encoding/csv).
	e, err := ParseCacheEntry(`type=s3,bucket=b,"names=a,b,c"`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != "s3" || e.Attrs["bucket"] != "b" || e.Attrs["names"] != "a,b,c" {
		t.Fatalf("comma-in-value mangled: %+v", e)
	}
}
