package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// validateExpectations enforces the /kernel.expect gate right after
// olddefconfig: the caller composed a config, olddefconfig may have silently
// dropped or overridden parts of it, and a service wants that surfaced in
// seconds — not after a full compile. Line grammar (blank and # lines skipped):
//
//	y CONFIG_X      X must have survived as =y or =m
//	n CONFIG_X      X must NOT be =y or =m
//	= CONFIG_X=val  the literal line must be present verbatim
//
// Absent file = no gate. Every violation is printed as a "KBF-EXPECT-FAIL"
// marker line (machine-parsable by the driving service) before the step fails,
// and nothing has been compiled or seeded at that point.
func validateExpectations(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cfg, err := os.ReadFile(".config")
	if err != nil {
		return fmt.Errorf("expect: no .config to validate: %w", err)
	}
	set := map[string]bool{}   // symbols that are =y or =m
	lines := map[string]bool{} // every full config line, for "=" checks
	for l := range strings.SplitSeq(string(cfg), "\n") {
		lines[l] = true
		if sym, val, ok := strings.Cut(l, "="); ok && strings.HasPrefix(sym, "CONFIG_") && (val == "y" || val == "m") {
			set[sym] = true
		}
	}
	t := time.Now()
	n, failures := 0, 0
	for i, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		op, arg, ok := strings.Cut(l, " ")
		arg = strings.TrimSpace(arg)
		if !ok || arg == "" {
			return fmt.Errorf("expect line %d: want 'y CONFIG_X' | 'n CONFIG_X' | '= CONFIG_LINE', got %q", i+1, l)
		}
		bad := false
		switch op {
		case "y", "n":
			if !expectSymRe.MatchString(arg) {
				return fmt.Errorf("expect line %d: %q is not a CONFIG_ symbol", i+1, arg)
			}
			bad = set[arg] != (op == "y")
		case "=":
			bad = !lines[arg]
		default:
			return fmt.Errorf("expect line %d: unknown op %q (want y|n|=)", i+1, op)
		}
		n++
		if bad {
			failures++
			fmt.Printf("KBF-EXPECT-FAIL %s %s\n", op, arg)
		}
	}
	if failures > 0 {
		return fmt.Errorf("config validation failed: %d of %d expectations not met (KBF-EXPECT-FAIL lines above)", failures, n)
	}
	fmt.Printf(">> validate: %d expectations ok\n", n)
	phase("validate", t)
	return nil
}

var expectSymRe = regexp.MustCompile(`^CONFIG_[A-Z0-9_]+$`)

// bundleKconfigs writes the tree's Kconfig files as one gzipped text blob
// ("### <path>\n<content>" sections, sorted) — the input a service parses into
// a symbol catalog. Needs only the extracted tree; no compilation.
func bundleKconfigs(out string) error {
	skip := map[string]bool{".git": true, "Documentation": true, "tools": true, "samples": true, "scripts": true}
	var names []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[p] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(filepath.Base(p), "Kconfig") {
			names = append(names, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(names)
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	zw, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		_ = f.Close()
		return err
	}
	for _, name := range names {
		if _, err := fmt.Fprintf(zw, "### %s\n", name); err != nil {
			_ = f.Close()
			return err
		}
		in, err := os.Open(name)
		if err != nil {
			_ = f.Close()
			return err
		}
		_, err = io.Copy(zw, in)
		_ = in.Close()
		if err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		return err
	}
	fmt.Printf(">> kconfigs: %d files bundled\n", len(names))
	return f.Close()
}
