package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb/sourceresolver"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/frontend/subrequests"
	"github.com/moby/buildkit/solver/pb"
	digest "github.com/opencontainers/go-digest"
	fstypes "github.com/tonistiigi/fsutil/types"

	kbuild "github.com/emirb/kernelbuild-buildkit"
)

// fakeGW is the slice of the gateway the frontend touches: build opts, local
// solves (the Kernelfile fetch and the context probe), image resolution, and
// the graph solve. Files and missing paths are declared per test.
type fakeGW struct {
	gwclient.Client
	opts    map[string]string
	files   map[string][]byte // the "dockerfile" local
	missing map[string]bool   // context paths StatFile reports absent
	solves  []gwclient.SolveRequest
}

func (f *fakeGW) BuildOpts() gwclient.BuildOpts { return gwclient.BuildOpts{Opts: f.opts} }

func (f *fakeGW) Solve(_ context.Context, req gwclient.SolveRequest) (*gwclient.Result, error) {
	f.solves = append(f.solves, req)
	res := gwclient.NewResult()
	res.SetRef(&fakeRef{gw: f})
	return res, nil
}

func (f *fakeGW) ResolveImageConfig(_ context.Context, ref string, _ sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	return ref, digest.FromString(ref), nil, nil
}

type fakeRef struct {
	gwclient.Reference
	gw *fakeGW
}

func (r *fakeRef) ReadFile(_ context.Context, req gwclient.ReadRequest) ([]byte, error) {
	b, ok := r.gw.files[req.Filename]
	if !ok {
		return nil, fmt.Errorf("%s: no such file", req.Filename)
	}
	return b, nil
}

func (r *fakeRef) StatFile(_ context.Context, req gwclient.StatRequest) (*fstypes.Stat, error) {
	if r.gw.missing[req.Path] {
		return nil, errors.New("no such file or directory")
	}
	return &fstypes.Stat{Path: req.Path}, nil
}

// compileEnv returns the compile exec's env from the last graph solve.
func compileEnv(t *testing.T, gw *fakeGW) map[string]string {
	t.Helper()
	for _, req := range slices.Backward(gw.solves) {
		def := req.Definition
		if def == nil {
			continue
		}
		for _, dt := range def.Def {
			var op pb.Op
			if err := op.Unmarshal(dt); err != nil {
				t.Fatal(err)
			}
			e, ok := op.Op.(*pb.Op_Exec)
			if !ok || len(e.Exec.Meta.Args) == 0 || e.Exec.Meta.Args[0] != kbuild.HelperPath {
				continue
			}
			env := map[string]string{}
			for _, kv := range e.Exec.Meta.Env {
				k, v, _ := strings.Cut(kv, "=")
				env[k] = v
			}
			return env
		}
	}
	t.Fatal("no compile exec was solved")
	return nil
}

// TestBuildDelegatesKernelfile: the #syntax= path end to end: the Kernelfile
// is read from the dockerfile local, parsed, layered with opts, checked
// against the context, and solved as the compile graph.
func TestBuildDelegatesKernelfile(t *testing.T) {
	gw := &fakeGW{
		opts: map[string]string{
			"source":   "ghcr.io/example/frontend:v1",
			"filename": "Kernelfile",
			"targets":  "config",
		},
		files: map[string][]byte{"Kernelfile": []byte("KERNEL 6.19.1\nCONFIG guest.config\nEPOCH 1700000000\n")},
	}
	if _, err := build(context.Background(), gw); err != nil {
		t.Fatal(err)
	}
	env := compileEnv(t, gw)
	if env["SRC_URL"] != kbuild.SourceURLFor("6.19.1") || env["SRC_SHA256"] != "" {
		t.Errorf("KERNEL line not honored: SRC_URL=%q SRC_SHA256=%q", env["SRC_URL"], env["SRC_SHA256"])
	}
	if env["SOURCE_DATE_EPOCH"] != "1700000000" {
		t.Errorf("EPOCH = %q", env["SOURCE_DATE_EPOCH"])
	}
	if env["TARGETS"] != "config" {
		t.Errorf("--opt targets did not layer over the Kernelfile: TARGETS=%q", env["TARGETS"])
	}
}

// TestBuildFailsOnUnreadableKernelfile: the dockerfile local exists but the
// named file cannot be read; falling through to defaults would build a
// kernel the user never asked for.
func TestBuildFailsOnUnreadableKernelfile(t *testing.T) {
	gw := &fakeGW{opts: map[string]string{"source": "ghcr.io/example/frontend:v1", "filename": "Kernelfile"}}
	_, err := build(context.Background(), gw)
	if err == nil || !strings.Contains(err.Error(), "Kernelfile") {
		t.Fatalf("missing Kernelfile: err = %v", err)
	}
	if len(gw.solves) > 1 {
		t.Error("compile graph solved despite the unreadable Kernelfile")
	}
}

func TestBuildRejectsUnknownKernelfileKey(t *testing.T) {
	gw := &fakeGW{
		opts:  map[string]string{"source": "ghcr.io/example/frontend:v1", "filename": "Kernelfile"},
		files: map[string][]byte{"Kernelfile": []byte("FROBNICATE yes\n")},
	}
	_, err := build(context.Background(), gw)
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("unknown key: err = %v", err)
	}
}

// TestBuildRequiresAHelperImage: a direct gateway.v0 invocation must carry
// opt source= or helper-ref, or there is no kbuild-step to mount.
func TestBuildRequiresAHelperImage(t *testing.T) {
	gw := &fakeGW{opts: map[string]string{"kernel-version": "6.19.1"}}
	_, err := build(context.Background(), gw)
	if err == nil || !strings.Contains(err.Error(), "helper") {
		t.Fatalf("no helper: err = %v", err)
	}
}

// TestBuildReportsMissingContextFile: a CONFIG the context lacks is named
// before any graph is solved.
func TestBuildReportsMissingContextFile(t *testing.T) {
	gw := &fakeGW{
		opts:    map[string]string{"source": "ghcr.io/example/frontend:v1", "config": "guest.config"},
		missing: map[string]bool{"guest.config": true},
	}
	_, err := build(context.Background(), gw)
	if err == nil || !strings.Contains(err.Error(), "CONFIG guest.config") {
		t.Fatalf("missing config: err = %v", err)
	}
}

// TestBuildAnswersSubrequests: docker build --print=... must be answered
// with a report, never with a kernel build; an unknown subrequest is an
// error, not a build either.
func TestBuildAnswersSubrequests(t *testing.T) {
	gw := &fakeGW{opts: map[string]string{"source": "ghcr.io/example/frontend:v1", "requestid": subrequests.RequestSubrequestsDescribe}}
	res, err := build(context.Background(), gw)
	if err != nil || res == nil || len(res.Metadata["result.json"]) == 0 {
		t.Fatalf("describe: res=%v err=%v", res, err)
	}
	if len(gw.solves) != 0 {
		t.Error("describe started a solve")
	}
	gw = &fakeGW{opts: map[string]string{"source": "ghcr.io/example/frontend:v1", "requestid": "frontend.outline"}}
	if _, err := build(context.Background(), gw); err == nil {
		t.Error("unsupported subrequest accepted")
	}
	if len(gw.solves) != 0 {
		t.Error("unsupported subrequest started a solve")
	}
}

// TestBuildRecoversFromPanic: a frontend must never die with a bare exit
// code; a panic becomes a readable error. A nil client panics on BuildOpts.
func TestBuildRecoversFromPanic(t *testing.T) {
	_, err := build(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("panic not converted to an error: %v", err)
	}
}
