package kbuild

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/moby/buildkit/client/llb"
)

// aptDeps is what the kernel build itself needs. Fetch, patch, seed and all
// codecs are native Go in kbuild-step; nothing else executes in the vertex.
const aptDeps = "bc bison build-essential dwarves flex libelf-dev " +
	"libssl-dev ca-certificates cpio kmod"

// sh wraps a script as a bash -c RunOption (toolchain provisioning only — the
// compile vertex runs kbuild-step directly, argv only, no shell in the
// orchestration; make itself of course runs shells).
func sh(script string) llb.RunOption {
	return llb.Args([]string{"bash", "-c", script})
}

// SeedCfg renders the cache-neutral seed configuration that the client passes
// as the "seed_cfg" BuildKit secret. It lives in a secret, not env or opts,
// deliberately: whether (and where) the object tree is seeded has no effect on
// the built vmlinux, so it must not perturb the vertex cache key — and secrets
// are excluded from cache keys while env vars are not. Returns nil when
// seeding is disabled.
func (s Spec) SeedCfg() []byte {
	if s.SeedURL == "" {
		return nil
	}
	push := "0"
	if s.SeedPush {
		push = "1"
	}
	// The key carries the base-image hash for the same reason the cache mount
	// does: an object tree is only valid for the toolchain that built it.
	key := fmt.Sprintf("%s/linux-%s-%s-%.8x%s.tzst", s.SeedPrefix, s.KernelVersion, s.archOrDefault(), s.toolchainHash(), s.patchedSuffix())
	region := s.SeedRegion
	if region == "" {
		region = "auto"
	}
	return fmt.Appendf(nil, "SEED_URL=%s\nSEED_KEY=%s\nSEED_REGION=%s\nSEED_PUSH=%s\n", s.SeedURL, key, region, push)
}

