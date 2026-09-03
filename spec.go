package kbuild

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Spec describes a kernel build. The frontend turns it into an LLB graph
// (see KernelLLB). It is deliberately small and JSON/opt-friendly so the same
// struct drives both the gateway frontend (build.kernel.v0) and the client
// driver (kbuildctl).
//
// Several fields are interpolated into the build script or into BuildKit
// identifiers; Validate() MUST pass before the Spec reaches KernelLLB.
type Spec struct {
	Base            string // base image, e.g. "docker.io/library/ubuntu:24.04"
	KernelVersion   string // e.g. "6.18.20"
	SourceURL       string // https tarball (used unless SourceLocalName is set)
	SourceSHA256    string // hex sha256 of the tarball; verified in-step and keys the compile vertex
	SourceLocalName string // if set, take the tarball from llb.Local("src")/<name>
	SourceDateEpoch string // reproducible-build epoch (SOURCE_DATE_EPOCH)
	ConfigName      string // config file inside the build context (llb.Local("context"))
	// ExpectName, when set, names a file in the context with post-olddefconfig
	// expectations the step validates BEFORE compiling (see kbuild-step's
	// validateExpectations for the line grammar: "y CONFIG_X", "n CONFIG_X",
	// "= CONFIG_X=val"). olddefconfig silently drops unknown symbols and unmet
	// dependencies; a service composing configs wants that surfaced in seconds,
	// not after a full compile.
	ExpectName string
	// BaseMake, when set, is a space-separated list of make config targets
	// (e.g. "x86_64_defconfig kvm_guest.config") run against the tree FIRST;
	// the context config is then appended as a fragment before olddefconfig.
	// Empty keeps the plain behavior: the context config IS the whole input.
	BaseMake       string
	ApplyPatches   bool   // apply patches/*.patch from the context before building
	HTTPSProxy     string // https_proxy for network steps; cache-neutral (llb.WithProxy)
	HTTPProxy      string // http_proxy for network steps (apt's stock mirrors are http); cache-neutral
	NoProxy        string // extra no_proxy entries (comma list) on top of localhost,127.0.0.1; cache-neutral
	ToolchainReady bool   // Base already has the kernel toolchain (e.g. tuxmake/*): skip the apt vertex
	Arch           string // target architecture ("x86_64" default, "arm64")
	CrossCompile   string // cross prefix override (derived from Arch when empty)
	// Targets selects the artifacts exported to /out. Tokens: "vmlinux",
	// "image" (bzImage on x86_64, Image on arm64), "modules"
	// (modules.tar.zst via modules_install), "config" (the post-olddefconfig
	// .config), "kconfigs" (the tree's Kconfig files bundled as
	// kconfig.txt.gz — symbol-catalog input, no compilation). Empty means the
	// arch default (vmlinux; arm64 also Image).
	Targets     []string
	ProxyCAFile string // CA cert FILE IN THE CONTEXT to trust (MITM-proxy sandbox); "" = none
	HelperRef   string // image ref carrying /kbuild-step; "" = llb.Local("helper") from the client
	NetworkHost bool   // run build steps with host networking (docker build --network=host)
	IgnoreCache bool   // force the build vertices to execute (docker build --no-cache)

	// Remote object-tree seed. BuildKit's cache exporters (registry/s3) cover
	// the vertex/layer cache but NOT the contents of cache mounts, so on a
	// fresh worker the /build mount is empty and a config change would compile
	// cold. The seed closes that gap: after a build the object tree can be
	// pushed to S3-compatible storage (R2), and a cold mount hydrates from it
	// before compiling. Credentials come in as BuildKit secrets (ids "seed_access_key"
	// and "seed_secret_key"), never as opts or env.
	SeedURL    string // bucket base URL on ANY S3-compatible store (AWS, MinIO, R2, Ceph, ...)
	SeedRegion string // bucket region; "auto" (default) suits region-less stores
	SeedPrefix string // key prefix inside the bucket (default "kbuild-seed")
	SeedPush   bool   // push the object tree after a successful build (CI role)
}

var (
	versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?(-rc[0-9]+)?$`)
	fileRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	prefixRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// Make config targets: x86_64_defconfig, kvm_guest.config, tinyconfig...
	// Must start alphanumeric so a token can never read as a make flag.
	baseMakeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	crossRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]+-$`)
	epochRe    = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	// Allowlist, not a metacharacter blocklist: the URL lands verbatim on a
	// line of the KV seed_cfg secret, so whitespace or control characters
	// (a newline especially) would inject extra KEY=VALUE lines.
	seedURLRe = regexp.MustCompile(`^https://[A-Za-z0-9._~:/?#@!$&'()*+,;=%[\]-]+$`)
	// OCI image references: registry/repo:tag@sha256:... — no whitespace or
	// control characters (a blocklist of shell metacharacters missed a bare
	// newline, found by fuzzing). This is deliberately broad within the
	// printable set; the daemon does the strict reference parse.
	imageRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)
	// Proxy URLs and the source URL land verbatim in the compile vertex's env
	// (HTTP_PROXY, HTTPS_PROXY, NO_PROXY, SRC_URL) and, unpinned, in the tree
	// stamp: one printable-ASCII token each (0x21-0x7e), no whitespace or
	// control characters. Same allowlist discipline as seedURLRe.
	proxyRe   = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://[\x21-\x7e]+$`)
	noProxyRe = regexp.MustCompile(`^[\x21-\x7e]*$`)
	tokenRe   = regexp.MustCompile(`^[\x21-\x7e]+$`)
)

