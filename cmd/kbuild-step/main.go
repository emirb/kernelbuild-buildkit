// kbuild-step is the compile-vertex runner. It executes INSIDE the build
// container (mounted at /helper by the frontend) and replaces the bash script
// that used to drive the build: stamp/self-heal, seed pull/push (native S3),
// source extraction, patch application (go-gitdiff), config handling, and the
// vmlinux lift are all Go. The only program it runs is `make`, the kernel
// build itself.
//
// kbuild-step's own orchestration uses no shell, so the frontend's inputs
// have no shell-injection surface: they arrive as env vars validated by the
// frontend and as secret files, and are never re-parsed by an interpreter.
// (make itself runs shells and $(shell ...); that is the build, downstream
// of every input check, and is why the build description itself is the
// trust boundary: see SECURITY.md.)
//
// Contract (all provided by KernelLLB; this block IS the interface spec):
//
//	env    SOURCE_DATE_EPOCH KBUILD_BUILD_USER KBUILD_BUILD_HOST TZ
//	       SRC_ID SRC_URL SRC_SHA256 SRC_LOCAL
//	       KARCH CROSS_COMPILE TARGETS APPLY_PATCHES BASE_MAKE
//	       SEED_PUSH=1 (push builds only: the seed_cfg secret must then exist)
//	       HTTP_PROXY HTTPS_PROXY NO_PROXY (cache-neutral, via llb.WithProxy)
//	mounts /build (persistent cache mount, cwd)  /kernel.config (ro)
//	       /out (captured output mount)
//	       /src (ro, local-source mode only)
//	       /patches (ro, when APPLY_PATCHES=1)
//	       /run/secrets/{seed_access_key,seed_secret_key,seed_cfg} (optional)
//	out    /out/<targets>: vmlinux, bzImage or Image, modules.tar.zst, config
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	kbuild "github.com/emirb/kernelbuild-buildkit"
)

// Mount-contract paths (see the package comment). Vars, not consts, so unit
// tests can point them at temp dirs; production never reassigns them.
var (
	buildDir   = "/build"
	patchesDir = "/patches"
	configPath = "/kernel.config"
	expectPath = "/kernel.expect"
	secretsDir = "/run/secrets"
	srcDir     = "/src"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "kbuild-step:", err)
		os.Exit(1)
	}
}

func phase(name string, start time.Time) {
	fmt.Printf("KBF-PHASE %s %dms\n", name, time.Since(start).Milliseconds())
}

type seedCfg struct {
	url, key, region string
	push             bool
}

