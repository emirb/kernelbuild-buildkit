// kbuild-frontend is the packaged BuildKit gateway frontend (build.kernel.v0).
//
// Its image carries two binaries: /kbuild-frontend (this gateway entrypoint)
// and /kbuild-step (the in-build step runner, mounted into the compile vertex).
//
// Two ways to invoke it:
//
//  1. Kernelfile + stock docker build — the dockerfile frontend reads the
//     #syntax= line and delegates the file to this image:
//
//     #syntax=ghcr.io/emirb/kernelbuild-buildkit
//     KERNEL  6.18.20
//     CONFIG  kernel.config
//     SHA256  837a5abd...
//
//     $ docker build -f Kernelfile --output type=local,dest=out kernel/
//
//  2. Directly via gateway.v0 with --opt keys (no file needed):
//
//     buildctl build --frontend gateway.v0 --opt source=<this-image> \
//     --opt kernel-version=6.18.20 --opt config=kernel.config \
//     --local context=kernel --output type=local,dest=out
//
// Object-tree seeding is enabled by passing secrets (never opts):
//
//	--secret id=seed_access_key,env=AWS_ACCESS_KEY_ID \
//	--secret id=seed_secret_key,env=AWS_SECRET_ACCESS_KEY \
//	--secret id=seed_cfg,src=seed.env
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/frontend/gateway/grpcclient"
	"github.com/moby/buildkit/frontend/subrequests"
	"github.com/moby/buildkit/solver/errdefs"
	"github.com/moby/buildkit/util/appcontext"

	kbuild "github.com/emirb/kernelbuild-buildkit"
)

func main() {
	if err := grpcclient.RunFromEnvironment(appcontext.Context(), build); err != nil {
		panic(err)
	}
}

// readKernelfile fetches the delegated build file (the "dockerfile" local)
// when one is present. Absent file (direct gateway.v0 invocation) is fine.
func readKernelfile(ctx context.Context, c gwclient.Client, filename string) ([]byte, error) {
	src := llb.Local("dockerfile",
		llb.FollowPaths([]string{filename}),
		llb.SharedKeyHint("kernelfile"),
		llb.WithCustomName("[internal] load "+filename))
	def, err := src.Marshal(ctx, llb.LinuxAmd64)
	if err != nil {
		return nil, err
	}
	res, err := c.Solve(ctx, gwclient.SolveRequest{Definition: def.ToPB()})
	if err != nil {
		return nil, fmt.Errorf("fetch build file %q: %w", filename, err)
	}
	ref, err := res.SingleRef()
	if err != nil {
		return nil, err
	}
	dt, err := ref.ReadFile(ctx, gwclient.ReadRequest{Filename: filename})
	if err != nil {
		// The dockerfile local EXISTS (the solve above succeeded) but the
		// named file can't be read from it. Silently falling back to the
		// built-in defaults here would build a kernel the user never asked
		// for — fail loudly instead.
		return nil, fmt.Errorf("build file %q not readable from the dockerfile context: %w", filename, err)
	}
	return dt, nil
}

