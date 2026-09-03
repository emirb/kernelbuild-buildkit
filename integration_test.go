//go:build integration

// The integration suite: the twelve ci.sh scenarios as ordered Go subtests,
// driving buildkitd through the client API — no binary exec, no log grepping.
//
// Requires a live buildkitd plus env:
//
//	KBF_ADDR      buildkitd address        (default tcp://127.0.0.1:1234)
//	KBF_CONTEXT   dir with kernel.config (+ patches/, + CA file) [required]
//	KBF_GOLDEN    expected vmlinux for the byte gate             [optional]
//	KBF_SHA       sha256 of the source tarball                   [optional]
//	KBF_PROXY     https proxy URL                                [optional]
//	KBF_PROXY_CA  CA filename inside the context                 [optional]
//	KBF_SEED_URL  https://host/bucket for seed + s3 cache        [optional]
//	KBF_HELPER    path to a built kbuild-step                    [required]
//	AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY                    [with seed]
//	KBF_QUICK=1   skip the slowest three subtests
//
// Run: go test -tags integration -timeout 90m -run TestIntegration .
package kbuild

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type itEnv struct {
	addr, ctxDir, golden, sha, proxy, proxyCA, seedURL, helper string
	quick                                                      bool
}

func itSetup(t *testing.T) itEnv {
	t.Helper()
	e := itEnv{
		addr:    envOr("KBF_ADDR", "tcp://127.0.0.1:1234"),
		ctxDir:  os.Getenv("KBF_CONTEXT"),
		golden:  os.Getenv("KBF_GOLDEN"),
		sha:     os.Getenv("KBF_SHA"),
		proxy:   os.Getenv("KBF_PROXY"),
		proxyCA: os.Getenv("KBF_PROXY_CA"),
		seedURL: os.Getenv("KBF_SEED_URL"),
		helper:  os.Getenv("KBF_HELPER"),
		quick:   os.Getenv("KBF_QUICK") == "1",
	}
	if e.ctxDir == "" || e.helper == "" {
		t.Skip("integration env not set (KBF_CONTEXT, KBF_HELPER)")
	}
	return e
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (e itEnv) spec() Spec {
	s := DefaultSpec()
	s.SourceSHA256 = e.sha
	s.HTTPSProxy = e.proxy
	s.ProxyCAFile = e.proxyCA
	return s
}

func (e itEnv) cfg(t *testing.T, ctxDir string) BuildConfig {
	return BuildConfig{
		Addr:       e.addr,
		ContextDir: ctxDir,
		HelperBin:  e.helper,
		OutDir:     t.TempDir(),
		AccessKey:  os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey:  os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return sha256.Sum256(b)
}

func requireBuild(t *testing.T, ctx context.Context, spec Spec, cfg BuildConfig) *BuildResult {
	t.Helper()
	res, err := Build(ctx, spec, cfg)
	if err == nil {
		return res
	}
	if res != nil && res.Logs != "" {
		t.Fatalf("%v\n%s", err, res.Logs)
	}
	t.Fatal(err)
	return nil
}

// editConfig copies the context and applies a sed-like line replacement,
// appending the new line when the old one is not in the input at all. A
// caller's config need not spell out the options it leaves at their default
// -- CI's context is a single comment line -- and to kbuild an option that is
// absent and one that is "not set" mean the same thing, so appending produces
// the same one-option delta the replacement does.
func editConfig(t *testing.T, srcCtx, old, new string) string {
	t.Helper()
	dst := t.TempDir()
	ents, err := os.ReadDir(srcCtx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		src := filepath.Join(srcCtx, e.Name())
		if e.IsDir() {
			if err := os.CopyFS(filepath.Join(dst, e.Name()), os.DirFS(src)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dst, "kernel.config")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(old)) {
		b = bytes.Replace(b, []byte(old), []byte(new), 1)
	} else {
		if len(b) > 0 && b[len(b)-1] != '\n' {
			b = append(b, '\n')
		}
		b = append(b, []byte(new+"\n")...)
	}
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestIntegration runs the scenarios IN ORDER — they share daemon state by
// design (that shared state is what is under test).
func TestIntegration(t *testing.T) {
	e := itSetup(t)
	ctx := context.Background()
	var coldSum [32]byte
	var coldCC int

	t.Run("validate", func(t *testing.T) {
		s := e.spec()
		s.KernelVersion = "6.18; rm -rf /"
		if _, err := Build(ctx, s, e.cfg(t, e.ctxDir)); err == nil || !strings.Contains(err.Error(), "must look like") {
			t.Fatalf("hostile version not rejected: %v", err)
		}
	})

	t.Run("prune", func(t *testing.T) {
		if err := Prune(ctx, e.addr); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("badSHA", func(t *testing.T) {
		s := e.spec()
		s.SourceSHA256 = strings.Repeat("0", 64)
		res, err := Build(ctx, s, e.cfg(t, e.ctxDir))
		if err == nil {
			t.Fatal("wrong checksum accepted")
		}
		// The fetch happens IN-STEP, so the mismatch detail is in the build
		// logs; the solve error itself only carries the helper's exit code.
		if res == nil || !strings.Contains(res.Logs, "sha256 mismatch") {
			logs := ""
			if res != nil {
				logs = res.Logs
			}
			t.Fatalf("build failed but not for the checksum: %v\n%s", err, logs)
		}
	})

	t.Run("cold", func(t *testing.T) {
		cfg := e.cfg(t, e.ctxDir)
		if e.seedURL != "" {
			s3, err := S3CacheURL(e.seedURL, "")
			if err != nil {
				t.Fatal(err)
			}
			cfg.CacheExports = append(cfg.CacheExports, s3)
			cfg.CacheImports = append(cfg.CacheImports, s3)
		}
		res := requireBuild(t, ctx, e.spec(), cfg)
		coldCC = res.CC
		coldSum = fileSum(t, filepath.Join(cfg.OutDir, "vmlinux"))
		if res.CC < 1000 {
			t.Errorf("cold build compiled only %d objects", res.CC)
		}
		if e.golden != "" && coldSum != fileSum(t, e.golden) {
			t.Error("cold vmlinux differs from golden")
		}
	})

	t.Run("noop", func(t *testing.T) {
		if coldSum == ([32]byte{}) {
			t.Skip("cold did not run")
		}
		cfg := e.cfg(t, e.ctxDir)
		res := requireBuild(t, ctx, e.spec(), cfg)
		if res.Wall > time.Minute || res.CC != 0 {
			t.Errorf("noop rebuild: wall=%s CC=%d (want cache hit)", res.Wall, res.CC)
		}
		if fileSum(t, filepath.Join(cfg.OutDir, "vmlinux")) != coldSum {
			t.Error("noop output differs from cold build")
		}
	})

	// Parent scope on purpose: subtests below (incr, seed*, concurrent) share
	// this context, and a subtest-owned t.TempDir is deleted the moment that
	// subtest returns — the next user would get a dangling path.
	minixCtx := editConfig(t, e.ctxDir, "# CONFIG_MINIX_FS is not set", "CONFIG_MINIX_FS=y")
	t.Run("incr", func(t *testing.T) {
		res := requireBuild(t, ctx, e.spec(), e.cfg(t, minixCtx))
		// Relative to the cold build, not an absolute object count: how much
		// one option touches depends on the config it lands in. MINIX_FS
		// selects BUFFER_HEAD, which a rich config already has (~8 objects)
		// and a minimal one does not (~35, measured in CI). What has to hold
		// everywhere is that an incremental is a small fraction of a cold
		// build, on a tree that was reused rather than rebuilt.
		warm := strings.Contains(res.Logs, "warm: reusing persisted object tree")
		if !warm || (coldCC > 0 && res.CC*20 > coldCC) {
			t.Errorf("incremental: CC=%d (cold %d), warm-reuse=%v", res.CC, coldCC, warm)
		}
	})

	if e.seedURL != "" {
		t.Run("seedPush", func(t *testing.T) {
			s := e.spec()
			s.SeedURL = e.seedURL
			s.SeedPush = true
			res := requireBuild(t, ctx, s, e.cfg(t, minixCtx))
			if !strings.Contains(res.Logs, "seed: pushed") {
				t.Error("seed was not pushed")
			}
			if !strings.Contains(res.Logs, "warm: reusing persisted object tree") || res.CC != 0 {
				t.Errorf("seed push rebuilt instead of reusing the warm tree: CC=%d", res.CC)
			}
		})

		t.Run("seedFresh", func(t *testing.T) {
			if err := Prune(ctx, e.addr); err != nil {
				t.Fatal(err)
			}
			s := e.spec()
			s.SeedURL = e.seedURL
			res := requireBuild(t, ctx, s, e.cfg(t, minixCtx))
			// Same relative rule as the incremental leg: a hydrated tree must
			// compile a small fraction of a cold build, whatever the config
			// context makes that fraction.
			pulled := strings.Contains(res.Logs, "seed: pulled")
			if !pulled || (coldCC > 0 && res.CC*20 > coldCC) {
				t.Errorf("seed-fresh: pulled=%v CC=%d (cold %d) wall=%s",
					pulled, res.CC, coldCC, res.Wall)
			}
		})

		t.Run("s3Hit", func(t *testing.T) {
			if coldSum == ([32]byte{}) {
				t.Skip("cold did not run")
			}
			if err := Prune(ctx, e.addr); err != nil {
				t.Fatal(err)
			}
			cfg := e.cfg(t, e.ctxDir)
			s3, err := S3CacheURL(e.seedURL, "")
			if err != nil {
				t.Fatal(err)
			}
			cfg.CacheImports = append(cfg.CacheImports, s3)
			res := requireBuild(t, ctx, e.spec(), cfg)
			if res.CC != 0 {
				t.Errorf("s3 vertex hit still compiled %d objects", res.CC)
			}
			if fileSum(t, filepath.Join(cfg.OutDir, "vmlinux")) != coldSum {
				t.Error("s3-hit output differs from cold build")
			}
		})
	}

	if e.quick {
		t.Log("KBF_QUICK=1: skipping patchOn/patchEdit/concurrent")
		return
	}

	t.Run("patchOn", func(t *testing.T) {
		if coldSum == ([32]byte{}) {
			t.Skip("cold did not run")
		}
		s := e.spec()
		s.ApplyPatches = true
		cfg := e.cfg(t, e.ctxDir)
		res := requireBuild(t, ctx, s, cfg)
		// The tree may be warm (stamp nuke) or absent (a prior prune whose
		// vertex-cache hits never recreated the mount) — either way the patch
		// must actually apply and change the output. The stamp-mismatch path
		// itself is patchEdit's assertion, which always has a warm tree.
		if !strings.Contains(res.Logs, "applying /patches/") {
			t.Error("patch was not applied")
		}
		if fileSum(t, filepath.Join(cfg.OutDir, "vmlinux")) == coldSum {
			t.Error("patched vmlinux identical to unpatched golden")
		}
	})

	t.Run("patchEdit", func(t *testing.T) {
		dst := editConfig(t, e.ctxDir, "# CONFIG_MINIX_FS is not set", "# CONFIG_MINIX_FS is not set")
		pdir := filepath.Join(dst, "patches")
		ents, err := os.ReadDir(pdir)
		if err != nil || len(ents) == 0 {
			t.Fatalf("no patches dir in context: %v", err)
		}
		// Prepend header text: legal in a unified diff (leading description
		// lines), accepted by go-gitdiff, and changes the patch-series sha.
		// (Appending after the hunk is NOT safe — patch(1) tolerates trailing
		// garbage but go-gitdiff correctly rejects it.)
		p := filepath.Join(pdir, ents[0].Name())
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append([]byte("integration: content change\n"), b...), 0o644); err != nil {
			t.Fatal(err)
		}
		s := e.spec()
		s.ApplyPatches = true
		res := requireBuild(t, ctx, s, e.cfg(t, dst))
		if !strings.Contains(res.Logs, "stamp mismatch") {
			t.Error("patch content change did not invalidate the tree")
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		errc := make(chan error, 2)
		go func() {
			_, err := Build(ctx, e.spec(), e.cfg(t, e.ctxDir))
			errc <- err
		}()
		go func() {
			_, err := Build(ctx, e.spec(), e.cfg(t, minixCtx))
			errc <- err
		}()
		for i := 0; i < 2; i++ {
			if err := <-errc; err != nil {
				t.Errorf("concurrent build %d: %v", i, err)
			}
		}
	})
}