func run(ctx context.Context) error {
	ts, err := kbuild.Timestamp(os.Getenv("SOURCE_DATE_EPOCH"))
	if err != nil {
		return err
	}
	if err := os.Setenv("KBUILD_BUILD_TIMESTAMP", ts); err != nil {
		return err
	}
	if err := os.Chdir(buildDir); err != nil {
		return err
	}
	// Contract errors (an unknown TARGETS token, an empty list) must fail in
	// milliseconds — not after a seed pull, an extract and an olddefconfig.
	makeTargets, artifacts, wantModules, wantConfig, wantKconfigs, err := resolveTargets()
	if err != nil {
		return err
	}
	seed := loadSeedCfg()
	if err := checkSeedPush(seed); err != nil {
		return err
	}
	// A push killed mid-pack leaves a multi-GB .kbf-seed-*.tmp on the mount
	// that survives every warm build; sweep them before doing anything.
	if stale, _ := filepath.Glob(filepath.Join(buildDir, ".kbf-seed-*.tmp")); len(stale) > 0 {
		for _, p := range stale {
			_ = os.Remove(p)
		}
	}
	if err := prepareTree(ctx, seed); err != nil {
		return err
	}

	if err := configure(); err != nil {
		return err
	}

	// ---- expectation gate: fail HERE, not after a full compile ----
	if err := validateExpectations(expectPath); err != nil {
		return err
	}

	// ---- compile ----
	// Reset the .version link counter so a warm tree relinks exactly like a
	// cold one: kbuild's build-version script embeds "#N SMP" in the FINAL
	// UTS_VERSION (version-timestamp.c) from this file, and increments it per
	// relink — without the reset, incremental rebuilds can never byte-match a
	// cold golden ("#1"). (KBUILD_BUILD_VERSION is NOT the fix: it also leaks
	// into the temporary UTS_VERSION in version.c, which a cold build leaves
	// empty.) Found by the golden gate on the first warm-tree comparison.
	if err := os.Remove(".version"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if wantKconfigs {
		// Needs only the extracted tree: TARGETS=config,kconfigs resolves a
		// base config and its symbol catalog without compiling anything.
		t := time.Now()
		if err := bundleKconfigs("/out/kconfig.txt.gz"); err != nil {
			return fmt.Errorf("kconfigs: %w", err)
		}
		phase("kconfigs", t)
	}
	if wantModules {
		b, err := os.ReadFile(".config")
		if err != nil || !modulesEnabled(b) {
			return errors.New("target modules requested but CONFIG_MODULES is not enabled in the config")
		}
	}
	if len(makeTargets) > 0 {
		args := []string{fmt.Sprintf("-j%d", jobs())}
		// Reproducibility: silence gas's .note.gnu.property emission, which
		// varies with the assembler build. The flag is x86-specific — aarch64
		// gas rejects unknown -m options outright, so it must not reach a
		// cross build.
		if arch := os.Getenv("KARCH"); arch == "" || arch == "x86_64" {
			args = append(args, "KCFLAGS=-Wa,-mx86-used-note=no")
		}
		args = append(args, makeTargets...)
		if v := os.Getenv("KARCH"); v != "" {
			args = append(args, "ARCH="+v)
		}
		if v := os.Getenv("CROSS_COMPILE"); v != "" {
			args = append(args, "CROSS_COMPILE="+v)
		}
		t := time.Now()
		if err := runMake(args...); err != nil {
			return err
		}
		phase("compile", t)
	}
	for src, dst := range artifacts {
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	if wantConfig {
		if err := copyFile(".config", "/out/config"); err != nil {
			return err
		}
	}
	if wantModules {
		t := time.Now()
		if err := packModules(); err != nil {
			return err
		}
		phase("modules-pack", t)
	}

	// ---- seed publish (CI role) ----
	if seed != nil && seed.push {
		if err := seedPush(ctx, seed); err != nil {
			return fmt.Errorf("seed push: %w", err)
		}
	}
	return nil
}

// checkSeedPush fails a build that ASKED for a seed push (SEED_PUSH=1 in env,
// set by the frontend for seed-push=true) but cannot perform one: without the
// seed_cfg and credential secrets the vertex would execute on every request
// and never publish, with nothing in the log saying so.
func checkSeedPush(seed *seedCfg) error {
	if os.Getenv("SEED_PUSH") != "1" {
		return nil
	}
	if seed == nil {
		return errors.New("seed push requested but the seed_cfg, seed_access_key and seed_secret_key secrets are not all present: pass them (--secret id=seed_cfg,... etc.) or drop seed-push")
	}
	if !seed.push {
		return errors.New("seed push requested but seed_cfg carries SEED_PUSH=0")
	}
	return nil
}

// prepareTree brings the persistent mount to a tree that matches this build's
// inputs (stampWant): a warm tree with a matching stamp is kept; a
// mismatching one is replaced, but only after the replacement source has been
// verified; an absent tree is hydrated from the seed or extracted from the
// tarball.
func prepareTree(ctx context.Context, seed *seedCfg) error {
	// ---- stamp: does the persisted tree match this build's inputs? ----
	want, err := stampWant()
	if err != nil {
		return err
	}
	// The verified tarball, once acquired; the extract below reuses it so a
	// replacement never hashes or downloads twice.
	src := ""
	if treePresent() && readStamp() != want {
		fmt.Printf(">> stamp mismatch: have [%s] want [%s] - verifying the replacement source\n", readStamp(), want)
		// Prove the replacement BEFORE discarding the warm tree: acquireSource
		// verifies the pinned sha (hashing the cached tarball, or downloading
		// and hashing), so a mistyped SHA256 fails here with the object tree —
		// and its incremental cache — intact. Nuking first turned every typo
		// into a cold rebuild, and on a shared worker let any Kernelfile wipe
		// the tree for that version. (With a seed configured this may fetch a
		// tarball the seed then supersedes; the tarball is cached on the mount
		// and a stamp change is rare, so that one-time cost is accepted.)
		if src, err = acquireSource(ctx); err != nil {
			return err
		}
		fmt.Println(">> discarding tree")
		t := time.Now()
		if err := nukeTree(); err != nil {
			return err
		}
		phase("nuke", t)
	}

	// ---- hydrate: seed pull on a cold mount, else tarball extract ----
	// treePresent()==false does NOT mean empty: a nuke or extract killed
	// midway leaves stale objects without a Makefile/stamp, and hydrating
	// over them would link objects from a previous generation. Clear first.
	if !treePresent() {
		if err := nukeTree(); err != nil {
			return err
		}
	}
	if !treePresent() && seed != nil {
		t := time.Now()
		n, found, err := seedPull(ctx, seed)
		switch {
		case err != nil:
			return fmt.Errorf("seed pull: %w", err)
		case !found:
			fmt.Printf(">> seed: none at %s\n", seed.key)
		default:
			fmt.Printf(">> seed: pulled %s (%dM)\n", seed.key, n>>20)
			phase("seed-pull", t)
			// The stream's own stamp arrived quarantined (.kbf-stamp.seed);
			// the tree gains a LIVE stamp only here, after the pull fully
			// succeeded AND the seed's identity matches this build's inputs.
			// A key collision with different content (a re-rolled tarball, a
			// foreign bucket) is a discard, exactly as before.
			seedStamp, _ := os.ReadFile(".kbf-stamp.seed")
			_ = os.Remove(".kbf-stamp.seed")
			if string(seedStamp) != want {
				fmt.Println(">> seed stamp mismatch - discarding seed")
				t = time.Now()
				if err := nukeTree(); err != nil {
					return err
				}
				phase("seed-discard", t)
			} else if err := os.WriteFile(".kbf-stamp", []byte(want), 0o644); err != nil {
				return err
			}
		}
	}
	if !treePresent() {
		if src == "" {
			if src, err = acquireSource(ctx); err != nil {
				return err
			}
		}
		fmt.Printf(">> cold: extracting %s\n", src)
		t := time.Now()
		if err := extractTarball(src); err != nil {
			return fmt.Errorf("extract: %w", err)
		}
		phase("extract", t)
		if os.Getenv("APPLY_PATCHES") == "1" {
			if err := applyPatches(); err != nil {
				return err
			}
		}
		if err := os.WriteFile(".kbf-stamp", []byte(want), 0o644); err != nil {
			return err
		}
	} else {
		fmt.Println(">> warm: reusing persisted object tree")
	}
	return nil
}

// configure produces .config from the context config (optionally over a
// BASE_MAKE base) and runs olddefconfig, skipping all of it when the input is
// byte-identical to the last successful run's.
func configure() error {
	cfg, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	// The skip identity covers BASE_MAKE too: the same fragment over a
	// different make base is a different .config. (Old trees recorded the
	// bare fragment; their first build under this format re-runs olddefconfig
	// once, which is the safe direction.)
	baseMake := strings.Fields(os.Getenv("BASE_MAKE"))
	cfgID := append([]byte("base-make="+strings.Join(baseMake, " ")+"\x00"), cfg...)
	if prev, err := os.ReadFile(".kbf-config-src"); err == nil && bytes.Equal(prev, cfgID) {
		fmt.Println(">> config unchanged: skipping olddefconfig")
		return nil
	}
	t := time.Now()
	// Invalidate the skip marker BEFORE touching .config. The marker is only
	// written after olddefconfig succeeds; if the run fails or is cancelled in
	// between, a surviving marker would still name the previous config while
	// .config holds the new one — and the next build with the previous config
	// would skip olddefconfig, compile the wrong .config, and cache that
	// result under the previous config's key (a seeder would push it, too).
	if err := os.Remove(".kbf-config-src"); err != nil && !os.IsNotExist(err) {
		return err
	}
	archArgs := func(args []string) []string {
		if v := os.Getenv("KARCH"); v != "" {
			args = append(args, "ARCH="+v)
		}
		if v := os.Getenv("CROSS_COMPILE"); v != "" {
			args = append(args, "CROSS_COMPILE="+v)
		}
		return args
	}
	if len(baseMake) > 0 {
		// Base-make mode: the tree's own config targets produce .config,
		// then the context config is appended as a fragment (later lines
		// win through olddefconfig) — "defconfig plus deltas" without the
		// caller needing a source tree of its own.
		fmt.Printf(">> base-make: %s\n", strings.Join(baseMake, " "))
		if err := runMake(archArgs(append([]string{}, baseMake...))...); err != nil {
			return fmt.Errorf("base-make: %w", err)
		}
		if len(bytes.TrimSpace(cfg)) > 0 {
			f, err := os.OpenFile(".config", os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if _, err := f.Write(append([]byte("\n"), cfg...)); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	} else if err := os.WriteFile(".config", cfg, 0o644); err != nil {
		return err
	}
	if err := runMake(archArgs([]string{"olddefconfig"})...); err != nil {
		return err
	}
	if err := os.WriteFile(".kbf-config-src", cfgID, 0o644); err != nil {
		return err
	}
	phase("olddefconfig", t)
	return nil
}

func treePresent() bool { _, err := os.Stat("Makefile"); return err == nil }

// acquireSource returns a path to the source tarball. Local mode uses the
// /src mount. URL mode downloads IN-STEP, only on this cold path — a warm or
// seeded tree never pays the (~200MB) transfer, which a graph-level source
// mount forced on every fresh worker. The tarball is cached on the mount
// (spared by nukeTree) and reused when its sha256 still matches, so stamp
// nukes (patch toggles) do not re-download either.
func acquireSource(ctx context.Context) (string, error) {
	if name := os.Getenv("SRC_LOCAL"); name != "" {
		// Validate() constrained this in another process; re-check here so
		// the binary is safe standalone (defense in depth for the one
		// component that runs inside the build).
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
			return "", fmt.Errorf("SRC_LOCAL %q: bad name", name)
		}
		return filepath.Join(srcDir, name), nil
	}
	srcURL, sum := os.Getenv("SRC_URL"), os.Getenv("SRC_SHA256")
	if sum == "" {
		fmt.Println(">> WARNING: no source sha256 — the fetch is not content-addressed; pin SHA256 for reproducible builds")
	}
	ext, err := kbuild.SourceExt(srcURL)
	if err != nil {
		return "", err
	}
	cached := filepath.Join(buildDir, ".kbf-src.tar"+ext)
	if sum != "" {
		if got, err := fileSHA256(cached); err == nil && got == sum {
			fmt.Println(">> source: cached tarball matches sha256, no download")
			return cached, nil
		}
	}
	t := time.Now()
	if err := download(ctx, srcURL, cached+".part"); err != nil {
		return "", fmt.Errorf("download %s: %w", redactURL(srcURL), err)
	}
	if sum != "" {
		got, err := fileSHA256(cached + ".part")
		if err != nil {
			return "", err
		}
		if got != sum {
			_ = os.Remove(cached + ".part")
			return "", fmt.Errorf("source sha256 mismatch: got %s want %s", got, sum)
		}
	}
	if err := os.Rename(cached+".part", cached); err != nil {
		return "", err
	}
	phase("source-fetch", t)
	return cached, nil
}

// policyError is a refusal the fetch itself decided on (a cleartext redirect,
// a link-local destination): final, never retried.
type policyError struct{ msg string }

func (e *policyError) Error() string { return e.msg }

// isPermanent reports whether a fetch error can only recur: the client's own
// policy refusals and a name that does not exist. Everything else (resets,
// timeouts, 5xx) is worth the retry loop.
func isPermanent(err error) bool {
	if _, ok := errors.AsType[*policyError](err); ok {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// lookupIP is the resolver behind the link-local guard; a var so tests can
// hand it answers no real DNS would give.
var lookupIP = net.DefaultResolver.LookupIPAddr

// downloadClient builds the HTTP client used for the in-step source fetch:
// env proxy honored, link-local destinations refused, non-https redirects
// rejected.
func downloadClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Minute, // whole-transfer ceiling; kernel.org serves ~230MB
		Transport: &http.Transport{
			// A custom Transport defaults Proxy to nil, which would ignore
			// HTTPS_PROXY that the frontend passes for MITM-proxy sandboxes.
			// Restore the standard env-proxy behavior.
			Proxy: http.ProxyFromEnvironment,
			// Refuse link-local destinations so a caller-supplied SRC_URL
			// (or a redirect to one) cannot reach the cloud metadata endpoint
			// (169.254.169.254 / fe80::, plus AWS's unique-local IPv6 IMDS
			// address) from inside a build with egress. Defense in depth: the
			// https-only rule already blocks the plain-HTTP metadata services,
			// and this dialer is not consulted when HTTPS_PROXY is set (the
			// proxy resolves the destination then). Private RFC1918 ranges
			// stay allowed: self-hosted mirrors use them.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := lookupIP(ctx, host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					if ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() || ip.IP.Equal(awsIMDSv6) {
						return nil, &policyError{fmt.Sprintf("refusing link-local or metadata destination %s", ip.IP)}
					}
				}
				// Dial the ADDRESS that was vetted, never the name again: a
				// second lookup is a second answer, and a rebinding resolver
				// (public address first, 169.254.169.254 next) would slip
				// through a check-then-dial-by-name.
				d := &net.Dialer{Timeout: 30 * time.Second}
				var lastErr error
				for _, ip := range ips {
					c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
					if err == nil {
						return c, nil
					}
					lastErr = err
				}
				if lastErr == nil {
					lastErr = fmt.Errorf("no addresses for %s", host)
				}
				return nil, lastErr
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return &policyError{"too many redirects"}
			}
			// Validate enforced https on the pinned URL; a redirect must not
			// quietly downgrade the fetch to cleartext.
			if req.URL.Scheme != "https" {
				return &policyError{"refusing non-https redirect to " + redactURL(req.URL.String())}
			}
			return nil
		},
	}
}