// Validate rejects anything that could smuggle shell metacharacters or path
// traversal into the generated script, a mount identifier, or an object key.
// Everything KernelLLB interpolates is checked here.
func (s Spec) Validate() error {
	if !versionRe.MatchString(s.KernelVersion) {
		return fmt.Errorf("kernel-version %q: must look like 6.18.20 or 7.1-rc3", s.KernelVersion)
	}
	if !fileRe.MatchString(s.ConfigName) || strings.Contains(s.ConfigName, "..") {
		return fmt.Errorf("config %q: bad filename", s.ConfigName)
	}
	if s.ExpectName != "" && (!fileRe.MatchString(s.ExpectName) || strings.Contains(s.ExpectName, "..")) {
		return fmt.Errorf("expect %q: bad filename", s.ExpectName)
	}
	// Each token becomes a make target on the step's command line: plain
	// config-target names only (no slashes, no dots-dots, no leading dash —
	// "-j" or "--eval" here would be argument injection into make).
	for tok := range strings.FieldsSeq(s.BaseMake) {
		if !baseMakeRe.MatchString(tok) || strings.Contains(tok, "..") {
			return fmt.Errorf("base-make token %q: want a plain make config target", tok)
		}
	}
	if s.SourceLocalName != "" && (!fileRe.MatchString(s.SourceLocalName) || strings.Contains(s.SourceLocalName, "..")) {
		return fmt.Errorf("src-name %q: bad filename", s.SourceLocalName)
	}
	// Canonical decimal only (no +, -, leading zeros): different spellings of
	// one instant build identical kernels but split the cache key. Then the
	// range check (Timestamp) rejects values the compile vertex would
	// die on (year > 9999).
	if !epochRe.MatchString(s.SourceDateEpoch) {
		return fmt.Errorf("source-date-epoch %q: must be a plain non-negative integer", s.SourceDateEpoch)
	}
	if _, err := Timestamp(s.SourceDateEpoch); err != nil {
		return err
	}
	if s.SourceLocalName == "" {
		if !strings.HasPrefix(s.SourceURL, "https://") {
			return fmt.Errorf("source-url %q: must be https", s.SourceURL)
		}
		if !tokenRe.MatchString(s.SourceURL) {
			return fmt.Errorf("source-url %q: must be a single printable token", s.SourceURL)
		}
		if _, err := SourceExt(s.SourceURL); err != nil {
			return err
		}
	} else if _, err := SourceExt(s.SourceLocalName); err != nil {
		return err
	}
	if s.SourceSHA256 != "" && !sha256Re.MatchString(s.SourceSHA256) {
		return fmt.Errorf("source-sha256 %q: must be 64 hex chars", s.SourceSHA256)
	}
	if s.SeedPush && s.SourceLocalName == "" && s.SourceSHA256 == "" {
		// Publishing a seed built from an unpinned, mutable URL poisons the
		// shared object tree for every worker that later pulls it. A seeder
		// must build from pinned source.
		return errors.New("seed-push requires a pinned source (set source-sha256)")
	}
	if s.SeedURL != "" {
		if !seedURLRe.MatchString(s.SeedURL) {
			return fmt.Errorf("seed-url %q: must be a plain https URL (single token)", s.SeedURL)
		}
		if !prefixRe.MatchString(s.SeedPrefix) || strings.Contains(s.SeedPrefix, "..") {
			return fmt.Errorf("seed-prefix %q: bad prefix", s.SeedPrefix)
		}
		if s.SeedRegion != "" && !fileRe.MatchString(s.SeedRegion) {
			return fmt.Errorf("seed-region %q: bad region", s.SeedRegion)
		}
	}
	for _, p := range []struct{ name, val string }{{"https-proxy", s.HTTPSProxy}, {"http-proxy", s.HTTPProxy}} {
		if p.val != "" && !proxyRe.MatchString(p.val) {
			return fmt.Errorf("%s %q: want scheme://host[:port], a single token", p.name, p.val)
		}
	}
	if !noProxyRe.MatchString(s.NoProxy) {
		return fmt.Errorf("no-proxy %q: comma-separated hosts, no whitespace", s.NoProxy)
	}
	if s.ProxyCAFile != "" && (!fileRe.MatchString(s.ProxyCAFile) || strings.Contains(s.ProxyCAFile, "..") || strings.Contains(s.ProxyCAFile, "/")) {
		return fmt.Errorf("proxy-ca %q: must be a plain filename in the context", s.ProxyCAFile)
	}
	if s.HelperRef != "" && (!imageRefRe.MatchString(s.HelperRef) || strings.Contains(s.HelperRef, "..")) {
		return fmt.Errorf("helper-ref %q: bad image reference", s.HelperRef)
	}
	if !imageRefRe.MatchString(s.Base) || strings.Contains(s.Base, "..") {
		return fmt.Errorf("base %q: bad image reference", s.Base)
	}
	switch s.archOrDefault() {
	case "x86_64":
	case "arm64":
		if !s.ToolchainReady {
			return errors.New("arch arm64 needs TOOLCHAIN ready with a cross toolchain image (e.g. tuxmake/arm64_gcc)")
		}
	default:
		return fmt.Errorf("arch %q: supported: x86_64, arm64", s.Arch)
	}
	if s.CrossCompile != "" && (!crossRe.MatchString(s.CrossCompile) || strings.Contains(s.CrossCompile, "..")) {
		// CROSS_COMPILE becomes make's $(CROSS_COMPILE)gcc tool prefix. No
		// slashes (crossRe) means "..-gcc" cannot traverse today, but a real
		// prefix never contains "..", and blocking it keeps the field safe
		// if the character set is ever widened.
		return fmt.Errorf("cross-compile %q: bad prefix", s.CrossCompile)
	}
	for _, tgt := range s.Targets {
		switch tgt {
		case "vmlinux", "image", "modules", "config", "kconfigs":
		default:
			return fmt.Errorf("target %q: supported: vmlinux, image, modules, config, kconfigs", tgt)
		}
	}
	return nil
}

