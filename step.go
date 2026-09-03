package kbuild

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// HelperPath is where the compile exec finds the kbuild-step runner: the
// helper state (frontend image or client-provided dir) is mounted at /helper.
const HelperPath = "/helper/kbuild-step"

// Timestamp renders SOURCE_DATE_EPOCH exactly as
//
//	date -u -d @EPOCH '+%a %b %e %T %Z %Y'
//
// does ("Sat Aug  1 00:00:00 UTC 2026" — %e is space-padded, hence _2).
// vmlinux embeds this string, so byte-reproducibility depends on it.
func Timestamp(epoch string) (string, error) {
	sec, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		return "", fmt.Errorf("SOURCE_DATE_EPOCH %q: %w", epoch, err)
	}
	// SOURCE_DATE_EPOCH is spec'd non-negative, and the %Y-compatible format
	// assumes a 4-digit year — a 5-digit year (epoch ≥ year 10000, found by
	// fuzzing) would embed a timestamp no downstream tool parses. Reject
	// rather than render garbage.
	ts := time.Unix(sec, 0).UTC()
	if sec < 0 || ts.Year() > 9999 {
		return "", fmt.Errorf("SOURCE_DATE_EPOCH %q: out of range (want 1970-9999)", epoch)
	}
	return ts.Format("Mon Jan _2 15:04:05 MST 2006"), nil
}

// ParseKernelfile reads the tiny build-description format that makes
// `docker build -f Kernelfile` work via the #syntax= directive:
//
//	#syntax=ghcr.io/emirb/kernelbuild-buildkit
//	KERNEL   6.18.20
//	CONFIG   kernel.config
//	SHA256   837a5abd...
//	EPOCH    1785542400
//	PATCHES  on
//	PROXY_CA ca-bundle.crt
//
// Lines are KEY VALUE; # starts a comment (whole-line or trailing, after
// whitespace); unknown keys are an error (a typo must not silently build
// something else), and so is a key given twice (the second value would win
// silently, and a stale line left above a new one is the same kind of typo).
// Values land in the Spec, which is still validated afterwards — this parser
// adds no trust.
func ParseKernelfile(r io.Reader, spec *Spec) error {
	sc := bufio.NewScanner(r)
	line := 0
	// Key order must not matter: KERNEL/SOURCE_URL clear the default sha pin
	// only when the file itself did not set SHA256, decided after EOF, so
	// "SHA256 before KERNEL" keeps its pin like any other order.
	var sawKernel, sawURL, sawSHA bool
	seen := map[string]int{} // key -> first line, for the duplicate check
	for sc.Scan() {
		line++
		t := sc.Text()
		if line == 1 {
			// Editors on Windows prepend a UTF-8 byte order mark. The
			// dockerfile parser drops it before honoring #syntax=, so the
			// delegation succeeds and the mark would surface here as an
			// unreadable "want KEY VALUE" error on the #syntax= line.
			t = strings.TrimPrefix(t, "\ufeff")
		}
		// Strip a trailing comment: "#" preceded by whitespace. No valid
		// value contains " #" (filenames/URLs are validated after parsing),
		// so this cannot eat real content.
		if i := strings.IndexByte(t, '#'); i >= 0 && (i == 0 || t[i-1] == ' ' || t[i-1] == '\t') {
			t = t[:i]
		}
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		// Split on the first run of whitespace: a tab between key and value
		// is what anyone aligning this file by hand produces, and rejecting
		// it as "want KEY VALUE" was a needless papercut.
		i := strings.IndexAny(t, " \t")
		if i < 0 {
			return fmt.Errorf("parse Kernelfile line %d: want KEY VALUE, got %q", line, t)
		}
		key, val := t[:i], strings.TrimSpace(t[i+1:])
		if val == "" {
			return fmt.Errorf("parse Kernelfile line %d: want KEY VALUE, got %q", line, t)
		}
		if first, dup := seen[key]; dup {
			return fmt.Errorf("parse Kernelfile line %d: duplicate key %q (first set on line %d)", line, key, first)
		}
		seen[key] = line
		switch key {
		case "KERNEL":
			spec.KernelVersion = val
			sawKernel = true
		case "SOURCE_URL":
			spec.SourceURL = val
			sawURL = true
		case "SHA256":
			spec.SourceSHA256 = val
			sawSHA = true
		case "EPOCH":
			spec.SourceDateEpoch = val
		case "CONFIG":
			spec.ConfigName = val
		case "EXPECT":
			spec.ExpectName = val
		case "BASE_MAKE":
			spec.BaseMake = val
		case "PATCHES":
			switch val {
			case "on":
				spec.ApplyPatches = true
			case "off":
				spec.ApplyPatches = false
			default:
				return fmt.Errorf("parse Kernelfile line %d: PATCHES wants on|off, got %q", line, val)
			}
		case "ARCH":
			spec.Arch = val
		case "CROSS_COMPILE":
			spec.CrossCompile = val
		case "TARGETS":
			// Accept spaces or commas: the env/opt form is comma-separated
			// and people will paste either.
			tgts := strings.FieldsFunc(val, func(r rune) bool { return r == ' ' || r == '\t' || r == ',' })
			if len(tgts) == 0 {
				// Separators only ("TARGETS ,"): silently falling back to the
				// arch default would build something the file did not ask for.
				return fmt.Errorf("parse Kernelfile line %d: TARGETS lists no target", line)
			}
			spec.Targets = tgts
		case "PROXY_CA":
			spec.ProxyCAFile = val
		case "BASE":
			spec.Base = val
		case "TOOLCHAIN":
			switch val {
			case "ready":
				spec.ToolchainReady = true
			case "apt":
				spec.ToolchainReady = false
			default:
				return fmt.Errorf("parse Kernelfile line %d: TOOLCHAIN wants ready|apt, got %q", line, val)
			}
		default:
			return fmt.Errorf("parse Kernelfile line %d: unknown key %q", line, key)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if sawKernel && !sawURL {
		// Derived after EOF, not at the KERNEL line: deriving inline would
		// let a KERNEL line silently overwrite an explicit SOURCE_URL written
		// above it, the order-dependence this parser promises not to have.
		spec.SourceURL = SourceURLFor(spec.KernelVersion)
	}
	if (sawKernel || sawURL) && !sawSHA {
		spec.SourceSHA256 = "" // the default pin is for the default tarball only
	}
	return nil
}
