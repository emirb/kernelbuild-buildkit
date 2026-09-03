package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

var dirsMu sync.Mutex

// withDirs points the mount-contract path vars at temp dirs for one test.
// The vars are package globals, so this MUST be called at most once per test
// and never from a t.Parallel test. TryLock turns either misuse into an
// immediate, readable failure instead of a deadlock (found while red-teaming
// with a bare loop over withDirs).
func withDirs(t *testing.T) (build, patches, secrets string) {
	t.Helper()
	if !dirsMu.TryLock() {
		t.Fatal("withDirs is already held: call it once per test, never under t.Parallel")
	}
	t.Cleanup(dirsMu.Unlock)
	build, patches, secrets = t.TempDir(), t.TempDir(), t.TempDir()
	obd, opd, osd := buildDir, patchesDir, secretsDir
	buildDir, patchesDir, secretsDir = build, patches, secrets
	t.Cleanup(func() { buildDir, patchesDir, secretsDir = obd, opd, osd })
	return
}

func gzTar(t *testing.T, entries ...[3]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(mkTar(t, entries...)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTarballFormats(t *testing.T) {
	entries := [][3]string{
		{"linux-9.9/Makefile", "f", "all:\n"},
		{"linux-9.9/dir/file.c", "f", "int x;\n"},
	}
	raw := mkTar(t, entries...)

	t.Run("gz", func(t *testing.T) {
		build, _, _ := withDirs(t)
		p := filepath.Join(t.TempDir(), "linux.tar.gz")
		if err := os.WriteFile(p, gzTar(t, entries...), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := extractTarball(p); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(build, "Makefile")); err != nil {
			t.Fatal("strip-components=1 did not land Makefile at the root:", err)
		}
	})

	t.Run("zst", func(t *testing.T) {
		build, _, _ := withDirs(t)
		var buf bytes.Buffer
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := zw.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "linux.tar.zst")
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := extractTarball(p); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(build, "dir", "file.c")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("xz", func(t *testing.T) {
		// Fixture built in-process: the decode path is pure Go now, and a
		// skip-if-no-xz test left it uncovered exactly where it matters.
		build, _, _ := withDirs(t)
		p := filepath.Join(t.TempDir(), "linux.tar.xz")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		xw, err := xz.NewWriter(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := xw.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := xw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := extractTarball(p); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(build, "Makefile")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		withDirs(t)
		if err := extractTarball("/nonexistent.tar.bz2"); err == nil {
			t.Fatal("want error for unsupported/missing tarball")
		}
		p := filepath.Join(t.TempDir(), "x.tar.bz2")
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := extractTarball(p); err == nil {
			t.Fatal("want error for unsupported extension")
		}
	})

	t.Run("corrupt-gz", func(t *testing.T) {
		withDirs(t)
		p := filepath.Join(t.TempDir(), "x.tar.gz")
		if err := os.WriteFile(p, []byte("not gzip"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := extractTarball(p); err == nil {
			t.Fatal("want error for corrupt gzip")
		}
	})
}

const testPatch = `--- a/foo.txt
+++ b/foo.txt
@@ -1,3 +1,3 @@
 keep
-old line
+new line
 keep
`

func TestApplyPatches(t *testing.T) {
	build, patches, _ := withDirs(t)
	orig := "keep\nold line\nkeep\n"
	if err := os.WriteFile(filepath.Join(build, "foo.txt"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(patches, "0001-x.patch"), []byte(testPatch), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applyPatches(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(build, "foo.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "keep\nnew line\nkeep\n"; string(got) != want {
		t.Fatalf("patched content = %q, want %q", got, want)
	}
}

func TestApplyPatchesFailures(t *testing.T) {
	t.Run("no-patches", func(t *testing.T) {
		withDirs(t)
		if err := applyPatches(); err == nil {
			t.Fatal("want error when the patches dir is empty")
		}
	})
	t.Run("stale-patch-aborts", func(t *testing.T) {
		// A patch whose context no longer matches must abort the build, never
		// silently build unpatched (this caught the repo's own stale patch).
		build, patches, _ := withDirs(t)
		if err := os.WriteFile(filepath.Join(build, "foo.txt"), []byte("something else\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(patches, "0001-x.patch"), []byte(testPatch), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := applyPatches(); err == nil {
			t.Fatal("want error for stale patch")
		}
	})
	t.Run("missing-target", func(t *testing.T) {
		_, patches, _ := withDirs(t)
		if err := os.WriteFile(filepath.Join(patches, "0001-x.patch"), []byte(testPatch), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := applyPatches(); err == nil {
			t.Fatal("want error when the patch target does not exist")
		}
	})
}

func TestStampRoundTrip(t *testing.T) {
	build, patches, _ := withDirs(t)
	t.Chdir(build)

	if treePresent() {
		t.Fatal("empty dir reported a tree")
	}
	if got := readStamp(); got != "none" {
		t.Fatalf("readStamp on empty dir = %q, want none", got)
	}

	t.Setenv("SRC_ID", "sha256:abc")
	t.Setenv("APPLY_PATCHES", "0")
	want, err := stampWant()
	if err != nil {
		t.Fatal(err)
	}
	if want != "src=sha256:abc patches=none" {
		t.Fatalf("stampWant = %q", want)
	}

	// With patches: the stamp must change with patch CONTENT, not just names.
	t.Setenv("APPLY_PATCHES", "1")
	if _, err := stampWant(); err == nil {
		t.Fatal("want error when APPLY_PATCHES=1 but no patches exist")
	}
	if err := os.WriteFile(filepath.Join(patches, "a.patch"), []byte(testPatch), 0o644); err != nil {
		t.Fatal(err)
	}
	s1, err := stampWant()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(patches, "a.patch"), []byte(testPatch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := stampWant()
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s2 {
		t.Fatal("stamp did not change when patch content changed")
	}

	if err := os.WriteFile(".kbf-stamp", []byte(s2), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readStamp(); got != s2 {
		t.Fatalf("readStamp = %q, want %q", got, s2)
	}

	if err := os.WriteFile("Makefile", []byte("all:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !treePresent() {
		t.Fatal("Makefile present but treePresent() = false")
	}

	if err := nukeTree(); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(build)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("nukeTree left %d entries", len(ents))
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "payload" {
		t.Fatalf("dst = %q, %v", got, err)
	}
	if err := copyFile(filepath.Join(dir, "missing"), dst); err == nil {
		t.Fatal("want error for missing source")
	}
}

func TestLoadSeedCfg(t *testing.T) {
	write := func(t *testing.T, secrets, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(secrets, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("absent", func(t *testing.T) {
		withDirs(t)
		if c := loadSeedCfg(); c != nil {
			t.Fatalf("no seed_cfg secret but got %+v", c)
		}
	})
	t.Run("incomplete", func(t *testing.T) {
		_, _, secrets := withDirs(t)
		write(t, secrets, "seed_cfg", "SEED_URL=https://h/b\n")
		if c := loadSeedCfg(); c != nil {
			t.Fatalf("missing SEED_KEY but got %+v", c)
		}
	})
	t.Run("no-credentials", func(t *testing.T) {
		_, _, secrets := withDirs(t)
		write(t, secrets, "seed_cfg", "SEED_URL=https://h/b\nSEED_KEY=k\nSEED_PUSH=1\n")
		if c := loadSeedCfg(); c != nil {
			t.Fatalf("no seed_access_key secret but got %+v", c)
		}
	})
	t.Run("half-mounted-credentials", func(t *testing.T) {
		// Access key without secret key: seeding must be skipped, not turn
		// into a hard failure inside s3Client later.
		_, _, secrets := withDirs(t)
		write(t, secrets, "seed_cfg", "SEED_URL=https://h/b\nSEED_KEY=k\nSEED_PUSH=1\n")
		write(t, secrets, "seed_access_key", "AK")
		if c := loadSeedCfg(); c != nil {
			t.Fatalf("half-mounted credentials but got %+v", c)
		}
	})
	t.Run("complete", func(t *testing.T) {
		_, _, secrets := withDirs(t)
		write(t, secrets, "seed_cfg", "SEED_URL=https://h/b\nSEED_KEY=k/linux.tzst\nSEED_PUSH=1\n")
		write(t, secrets, "seed_access_key", "AK")
		write(t, secrets, "seed_secret_key", "SK")
		c := loadSeedCfg()
		if c == nil || c.url != "https://h/b" || c.key != "k/linux.tzst" || !c.push {
			t.Fatalf("loadSeedCfg = %+v", c)
		}
	})
}

func TestS3Client(t *testing.T) {
	_, _, secrets := withDirs(t)
	ctx := context.Background()

	if _, _, err := s3Client(ctx, &seedCfg{url: "https://hostonly"}); err == nil {
		t.Fatal("want error for URL without bucket")
	}
	if _, _, err := s3Client(ctx, &seedCfg{url: "https://h/b"}); err == nil {
		t.Fatal("want error when credential secrets are missing")
	}
	for name, v := range map[string]string{"seed_access_key": "AK\n", "seed_secret_key": "SK\n"} {
		if err := os.WriteFile(filepath.Join(secrets, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cl, bucket, err := s3Client(ctx, &seedCfg{url: "https://acct.example.com/mybucket", key: "k"})
	if err != nil || cl == nil || bucket != "mybucket" {
		t.Fatalf("s3Client = %v, %q, %v", cl, bucket, err)
	}
}

func withSrcDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := srcDir
	srcDir = dir
	t.Cleanup(func() { srcDir = old })
	return dir
}

func TestUntarSeedQuarantinesStamp(t *testing.T) {
	// The fix for the permanent-wedge bug: a seed stream's stamp must land
	// quarantined, never as the live stamp, so an interrupted pull can not
	// bless a partial tree.
	dir := t.TempDir()
	stream := mkTar(t,
		[3]string{".kbf-stamp", "f", "src=evil patches=none"},
		[3]string{"Makefile", "f", "all:\n"},
	)
	if err := untarSeed(dir, bytes.NewReader(stream)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".kbf-stamp")); !os.IsNotExist(err) {
		t.Fatal("seed stream planted a LIVE stamp")
	}
	q, err := os.ReadFile(filepath.Join(dir, ".kbf-stamp.seed"))
	if err != nil || string(q) != "src=evil patches=none" {
		t.Fatalf("quarantined stamp = %q, %v", q, err)
	}
}

func TestStampWantLocalSourceIsContentHash(t *testing.T) {
	src := withSrcDir(t)
	t.Setenv("APPLY_PATCHES", "0")
	t.Setenv("SRC_ID", "should-be-ignored")
	t.Setenv("SRC_LOCAL", "k.tar.gz")
	if err := os.WriteFile(filepath.Join(src, "k.tar.gz"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	s1, err := stampWant()
	if err != nil {
		t.Fatal(err)
	}
	// Same filename, different bytes: the stamp MUST change (identity by
	// filename alone would reuse a stale tree and cache it against the new
	// content).
	if err := os.WriteFile(filepath.Join(src, "k.tar.gz"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := stampWant()
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s2 {
		t.Fatal("swapping local tarball content did not change the stamp")
	}
	if strings.Contains(s1, "should-be-ignored") {
		t.Error("local mode used SRC_ID instead of content")
	}
}

func TestAcquireSourceRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"../evil.tar.gz", "/abs.tar.gz"} {
		t.Setenv("SRC_LOCAL", bad)
		if _, err := acquireSource(context.Background()); err == nil {
			t.Errorf("SRC_LOCAL %q accepted", bad)
		}
	}
}

// TestDownloadRefusesHTTPRedirectWithoutRetry: a redirect the client itself refuses
// (non-https) is permanent. Retrying it five times with the full backoff
// stalls the build for ~28s and hits the server five times for nothing.
func TestDownloadRefusesHTTPRedirectWithoutRetry(t *testing.T) {
	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("cleartext"))
	}))
	defer insecure.Close()
	var hits int
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, insecure.URL, http.StatusFound)
	}))
	defer redirecting.Close()
	start := time.Now()
	err := download(context.Background(), redirecting.URL, filepath.Join(t.TempDir(), "out"))
	if err == nil || !strings.Contains(err.Error(), "non-https redirect") {
		t.Fatalf("http redirect not refused: %v", err)
	}
	if hits != 1 {
		t.Errorf("server was hit %d times for a permanent refusal, want 1", hits)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("permanent refusal took %s (retry backoff applied to a non-retryable error)", el)
	}
}

func TestDownloadHonorsContext(t *testing.T) {
	stall := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer stall.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := download(ctx, stall.URL, filepath.Join(t.TempDir(), "out"))
	if err == nil {
		t.Fatal("stalled server did not error under a cancelled context")
	}
}

const createPatch = `--- /dev/null
+++ b/newfile.txt
@@ -0,0 +1,2 @@
+line one
+line two
`

const deletePatch = `--- a/gone.txt
+++ /dev/null
@@ -1,1 +0,0 @@
-goodbye
`

func TestApplyPatchesFileOps(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		build, patches, _ := withDirs(t)
		if err := os.WriteFile(filepath.Join(patches, "0001.patch"), []byte(createPatch), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := applyPatches(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(build, "newfile.txt"))
		if err != nil || string(b) != "line one\nline two\n" {
			t.Fatalf("created file = %q, %v", b, err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		build, patches, _ := withDirs(t)
		if err := os.WriteFile(filepath.Join(build, "gone.txt"), []byte("goodbye\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(patches, "0001.patch"), []byte(deletePatch), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := applyPatches(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(build, "gone.txt")); !os.IsNotExist(err) {
			t.Fatal("deleted file survived (left behind as an empty file the stamp then blesses)")
		}
	})
}

func TestModulesEnabled(t *testing.T) {
	cases := map[string]bool{
		"CONFIG_MODULES=y\nCONFIG_X=y\n":  true, // first line, no preceding newline
		"# header\nCONFIG_MODULES=y\n":    true,
		"# CONFIG_MODULES is not set\n":   false,
		"CONFIG_MODULES=n\n":              false,
		"CONFIG_MODULES=yes_not_really\n": false,
	}
	for cfg, want := range cases {
		if got := modulesEnabled([]byte(cfg)); got != want {
			t.Errorf("modulesEnabled(%q) = %v, want %v", cfg, got, want)
		}
	}
}

func TestDownloadResumesAfterTruncation(t *testing.T) {
	const full = "0123456789abcdefghijABCDEFGHIJ" // 30 bytes
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		start := 0
		if rng := r.Header.Get("Range"); rng != "" {
			// bytes=<start>-
			_, _ = fmtSscanf(rng, &start)
			w.Header().Set("Content-Range", "bytes")
			w.WriteHeader(http.StatusPartialContent)
		}
		body := full[start:]
		if served == 1 {
			// First response: hijack and truncate to force an unexpected EOF.
			w.Header().Set("Content-Length", "30")
			_, _ = io.WriteString(w, body[:10])
			if hj, ok := w.(http.Hijacker); ok {
				c, _, _ := hj.Hijack()
				_ = c.Close()
			}
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "out")
	if err := download(context.Background(), srv.URL, dst); err != nil {
		t.Fatalf("download did not recover: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != full {
		t.Fatalf("resumed content = %q, want %q", got, full)
	}
	if served < 2 {
		t.Errorf("expected a retry, server saw %d requests", served)
	}
}

func fmtSscanf(rng string, start *int) (int, error) { return fmt.Sscanf(rng, "bytes=%d-", start) }

// TestMain: no test in this package sleeps through a real retry backoff.
// The default records nothing and returns at once; a test that wants the
// schedule installs a recorder with recordRetries.
func TestMain(m *testing.M) {
	retrySleep = func(context.Context, time.Duration) error { return nil }
	os.Exit(m.Run())
}

// recordRetries captures the backoff download asks for, without waiting.
func recordRetries(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	old := retrySleep
	retrySleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	t.Cleanup(func() { retrySleep = old })
	return &waits
}

func TestDownloadRetriesTransient503(t *testing.T) {
	const body = "kernel-source-bytes"
	waits := recordRetries(t)
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "out")
	if err := download(context.Background(), srv.URL, dst); err != nil {
		t.Fatalf("download did not survive transient 503s: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != body {
		t.Fatalf("content = %q, want %q", got, body)
	}
	// Two 503s carrying Retry-After: 1 -> two waits, each the larger of the
	// header and the attempt's own backoff (2*base, then 3*base).
	if want := []time.Duration{2 * retryBase, 3 * retryBase}; !slices.Equal(*waits, want) {
		t.Errorf("backoff schedule = %v, want %v", *waits, want)
	}
	if n < 3 {
		t.Errorf("expected retries past the 503s, server saw %d requests", n)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"":     0,
		"5":    5 * time.Second,
		" 5 ":  5 * time.Second, //nolint:gocritic // the padding is the trimming case under test
		"0":    0,
		"-1":   0,
		"abc":  0,
		"9999": 30 * time.Second, // capped
	}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDownloadRestartsOnUnsatisfiableRange(t *testing.T) {
	// Truncate once, then answer the resume with 416 (what a mirror serving
	// a re-rolled or shorter file does). The fetch must start over instead of
	// failing the build on a fatal HTTP status.
	const full = "0123456789abcdefghij"
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		switch {
		case served == 1:
			w.Header().Set("Content-Length", "20")
			_, _ = io.WriteString(w, full[:8])
			if hj, ok := w.(http.Hijacker); ok {
				c, _, _ := hj.Hijack()
				_ = c.Close()
			}
		case r.Header.Get("Range") != "":
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		default:
			_, _ = io.WriteString(w, full)
		}
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "out")
	if err := download(context.Background(), srv.URL, dst); err != nil {
		t.Fatalf("download did not restart after 416: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != full {
		t.Fatalf("content = %q, want %q", got, full)
	}
}

// TestRedactURL: userinfo, query and fragment never reach the build log; the
// host and path (what a reader needs to recognize the mirror) do.
func TestRedactURL(t *testing.T) {
	got := redactURL("https://user:tok@mirror.example/pub/linux-6.18.20.tar.gz?X-Amz-Signature=abc#frag")
	if got != "https://mirror.example/pub/linux-6.18.20.tar.gz" {
		t.Errorf("redactURL = %q", got)
	}
}

// TestDownloadErrorsRedactURL: a failed fetch of a URL carrying basic-auth
// and a presigned query reports neither, in the direct error nor in the
// *url.Error net/http wraps around a refused redirect.
func TestDownloadErrorsRedactURL(t *testing.T) {
	insecure := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer insecure.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, insecure.URL+"/x?X-Amz-Signature=presigned", http.StatusFound)
	}))
	defer redirecting.Close()
	host := strings.TrimPrefix(redirecting.URL, "http://")
	err := download(context.Background(), "http://alice:s3cret@"+host+"/linux.tar.gz?token=leak", filepath.Join(t.TempDir(), "out"))
	if err == nil {
		t.Fatal("http redirect accepted")
	}
	for _, secret := range []string{"s3cret", "token=leak", "presigned", "alice"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %v", secret, err)
		}
	}
}
