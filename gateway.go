package kbuild

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	digest "github.com/opencontainers/go-digest"
)

// execPlatform is the platform the build steps run on. The compile vertex is
// always linux/amd64 (foreign targets are cross-compiled, see the arch/
// cross-compile options), so image metadata is resolved for it.
var execPlatform = platforms.MustParse("linux/amd64")

// GatewayOpts carries the per-invocation knobs the gateway frontend forwards
// from docker build; the client driver leaves them zero.
type GatewayOpts struct {
	ResolveMode  string                       // image-resolve-mode ("pull" for docker build --pull)
	CacheImports []gwclient.CacheOptionsEntry // --cache-from / cache-imports
}

// GatewaySolve is the one solve path shared by the gateway frontend and the
// client driver (kbuild.Build runs it inside client.Build). It does what the
// graph alone cannot: pin the base image by digest through the daemon's
// resolver (the object tree and the seed are keyed by the Base string, so a
// moving tag must never reach KernelLLB), confirm that the build context has
// the files the graph mounts (BuildKit's own failure for a missing selector
// path is an opaque checksum error), then generate and solve the graph.
func GatewaySolve(ctx context.Context, c gwclient.Client, spec Spec, opts GatewayOpts) (*gwclient.Result, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if !strings.Contains(spec.Base, "@") {
		ref, dgst, err := resolveBase(ctx, c, spec.Base, opts.ResolveMode)
		if err != nil {
			return nil, err
		}
		spec.Base = ref + "@" + dgst.String()
	}
	if err := checkContext(ctx, c, spec); err != nil {
		return nil, err
	}
	st, err := KernelLLB(spec)
	if err != nil {
		return nil, err
	}
	def, err := st.Marshal(ctx, llb.LinuxAmd64)
	if err != nil {
		return nil, fmt.Errorf("marshal llb: %w", err)
	}
	return c.Solve(ctx, gwclient.SolveRequest{Definition: def.ToPB(), CacheImports: opts.CacheImports})
}

// resolveBase resolves an image reference to its manifest digest, honoring
// the standard image-resolve-mode opt ("pull" for `docker build --pull`).
func resolveBase(ctx context.Context, c gwclient.Client, ref, resolveMode string) (string, digest.Digest, error) {
	mutRef, dgst, _, err := c.ResolveImageConfig(ctx, ref, sourceresolver.Opt{
		LogName: "[internal] load metadata for " + ref,
		ImageOpt: &sourceresolver.ResolveImageOpt{
			Platform:    &execPlatform,
			ResolveMode: resolveMode,
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("resolve base image %q: %w", ref, err)
	}
	if dgst == "" {
		return "", "", fmt.Errorf("resolve base image %q: resolver returned no digest", ref)
	}
	// The resolver may return a ref that already carries a digest; the tag
	// part is what we re-pin, so drop anything after "@".
	if i := strings.IndexByte(mutRef, '@'); i >= 0 {
		mutRef = mutRef[:i]
	}
	return mutRef, dgst, nil
}

// contextFiles lists the files the graph mounts out of the build context by
// selector, with the Kernelfile key that names each: the config always, the
// expectations and proxy CA when set.
func (s Spec) contextFiles() [][2]string {
	files := [][2]string{{"CONFIG", s.ConfigName}}
	if s.ExpectName != "" {
		files = append(files, [2]string{"EXPECT", s.ExpectName})
	}
	if s.ProxyCAFile != "" {
		files = append(files, [2]string{"PROXY_CA", s.ProxyCAFile})
	}
	return files
}

// checkContext confirms the client's build context carries every file the
// compile graph mounts by selector, plus patches/*.patch when PATCHES is on,
// before that graph is solved: one small local solve that transfers only
// those paths, then stat calls on the result. BuildKit's own failure for a
// missing selector path is an opaque "failed to calculate checksum" error
// that never names the Kernelfile line at fault.
func checkContext(ctx context.Context, c gwclient.Client, spec Spec) error {
	files := spec.contextFiles()
	follow := make([]string, 0, len(files)+1)
	for _, f := range files {
		follow = append(follow, f[1])
	}
	if spec.ApplyPatches {
		follow = append(follow, "patches")
	}
	probe := llb.Local("context", llb.FollowPaths(follow),
		llb.SharedKeyHint("kbuild-context"), llb.WithCustomName("[internal] check "+strings.Join(follow, ", ")))
	def, err := probe.Marshal(ctx, llb.LinuxAmd64)
	if err != nil {
		return err
	}
	res, err := c.Solve(ctx, gwclient.SolveRequest{Definition: def.ToPB()})
	if err != nil {
		return fmt.Errorf("check build context: %w", err)
	}
	ref, err := res.SingleRef()
	if err != nil {
		return err
	}
	for _, f := range files {
		if _, err := ref.StatFile(ctx, gwclient.StatRequest{Path: f[1]}); err != nil {
			return missingContextFile(f[0], f[1])
		}
	}
	if !spec.ApplyPatches {
		return nil
	}
	ents, err := ref.ReadDir(ctx, gwclient.ReadDirRequest{Path: "patches", IncludePattern: "*.patch"})
	if err != nil {
		return fmt.Errorf("PATCHES on, but the build context has no patches/ directory (%w)", err)
	}
	n := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Path, ".patch") {
			n++
		}
	}
	if n == 0 {
		return errors.New("PATCHES on, but the build context has no patches/ directory with *.patch files")
	}
	return nil
}

// missingContextFile names the Kernelfile key whose file is absent. CONFIG
// is the one people hit: the key is optional in the Kernelfile but the file
// it defaults to is not.
func missingContextFile(key, name string) error {
	hint := ""
	if key == "CONFIG" {
		hint = " (CONFIG names the kernel config; omitting it selects kernel.config)"
	}
	return fmt.Errorf("%s %s: not found in the build context%s", key, name, hint)
}

// checkContextDir is the client-side form of checkContext: the driver has
// the context on disk and can answer before dialing the daemon.
func checkContextDir(contextDir string, spec Spec) error {
	for _, f := range spec.contextFiles() {
		if _, err := os.Stat(filepath.Join(contextDir, f[1])); err != nil {
			return missingContextFile(f[0], f[1])
		}
	}
	if !spec.ApplyPatches {
		return nil
	}
	names, err := filepath.Glob(filepath.Join(contextDir, "patches", "*.patch"))
	if err != nil || len(names) == 0 {
		return fmt.Errorf("apply-patches: %s has no patches/*.patch files", contextDir)
	}
	return nil
}