// targetsOrDefault returns the artifact set, defaulting to the arch's
// conventional output: vmlinux everywhere, plus the PE Image on arm64 (its
// bootloaders and Firecracker consume it).
func (s Spec) targetsOrDefault() []string {
	if len(s.Targets) > 0 {
		return s.Targets
	}
	if s.archOrDefault() == "arm64" {
		return []string{"vmlinux", "image"}
	}
	return []string{"vmlinux"}
}

// SourceURLFor returns the kernel.org tarball URL for a given version. The
// directory is keyed by MAJOR version (v6.x, v7.x, ...), derived from the
// version string — a hardcoded v6.x would silently 404 (or worse, fetch the
// wrong tree) for 7.x kernels.
//
// The default is .tar.gz, not .tar.xz: gzip decodes in pure Go (klauspost)
// faster than C xz, so the whole extract path needs no external codec. Both
// tarballs compress the same tar, so the extracted tree — and the built
// vmlinux — are byte-identical either way. A .tar.xz SOURCE_URL decodes via
// ulikunitz/xz (also pure Go); .tar.zst is supported for self-hosted mirrors.
func SourceURLFor(version string) string {
	if strings.Contains(version, "-rc") {
		// Release candidates are never in the release directories — the
		// derived cdn.kernel.org path 404s for every -rc (measured). kernel.org
		// publishes them as git snapshots instead, which is what its own front
		// page links. Snapshot tarballs are regenerated on demand, so pin
		// SHA256 only after fetching the bytes you intend to keep.
		return "https://git.kernel.org/torvalds/t/linux-" + version + ".tar.gz"
	}
	major, _, _ := strings.Cut(version, ".")
	return fmt.Sprintf("https://cdn.kernel.org/pub/linux/kernel/v%s.x/linux-%s.tar.gz", major, version)
}

// SourceExt returns the tarball extension (".gz", ".xz", ".zst") for a source
// URL or filename, or an error for an unsupported one.
func SourceExt(name string) (string, error) {
	for _, ext := range []string{".gz", ".xz", ".zst"} {
		if strings.HasSuffix(name, ".tar"+ext) {
			return ext, nil
		}
	}
	return "", fmt.Errorf("source %q: want .tar.gz, .tar.xz, or .tar.zst", name)
}

const defaultBaseDigest = "sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517"

// DefaultSpec is the out-of-the-box build: Linux 6.18.20 from kernel.org with
// a pinned sha256, a fixed epoch, and kernel.config from the context.
func DefaultSpec() Spec {
	v := "6.18.20"
	return Spec{
		// Pinned by digest: the tag moves with Ubuntu point releases, and a
		// toolchain drifting under a "reproducible" default would break
		// byte-identity as weather rather than as a decision. Bumping the
		// digest is a deliberate change that ships with a new golden.
		Base:          "docker.io/library/ubuntu:24.04@" + defaultBaseDigest,
		KernelVersion: v,
		// Pinned sha256 of linux-6.18.20.tar.gz: the out-of-the-box build is
		// content-addressed. Every kernel-version/source-url override CLEARS
		// this (a stale pin would fail every other version); an explicit
		// SHA256/-sha256/source-sha256 is applied after and wins.
		SourceSHA256:    "a1415e257075c2fadf070f44bbb029469efbde5b6cf07d1433fe72207acff03c",
		SourceURL:       SourceURLFor(v),
		SourceDateEpoch: "1785542400",
		ConfigName:      "kernel.config",
		SeedPrefix:      "kbuild-seed",
	}
}