// KernelLLB builds the LLB graph and returns the captured /out state holding
// the selected target artifacts (default: vmlinux). This IS the frontend — the build graph is
// generated in Go, not parsed from a Dockerfile.
//
// Caching has three layers:
//
//  1. Coarse, automatic, content-addressed (BuildKit vertex cache): the
//     toolchain vertex and the compile vertex are cache-keyed by content, the
//     source identity (pinned sha256) among the inputs — an identical build is
//     an instant full hit. Exportable to S3/R2 or a registry via the client's
//     cache export options, so a fresh worker gets the full hit too.
//
//  2. Fine, object-level incremental (persistent cache mount): the kernel tree
//     lives in a locked per-version cache mount at /build. A config change
//     re-runs the compile vertex, but kbuild's own dependency tracking
//     recompiles only the objects the changed CONFIG symbols touch. No ccache.
//
//  3. Remote object-tree seed (ours): BuildKit's cache exporters do NOT cover
//     cache-mount contents, so layer 2 alone dies with the worker. When a
//     seed is configured (via the seed_cfg/seed_access_key/seed_secret_key secrets), a cold
//     mount hydrates from S3-compatible storage before compiling, and a CI
//     build can push the tree back after compiling.
//
// The compile vertex runs the kbuild-step Go binary directly (argv, no shell):
// stamp/self-heal, seed transfer, fetch, extraction, and patching are Go; the
// only program it executes is `make`. Cache-key hygiene: everything that
// affects the output (source identity, config, patches, epoch, CA file) is env
// or graph structure — in the key. Everything that doesn't (proxy via
// llb.WithProxy, seed destination + credentials via secrets) — out of the key.
func KernelLLB(s Spec) (llb.State, error) {
	if err := s.Validate(); err != nil {
		return llb.State{}, err
	}

	if !strings.Contains(s.Base, "@") {
		// The persistent object tree and the seed are keyed by the Base
		// STRING (toolchainHash). A moving tag would share one tree across two
		// different compilers — the exact mixing the mount id exists to
		// prevent — so the graph refuses an unpinned ref. GatewaySolve (the
		// path both the frontend and the client driver take) resolves a tag
		// to its digest through the daemon before calling here.
		return llb.State{}, fmt.Errorf("base %q: must be pinned by digest (ref@sha256:...); resolve the tag first", s.Base)
	}

	var proxy []llb.RunOption
	if s.HTTPSProxy != "" || s.HTTPProxy != "" || s.NoProxy != "" {
		// llb.WithProxy lands in ExecOp.Meta.ProxyEnv, which CacheMap nils out
		// before hashing — the proxy can move (it does, across sandbox
		// sessions) without invalidating a single vertex. All three standard
		// variables travel: apt's stock mirrors are http://, so an https-only
		// proxy left the toolchain vertex without egress behind a plain
		// corporate proxy, and a user no_proxy (an internal mirror) was dropped.
		noProxy := "localhost,127.0.0.1"
		if s.NoProxy != "" {
			noProxy += "," + s.NoProxy
		}
		proxy = append(proxy, llb.WithProxy(llb.ProxyEnv{
			HTTPProxy:  s.HTTPProxy,
			HTTPSProxy: s.HTTPSProxy,
			NoProxy:    noProxy,
		}))
	}

	// --- toolchain vertex (cached; exportable) ---
	base := llb.Image(s.Base)
	if s.NetworkHost {
		// docker build --network=host reaches RUN ops only if the frontend
		// applies it; a worker whose sandbox network has no egress (e.g.
		// dockerd with iptables disabled) would otherwise fail apt/seed
		// transfers. Set once on the base state — it threads through the
		// whole chain to both Runs.
		base = base.Network(llb.NetModeHost)
	}
	// With ToolchainReady (a prebuilt toolchain image: tuxmake/*, a pinned
	// GHCR image), the graph starts from the image directly — no apt vertex.
	// The tiny CA-append exec survives only for MITM-proxy sandboxes.
	toolchain := base
	if !s.ToolchainReady || s.ProxyCAFile != "" {
		script := toolchainScript
		if s.ToolchainReady {
			script = caOnlyScript
		}
		tcOpts := []llb.RunOption{
			sh(script),
			llb.AddEnv("DEBIAN_FRONTEND", "noninteractive"),
			llb.WithCustomName("[toolchain] install build deps"),
		}
		tcOpts = append(tcOpts, proxy...)
		if s.IgnoreCache {
			tcOpts = append(tcOpts, llb.IgnoreCache)
		}
		if s.ProxyCAFile != "" {
			// The CA comes from the build context (a plain public cert, not a
			// secret), so stock `docker build` can provide it. Only that file
			// is synced/hashed — the toolchain vertex must not depend on the
			// rest of the context.
			caState := llb.Local("context", llb.FollowPaths([]string{s.ProxyCAFile}),
				llb.SharedKeyHint("kbuild-ca"), llb.WithCustomName("[internal] load "+s.ProxyCAFile))
			tcOpts = append(tcOpts,
				llb.AddMount("/certs", caState, llb.Readonly),
				llb.AddEnv("CA_FILE", s.ProxyCAFile))
		}
		toolchain = base.Run(tcOpts...).Root()
	}

	// --- source identity ---
	// No graph-level source vertex for URL mode: a mount is materialized
	// before exec regardless of need, which forced every fresh worker to
	// download ~200MB the seeded tree never reads. kbuild-step fetches
	// in-step, only on the cold path, sha256-verified, and caches the tarball
	// on the mount (surviving tree nukes). URL and sha stay in env — in the
	// cache key, exactly as the HTTP vertex digest was. Note the URL is part
	// of the key even when the sha pins the content: the step needs it and
	// env is the only channel a stock docker build can carry it on (a secret
	// would need the client's cooperation), so two mirrors of the same
	// tarball are two vertices. Use one canonical SOURCE_URL across workers.
	srcID := s.SourceSHA256
	srcURL, srcSHA := s.SourceURL, s.SourceSHA256
	if s.SourceLocalName != "" {
		// Local mode: URL and sha are unused; keeping DefaultSpec leftovers
		// in the env would pollute the compile vertex's cache key. Identity
		// comes from the mount content plus the in-step content hash.
		srcID = "local:" + s.SourceLocalName
		srcURL, srcSHA = "", ""
	} else if srcID == "" {
		srcID = s.SourceURL
	}

	// --- config (+ optional patches) as selector bind mounts ---
	// Mounts, not FileOp-staged layers: the mount input's content digest keys
	// the compile vertex exactly like a copied layer would, without paying a
	// snapshot commit per config change. (This context local stays separate
	// from the CA one above on purpose — the toolchain vertex's cache key
	// must not depend on config content.)
	follow := []string{s.ConfigName}
	if s.ExpectName != "" {
		follow = append(follow, s.ExpectName)
	}
	applyPatches := "0"
	if s.ApplyPatches {
		follow = append(follow, "patches")
		applyPatches = "1"
	}
	ctx := llb.Local("context", llb.FollowPaths(follow), llb.SharedKeyHint("kbuild-context"),
		llb.WithCustomName("[internal] load "+strings.Join(follow, ", ")))

	// --- the kbuild-step helper binary ---
	// Gateway form: the frontend's own image (opts["source"]) carries
	// /kbuild-step — mount it. Client form: the driver provides it as the
	// "helper" local.
	var helper llb.State
	if s.HelperRef != "" {
		// Only the one file, lifted out of the image by a FileOp: the mount is
		// read-only, so the compile vertex keys on its CONTENT digest, and a
		// frontend rebuild that ships an identical kbuild-step stays a cache
		// hit. Mounting the whole image keyed every compile vertex on the
		// frontend binary as well.
		helper = llb.Scratch().File(
			llb.Copy(llb.Image(s.HelperRef), "/kbuild-step", "/kbuild-step"),
			llb.WithCustomName("[internal] load kbuild-step from "+s.HelperRef))
	} else {
		helper = llb.Local("helper", llb.FollowPaths([]string{"kbuild-step"}),
			llb.SharedKeyHint("kbuild-helper"), llb.WithCustomName("[internal] load kbuild-step"))
	}

	// --- compile vertex, over a persistent per-version object cache ---
	// The secret mounts are declared unconditionally (SecretOptional) so that
	// enabling/disabling seeding does not change the op shape, hence not the
	// cache key.
	runOpts := []llb.RunOption{
		llb.Args([]string{HelperPath}),
		llb.WithCustomName(fmt.Sprintf("[compile] linux-%s %s -> %s",
			s.KernelVersion, s.archOrDefault(), strings.Join(s.targetsOrDefault(), ","))),
		llb.AddEnv("SOURCE_DATE_EPOCH", s.SourceDateEpoch),
		llb.AddEnv("KBUILD_BUILD_USER", "builder"),
		llb.AddEnv("KBUILD_BUILD_HOST", "linux"),
		llb.AddEnv("TZ", "UTC"),
		llb.AddEnv("SRC_ID", srcID),
		llb.AddEnv("SRC_URL", srcURL),
		llb.AddEnv("SRC_SHA256", srcSHA),
		llb.AddEnv("SRC_LOCAL", s.SourceLocalName),
		llb.AddEnv("KARCH", s.archOrDefault()),
		llb.AddEnv("CROSS_COMPILE", s.crossOrDefault()),
		llb.AddEnv("TARGETS", strings.Join(s.targetsOrDefault(), ",")),
		llb.AddEnv("APPLY_PATCHES", applyPatches),
		// In the cache key on purpose: the same fragment over a different make
		// base is a different config. Always present so the op shape is stable.
		llb.AddEnv("BASE_MAKE", s.BaseMake),
		llb.Dir("/build"),
		llb.AddMount("/helper", helper, llb.Readonly),
		llb.AddMount("/kernel.config", ctx, llb.SourcePath(s.ConfigName), llb.Readonly),
		llb.AddMount("/build", llb.Scratch(),
			// Keyed by version AND base image: two toolchains must never share
			// an object tree (kbuild's command-line tracking cannot tell two
			// different `gcc` binaries apart, so mixing silently links objects
			// from both compilers).
			llb.AsPersistentCacheDir(
				fmt.Sprintf("kernel-build-%s-%s-%.8x%s", s.KernelVersion, s.archOrDefault(), s.toolchainHash(), s.patchedSuffix()),
				llb.CacheMountLocked)),
		llb.AddSecret("/run/secrets/seed_access_key", llb.SecretID("seed_access_key"), llb.SecretOptional),
		llb.AddSecret("/run/secrets/seed_secret_key", llb.SecretID("seed_secret_key"), llb.SecretOptional),
		llb.AddSecret("/run/secrets/seed_cfg", llb.SecretID("seed_cfg"), llb.SecretOptional),
	}
	if s.ApplyPatches {
		runOpts = append(runOpts, llb.AddMount("/patches", ctx, llb.SourcePath("patches"), llb.Readonly))
	}
	if s.ExpectName != "" {
		// The expectation content keys the vertex via the mount digest, just
		// like the config: same composed config + same expectations = hit.
		runOpts = append(runOpts, llb.AddMount("/kernel.expect", ctx, llb.SourcePath(s.ExpectName), llb.Readonly))
	}
	if s.SourceLocalName != "" {
		runOpts = append(runOpts, llb.AddMount("/src",
			llb.Local("src", llb.FollowPaths([]string{s.SourceLocalName}), llb.SharedKeyHint("kbuild-src"),
				llb.WithCustomName("[internal] load "+s.SourceLocalName)),
			llb.Readonly))
	}
	runOpts = append(runOpts, proxy...)
	if s.IgnoreCache {
		// Costly on purpose, and more costly than it looks: BuildKit hands an
		// ignore-cache exec a NEW, EMPTY cache mount and re-indexes it under
		// the same id, so the persisted object tree is not reused and the old
		// one is dropped (measured on BuildKit 0.32.2 with a stock
		// `RUN --mount=type=cache` probe, three runs plus a control). A
		// --no-cache kernel build is therefore a full cold rebuild, not a
		// warm re-run.
		runOpts = append(runOpts, llb.IgnoreCache)
	}
	if s.SeedPush {
		// Seed publication is an ACTION, and the seed config lives in a
		// cache-neutral secret — without a unique action key, a vertex cache hit
		// would skip the run and silently push nothing. A changing env value
		// forces the vertex while preserving its persistent cache mount;
		// llb.IgnoreCache would replace that mount with an empty one. SEED_PUSH=1
		// independently tells the step that a push was requested, so missing
		// secrets fail loudly.
		runOpts = append(runOpts,
			llb.AddEnv("SEED_PUSH", "1"),
			llb.AddEnv("SEED_ACTION", rand.Text()))
	}

	// The result is a captured OUTPUT MOUNT, not the exec's rootfs: kbuild-step
	// writes /out/vmlinux, and that mount state is the build result directly.
	// No 22MB rootfs diff commit, no trailing copy vertex.
	run := toolchain.Run(runOpts...)
	return run.AddMount("/out", llb.Scratch()), nil
}