func build(ctx context.Context, c gwclient.Client) (res *gwclient.Result, err error) {
	// A frontend must never die with a bare "exit code: 255": turn a panic
	// into a readable build error carrying the stack.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("kbuild frontend panic: %v\n%s", r, debug.Stack())
		}
	}()
	opts := c.BuildOpts().Opts

	// Subrequests (docker build --print=..., --check) must be answered, not
	// built: an unhandled requestid would silently start a kernel build and
	// hand back artifacts the client then fails to parse as a report.
	if req, ok := opts["requestid"]; ok && req != "" {
		if req == subrequests.RequestSubrequestsDescribe {
			return describeSubrequests()
		}
		return nil, errdefs.NewUnsupportedSubrequestError(req)
	}

	spec := kbuild.DefaultSpec()

	// The frontend's own image carries /kbuild-step for the compile vertex.
	if src := opts["source"]; src != "" {
		spec.HelperRef = src
	}

	// Layer 1: the Kernelfile. The dockerfile frontend always forwards the
	// "filename" opt in #syntax= delegation; its absence means a direct
	// gateway.v0 invocation (opts only), where no dockerfile local exists.
	// Inside dockerfile mode EVERY error is fatal: a transient failure
	// fetching the user's build file must never fall through to defaults.
	if filename := opts["filename"]; filename != "" {
		dt, err := readKernelfile(ctx, c, filename)
		if err != nil {
			return nil, err
		}
		if err := kbuild.ParseKernelfile(bytes.NewReader(dt), &spec); err != nil {
			return nil, err
		}
	}

	// Layer 2: --opt / --build-arg overrides.
	if err := applyOpts(opts, &spec); err != nil {
		return nil, err
	}
	if spec.HelperRef == "" {
		return nil, errors.New("no helper image: gateway invocation must carry opt source=<frontend image> or helper-ref")
	}

	// Validate BEFORE the registry round-trip in GatewaySolve: an unchecked
	// base ref must not become an outbound request, and a rejected build
	// should fail on its own input, not on a resolver error about it.
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	// Standard remote-cache import: without threading these, a
	// `docker build --cache-from ...` handed to the frontend is silently
	// ignored (it worked only through the kbuildctl driver).
	cacheImports, err := parseCacheImports(opts)
	if err != nil {
		return nil, err
	}
	// GatewaySolve pins the base image by digest (the object tree and the
	// seed are keyed by the Base string, so a moving tag must not reach the
	// graph; resolving here is also what makes `docker build --pull` mean
	// anything), checks PATCHES on against the context, then solves.
	return kbuild.GatewaySolve(ctx, c, spec, kbuild.GatewayOpts{
		ResolveMode:  opts["image-resolve-mode"],
		CacheImports: cacheImports,
	})
}

// applyOpts maps the --opt / --build-arg layer onto the Spec. This is not
// command-line parsing (there is no argv here): opts is BuildKit's
// gateway-protocol map, delivered over gRPC, and reading it key by key is the
// standard frontend idiom (docker's dockerfile frontend does exactly this in
// dockerui/config.go). Kept as plain lines on purpose: every value flows into
// Spec.Validate, and this path stays greppable.
func applyOpts(opts map[string]string, spec *kbuild.Spec) error {
	if v := opts["kernel-version"]; v != "" && v != spec.KernelVersion {
		spec.KernelVersion = v
		spec.SourceURL = kbuild.SourceURLFor(v)
		spec.SourceSHA256 = "" // the existing pin was for the previous version
	}
	if v := opts["source-url"]; v != "" && v != spec.SourceURL {
		spec.SourceURL = v
		spec.SourceSHA256 = ""
	}
	if v := opts["source-sha256"]; v != "" {
		spec.SourceSHA256 = v
	}
	if v := opts["source-date-epoch"]; v != "" {
		spec.SourceDateEpoch = v
	}
	if v := opts["config"]; v != "" {
		spec.ConfigName = v
	}
	if v := opts["expect"]; v != "" {
		spec.ExpectName = v
	}
	if v := opts["base-make"]; v != "" {
		spec.BaseMake = v
	}
	if v := opts["proxy-ca"]; v != "" {
		spec.ProxyCAFile = v
	}
	if v := opts["helper-ref"]; v != "" {
		spec.HelperRef = v
	}
	if v := opts["src-name"]; v != "" {
		spec.SourceLocalName = v // needs --local src=<dir> on the invocation
	}
	if v := opts["base"]; v != "" {
		spec.Base = v
	}
	switch opts["toolchain-ready"] {
	case "true":
		spec.ToolchainReady = true
	case "false":
		spec.ToolchainReady = false
	}
	if v := opts["arch"]; v != "" {
		spec.Arch = v
	}
	if v := opts["cross-compile"]; v != "" {
		spec.CrossCompile = v
	}
	// Proxies arrive the canonical way, as build-args under either spelling.
	// All three matter: apt's stock mirrors are http://, so HTTPS_PROXY alone
	// leaves the toolchain vertex without egress behind a plain proxy, and
	// NO_PROXY is how an internal mirror is exempted.
	for _, m := range []struct {
		keys []string
		dst  *string
	}{
		{[]string{"build-arg:https_proxy", "build-arg:HTTPS_PROXY"}, &spec.HTTPSProxy},
		{[]string{"build-arg:http_proxy", "build-arg:HTTP_PROXY"}, &spec.HTTPProxy},
		{[]string{"build-arg:no_proxy", "build-arg:NO_PROXY"}, &spec.NoProxy},
	} {
		for _, k := range m.keys {
			if v := opts[k]; v != "" {
				*m.dst = v
				break
			}
		}
	}
	// force-network-mode is BuildKit's canonical key for --network=host.
	if opts["force-network-mode"] == "host" {
		spec.NetworkHost = true
	}
	// platform maps onto the target arch so the standard --platform knob
	// works. Full multi-platform result maps are not modeled: the output is
	// a raw artifact exported to a local dir, not an image index.
	if p := opts["platform"]; p != "" {
		arch, err := archForPlatform(p)
		if err != nil {
			return err
		}
		spec.Arch = arch
	}
	if v := opts["targets"]; v != "" {
		spec.Targets = strings.Split(v, ",")
	}
	switch opts["apply-patches"] {
	case "true":
		spec.ApplyPatches = true
	case "false":
		spec.ApplyPatches = false
	}
	if _, ok := opts["no-cache"]; ok {
		// `docker build --no-cache` sends this key with an empty value (a
		// value would be a stage list, and this frontend has one build). A
		// frontend that ignores it hands back a cached result for a build the
		// user explicitly asked to re-run.
		spec.IgnoreCache = true
	}
	if opts["seed-push"] == "true" {
		// The seed destination arrives via the seed_cfg secret (cache-
		// neutral); this opt forces the compile vertex to execute so the
		// push actually happens instead of being cache-elided, and tells the
		// step a push was requested (a missing secret then fails loudly).
		spec.SeedPush = true
	}
	return nil
}

