package kbuild

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// hostile is the character set none of the validated, interpolated fields may
// ever contain after Validate accepts a Spec. This is the independent oracle
// for the input firewall: it deliberately does NOT reuse the regexes from
// spec.go — if a regex is loosened by mistake, this catches it.
const hostile = " \t\n;|&$`'\"\\<>(){}*?!#~"

func assertFirewalled(t *testing.T, field, v string) {
	t.Helper()
	if strings.ContainsAny(v, hostile) || strings.Contains(v, "..") {
		t.Errorf("Validate accepted %s %q containing hostile content", field, v)
	}
}

// FuzzValidate: Validate must never panic, and any Spec it accepts must have
// firewalled every field that later reaches env vars, mount identifiers, or
// object keys.
func FuzzValidate(f *testing.F) {
	f.Add("6.18.20", "kernel.config", "1785542400", "aaaa", "kbuild-seed", "ca.crt", "x.tar.gz", "aarch64-linux-gnu-", "docker.io/tuxmake/x86_64_gcc", "auto")
	f.Add("6.18; rm -rf /", "../../etc/passwd", "0; reboot", "nothex", "../escape", "a/b", "x.tar", "p re fix-", "img `id`", "re gion")
	f.Add("7.1-rc3", "configs/tiny.config", "0", strings.Repeat("ab", 32), "p/q", "bundle.pem", "linux.tar.zst", "x-", "ghcr.io/a/b:1@sha256:abc", "us-east-1")
	f.Fuzz(func(t *testing.T, version, config, epoch, sha, prefix, ca, local, cross, base, region string) {
		s := DefaultSpec()
		s.KernelVersion = version
		s.ConfigName = config
		s.SourceDateEpoch = epoch
		s.SourceSHA256 = sha
		s.SeedURL = "https://acct.example.com/bucket"
		s.SeedPrefix = prefix
		s.ProxyCAFile = ca
		s.SourceLocalName = local
		s.CrossCompile = cross
		if base != "" {
			s.Base = base
		}
		s.SeedRegion = region
		if err := s.Validate(); err != nil {
			return
		}
		assertFirewalled(t, "kernel-version", s.KernelVersion)
		assertFirewalled(t, "config", s.ConfigName)
		assertFirewalled(t, "seed-prefix", s.SeedPrefix)
		assertFirewalled(t, "proxy-ca", s.ProxyCAFile)
		assertFirewalled(t, "src-name", s.SourceLocalName)
		assertFirewalled(t, "cross-compile", s.CrossCompile)
		assertFirewalled(t, "seed-region", s.SeedRegion)
		if strings.ContainsAny(s.Base, " \t\n\"'$`") || strings.Contains(s.Base, "..") {
			t.Errorf("Validate accepted base %q with hostile content", s.Base)
		}
		if s.SourceSHA256 != "" && len(s.SourceSHA256) != 64 {
			t.Errorf("accepted sha256 of length %d", len(s.SourceSHA256))
		}
		for _, r := range s.SourceDateEpoch {
			if r < '0' || r > '9' {
				t.Errorf("accepted non-numeric epoch %q", s.SourceDateEpoch)
			}
		}
	})
}

// FuzzParseKernelfile: the parser must never panic, and everything it parses
// either validates or is rejected by Validate — never a third state where a
// hostile value flows on unvalidated.
func FuzzParseKernelfile(f *testing.F) {
	f.Add("KERNEL 6.18.20\nCONFIG kernel.config\n")
	f.Add("#syntax=x # c\nKERNEL 6.18.20   # trailing\nPATCHES on\nTOOLCHAIN ready\n")
	f.Add("SHA256 " + strings.Repeat("ab", 32) + "\nEPOCH 1785542400\nBASE ubuntu:24.04\n")
	f.Add("KERNEL\n")
	f.Add("PATCHES maybe\nSOURCE_URL https://x/linux.tar.zst\nPROXY_CA a.crt\n")
	f.Fuzz(func(t *testing.T, src string) {
		spec := DefaultSpec()
		if err := ParseKernelfile(strings.NewReader(src), &spec); err != nil {
			return
		}
		if err := spec.Validate(); err != nil {
			return
		}
		// Parsed AND validated: every interpolated field must be firewalled,
		// or a hostile Kernelfile value flowed through both gates.
		assertFirewalled(t, "kernel-version", spec.KernelVersion)
		assertFirewalled(t, "config", spec.ConfigName)
		assertFirewalled(t, "proxy-ca", spec.ProxyCAFile)
		assertFirewalled(t, "src-name", spec.SourceLocalName)
		for _, tgt := range spec.Targets {
			assertFirewalled(t, "target", tgt)
		}
	})
}

// FuzzSourceExt: never panics; only ever returns one of the three supported
// extensions.
func FuzzSourceExt(f *testing.F) {
	f.Add("linux-6.18.20.tar.gz")
	f.Add("x.tar.xz")
	f.Add("x.tar.zst")
	f.Add("x.tgz")
	f.Add("")
	f.Fuzz(func(t *testing.T, name string) {
		ext, err := SourceExt(name)
		if err != nil {
			return
		}
		switch ext {
		case ".gz", ".xz", ".zst":
		default:
			t.Errorf("SourceExt(%q) returned unsupported %q", name, ext)
		}
		if !strings.HasSuffix(name, ".tar"+ext) {
			t.Errorf("SourceExt(%q) = %q but name lacks that suffix", name, ext)
		}
	})
}

// FuzzTimestamp: never panics; an accepted epoch renders a timestamp
// that Go can parse back to the exact same instant (byte-format stability is
// pinned separately by TestTimestamp).
func FuzzTimestamp(f *testing.F) {
	f.Add("1785542400")
	f.Add("0")
	f.Add("-1")
	f.Add("99999999999999999999")
	f.Fuzz(func(t *testing.T, epoch string) {
		got, err := Timestamp(epoch)
		if err != nil {
			return
		}
		back, err := time.Parse("Mon Jan _2 15:04:05 MST 2006", got)
		if err != nil {
			t.Fatalf("Timestamp(%q) = %q: not parseable back: %v", epoch, got, err)
		}
		sec, err := strconv.ParseInt(epoch, 10, 64)
		if err != nil {
			t.Fatalf("accepted unparseable epoch %q", epoch)
		}
		if back.Unix() != sec {
			t.Fatalf("Timestamp(%q) = %q: round-trips to %d, not %d", epoch, got, back.Unix(), sec)
		}
	})
}
