package main

import (
	"reflect"
	"strings"
	"testing"

	kbuild "github.com/emirb/kernelbuild-buildkit"
)

// TestApplyOptsProxies: docker build forwards proxy build-args under both
// spellings; all three (http, https, no_proxy) must reach the Spec: apt's
// http:// mirrors need http_proxy behind a plain corporate proxy, and
// no_proxy is how an internal mirror is exempted.
func TestApplyOptsProxies(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts map[string]string
	}{
		{"upper", map[string]string{
			"build-arg:HTTP_PROXY": "http://p:1", "build-arg:HTTPS_PROXY": "http://p:2", "build-arg:NO_PROXY": "mirror.corp",
		}},
		{"lower", map[string]string{
			"build-arg:http_proxy": "http://p:1", "build-arg:https_proxy": "http://p:2", "build-arg:no_proxy": "mirror.corp",
		}},
	} {
		spec := kbuild.DefaultSpec()
		if err := applyOpts(tc.opts, &spec); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if spec.HTTPProxy != "http://p:1" || spec.HTTPSProxy != "http://p:2" || spec.NoProxy != "mirror.corp" {
			t.Errorf("%s: proxies = %q %q %q", tc.name, spec.HTTPProxy, spec.HTTPSProxy, spec.NoProxy)
		}
	}
	// The mapper still owns the platform error path.
	spec := kbuild.DefaultSpec()
	if err := applyOpts(map[string]string{"platform": "linux/amd64,linux/arm64"}, &spec); err == nil {
		t.Error("multi-platform accepted by applyOpts")
	}
}

// TestApplyOptsEveryKey: each --opt key lands on exactly the Spec field it
// names, so a typo in the mapping cannot silently build a different kernel.
func TestApplyOptsEveryKey(t *testing.T) {
	spec := kbuild.DefaultSpec()
	err := applyOpts(map[string]string{
		"kernel-version":     "6.19.1",
		"source-sha256":      strings.Repeat("ab", 32),
		"source-date-epoch":  "1700000000",
		"config":             "guest.config",
		"expect":             "guest.expect",
		"base-make":          "x86_64_defconfig kvm_guest.config",
		"proxy-ca":           "ca.crt",
		"helper-ref":         "ghcr.io/x/helper:v1",
		"src-name":           "linux-6.19.1.tar.xz",
		"base":               "docker.io/tuxmake/x86_64_gcc:latest",
		"toolchain-ready":    "true",
		"arch":               "arm64",
		"cross-compile":      "aarch64-linux-gnu-",
		"force-network-mode": "host",
		"targets":            "vmlinux,image",
		"apply-patches":      "true",
		"no-cache":           "",
		"seed-push":          "true",
	}, &spec)
	if err != nil {
		t.Fatal(err)
	}
	want := kbuild.Spec{
		KernelVersion: "6.19.1", SourceURL: kbuild.SourceURLFor("6.19.1"), SourceSHA256: strings.Repeat("ab", 32),
		SourceDateEpoch: "1700000000", ConfigName: "guest.config", ExpectName: "guest.expect",
		BaseMake: "x86_64_defconfig kvm_guest.config", ProxyCAFile: "ca.crt", HelperRef: "ghcr.io/x/helper:v1",
		SourceLocalName: "linux-6.19.1.tar.xz", Base: "docker.io/tuxmake/x86_64_gcc:latest", ToolchainReady: true,
		Arch: "arm64", CrossCompile: "aarch64-linux-gnu-", NetworkHost: true, Targets: []string{"vmlinux", "image"},
		ApplyPatches: true, IgnoreCache: true, SeedPush: true, SeedPrefix: "kbuild-seed",
	}
	if !reflect.DeepEqual(spec, want) {
		t.Errorf("applyOpts mapped\n got %+v\nwant %+v", spec, want)
	}
}

// TestApplyOptsRejectsMalformedBooleans: "apply-patches=1" must not build an
// unpatched kernel and report success, and "seed-push=yes" must not run a
// build that publishes nothing. Anything but true/false is an error.
func TestApplyOptsRejectsMalformedBooleans(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"toolchain-ready", "yes"},
		{"apply-patches", "1"},
		{"seed-push", "on"},
	} {
		spec := kbuild.DefaultSpec()
		if err := applyOpts(map[string]string{tc.key: tc.val}, &spec); err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%s: err = %v, want an error naming the key", tc.key, tc.val, err)
		}
	}
	for _, val := range []string{"true", "false"} {
		spec := kbuild.DefaultSpec()
		if err := applyOpts(map[string]string{"apply-patches": val}, &spec); err != nil {
			t.Errorf("apply-patches=%s rejected: %v", val, err)
		}
	}
}

// TestApplyOptsTargetsTolerateSpaces: the Kernelfile accepts "vmlinux, image";
// the opt form must too, instead of failing Validate on " image".
func TestApplyOptsTargetsTolerateSpaces(t *testing.T) {
	spec := kbuild.DefaultSpec()
	if err := applyOpts(map[string]string{"targets": "vmlinux, image config"}, &spec); err != nil {
		t.Fatal(err)
	}
	if want := []string{"vmlinux", "image", "config"}; !reflect.DeepEqual(spec.Targets, want) {
		t.Errorf("Targets = %q, want %q", spec.Targets, want)
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("Validate after opt targets: %v", err)
	}
}

// TestApplyOptsSourceOverridesClearThePin: a new kernel version or URL
// invalidates the default tarball's sha256; an explicit sha256 wins.
func TestApplyOptsSourceOverridesClearThePin(t *testing.T) {
	spec := kbuild.DefaultSpec()
	if err := applyOpts(map[string]string{"kernel-version": "6.19.1"}, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.SourceSHA256 != "" || spec.SourceURL != kbuild.SourceURLFor("6.19.1") {
		t.Errorf("version override kept the default pin: %q %q", spec.SourceSHA256, spec.SourceURL)
	}
	spec = kbuild.DefaultSpec()
	if err := applyOpts(map[string]string{"source-url": "https://mirror.example/linux-6.18.20.tar.xz"}, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.SourceSHA256 != "" {
		t.Error("source-url override kept the default pin")
	}
	spec = kbuild.DefaultSpec()
	sha := strings.Repeat("cd", 32)
	if err := applyOpts(map[string]string{"kernel-version": "6.19.1", "source-sha256": sha}, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.SourceSHA256 != sha {
		t.Errorf("explicit sha256 lost: %q", spec.SourceSHA256)
	}
}