// toolchainHash identifies the compiler that produces the object tree: the
// base image AND the effective cross-compile prefix. Two builds that share a
// mount and seed reuse each other's objects, so anything that changes which
// compiler runs must change this hash, or a build links objects a different
// compiler emitted. (Arch is already a separate field in the id.)
func (s Spec) toolchainHash() [32]byte {
	// The apt path (ToolchainReady=false) installs whatever gcc/binutils the
	// Ubuntu archive serves at build time, so an unpinned base+apt build is
	// NOT byte-reproducible across time and its tree must not be shared with
	// a prebuilt-image build of the same Base string. ToolchainReady is in
	// the identity; the apt package set is not pinnable from here (see the
	// reproducibility note in the README: TOOLCHAIN ready is the reproducible
	// path, apt is best-effort).
	mode := "apt"
	if s.ToolchainReady {
		mode = "ready"
	}
	return sha256.Sum256([]byte(s.Base + "\x00" + s.crossOrDefault() + "\x00" + mode))
}

// patchedSuffix splits the persistent tree by the patched-or-not BIT (not
// patch content, which the stamp handles): a patched and an unpatched build
// of the same version each keep their own mount and seed, so alternating
// between them stays incremental instead of discarding the tree each way.
// Costs one extra tree (~3 GB) on disk per version that uses patches.
func (s Spec) patchedSuffix() string {
	if s.ApplyPatches {
		return "-patched"
	}
	return ""
}