// download fetches url to dst, resuming with HTTP Range on a mid-stream
// failure (a slow or flaky CI egress truncated a 230MB fetch with unexpected
// EOF and no retry). The sha256 check in the caller is the integrity gate;
// this only makes the transfer survive a dropped connection.
func download(ctx context.Context, rawURL, dst string) error {
	client := downloadClient()
	// Start clean: a stale .part from a prior run would make attempt 1 send a
	// Range past EOF. Resume then only accumulates within this call.
	_ = os.Remove(dst)
	const maxAttempts = 5
	var lastErr error
	var retryAfter time.Duration
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			wait := max(retryAfter, time.Duration(attempt)*retryBase)
			retryAfter = 0
			if err := retrySleep(ctx, wait); err != nil {
				return err
			}
		}
		have := int64(0)
		if fi, err := os.Stat(dst); err == nil {
			have = fi.Size()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		if have > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		}
		resp, err := client.Do(req)
		if err != nil {
			redactErr(err)
			if isPermanent(err) {
				// A refusal we made ourselves, or a name that does not exist:
				// five more attempts and ~28s of backoff change nothing.
				return err
			}
			lastErr = err
			continue
		}
		// 206 resumes from `have`; 200 means Range was ignored, restart clean.
		flag := os.O_APPEND | os.O_WRONLY
		switch {
		case resp.StatusCode == http.StatusOK:
			flag = os.O_TRUNC | os.O_WRONLY | os.O_CREATE
		case resp.StatusCode == http.StatusPartialContent:
			// keep appending from `have`
		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && have > 0:
			// The server says our resume offset is past the end (a truncated
			// write that still reached EOF, a re-rolled file). Drop what we
			// have and restart the transfer instead of failing the build.
			_ = resp.Body.Close()
			_ = os.Remove(dst)
			lastErr = fmt.Errorf("HTTP %s", resp.Status)
			continue
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			// cdn.kernel.org rate-limits/503s a busy egress IP; transient.
			if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > retryAfter {
				retryAfter = ra
			}
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %s", resp.Status)
			if attempt < maxAttempts {
				fmt.Printf(">> download got %s, retrying\n", resp.Status)
			}
			continue
		default:
			_ = resp.Body.Close()
			return fmt.Errorf("HTTP %s", resp.Status)
		}
		f, err := os.OpenFile(dst, flag|os.O_CREATE, 0o644)
		if err != nil {
			_ = resp.Body.Close()
			return err
		}
		_, copyErr := io.Copy(f, resp.Body)
		_ = resp.Body.Close()
		if cerr := f.Close(); cerr != nil && copyErr == nil {
			copyErr = cerr
		}
		if copyErr == nil {
			return nil // clean EOF = complete
		}
		lastErr = copyErr
		if attempt < maxAttempts {
			fmt.Printf(">> download interrupted (%v), resuming from %d\n", copyErr, have)
		}
	}
	return fmt.Errorf("fetch failed after %d attempts: %w", maxAttempts, lastErr)
}

