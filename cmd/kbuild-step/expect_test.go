package main

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chtmp runs the test chdir'd into a temp dir (validateExpectations and
// bundleKconfigs read the tree relative to the build dir).
func chtmp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return dir
}

func TestValidateExpectations(t *testing.T) {
	dir := chtmp(t)
	cfg := "CONFIG_A=y\nCONFIG_B=m\n# CONFIG_C is not set\nCONFIG_HZ=100\n"
	if err := os.WriteFile(".config", []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(expect string) string {
		p := filepath.Join(dir, "kernel.expect")
		if err := os.WriteFile(p, []byte(expect), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Absent file: no gate.
	if err := validateExpectations(filepath.Join(dir, "missing")); err != nil {
		t.Errorf("absent expect file must be a no-op: %v", err)
	}
	// All satisfied: y matches =y and =m, n matches not-set and absent, = is verbatim.
	ok := "# comment\n\ny CONFIG_A\ny CONFIG_B\nn CONFIG_C\nn CONFIG_ABSENT\n= CONFIG_HZ=100\n"
	if err := validateExpectations(write(ok)); err != nil {
		t.Errorf("satisfied expectations failed: %v", err)
	}
	// Each violation class must fail.
	for _, bad := range []string{"y CONFIG_C", "y CONFIG_ABSENT", "n CONFIG_A", "n CONFIG_B", "= CONFIG_HZ=250"} {
		if err := validateExpectations(write(bad + "\n")); err == nil {
			t.Errorf("expectation %q passed against %q", bad, cfg)
		}
	}
	// Malformed input fails closed: bad op, bare line, non-CONFIG symbol.
	for _, malformed := range []string{"z CONFIG_A\n", "CONFIG_A\n", "y not_a_symbol\n", "y CONFIG_A; reboot\n"} {
		if err := validateExpectations(write(malformed)); err == nil {
			t.Errorf("malformed expect %q accepted", malformed)
		}
	}
}

func TestBundleKconfigs(t *testing.T) {
	dir := chtmp(t)
	files := map[string]string{
		"Kconfig":               "root menu\n",
		"drivers/net/Kconfig":   "config NET_A\n\tbool \"a\"\n",
		"arch/x86/Kconfig.cpu":  "cpu options\n",
		"Documentation/Kconfig": "MUST NOT APPEAR\n",
		"scripts/Kconfig.inc":   "MUST NOT APPEAR\n",
		"drivers/net/main.c":    "not a Kconfig\n",
	}
	for p, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "kconfig.txt.gz")
	if err := bundleKconfigs(out); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	// Sorted "### <path>" sections, pruned dirs and non-Kconfig files excluded.
	for _, want := range []string{"### Kconfig\nroot menu\n", "### arch/x86/Kconfig.cpu\n", "### drivers/net/Kconfig\nconfig NET_A\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("bundle missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "MUST NOT APPEAR") || strings.Contains(got, "main.c") {
		t.Errorf("bundle contains pruned/non-Kconfig content:\n%s", got)
	}
	if strings.Index(got, "### Kconfig\n") > strings.Index(got, "### arch/") {
		t.Errorf("sections not sorted:\n%s", got)
	}
}
