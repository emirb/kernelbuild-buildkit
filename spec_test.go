package kbuild

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb"
)

func TestSourceURLReleaseCandidate(t *testing.T) {
	// -rc tarballs are NOT in the release directories: the derived
	// cdn.kernel.org path 404s for every one of them (measured 2026-08-28).
	// kernel.org serves them as git snapshots.
	got := SourceURLFor("6.19-rc1")
	if got != "https://git.kernel.org/torvalds/t/linux-6.19-rc1.tar.gz" {
		t.Errorf("SourceURLFor(rc) = %q", got)
	}
	s := DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("KERNEL 6.19-rc1\n"), &s); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("rc spec must validate: %v", err)
	}
}

func TestSourceURLMajorVersion(t *testing.T) {
	cases := map[string]string{
		"6.18.20":  "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.18.20.tar.gz",
		"7.1":      "https://cdn.kernel.org/pub/linux/kernel/v7.x/linux-7.1.tar.gz",
		"5.10.234": "https://cdn.kernel.org/pub/linux/kernel/v5.x/linux-5.10.234.tar.gz",
	}
	for v, want := range cases {
		if got := SourceURLFor(v); got != want {
			t.Errorf("SourceURLFor(%q) = %q, want %q", v, got, want)
		}
	}
}

func TestSourceExt(t *testing.T) {
	for name, want := range map[string]string{
		"linux-6.18.20.tar.gz":         ".gz",
		"https://x/linux-7.1.tar.xz":   ".xz",
		"mirror/linux-6.18.20.tar.zst": ".zst",
	} {
		if got, err := SourceExt(name); err != nil || got != want {
			t.Errorf("SourceExt(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, bad := range []string{"linux.tar.bz2", "linux.tgz", "linux.tar"} {
		if _, err := SourceExt(bad); err == nil {
			t.Errorf("SourceExt(%q) accepted", bad)
		}
	}
}

func TestValidateRejectsInjection(t *testing.T) {
	bad := []func(*Spec){
		func(s *Spec) { s.KernelVersion = "6.18; rm -rf /" },
		func(s *Spec) { s.KernelVersion = "6.18.20$(reboot)" },
		func(s *Spec) { s.ConfigName = "../../etc/passwd" },
		func(s *Spec) { s.ConfigName = "a;b" },
		func(s *Spec) { s.SourceLocalName = "x`id`.tar.xz" },
		func(s *Spec) { s.SourceDateEpoch = "0; reboot" },
		func(s *Spec) { s.SourceURL = "http://evil.example/x.tar.xz" },
		func(s *Spec) { s.SourceSHA256 = "nothex" },
		func(s *Spec) { s.SeedURL = `https://h/b"; rm -rf /; "` },
		func(s *Spec) { s.SeedURL = "https://h/b"; s.SeedPrefix = "../escape" },
		func(s *Spec) { s.ExpectName = "../escape" },
		func(s *Spec) { s.ExpectName = "a;b" },
		func(s *Spec) { s.BaseMake = "-j4" },
		func(s *Spec) { s.BaseMake = "defconfig --eval=$(reboot)" },
		func(s *Spec) { s.BaseMake = "x86_64_defconfig ../../evil" },
		func(s *Spec) { s.Targets = []string{"kconfigs", "everything"} },
	}
	for i, mutate := range bad {
		s := DefaultSpec()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("case %d: Validate accepted %+v", i, s)
		}
	}
	good := DefaultSpec()
	good.SeedURL = "https://acct.r2.cloudflarestorage.com/bucket"
	good.ExpectName = "kernel.expect"
	good.BaseMake = "x86_64_defconfig kvm_guest.config"
	good.Targets = []string{"config", "kconfigs"}
	if err := good.Validate(); err != nil {
		t.Errorf("Validate rejected a good spec: %v", err)
	}
}

func TestKbuildTimestamp(t *testing.T) {
	// Must byte-match `date -u -d @EPOCH '+%a %b %e %T %Z %Y'` — vmlinux
	// embeds this string, so reproducibility depends on it.
	got, err := KbuildTimestamp("1785542400")
	if err != nil || got != "Sat Aug  1 00:00:00 UTC 2026" {
		t.Errorf("KbuildTimestamp = %q, %v; want %q", got, err, "Sat Aug  1 00:00:00 UTC 2026")
	}
	if _, err := KbuildTimestamp("0; reboot"); err == nil {
		t.Error("non-numeric epoch accepted")
	}
}

func TestParseKernelfile(t *testing.T) {
	// Exercises the documented Kernelfile syntax, including inline comments.
	src := `#syntax=kbuild-frontend:local          # test-only frontend ref
KERNEL  6.18.20
CONFIG  kernel.config
SHA256  ` + strings.Repeat("ab", 32) + `
EPOCH   1785542400
PATCHES on
PROXY_CA ca-bundle.crt                 # only behind a MITM-proxy sandbox
TARGETS  vmlinux image config
`
	spec := DefaultSpec()
	if err := ParseKernelfile(strings.NewReader(src), &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Targets) != 3 || spec.Targets[1] != "image" {
		t.Errorf("TARGETS parse = %v", spec.Targets)
	}
	if !spec.ApplyPatches || spec.ProxyCAFile != "ca-bundle.crt" || spec.ConfigName != "kernel.config" {
		t.Errorf("parse result wrong: %+v", spec)
	}

	// Service keys: EXPECT names the validation file, BASE_MAKE keeps its
	// whole rest-of-line value (targets are space-separated).
	svc := DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("EXPECT kernel.expect\nBASE_MAKE x86_64_defconfig kvm_guest.config\n"), &svc); err != nil {
		t.Fatal(err)
	}
	if svc.ExpectName != "kernel.expect" || svc.BaseMake != "x86_64_defconfig kvm_guest.config" {
		t.Errorf("EXPECT/BASE_MAKE parse: %q %q", svc.ExpectName, svc.BaseMake)
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("parsed spec must validate: %v", err)
	}
	bad := DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("FROBNICATE yes\n"), &bad); err == nil {
		t.Error("unknown key must be rejected")
	}
}