// retryBase is the per-attempt backoff unit: attempt n waits n*retryBase
// (or the server's Retry-After when longer).
const retryBase = 2 * time.Second

// retrySleep waits out a retry backoff, or returns early when ctx ends. A
// variable so the tests record the schedule instead of sleeping through it.
var retrySleep = func(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// awsIMDSv6 is the AWS instance metadata service's IPv6 address. It is
// unique-local (fd00::/8), not link-local, so the IsLinkLocal checks miss it.
var awsIMDSv6 = net.ParseIP("fd00:ec2::254")

// redactURL strips userinfo, query and fragment from a URL before it is
// logged or embedded in an error: a private mirror may carry basic-auth in
// the URL, and a redirect target is often a presigned URL whose signature
// lives in the query. The build log is not a secret channel.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	u.User = nil
	u.RawQuery, u.ForceQuery, u.Fragment = "", false, ""
	return u.String()
}

// redactErr applies redactURL to the URL net/http embeds in its *url.Error
// (which strips the password but keeps the query).
func redactErr(err error) {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		ue.URL = redactURL(ue.URL)
	}
}

// parseRetryAfter reads a Retry-After header in delta-seconds form (the HTTP-date
// form is ignored here; the fixed backoff covers it). Capped so a hostile value
// cannot stall the build.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	if n > 30 {
		n = 30
	}
	return time.Duration(n) * time.Second
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readStamp() string {
	b, err := os.ReadFile(".kbf-stamp")
	if err != nil {
		return "none"
	}
	return string(b)
}