// describeSubrequests answers frontend.subrequests.describe. A Kernelfile has
// no stages, no build args and no targets to enumerate, so describe is the
// only subrequest this frontend implements — but saying so is what lets a
// client discover that instead of guessing.
func describeSubrequests() (*gwclient.Result, error) {
	all := []subrequests.Request{subrequests.SubrequestsDescribeDefinition}
	dt, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := subrequests.PrintDescribe(dt, &buf); err != nil {
		return nil, err
	}
	res := gwclient.NewResult()
	res.Metadata = map[string][]byte{
		"result.json": dt,
		"result.txt":  buf.Bytes(),
		"version":     []byte(subrequests.SubrequestsDescribeDefinition.Version),
	}
	return res, nil
}

// archForPlatform maps a --platform value onto the Spec's target arch.
// platforms.Parse, not a name suffix match: it normalizes aliases and
// variants, so linux/arm64/v8 and linux/x86_64 land on the right arch instead
// of being rejected.
func archForPlatform(p string) (string, error) {
	if strings.Contains(p, ",") {
		return "", fmt.Errorf("platform %q: this frontend builds one target arch per invocation", p)
	}
	if !strings.Contains(p, "/") {
		// A bare arch ("arm64") means linux/<arch> here: platforms.Parse fills
		// the OS in from the HOST (darwin on a Mac running the unit tests),
		// which the linux-only check below would then reject.
		p = "linux/" + p
	}
	pp, err := platforms.Parse(p)
	if err != nil {
		return "", fmt.Errorf("platform %q: %w", p, err)
	}
	if pp.OS != "" && pp.OS != "linux" {
		return "", fmt.Errorf("platform %q: only linux targets are supported", p)
	}
	switch pp.Architecture {
	case "arm64":
		return "arm64", nil
	case "amd64":
		return "x86_64", nil
	default:
		return "", fmt.Errorf("platform %q: supported linux/amd64, linux/arm64", p)
	}
}

// parseCacheImports reads the standard cache-imports (JSON array) and legacy
// cache-from (comma-separated registry refs) opts, mirroring dockerui.
func parseCacheImports(opts map[string]string) ([]gwclient.CacheOptionsEntry, error) {
	var out []gwclient.CacheOptionsEntry
	if v := opts["cache-imports"]; v != "" {
		var ci []struct {
			Type  string            `json:"type"`
			Attrs map[string]string `json:"attrs"`
		}
		if err := json.Unmarshal([]byte(v), &ci); err != nil {
			return nil, fmt.Errorf("cache-imports: %w", err)
		}
		for _, e := range ci {
			out = append(out, gwclient.CacheOptionsEntry{Type: e.Type, Attrs: e.Attrs})
		}
	}
	for ref := range strings.SplitSeq(opts["cache-from"], ",") {
		if ref = strings.TrimSpace(ref); ref != "" {
			out = append(out, gwclient.CacheOptionsEntry{Type: "registry", Attrs: map[string]string{"ref": ref}})
		}
	}
	return out, nil
}