func TestSeedCfg(t *testing.T) {
	s := DefaultSpec()
	if s.SeedCfg() != nil {
		t.Error("SeedCfg must be nil when seeding is disabled")
	}
	s.SeedURL = "https://acct.r2.cloudflarestorage.com/bucket"
	s.SeedPush = true
	cfg := string(s.SeedCfg())
	baseHash := fmt.Sprintf("%.8x", s.toolchainHash())
	for _, want := range []string{
		"SEED_URL=https://acct.r2.cloudflarestorage.com/bucket\n",
		"SEED_KEY=kbuild-seed/linux-6.18.20-x86_64-" + baseHash + ".tzst\n",
		"SEED_PUSH=1\n",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("SeedCfg missing %q in %q", want, cfg)
		}
	}
	// The seed key and cache-mount id both carry the base hash: an object tree
	// is only valid for the toolchain that built it.
	tux := s
	tux.Base = "docker.io/tuxmake/x86_64_gcc"
	if string(tux.SeedCfg()) == cfg {
		t.Error("seed key must change when the base image changes")
	}
}

func TestKernelLLBMarshals(t *testing.T) {
	s := DefaultSpec()
	s.SourceSHA256 = strings.Repeat("ab", 32)
	s.HTTPSProxy = "http://127.0.0.1:1"
	st, err := KernelLLB(s)
	if err != nil {
		t.Fatalf("KernelLLB: %v", err)
	}
	def, err := st.Marshal(context.Background(), llb.LinuxAmd64)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var all []byte
	for _, dt := range def.Def {
		all = append(all, dt...)
	}
	if !strings.Contains(string(all), "kernel-build-6.18.20") {
		t.Error("marshalled graph is missing the per-version cache-mount id")
	}

	if _, err := KernelLLB(Spec{KernelVersion: "bad version"}); err == nil {
		t.Error("KernelLLB must refuse an invalid spec")
	}
}

func TestTargets(t *testing.T) {
	s := DefaultSpec()
	if got := s.targetsOrDefault(); len(got) != 1 || got[0] != "vmlinux" {
		t.Errorf("x86 default targets = %v", got)
	}
	s.Targets = []string{"vmlinux", "image", "modules", "config"}
	if err := s.Validate(); err != nil {
		t.Errorf("full target set rejected: %v", err)
	}
	s.Targets = []string{"bzImage"}
	if err := s.Validate(); err == nil {
		t.Error("unknown target token accepted (raw make targets must not pass through)")
	}
	s.Targets = []string{"config; rm -rf /"}
	if err := s.Validate(); err == nil {
		t.Error("hostile target accepted")
	}

	arm := DefaultSpec()
	arm.Arch = "arm64"
	if got := arm.targetsOrDefault(); len(got) != 2 || got[1] != "image" {
		t.Errorf("arm64 default targets = %v", got)
	}
}

func TestDefaultSpecPinnedAndOverridesClear(t *testing.T) {
	// Out of the box: content-addressed (Copilot review 2026-08-26).
	if DefaultSpec().SourceSHA256 == "" {
		t.Error("DefaultSpec is not content-addressed")
	}
	// The toolchain too: a moving tag lets binutils drift under the
	// reproducible default (review 2026-08-27).
	if !strings.Contains(DefaultSpec().Base, "@sha256:") {
		t.Error("DefaultSpec.Base is not digest-pinned")
	}
	// A version override must clear the stale pin (Kernelfile form).
	s := DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("KERNEL 6.19\n"), &s); err != nil {
		t.Fatal(err)
	}
	if s.SourceSHA256 != "" {
		t.Error("KERNEL override kept the default tarball's sha")
	}
	// An explicit SHA256 after KERNEL wins.
	s = DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("KERNEL 6.19\nSHA256 "+strings.Repeat("cd", 32)+"\n"), &s); err != nil {
		t.Fatal(err)
	}
	if s.SourceSHA256 != strings.Repeat("cd", 32) {
		t.Error("explicit SHA256 did not survive")
	}
	// SOURCE_URL override clears too.
	s = DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("SOURCE_URL https://mirror/linux.tar.zst\n"), &s); err != nil {
		t.Fatal(err)
	}
	if s.SourceSHA256 != "" {
		t.Error("SOURCE_URL override kept the default tarball's sha")
	}
}