func (s Spec) archOrDefault() string {
	if s.Arch == "" {
		return "x86_64"
	}
	return s.Arch
}

// crossOrDefault derives the conventional cross prefix when Arch is foreign
// and none was given.
func (s Spec) crossOrDefault() string {
	if s.CrossCompile != "" || s.archOrDefault() == "x86_64" {
		return s.CrossCompile
	}
	prefixes := map[string]string{"arm64": "aarch64-linux-gnu-"}
	return prefixes[s.archOrDefault()]
}

// toolchainScript installs the build deps. The Ubuntu base has no CA bundle,
// so a custom proxy CA needs a bootstrap step: install ca-certificates from
// the stock HTTP sources first, append the CA, then switch apt to HTTPS. This
// is image provisioning, not the build — the compile vertex itself runs no
// shell.
//
// The CA is trusted twice on purpose. The first append is what lets apt
// itself reach the https mirrors. A later package trigger can run
// update-ca-certificates, which REGENERATES /etc/ssl/certs/ca-certificates.crt
// from the package's store and drops anything appended by hand (measured:
// the compile vertex's tarball fetch failed with "certificate signed by
// unknown authority" while the apt step before it had succeeded). Dropping
// the file into /usr/local/share/ca-certificates and regenerating is the
// supported way to make it survive that and every later trigger.
const toolchainScript = `set -eux
if [ -n "${CA_FILE:-}" ] && [ -f "/certs/${CA_FILE}" ]; then
  apt-get update -qq
  apt-get install -y --no-install-recommends ca-certificates
  cat "/certs/${CA_FILE}" >> /etc/ssl/certs/ca-certificates.crt
  sed -i 's|http://|https://|g' /etc/apt/sources.list.d/ubuntu.sources
fi
apt-get update -qq
apt-get install -y --no-install-recommends ` + aptDeps + `
if [ -n "${CA_FILE:-}" ] && [ -f "/certs/${CA_FILE}" ]; then
  mkdir -p /usr/local/share/ca-certificates
  cp "/certs/${CA_FILE}" /usr/local/share/ca-certificates/kbuild-proxy-ca.crt
  update-ca-certificates
fi
rm -rf /var/lib/apt/lists/*
`

// caOnlyScript is the ToolchainReady variant: trust the sandbox proxy CA,
// install nothing.
const caOnlyScript = `set -eux
if [ -n "${CA_FILE:-}" ] && [ -f "/certs/${CA_FILE}" ]; then
  mkdir -p /etc/ssl/certs
  cat "/certs/${CA_FILE}" >> /etc/ssl/certs/ca-certificates.crt
fi
`