// stampWant identifies the inputs the tree must have been produced from:
// source identity + sha of the patch series content.
func stampWant() (string, error) {
	psha := "none"
	if os.Getenv("APPLY_PATCHES") == "1" {
		names, err := patchFiles()
		if err != nil {
			return "", err
		}
		h := sha256.New()
		for _, n := range names {
			b, err := os.ReadFile(n)
			if err != nil {
				return "", err
			}
			h.Write(b)
		}
		psha = hex.EncodeToString(h.Sum(nil))[:16]
	}
	srcID := os.Getenv("SRC_ID")
	if name := os.Getenv("SRC_LOCAL"); name != "" {
		// A filename is not an identity: hash the actual tarball, or a file
		// swapped under the same name would reuse the previous tree AND
		// cache the stale result against the new content.
		sum, err := fileSHA256(filepath.Join(srcDir, name))
		if err != nil {
			return "", fmt.Errorf("hash local source: %w", err)
		}
		srcID = "local:" + sum
	}
	return fmt.Sprintf("src=%s patches=%s", srcID, psha), nil
}

func patchFiles() ([]string, error) {
	names, err := filepath.Glob(filepath.Join(patchesDir, "*.patch"))
	if err != nil || len(names) == 0 {
		return nil, fmt.Errorf("APPLY_PATCHES=1 but no %s/*.patch found", patchesDir)
	}
	sort.Strings(names)
	return names, nil
}

