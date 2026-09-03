package kbuild

import (
	"context"
	"debug/elf"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/buildkit/solver/pb"
)

// TestKernelfileDuplicateKey: a key given twice is a typo, not "last wins".
// Silently taking the second value builds something the author did not
// intend (the same reasoning that makes unknown keys an error).
func TestKernelfileDuplicateKey(t *testing.T) {
	s := DefaultSpec()
	err := ParseKernelfile(strings.NewReader("KERNEL 6.18.20\nCONFIG a.config\nKERNEL 6.18.19\n"), &s)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate KERNEL accepted: %v", err)
	}
}

// TestValidateRejectsControlChars: every value that reaches the compile
// vertex's env must be a single printable token. A newline in SourceURL or a
// proxy URL lands in env verbatim (SRC_URL, HTTPS_PROXY) and, unpinned, in
// the tree stamp — the README promises Validate covers "every value that
// reaches env".
func TestValidateRejectsControlChars(t *testing.T) {
	reject := map[string]func(*Spec){
		"url-newline":    func(s *Spec) { s.SourceURL = "https://h/linux-6.18.20\n.tar.gz" },
		"url-space":      func(s *Spec) { s.SourceURL = "https://h/a b/linux-6.18.20.tar.gz" },
		"url-tab":        func(s *Spec) { s.SourceURL = "https://h/a\tb/linux-6.18.20.tar.gz" },
		"proxy-newline":  func(s *Spec) { s.HTTPSProxy = "http://p:3128\nEVIL=1" },
		"proxy-space":    func(s *Spec) { s.HTTPSProxy = "http://p:3128 x" },
		"proxy-noscheme": func(s *Spec) { s.HTTPSProxy = "p:3128" },
	}
	for name, mut := range reject {
		s := DefaultSpec()
		mut(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, s)
		}
	}
	ok := DefaultSpec()
	ok.HTTPSProxy = "http://user:pass@proxy.corp.example:3128"
	ok.SourceURL = "https://mirrors.edge.kernel.org/pub/linux/kernel/v6.x/linux-6.18.20.tar.gz"
	if err := ok.Validate(); err != nil {
		t.Errorf("good proxy/url rejected: %v", err)
	}
}

// TestKernelLLBRequiresPinnedBase: the persistent object tree and the seed are
// keyed by the Base STRING (toolchainHash). A moving tag would share one tree
// across two different compilers — the exact mixing the mount id exists to
// prevent — so the graph must refuse to be generated from an unpinned ref.
// GatewaySolve resolves the tag to a digest before calling KernelLLB.
func TestKernelLLBRequiresPinnedBase(t *testing.T) {
	s := baseSpec()
	s.Base = "docker.io/tuxmake/x86_64_gcc"
	s.ToolchainReady = true
	if _, err := KernelLLB(s); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("unpinned base accepted by KernelLLB: %v", err)
	}
	s.Base = "docker.io/tuxmake/x86_64_gcc@sha256:" + strings.Repeat("ab", 32)
	if _, err := KernelLLB(s); err != nil {
		t.Fatalf("digest-pinned base rejected: %v", err)
	}
}

// TestGraphSeedPushEnv: the step must be able to tell that a push was
// REQUESTED, independently of whether the seed_cfg secret arrived. Without
// SEED_PUSH in env, a gateway build with seed-push=true and a mistyped
// --secret forces executions forever and never pushes, silently.
func TestGraphSeedPushEnv(t *testing.T) {
	envOf := func(s Spec) map[string]string {
		env := map[string]string{}
		for _, e := range mustGraph(t, s).compileExec(t).Meta.Env {
			if k, v, ok := strings.Cut(e, "="); ok {
				env[k] = v
			}
		}
		return env
	}
	if v, ok := envOf(baseSpec())["SEED_PUSH"]; ok {
		t.Errorf("SEED_PUSH=%q set on a non-push build (op shape must not change for the common case)", v)
	}
	s := baseSpec()
	s.SeedURL = "https://h/b"
	s.SeedPush = true
	if v := envOf(s)["SEED_PUSH"]; v != "1" {
		t.Errorf("SEED_PUSH env = %q on a push build, want 1", v)
	}
}

