package kbuild

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	digest "github.com/opencontainers/go-digest"
	fstypes "github.com/tonistiigi/fsutil/types"
)

// TestGraphProxyEnvHTTPAndNoProxy: a plain (non-MITM) corporate proxy is
// http_proxy + https_proxy + no_proxy, and all three must reach both execs:
// apt's stock mirrors are http://, and no_proxy is how an internal mirror is
// exempted.
func TestGraphProxyEnvHTTPAndNoProxy(t *testing.T) {
	s := baseSpec()
	s.HTTPProxy = "http://proxy.corp:3128"
	s.HTTPSProxy = "http://proxy.corp:3128"
	s.NoProxy = "mirror.corp,.internal"
	for _, e := range mustGraph(t, s).execs() {
		pe := e.Meta.ProxyEnv
		if pe == nil || pe.HttpProxy != s.HTTPProxy || pe.HttpsProxy != s.HTTPSProxy {
			t.Fatalf("exec %v proxy env = %+v", e.Meta.Args[:1], pe)
		}
		for _, want := range []string{"localhost", "127.0.0.1", "mirror.corp", ".internal"} {
			if !strings.Contains(pe.NoProxy, want) {
				t.Errorf("NoProxy %q lacks %q", pe.NoProxy, want)
			}
		}
	}
	// http_proxy alone must still produce a proxy env (apt is http).
	only := baseSpec()
	only.HTTPProxy = "http://proxy.corp:3128"
	for _, e := range mustGraph(t, only).execs() {
		if e.Meta.ProxyEnv == nil || e.Meta.ProxyEnv.HttpProxy != only.HTTPProxy {
			t.Errorf("HTTPProxy alone produced proxy env %+v", e.Meta.ProxyEnv)
		}
	}
	// And they stay cache-neutral like HTTPSProxy.
	plain := normalizedOps(t, mustGraph(t, baseSpec()))
	withProxy := normalizedOps(t, mustGraph(t, s))
	if len(plain) != len(withProxy) {
		t.Fatalf("proxy changed op count")
	}
	for i := range plain {
		if plain[i] != withProxy[i] {
			t.Errorf("op %d cache-relevant content changed when only proxies changed", i)
		}
	}
	// Validate keeps the new fields single-token like the others.
	for name, mut := range map[string]func(*Spec){
		"http-newline":    func(s *Spec) { s.HTTPProxy = "http://p\nX=1" },
		"http-noscheme":   func(s *Spec) { s.HTTPProxy = "p:3128" },
		"noproxy-space":   func(s *Spec) { s.NoProxy = "a.corp, b.corp" },
		"noproxy-newline": func(s *Spec) { s.NoProxy = "a.corp\nEVIL=1" },
	} {
		s := DefaultSpec()
		mut(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// TestS3CacheURLRegion: the region is a parameter (empty = auto). Region-less
// stores accept "auto"; real AWS S3 rejects it for SigV4.
func TestS3CacheURLRegion(t *testing.T) {
	spec, err := S3CacheURL("https://s3.eu-west-1.amazonaws.com/bucket", "eu-west-1")
	if err != nil || !strings.Contains(spec, "region=eu-west-1,") {
		t.Errorf("spec = %q, %v", spec, err)
	}
	spec, err = S3CacheURL("https://acct.r2.cloudflarestorage.com/bucket", "")
	if err != nil || !strings.Contains(spec, "region=auto,") {
		t.Errorf("default region: spec = %q, %v", spec, err)
	}
	if _, err := S3CacheURL("https://h/b", "eu west"); err == nil {
		t.Error("region with whitespace accepted")
	}
}

// fakeGW is the slice of gwclient.Client the shared gateway solve touches:
// image resolution and solves. Everything else panics on use (nil embedded
// interface), which is what we want from a test double.
type fakeGW struct {
	gwclient.Client
	dgst     digest.Digest
	resolved []string
	solves   []gwclient.SolveRequest
	readDir  func(path string) ([]*fstypes.Stat, error)
	statFile func(path string) error // nil: every file exists
}

func (f *fakeGW) ResolveImageConfig(_ context.Context, ref string, _ sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	f.resolved = append(f.resolved, ref)
	return ref, f.dgst, nil, nil
}

func (f *fakeGW) Solve(_ context.Context, req gwclient.SolveRequest) (*gwclient.Result, error) {
	f.solves = append(f.solves, req)
	res := gwclient.NewResult()
	res.SetRef(&fakeRef{readDir: f.readDir, statFile: f.statFile})
	return res, nil
}

type fakeRef struct {
	gwclient.Reference
	readDir  func(path string) ([]*fstypes.Stat, error)
	statFile func(path string) error
}

func (r *fakeRef) StatFile(_ context.Context, req gwclient.StatRequest) (*fstypes.Stat, error) {
	if r.statFile != nil {
		if err := r.statFile(req.Path); err != nil {
			return nil, err
		}
	}
	return &fstypes.Stat{Path: req.Path}, nil
}

// graphSolves counts the solves that carried the compile graph (as opposed
// to the context probe).
func graphSolves(t *testing.T, gw *fakeGW) int {
	t.Helper()
	n := 0
	for _, req := range gw.solves {
		for _, op := range opsOf(t, req.Definition) {
			if e, ok := op.Op.(*pb.Op_Exec); ok && len(e.Exec.Meta.Args) > 0 && e.Exec.Meta.Args[0] == HelperPath {
				n++
				break
			}
		}
	}
	return n
}

func (r *fakeRef) ReadDir(_ context.Context, req gwclient.ReadDirRequest) ([]*fstypes.Stat, error) {
	return r.readDir(req.Path)
}

func opsOf(t *testing.T, def *pb.Definition) []*pb.Op {
	t.Helper()
	var ops []*pb.Op
	for _, dt := range def.Def {
		var op pb.Op
		if err := op.Unmarshal(dt); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, &op)
	}
	return ops
}

func buildMountID(t *testing.T, ops []*pb.Op) string {
	t.Helper()
	for _, op := range ops {
		if e, ok := op.Op.(*pb.Op_Exec); ok && len(e.Exec.Meta.Args) > 0 && e.Exec.Meta.Args[0] == HelperPath {
			for _, m := range e.Exec.Mounts {
				if m.Dest == "/build" {
					return m.CacheOpt.ID
				}
			}
		}
	}
	t.Fatal("no compile exec with a /build mount in the solved definition")
	return ""
}

// TestGatewaySolvePinsBase: the shared solve used by BOTH the frontend and
// the client driver must resolve an unpinned Base to its digest through the
// daemon before the graph is generated, so kbuildctl -base tuxmake/x86_64_gcc
// keys the object tree by the digest that actually ran, not by a moving tag.
func TestGatewaySolvePinsBase(t *testing.T) {
	dgst := digest.FromString("toolchain-image")
	gw := &fakeGW{dgst: dgst}
	spec := baseSpec()
	spec.Base = "docker.io/tuxmake/x86_64_gcc"
	spec.ToolchainReady = true
	if _, err := GatewaySolve(context.Background(), gw, spec, GatewayOpts{}); err != nil {
		t.Fatal(err)
	}
	if len(gw.resolved) != 1 || gw.resolved[0] != "docker.io/tuxmake/x86_64_gcc" {
		t.Fatalf("resolver calls = %v, want exactly the unpinned base", gw.resolved)
	}
	if n := graphSolves(t, gw); n != 1 {
		t.Fatalf("graph solves = %d, want 1", n)
	}
	ops := opsOf(t, gw.solves[len(gw.solves)-1].Definition)
	var pinned bool
	for _, op := range ops {
		if s, ok := op.Op.(*pb.Op_Source); ok && strings.Contains(s.Source.Identifier, "tuxmake") {
			pinned = strings.Contains(s.Source.Identifier, "@"+dgst.String())
		}
	}
	if !pinned {
		t.Error("solved graph's base image is not digest-pinned")
	}
	// The mount id must be the one the PINNED spec produces (the tree is
	// keyed by what ran), not the tag's.
	want := spec
	want.Base = spec.Base + "@" + dgst.String()
	wantID := ""
	for _, m := range mustGraph(t, want).compileExec(t).Mounts {
		if m.Dest == "/build" {
			wantID = m.CacheOpt.ID
		}
	}
	if got := buildMountID(t, ops); got != wantID {
		t.Errorf("/build mount id = %q, want %q (keyed by the resolved digest)", got, wantID)
	}

	// Already pinned: no resolver round-trip at all.
	gw2 := &fakeGW{dgst: dgst}
	if _, err := GatewaySolve(context.Background(), gw2, baseSpec(), GatewayOpts{}); err != nil {
		t.Fatal(err)
	}
	if len(gw2.resolved) != 0 {
		t.Errorf("pinned base still resolved: %v", gw2.resolved)
	}
}

// TestGatewaySolveChecksPatchesDir: in gateway mode the context lives on the
// client; PATCHES on with no patches/*.patch must be reported as such before
// the compile graph is solved, instead of BuildKit's "failed to calculate
// checksum ... /patches: not found".
func TestGatewaySolveChecksPatchesDir(t *testing.T) {
	spec := baseSpec()
	spec.ApplyPatches = true
	missing := &fakeGW{dgst: digest.FromString("x"), readDir: func(string) ([]*fstypes.Stat, error) {
		return nil, errors.New("lstat /patches: no such file or directory")
	}}
	_, err := GatewaySolve(context.Background(), missing, spec, GatewayOpts{})
	if err == nil || !strings.Contains(err.Error(), "patches") {
		t.Fatalf("missing patches dir: %v", err)
	}
	for _, req := range missing.solves {
		for _, op := range opsOf(t, req.Definition) {
			if e, ok := op.Op.(*pb.Op_Exec); ok && len(e.Exec.Meta.Args) > 0 && e.Exec.Meta.Args[0] == HelperPath {
				t.Fatal("compile graph was solved despite the missing patches dir")
			}
		}
	}
	present := &fakeGW{dgst: digest.FromString("x"), readDir: func(string) ([]*fstypes.Stat, error) {
		return []*fstypes.Stat{{Path: "0001-fix.patch"}}, nil
	}}
	if _, err := GatewaySolve(context.Background(), present, spec, GatewayOpts{}); err != nil {
		t.Fatalf("patches present but solve failed: %v", err)
	}
	var compiled bool
	for _, req := range present.solves {
		for _, op := range opsOf(t, req.Definition) {
			if e, ok := op.Op.(*pb.Op_Exec); ok && len(e.Exec.Meta.Args) > 0 && e.Exec.Meta.Args[0] == HelperPath {
				compiled = true
			}
		}
	}
	if !compiled {
		t.Error("patches present but the compile graph was never solved")
	}
	_ = llb.LinuxAmd64
}

// TestGatewaySolveChecksContextFiles: a Kernelfile whose CONFIG (or EXPECT,
// PROXY_CA) names a file the context does not have must fail naming that key
// before the compile graph is solved, instead of BuildKit's "failed to
// calculate checksum of ref ...: not found".
func TestGatewaySolveChecksContextFiles(t *testing.T) {
	spec := baseSpec()
	spec.ExpectName = "kernel.expect"
	for _, missing := range []struct{ file, key string }{
		{spec.ConfigName, "CONFIG"},
		{"kernel.expect", "EXPECT"},
	} {
		gw := &fakeGW{dgst: digest.FromString("x"), statFile: func(path string) error {
			if path == missing.file {
				return errors.New("no such file or directory")
			}
			return nil
		}}
		_, err := GatewaySolve(context.Background(), gw, spec, GatewayOpts{})
		if err == nil || !strings.Contains(err.Error(), missing.key+" "+missing.file) {
			t.Errorf("missing %s: err = %v, want it named", missing.file, err)
		}
		if graphSolves(t, gw) != 0 {
			t.Errorf("missing %s: compile graph was solved anyway", missing.file)
		}
	}
	ok := &fakeGW{dgst: digest.FromString("x")}
	if _, err := GatewaySolve(context.Background(), ok, spec, GatewayOpts{}); err != nil {
		t.Fatalf("all files present: %v", err)
	}
	if graphSolves(t, ok) != 1 {
		t.Error("all files present but the compile graph was not solved")
	}
}