func nukeTree() error {
	ents, err := os.ReadDir(buildDir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".kbf-src") {
			continue // the cached source tarball survives tree nukes
		}
		if err := os.RemoveAll(filepath.Join(buildDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// extractTarball decompresses and extracts the source tarball, stripping the
// leading path component (linux-X.Y.Z/) like tar --strip-components=1.
// .tar.gz and .tar.zst decode natively (klauspost, pure Go — gzip decodes
// faster than C xz, which is why .tar.gz is the default source); .tar.xz
// decodes via ulikunitz/xz, slower but pure Go and rare by default.
func extractTarball(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	switch {
	case strings.HasSuffix(path, ".gz"):
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer func() { _ = zr.Close() }()
		return untar(buildDir, zr, 1)
	case strings.HasSuffix(path, ".zst"):
		zr, err := zstd.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		return untar(buildDir, zr, 1)
	case strings.HasSuffix(path, ".xz"):
		zr, err := xz.NewReader(f)
		if err != nil {
			return err
		}
		return untar(buildDir, zr, 1)
	default:
		return fmt.Errorf("unsupported source tarball %q", path)
	}
}

// untar extracts into dir, restoring mode and mtime (make correctness
// depends on mtimes) and handling symlinks and hardlinks. All filesystem
// operations go through os.Root, so neither ../ names nor symlink members can
// write outside dir (the kernel refuses traversal) — hostile tarballs are a
// real input once SOURCE_URL is user-supplied.
func untar(dir string, r io.Reader, strip int) error {
	return untarFiltered(dir, r, strip, nil)
}

func untarFiltered(dir string, r io.Reader, strip int, rename func(string) string) (err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	// Small regular files go to the path-sharded write pool (untarpool.go);
	// anything order-sensitive stays inline behind a flush barrier. The pool
	// must always be drained before this function returns, even on a reader
	// error, or its workers race the caller's cleanup (nukeTree).
	pool := newUntarPool(root)
	defer func() {
		if cerr := pool.close(); err == nil {
			err = cerr
		}
	}()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name, ok := stripComponents(hdr.Name, strip)
		if !ok {
			continue
		}
		if rename != nil {
			if name = rename(name); name == "" {
				continue
			}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			// No flush needed: tar walk order puts a directory before its
			// contents, so no queued write touches it yet.
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
			if err := root.Chmod(name, hdr.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size > untarInlineLimit {
				// Streamed, not buffered — and behind the barrier so a
				// same-path queued write cannot interleave.
				if err := pool.flush(); err != nil {
					return err
				}
				if err := writeEntry(root, name, tr, hdr); err != nil {
					return err
				}
			} else if err := pool.dispatch(name, hdr, tr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Barrier: a queued write to this path (or through it) must land
			// under pre-symlink semantics, exactly like the serial order did.
			if err := pool.flush(); err != nil {
				return err
			}
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return err
			}
			if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
		case tar.TypeLink:
			ln, ok := stripComponents(hdr.Linkname, strip)
			if !ok {
				continue
			}
			// Barrier: the link target may still be sitting in a worker queue.
			if err := pool.flush(); err != nil {
				return err
			}
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return err
			}
			if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := root.Link(ln, name); err != nil {
				return err
			}
		}
	}
}

// writeEntry writes one regular file, restoring mode and mtime.
func writeEntry(root *os.Root, name string, r io.Reader, hdr *tar.Header) error {
	if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	// Remove-then-create-exclusively: O_TRUNC on an existing path follows a
	// symlink planted by an earlier entry of the same stream and writes
	// THROUGH it (os.Root only confines the destination, it does not refuse
	// links). Tar semantics replace the entry instead.
	if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, hdr.FileInfo().Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Chmod(name, hdr.FileInfo().Mode().Perm()); err != nil {
		return err
	}
	return root.Chtimes(name, hdr.ModTime, hdr.ModTime)
}

func stripComponents(name string, strip int) (string, bool) {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(name)), "/")
	if len(parts) <= strip {
		return "", false
	}
	return filepath.Join(parts[strip:]...), true
}