// compileOp returns the raw op (with its inputs) for the compile exec.
func (g graph) compileOp(t *testing.T) *pb.Op {
	t.Helper()
	for _, op := range g.ops {
		if e, ok := op.Op.(*pb.Op_Exec); ok && len(e.Exec.Meta.Args) > 0 && e.Exec.Meta.Args[0] == HelperPath {
			return op
		}
	}
	t.Fatal("no compile op")
	return nil
}

// TestGraphHelperImageMountIsFileCopy: in gateway mode the helper mount must
// be a FileOp holding only /kbuild-step, not the whole frontend image. A
// read-only mount keys the compile vertex by its CONTENT digest, so with the
// single file every frontend rebuild that ships an identical kbuild-step
// stays a cache hit; with the whole image, a new kbuild-frontend binary
// invalidates every compile vertex.
func TestGraphHelperImageMountIsFileCopy(t *testing.T) {
	s := baseSpec()
	s.HelperRef = "ghcr.io/example/kbuild-frontend:v1"
	g := mustGraph(t, s)
	op := g.compileOp(t)
	var helperIn *pb.Input
	for _, m := range op.Op.(*pb.Op_Exec).Exec.Mounts {
		if m.Dest == "/helper" {
			helperIn = op.Inputs[m.Input]
		}
	}
	if helperIn == nil {
		t.Fatal("no /helper mount")
	}
	var src *pb.Op
	for i, d := range g.dgst {
		if string(d) == string(helperIn.Digest) {
			src = g.ops[i]
		}
	}
	if src == nil {
		t.Fatal("helper input is not an op in the graph")
	}
	f, ok := src.Op.(*pb.Op_File)
	if !ok {
		t.Fatalf("helper mount input is %T, want a FileOp copying only /kbuild-step out of the image", src.Op)
	}
	var copies bool
	for _, a := range f.File.Actions {
		if c, ok := a.Action.(*pb.FileAction_Copy); ok && strings.HasSuffix(c.Copy.Src, "/kbuild-step") {
			copies = true
		}
	}
	if !copies {
		t.Errorf("helper FileOp does not copy /kbuild-step: %+v", f.File.Actions)
	}
}

// TestBuildRejectsMissingPatchesDir: PATCHES on with no patches/ directory in
// the context must fail with a readable message before any daemon dial —
// BuildKit's own failure is an opaque "failed to calculate checksum ...
// /patches: not found".
func TestBuildRejectsMissingPatchesDir(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "kbuild-step")
	if err := os.WriteFile(helper, minimalELF(t, elf.EM_X86_64), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := DefaultSpec()
	spec.ApplyPatches = true
	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "kernel.config"), []byte("# defaults\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Build(context.Background(), spec, BuildConfig{
		Addr: "tcp://127.0.0.1:1", ContextDir: ctxDir, HelperBin: helper,
	})
	if err == nil || !strings.Contains(err.Error(), "patches") || strings.Contains(err.Error(), "connect") {
		t.Fatalf("missing patches dir: want a patches error before the dial, got %v", err)
	}
}

// TestBuildRejectsMissingConfig: the client driver has the context on disk,
// so a missing config file is reported by name before any daemon dial.
func TestBuildRejectsMissingConfig(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "kbuild-step")
	if err := os.WriteFile(helper, minimalELF(t, elf.EM_X86_64), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Build(context.Background(), DefaultSpec(), BuildConfig{
		Addr: "tcp://127.0.0.1:1", ContextDir: t.TempDir(), HelperBin: helper,
	})
	if err == nil || !strings.Contains(err.Error(), "CONFIG kernel.config") || strings.Contains(err.Error(), "connect") {
		t.Fatalf("missing config: want a CONFIG error before the dial, got %v", err)
	}
}

// TestKernelfileBOM: a UTF-8 byte order mark before #syntax= is dropped, so
// a Kernelfile saved by a Windows editor parses like any other.
func TestKernelfileBOM(t *testing.T) {
	s := DefaultSpec()
	if err := ParseKernelfile(strings.NewReader("\ufeff#syntax=ghcr.io/x/y\nCONFIG a.config\n"), &s); err != nil {
		t.Fatalf("BOM rejected: %v", err)
	}
	if s.ConfigName != "a.config" {
		t.Errorf("ConfigName = %q", s.ConfigName)
	}
}
