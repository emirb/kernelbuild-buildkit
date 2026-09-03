package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMake puts a `make` on PATH that succeeds, or fails when FAKE_MAKE_FAIL
// is set, so the configure step can be driven without a kernel tree.
func fakeMake(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ -n \"$FAKE_MAKE_FAIL\" ]; then exit 1; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "make"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func withConfig(t *testing.T, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kernel.config")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := configPath
	configPath = p
	t.Cleanup(func() { configPath = old })
}

// TestConfigureRecoversFromInterruptedOlddefconfig: .config is overwritten
// BEFORE olddefconfig and the skip marker written AFTER it. If olddefconfig
// fails (or the build is cancelled) in between, the marker still names the
// previous config while .config holds the new one; the next build with the
// previous config then skips olddefconfig and compiles the WRONG config, and
// BuildKit caches that result under the previous config's key.
func TestConfigureRecoversFromInterruptedOlddefconfig(t *testing.T) {
	fakeMake(t)
	build, _, _ := withDirs(t)
	t.Chdir(build)
	t.Setenv("BASE_MAKE", "")
	t.Setenv("KARCH", "")
	t.Setenv("CROSS_COMPILE", "")
	if err := os.WriteFile("Makefile", []byte("all:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const a, b = "CONFIG_A=y\n", "CONFIG_B=y\n"

	withConfig(t, a)
	if err := configure(); err != nil {
		t.Fatal(err)
	}
	withConfig(t, b)
	t.Setenv("FAKE_MAKE_FAIL", "1")
	if err := configure(); err == nil {
		t.Fatal("olddefconfig failure not reported")
	}
	t.Setenv("FAKE_MAKE_FAIL", "")
	withConfig(t, a)
	if err := configure(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(".config")
	if string(got) != a {
		t.Fatalf(".config = %q after rebuilding with config A, want %q (olddefconfig skipped against a stale marker)", got, a)
	}
}

func gzTarSHA(t *testing.T, entries ...[3]string) ([]byte, string) {
	t.Helper()
	b := gzTar(t, entries...)
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

// TestPrepareTreeVerifiesSourceBeforeDiscardingTree: on a stamp mismatch the
// replacement source must be verified before the warm object tree is
// discarded, so a mistyped SHA256 fails without destroying the tree and its
// incremental cache.
func TestPrepareTreeVerifiesSourceBeforeDiscardingTree(t *testing.T) {
	build, _, _ := withDirs(t)
	t.Chdir(build)
	good, goodSum := gzTarSHA(t, [3]string{"linux-9.9/Makefile", "f", "new-tree:\n"})
	var serveGood bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveGood {
			_, _ = w.Write(good)
			return
		}
		_, _ = w.Write([]byte("not the bytes the sha names"))
	}))
	defer srv.Close()
	if err := os.WriteFile("Makefile", []byte("old-tree:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".kbf-stamp", []byte("src=OLD patches=none"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APPLY_PATCHES", "0")
	t.Setenv("SRC_LOCAL", "")
	t.Setenv("SRC_URL", srv.URL+"/linux-9.9.tar.gz")

	// A typo'd pin: the fetch must fail AND the warm tree must survive.
	t.Setenv("SRC_ID", strings.Repeat("0", 64))
	t.Setenv("SRC_SHA256", strings.Repeat("0", 64))
	err := prepareTree(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("bad pin: %v", err)
	}
	if b, err := os.ReadFile("Makefile"); err != nil || string(b) != "old-tree:\n" {
		t.Fatalf("warm tree discarded before the replacement source was verified: Makefile=%q %v", b, err)
	}
	if readStamp() != "src=OLD patches=none" {
		t.Fatalf("stamp = %q, want the old tree's", readStamp())
	}

	// The correct pin: the tree is replaced and re-stamped.
	serveGood = true
	t.Setenv("SRC_ID", goodSum)
	t.Setenv("SRC_SHA256", goodSum)
	if err := prepareTree(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile("Makefile"); string(b) != "new-tree:\n" {
		t.Fatalf("tree not replaced: Makefile=%q", b)
	}
	if readStamp() != "src="+goodSum+" patches=none" {
		t.Fatalf("stamp = %q", readStamp())
	}
}

// TestCheckSeedPush: seed-push=true reaches the step as SEED_PUSH=1 whether or
// not the seed_cfg secret arrived; without a usable seed config the build
// must fail loudly instead of recompiling forever and never publishing.
func TestCheckSeedPush(t *testing.T) {
	t.Setenv("SEED_PUSH", "1")
	if err := checkSeedPush(nil); err == nil {
		t.Error("push requested with no seed config accepted")
	}
	if err := checkSeedPush(&seedCfg{url: "https://h/b", key: "k"}); err == nil {
		t.Error("push requested but seed_cfg says SEED_PUSH=0 accepted")
	}
	if err := checkSeedPush(&seedCfg{url: "https://h/b", key: "k", push: true}); err != nil {
		t.Errorf("valid push config rejected: %v", err)
	}
	t.Setenv("SEED_PUSH", "")
	if err := checkSeedPush(nil); err != nil {
		t.Errorf("no push requested, no seed: %v", err)
	}
}

// TestIsPermanent classifies fetch errors: the client's own policy refusals
// and NXDOMAIN are final; everything else is retried.
func TestIsPermanent(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"policy":         {&url.Error{Op: "Get", Err: &policyError{"refusing non-https redirect"}}, true},
		"nxdomain":       {&url.Error{Op: "Get", Err: &net.DNSError{IsNotFound: true}}, true},
		"dns-timeout":    {&url.Error{Op: "Get", Err: &net.DNSError{IsTimeout: true}}, false},
		"reset":          {errors.New("read: connection reset by peer"), false},
		"wrapped-policy": {errors.Join(errors.New("ctx"), &policyError{"refusing link-local destination"}), true},
	}
	for name, c := range cases {
		if got := isPermanent(c.err); got != c.want {
			t.Errorf("%s: isPermanent = %v, want %v", name, got, c.want)
		}
	}
}

// TestDialUsesVettedAddress: the link-local guard resolves the name and vets
// the answers; the dial must then go to the vetted address, never the name
// again, because a rebinding resolver could answer a public address first and
// 169.254.169.254 second. "vetted.test" has no DNS; only a dial to the
// injected 127.0.0.1 answer can reach the test server.
func TestDialUsesVettedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	old := lookupIP
	t.Cleanup(func() { lookupIP = old })
	lookupIP = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "vetted.test" {
			t.Errorf("lookup for %q, want vetted.test", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := download(context.Background(), "http://vetted.test:"+port+"/x", dst); err != nil {
		t.Fatalf("dial did not use the vetted address: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "payload" {
		t.Fatalf("content = %q", b)
	}

	lookupIP = func(_ context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
	}
	err = download(context.Background(), "http://metadata.test:"+port+"/x", filepath.Join(t.TempDir(), "out2"))
	if err == nil || !strings.Contains(err.Error(), "link-local") {
		t.Fatalf("link-local answer not refused: %v", err)
	}
}