// applyPatches applies the series with go-gitdiff — strict positions, no
// fuzz, byte-identical to patch(1) on a clean apply.
func applyPatches() error {
	names, err := patchFiles()
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(buildDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, n := range names {
		fmt.Printf("applying %s\n", n)
		raw, err := os.ReadFile(n)
		if err != nil {
			return err
		}
		files, _, err := gitdiff.Parse(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("%s: parse: %w", n, err)
		}
		for _, f := range files {
			oldName := strings.TrimPrefix(strings.TrimPrefix(f.OldName, "a/"), "b/")
			newName := strings.TrimPrefix(strings.TrimPrefix(f.NewName, "b/"), "a/")
			switch {
			case f.IsNew:
				var out bytes.Buffer
				if err := gitdiff.Apply(&out, bytes.NewReader(nil), f); err != nil {
					return fmt.Errorf("%s: create %s: %w", n, newName, err)
				}
				if err := root.MkdirAll(filepath.Dir(newName), 0o755); err != nil {
					return err
				}
				// git records the mode ("new file mode 100755"); a script
				// created 0644 fails the moment a Makefile invokes it.
				mode := os.FileMode(0o644)
				if f.NewMode != 0 {
					mode = f.NewMode.Perm()
				}
				if err := root.WriteFile(newName, out.Bytes(), mode); err != nil {
					return err
				}
				if err := root.Chmod(newName, mode); err != nil {
					return err
				}
			case f.IsDelete:
				if err := root.Remove(oldName); err != nil {
					return fmt.Errorf("%s: delete %s: %w", n, oldName, err)
				}
			default:
				src, err := root.ReadFile(oldName)
				if err != nil {
					return fmt.Errorf("%s: %w", n, err)
				}
				var out bytes.Buffer
				if len(f.TextFragments) == 0 && f.BinaryFragment == nil {
					// A mode-only (or pure rename) entry carries no hunks:
					// the content passes through unchanged.
					out.Write(src)
				} else if err := gitdiff.Apply(&out, bytes.NewReader(src), f); err != nil {
					return fmt.Errorf("%s: apply to %s: %w", n, oldName, err)
				}
				st, err := root.Stat(oldName)
				if err != nil {
					return err
				}
				// "old mode / new mode" is a real change (a script becoming
				// executable); it used to be silently dropped while the stamp
				// still recorded the patch as applied.
				mode := st.Mode().Perm()
				if f.NewMode != 0 {
					mode = f.NewMode.Perm()
				}
				if err := root.WriteFile(newName, out.Bytes(), mode); err != nil {
					return err
				}
				if err := root.Chmod(newName, mode); err != nil {
					return err
				}
				if f.IsRename && oldName != newName {
					if err := root.Remove(oldName); err != nil {
						return fmt.Errorf("%s: rename %s: %w", n, oldName, err)
					}
				}
			}
		}
	}
	return nil
}

// resolveTargets maps the TARGETS env (validated tokens from Spec.Targets)
// to kbuild make targets and the artifacts lifted into /out. "config" and
// "modules" have no plain-file mapping — the caller copies .config and packs
// modules_install output respectively.
// modulesEnabled reports whether the resolved config builds modules,
// line-anchored so a first-line CONFIG_MODULES=y counts and a commented or
// suffixed occurrence does not.
func modulesEnabled(config []byte) bool {
	return bytes.HasPrefix(config, []byte("CONFIG_MODULES=y\n")) ||
		bytes.Contains(config, []byte("\nCONFIG_MODULES=y\n"))
}

func resolveTargets() (makeTargets []string, artifacts map[string]string, wantModules, wantConfig, wantKconfigs bool, err error) {
	arch := os.Getenv("KARCH")
	artifacts = map[string]string{}
	raw := os.Getenv("TARGETS")
	if raw == "" {
		// The frontend always sets TARGETS; an empty value means the runner
		// is being driven outside its contract, and "successfully built
		// nothing" would be the worst possible answer.
		return nil, nil, false, false, false, errors.New("TARGETS is empty")
	}
	for tgt := range strings.SplitSeq(raw, ",") {
		switch tgt {
		case "vmlinux":
			makeTargets = append(makeTargets, "vmlinux")
			artifacts["vmlinux"] = "/out/vmlinux"
		case "image":
			// The arch's bootable image: bzImage on x86, the PE Image on
			// arm64 (what its bootloaders and Firecracker consume).
			if arch == "arm64" {
				makeTargets = append(makeTargets, "Image")
				artifacts["arch/arm64/boot/Image"] = "/out/Image"
			} else {
				makeTargets = append(makeTargets, "bzImage")
				artifacts["arch/x86/boot/bzImage"] = "/out/bzImage"
			}
		case "modules":
			makeTargets = append(makeTargets, "modules")
			wantModules = true
		case "config":
			wantConfig = true
		case "kconfigs":
			wantKconfigs = true
		default:
			return nil, nil, false, false, false, fmt.Errorf("TARGETS token %q unknown", tgt)
		}
	}
	return
}

// packModules stages modules_install into a temp dir and packs it as
// /out/modules.tar.zst (Go tar+zstd, mtimes preserved — no shell).
func packModules() error {
	stage, err := os.MkdirTemp("", "modstage-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	args := []string{"modules_install", "INSTALL_MOD_PATH=" + stage, "INSTALL_MOD_STRIP=1"}
	if v := os.Getenv("KARCH"); v != "" {
		args = append(args, "ARCH="+v)
	}
	if v := os.Getenv("CROSS_COMPILE"); v != "" {
		args = append(args, "CROSS_COMPILE="+v)
	}
	if err := runMake(args...); err != nil {
		return fmt.Errorf("modules_install: %w", err)
	}
	out, err := os.Create("/out/modules.tar.zst")
	if err != nil {
		return err
	}
	zw, err := zstd.NewWriter(out, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		_ = out.Close()
		return err
	}
	tw := tar.NewWriter(zw)
	err = filepath.WalkDir(stage, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == stage {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(stage, p)
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if info.IsDir() {
			hdr.Name += "/"
		}
		// The exported artifact must be content-addressed like everything
		// else this pipeline emits: clamp wall-clock install mtimes to the
		// build epoch and drop host uid/gid. (The seed pack keeps real
		// mtimes on purpose; make needs them.)
		if ts, err := strconv.ParseInt(os.Getenv("SOURCE_DATE_EPOCH"), 10, 64); err == nil {
			hdr.ModTime = time.Unix(ts, 0).UTC()
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p) //nolint:gosec // walking the build's own locked tree, not user-controlled paths
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			_ = f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Close()
}

// jobs is make's -j. GOMAXPROCS, not NumCPU: the Go runtime derives it from
// the cgroup CPU limit, while NumCPU reports the host's CPUs even when the
// build container is capped (measured 16 vs a 2-CPU quota) — a build under
// `docker build --cpus` or a k8s limit would otherwise oversubscribe make by
// the quota ratio and thrash.
func jobs() int {
	if n := runtime.GOMAXPROCS(0); n > 0 {
		return n
	}
	return 1
}

func runMake(args ...string) error {
	cmd := exec.Command("make", args...) //nolint:gosec,noctx // argv only, every element validated; make runs to completion, BuildKit cancels the whole exec
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ---- seed: native S3 against R2 ----

func loadSeedCfg() *seedCfg {
	raw, err := os.ReadFile(filepath.Join(secretsDir, "seed_cfg"))
	if err != nil {
		return nil
	}
	c := &seedCfg{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "SEED_URL":
			c.url = v
		case "SEED_KEY":
			c.key = v
		case "SEED_REGION":
			c.region = v
		case "SEED_PUSH":
			c.push = v == "1"
		}
	}
	if c.url == "" || c.key == "" {
		return nil
	}
	// Both credential secrets, or no seeding: gating on one while s3Client
	// reads both would turn a half-mounted credential pair into a hard build
	// failure instead of a skipped seed.
	for _, name := range []string{"seed_access_key", "seed_secret_key"} {
		if _, err := os.Stat(filepath.Join(secretsDir, name)); err != nil {
			return nil
		}
	}
	return c
}

func s3Client(ctx context.Context, c *seedCfg) (*s3.Client, string, error) {
	endpoint, bucket, ok := strings.Cut(strings.TrimPrefix(c.url, "https://"), "/")
	if !ok || bucket == "" {
		return nil, "", fmt.Errorf("SEED_URL %q: want https://host/bucket", c.url)
	}
	ak, err := os.ReadFile(filepath.Join(secretsDir, "seed_access_key"))
	if err != nil {
		return nil, "", err
	}
	sk, err := os.ReadFile(filepath.Join(secretsDir, "seed_secret_key"))
	if err != nil {
		return nil, "", err
	}
	region := c.region
	if region == "" {
		region = "auto"
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			strings.TrimSpace(string(ak)), strings.TrimSpace(string(sk)), "")),
	)
	if err != nil {
		return nil, "", err
	}
	cl := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("https://" + endpoint)
		o.UsePathStyle = true
	})
	return cl, bucket, nil
}