func TestKernelfileKeyOrder(t *testing.T) {
	// SHA256 before KERNEL must keep its pin (review finding: the version
	// override used to clear it regardless of order).
	s := DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("SHA256 "+strings.Repeat("cd", 32)+"\nKERNEL 6.19\n"), &s); err != nil {
		t.Fatal(err)
	}
	if s.SourceSHA256 != strings.Repeat("cd", 32) {
		t.Errorf("SHA256-before-KERNEL lost the pin: %q", s.SourceSHA256)
	}
	// An explicit SOURCE_URL must survive a KERNEL line written after it:
	// KERNEL used to derive (and overwrite) the URL inline, so a mirror URL
	// silently reverted to cdn.kernel.org depending on line order.
	s = DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("SOURCE_URL https://mirror/linux-6.19.tar.gz\nKERNEL 6.19\n"), &s); err != nil {
		t.Fatal(err)
	}
	if s.SourceURL != "https://mirror/linux-6.19.tar.gz" {
		t.Errorf("SOURCE_URL-before-KERNEL lost the override: %q", s.SourceURL)
	}
	// ... and KERNEL alone still derives one, whatever the order.
	s = DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("CONFIG k.config\nKERNEL 6.19\n"), &s); err != nil {
		t.Fatal(err)
	}
	if s.SourceURL != SourceURLFor("6.19") {
		t.Errorf("KERNEL did not derive the source URL: %q", s.SourceURL)
	}
	// Tabs separate KEY from VALUE like any other whitespace.
	s = DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("KERNEL\t6.19\nTARGETS\tvmlinux\tconfig\n"), &s); err != nil {
		t.Fatalf("tab-separated Kernelfile rejected: %v", err)
	}
	if s.KernelVersion != "6.19" || len(s.Targets) != 2 {
		t.Errorf("tab parse = %q %v", s.KernelVersion, s.Targets)
	}
	// A TARGETS line with no actual target is a typo, not "use the default".
	s = DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("TARGETS ,\n"), &s); err == nil {
		t.Error("empty TARGETS accepted")
	}
	// TARGETS accepts commas as well as spaces.
	s = DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("TARGETS vmlinux,image config\n"), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Targets) != 3 {
		t.Errorf("mixed-separator TARGETS = %v", s.Targets)
	}
}

func TestValidateSeedURLInjection(t *testing.T) {
	for _, bad := range []string{
		"https://h/b\nSEED_PUSH=1",
		"https://h/b junk",
		"https://h/b\tx",
	} {
		s := DefaultSpec()
		s.SeedURL = bad
		if err := s.Validate(); err == nil {
			t.Errorf("SeedURL %q accepted", bad)
		}
	}
}

func TestValidateEpochRange(t *testing.T) {
	s := DefaultSpec()
	s.SourceDateEpoch = "270000000000" // year 10525: KbuildTimestamp rejects
	if err := s.Validate(); err == nil {
		t.Error("out-of-range epoch passed Validate but would die in the vertex")
	}
}

// Regression tests for the red-team pass (2026-08-27): four inputs the
// fuzzer accepted that Validate now rejects, plus the cache-key collision.
func TestValidateRedTeam(t *testing.T) {
	reject := map[string]func(*Spec){
		"base-newline":   func(s *Spec) { s.Base = "ubuntu\n24.04" },
		"base-space":     func(s *Spec) { s.Base = "ubuntu 24.04" },
		"helperref-ctrl": func(s *Spec) { s.HelperRef = "img\ttag" },
		"cross-dotdot":   func(s *Spec) { s.CrossCompile = "..-" },
		"epoch-plus":     func(s *Spec) { s.SourceDateEpoch = "+0" },
		"epoch-leadzero": func(s *Spec) { s.SourceDateEpoch = "007" },
		"epoch-neg":      func(s *Spec) { s.SourceDateEpoch = "-1" },
	}
	for name, mut := range reject {
		s := DefaultSpec()
		mut(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate accepted hostile input", name)
		}
	}
	// The digest-pinned default and a real cross prefix must still pass.
	if err := DefaultSpec().Validate(); err != nil {
		t.Errorf("default spec rejected: %v", err)
	}
	ok := DefaultSpec()
	ok.CrossCompile = "aarch64-linux-gnu-"
	if err := ok.Validate(); err != nil {
		t.Errorf("real cross prefix rejected: %v", err)
	}
}

func TestSeedPushRequiresPin(t *testing.T) {
	s := DefaultSpec()
	s.SourceSHA256 = "" // unpinned
	s.SeedURL = "https://acct.example.com/bucket"
	s.SeedPush = true
	if err := s.Validate(); err == nil {
		t.Error("seed-push with unpinned source accepted (would poison a shared seed)")
	}
	s.SourceSHA256 = strings.Repeat("ab", 32) // pinned
	if err := s.Validate(); err != nil {
		t.Errorf("seed-push with a pinned source rejected: %v", err)
	}
}