// seedPull downloads and extracts the seed. found is false when the seed
// object does not exist yet.
func seedPull(ctx context.Context, c *seedCfg) (n int64, found bool, err error) {
	cl, bucket, err := s3Client(ctx, c)
	if err != nil {
		return 0, false, err
	}
	obj, err := cl.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(c.key),
	})
	if err != nil {
		if _, ok := errors.AsType[*types.NoSuchKey](err); ok {
			return 0, false, nil
		}
		return 0, false, err
	}
	defer func() { _ = obj.Body.Close() }()
	zr, err := zstd.NewReader(obj.Body)
	if err != nil {
		return 0, false, err
	}
	defer zr.Close()
	// The stamp is the LAST thing a hydrated tree may gain: untarStripStamp
	// refuses to extract .kbf-stamp from the seed, and the caller writes it
	// only after the pull fully succeeds. Without this, an interrupted pull
	// leaves a partial tree that the (lexically early) stamp would bless as
	// warm forever, with no self-heal path.
	if err := untarSeed(buildDir, zr); err != nil {
		// A partial tree without a stamp already self-heals (stamp mismatch
		// nukes it), but clean up eagerly so the next build starts cold.
		_ = nukeTree()
		return 0, false, err
	}
	return aws.ToInt64(obj.ContentLength), true, nil
}

// untarSeed extracts a seed stream with the stamp QUARANTINED: the seed's
// .kbf-stamp lands as .kbf-stamp.seed, so an interrupted pull can never
// leave a tree the live stamp blesses, while the caller can still compare
// the seed's identity against its own.
func untarSeed(dir string, r io.Reader) error {
	return untarFiltered(dir, r, 0, func(name string) string {
		if name == ".kbf-stamp" {
			return ".kbf-stamp.seed"
		}
		return name
	})
}

// seedPush packs the tree (Go tar + zstd, mtimes preserved for make) into a
// temp file and uploads it.
func seedPush(ctx context.Context, c *seedCfg) error {
	cl, bucket, err := s3Client(ctx, c)
	if err != nil {
		return err
	}
	t := time.Now()
	// The temp file lives in the cache mount (excluded from the pack walk),
	// not the rootfs — the exec's rootfs diff stays empty.
	tmp, err := os.CreateTemp(buildDir, ".kbf-seed-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	defer func() { _ = tmp.Close() }()
	zw, err := zstd.NewWriter(tmp, zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(runtime.NumCPU()))
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	err = filepath.WalkDir(buildDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == buildDir {
			return err
		}
		rel, _ := filepath.Rel(buildDir, p)
		if base := filepath.Base(rel); strings.HasPrefix(base, ".kbf-seed-") || strings.HasPrefix(base, ".kbf-src") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p) //nolint:gosec // walking the build's own locked tree, not user-controlled paths
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			_ = f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	phase("seed-pack", t)

	st, err := tmp.Stat()
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	t = time.Now()
	// Use multipart even though the archive has a known length. New AWS SDKs
	// attach checksum trailers with aws-chunked encoding to PutObject; MinIO
	// rejects chunks over 16 MiB. Eight-MiB multipart parts work with AWS S3
	// and S3-compatible stores while preserving per-part checksums.
	uploader := transfermanager.New(cl, func(o *transfermanager.Options) {
		o.PartSizeBytes = 8 << 20
		o.MultipartUploadThreshold = 8 << 20
		o.Concurrency = 2
	})
	_, err = uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(c.key), Body: tmp,
		ContentLength: aws.Int64(st.Size()),
	})
	if err != nil {
		return err
	}
	phase("seed-push", t)
	fmt.Printf(">> seed: pushed %s (%dM)\n", c.key, st.Size()>>20)
	return nil
}
