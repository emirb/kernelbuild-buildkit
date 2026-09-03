package kbuild

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/solver/pb"
	digest "github.com/opencontainers/go-digest"
)

// graph marshals a Spec and returns its decoded ops plus per-op metadata —
// the precise view of what KernelLLB actually emits (no substring grepping).
type graph struct {
	ops  []*pb.Op
	meta map[digest.Digest]llb.OpMetadata
	dgst []digest.Digest
}

func mustGraph(t *testing.T, s Spec) graph {
	t.Helper()
	st, err := KernelLLB(s)
	if err != nil {
		t.Fatal(err)
	}
	def, err := st.Marshal(context.Background(), llb.LinuxAmd64)
	if err != nil {
		t.Fatal(err)
	}
	g := graph{meta: map[digest.Digest]llb.OpMetadata{}}
	for _, dt := range def.Def {
		var op pb.Op
		if err := op.Unmarshal(dt); err != nil {
			t.Fatal(err)
		}
		d := digest.FromBytes(dt)
		g.ops = append(g.ops, &op)
		g.dgst = append(g.dgst, d)
		g.meta[d] = def.Metadata[d]
	}
	return g
}

func (g graph) execs() []*pb.ExecOp {
	var out []*pb.ExecOp
	for _, op := range g.ops {
		if e, ok := op.Op.(*pb.Op_Exec); ok {
			out = append(out, e.Exec)
		}
	}
	return out
}

func (g graph) sources() []string {
	var out []string
	for _, op := range g.ops {
		if s, ok := op.Op.(*pb.Op_Source); ok {
			out = append(out, s.Source.Identifier)
		}
	}
	return out
}

// compileExec finds the exec running the kbuild-step helper.
func (g graph) compileExec(t *testing.T) *pb.ExecOp {
	t.Helper()
	for _, e := range g.execs() {
		if len(e.Meta.Args) > 0 && e.Meta.Args[0] == HelperPath {
			return e
		}
	}
	t.Fatal("no exec runs the kbuild-step helper")
	return nil
}

func baseSpec() Spec {
	s := DefaultSpec()
	s.SourceSHA256 = strings.Repeat("ab", 32)
	return s
}

func TestGraphDefaultShape(t *testing.T) {
	g := mustGraph(t, baseSpec())

	// Default toolchain mode: exactly two execs — apt provisioning + compile.
	execs := g.execs()
	if len(execs) != 2 {
		t.Fatalf("got %d execs, want 2 (toolchain + compile)", len(execs))
	}

	ce := g.compileExec(t)
	// The compile exec must be argv-only (no shell) with the contract mounts.
	// (No /src mount in URL mode — kbuild-step fetches in-step on the cold
	// path so a seeded worker never downloads the tarball.)
	mounts := map[string]*pb.Mount{}
	for _, m := range ce.Mounts {
		mounts[m.Dest] = m
	}
	for _, dest := range []string{"/", "/helper", "/kernel.config", "/build", "/out"} {
		if mounts[dest] == nil {
			t.Fatalf("compile exec is missing mount %s", dest)
		}
	}
	if mounts["/src"] != nil {
		t.Error("URL mode must not materialize a /src mount")
	}
	if m := mounts["/build"]; m.MountType != pb.MountType_CACHE || m.CacheOpt.Sharing != pb.CacheSharingOpt_LOCKED {
		t.Errorf("/build must be a LOCKED cache mount, got %+v", mounts["/build"])
	}
	for _, ro := range []string{"/helper", "/kernel.config"} {
		if !mounts[ro].Readonly {
			t.Errorf("%s must be read-only", ro)
		}
	}
	// Secrets declared unconditionally so op shape (= cache key) never varies
	// with seeding.
	secrets := 0
	for _, m := range ce.Mounts {
		if m.MountType == pb.MountType_SECRET {
			secrets++
		}
	}
	if secrets != 3 {
		t.Errorf("got %d secret mounts, want 3 (seed_access_key, seed_secret_key, seed_cfg)", secrets)
	}
	// Source identity rides in env (in the cache key); there is no HTTP
	// source vertex — the fetch happens in-step, sha256-verified.
	env := map[string]string{}
	for _, e := range ce.Meta.Env {
		if k, v, ok := strings.Cut(e, "="); ok {
			env[k] = v
		}
	}
	if env["SRC_ID"] != baseSpec().SourceSHA256 || env["SRC_SHA256"] != baseSpec().SourceSHA256 {
		t.Errorf("source identity not in env: SRC_ID=%q SRC_SHA256=%q", env["SRC_ID"], env["SRC_SHA256"])
	}
	if !strings.HasPrefix(env["SRC_URL"], "https://") {
		t.Errorf("SRC_URL = %q", env["SRC_URL"])
	}
	for _, op := range g.ops {
		if s, ok := op.Op.(*pb.Op_Source); ok && strings.HasPrefix(s.Source.Identifier, "https://") {
			t.Error("unexpected HTTP source vertex — the in-step fetch design removed it")
		}
	}
}

func TestGraphSeedPushForcesExecution(t *testing.T) {
	s := baseSpec()
	s.SeedURL = "https://h/b"
	s.SeedPush = true
	action := func() string {
		for _, env := range mustGraph(t, s).compileExec(t).Meta.Env {
			if v, ok := strings.CutPrefix(env, "SEED_ACTION="); ok {
				return v
			}
		}
		return ""
	}
	a, b := action(), action()
	if a == "" || b == "" || a == b {
		t.Errorf("seed action keys = %q, %q; want distinct non-empty values", a, b)
	}

	// Neither a push nor a normal build may use IgnoreCache: on BuildKit
	// 0.32.2 it replaces the persistent object tree with an empty mount.
	for _, candidate := range []Spec{s, baseSpec()} {
		g := mustGraph(t, candidate)
		for _, d := range g.dgst {
			if g.meta[d].IgnoreCache {
				t.Error("IgnoreCache set on graph; object-tree reuse would be lost")
			}
		}
	}

	// The action key is push-only; a normal build keeps a stable op shape.
	for _, env := range mustGraph(t, baseSpec()).compileExec(t).Meta.Env {
		if strings.HasPrefix(env, "SEED_ACTION=") {
			t.Errorf("seed action key on normal build: %q", env)
		}
	}
}

// normalizedOps renders the graph with the two fields BuildKit's cache key
// itself ignores stripped out: `local.unique` (fresh session identity on every
// Marshal — pure noise) and `ProxyEnv` (serialized into the op but nil'd by
// CacheMap before hashing). Input digests are rewritten to op INDICES so a
// digest ripple from those fields doesn't cascade into every downstream op.
// What remains is exactly the cache-relevant content of each vertex.
func normalizedOps(t *testing.T, g graph) []string {
	t.Helper()
	idx := map[string]int{}
	for i, d := range g.dgst {
		idx[string(d)] = i
	}
	out := make([]string, 0, len(g.ops))
	for _, op := range g.ops {
		c := op.CloneVT()
		for _, in := range c.Inputs {
			i, ok := idx[string(in.Digest)]
			if !ok {
				t.Fatalf("input digest not an op in this graph")
			}
			in.Digest = fmt.Sprintf("op#%d", i)
		}
		// Source.Attrs is a proto MAP — MarshalVT serializes its keys in
		// random order, so canonicalize it out-of-band (sorted) and marshal
		// the rest of the op, which is map-free, deterministically.
		var attrs strings.Builder
		if s, ok := c.Op.(*pb.Op_Source); ok {
			delete(s.Source.Attrs, "local.unique")
			keys := make([]string, 0, len(s.Source.Attrs))
			for k := range s.Source.Attrs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				attrs.WriteByte('|')
				attrs.WriteString(k)
				attrs.WriteByte('=')
				attrs.WriteString(s.Source.Attrs[k])
			}
			s.Source.Attrs = nil
		}
		if e, ok := c.Op.(*pb.Op_Exec); ok {
			e.Exec.Meta.ProxyEnv = nil
		}
		b, err := c.MarshalVT()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b)+attrs.String())
	}
	return out
}

func TestGraphCacheKeyHygiene(t *testing.T) {
	// The proxy and the seed DESTINATION must not change the cache-relevant
	// content of any vertex — that is the cache-neutrality contract. (Raw op
	// digests are NOT comparable across Marshals: local sources carry a fresh
	// `local.unique` every time; see normalizedOps.)
	plain := normalizedOps(t, mustGraph(t, baseSpec()))

	proxied := baseSpec()
	proxied.HTTPSProxy = "http://10.0.0.1:3128"
	gp := normalizedOps(t, mustGraph(t, proxied))
	if len(plain) != len(gp) {
		t.Fatalf("proxy changed op count: %d vs %d", len(plain), len(gp))
	}
	for i := range plain {
		if plain[i] != gp[i] {
			t.Errorf("op %d cache-relevant content changed when only the proxy changed", i)
		}
	}

	seeded := baseSpec()
	seeded.SeedURL = "https://h/b" // destination via secret; no SeedPush
	gs := normalizedOps(t, mustGraph(t, seeded))
	if len(plain) != len(gs) {
		t.Fatalf("seed destination changed op count")
	}
	for i := range plain {
		if plain[i] != gs[i] {
			t.Errorf("op %d cache-relevant content changed when only the seed destination changed", i)
		}
	}

	// Control: the normalization must not be so aggressive that REAL changes
	// disappear — a different config filename must still change the graph.
	other := baseSpec()
	other.ConfigName = "other.config"
	go2 := normalizedOps(t, mustGraph(t, other))
	same := len(go2) == len(plain)
	if same {
		for i := range plain {
			if plain[i] != go2[i] {
				same = false
				break
			}
		}
	}
	if same {
		t.Error("normalization erased a real input change (config filename)")
	}
}

func TestGraphIgnoreCache(t *testing.T) {
	// docker build --no-cache must reach the vertices; a frontend that drops
	// the opt hands back a cached result for a build asked to re-run.
	count := func(s Spec) int {
		g := mustGraph(t, s)
		n := 0
		for i, op := range g.ops {
			if _, ok := op.Op.(*pb.Op_Exec); ok && g.meta[g.dgst[i]].IgnoreCache {
				n++
			}
		}
		return n
	}
	if n := count(baseSpec()); n != 0 {
		t.Errorf("default spec marked %d execs ignore-cache, want 0", n)
	}
	s := baseSpec()
	s.IgnoreCache = true
	if n := count(s); n != 2 {
		t.Errorf("IgnoreCache marked %d execs, want both (toolchain + compile)", n)
	}
}

func TestGraphToolchainReady(t *testing.T) {
	s := baseSpec()
	s.Base = "docker.io/tuxmake/x86_64_gcc@sha256:" + strings.Repeat("cd", 32)
	s.ToolchainReady = true
	g := mustGraph(t, s)
	if execs := g.execs(); len(execs) != 1 {
		t.Fatalf("TOOLCHAIN ready: got %d execs, want 1 (compile only, no apt)", len(execs))
	}
	var found bool
	for _, id := range g.sources() {
		if strings.Contains(id, "tuxmake") {
			found = true
		}
	}
	if !found {
		t.Error("base image source not in graph")
	}
}

func TestGraphNetworkHost(t *testing.T) {
	s := baseSpec()
	s.NetworkHost = true
	for _, e := range mustGraph(t, s).execs() {
		if e.Network != pb.NetMode_HOST {
			t.Errorf("NetworkHost: exec %v not host-net", e.Meta.Args[:1])
		}
	}
	for _, e := range mustGraph(t, baseSpec()).execs() {
		if e.Network == pb.NetMode_HOST {
			t.Error("default graph unexpectedly uses host networking")
		}
	}
}

func TestGraphVariants(t *testing.T) {
	t.Run("local-source", func(t *testing.T) {
		s := baseSpec()
		s.SourceSHA256 = ""
		s.SourceLocalName = "linux-6.18.20.tar.gz"
		g := mustGraph(t, s)
		var local bool
		for _, id := range g.sources() {
			if id == "local://src" {
				local = true
			}
		}
		if !local {
			t.Errorf("no local://src source; sources = %v", g.sources())
		}
		var srcMount bool
		for _, m := range g.compileExec(t).Mounts {
			if m.Dest == "/src" && m.Readonly {
				srcMount = true
			}
		}
		if !srcMount {
			t.Error("local mode must mount /src read-only")
		}
	})
	t.Run("patches", func(t *testing.T) {
		s := baseSpec()
		s.ApplyPatches = true
		ce := mustGraph(t, s).compileExec(t)
		var env string
		for _, e := range ce.Meta.Env {
			if strings.HasPrefix(e, "APPLY_PATCHES=") {
				env = e
			}
		}
		if env != "APPLY_PATCHES=1" {
			t.Errorf("APPLY_PATCHES env = %q", env)
		}
		// The env var alone is not the feature: without the mount every
		// patched build fails at runtime (review: delete the mount and the
		// suite stayed green).
		var patchMount bool
		for _, m := range ce.Mounts {
			if m.Dest == "/patches" && m.Readonly {
				patchMount = true
			}
		}
		if !patchMount {
			t.Error("ApplyPatches set but no read-only /patches mount")
		}
		for _, m := range mustGraph(t, baseSpec()).compileExec(t).Mounts {
			if m.Dest == "/patches" {
				t.Error("/patches mounted without ApplyPatches")
			}
		}
	})
	t.Run("targets-env", func(t *testing.T) {
		s := baseSpec()
		s.Targets = []string{"image", "modules"}
		ce := mustGraph(t, s).compileExec(t)
		var got string
		for _, e := range ce.Meta.Env {
			if strings.HasPrefix(e, "TARGETS=") {
				got = e
			}
		}
		if got != "TARGETS=image,modules" {
			t.Errorf("TARGETS env = %q", got)
		}
	})
	t.Run("proxy-ca", func(t *testing.T) {
		s := baseSpec()
		s.ProxyCAFile = "ca-bundle.crt"
		g := mustGraph(t, s)
		var caMount bool
		for _, e := range g.execs() {
			for _, m := range e.Mounts {
				if m.Dest == "/certs" && m.Readonly {
					caMount = true
				}
			}
		}
		if !caMount {
			t.Error("ProxyCAFile set but no read-only /certs mount")
		}
	})
	t.Run("helper-image", func(t *testing.T) {
		s := baseSpec()
		s.HelperRef = "ghcr.io/example/kbuild-frontend:v1"
		g := mustGraph(t, s)
		var img bool
		for _, id := range g.sources() {
			if strings.Contains(id, "ghcr.io/example/kbuild-frontend") {
				img = true
			}
		}
		if !img {
			t.Errorf("HelperRef image not a source; sources = %v", g.sources())
		}
	})
	t.Run("xz-source", func(t *testing.T) {
		s := baseSpec()
		s.SourceURL = "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.18.20.tar.xz"
		ce := mustGraph(t, s).compileExec(t)
		var url string
		for _, e := range ce.Meta.Env {
			if after, ok := strings.CutPrefix(e, "SRC_URL="); ok {
				url = after
			}
		}
		if !strings.HasSuffix(url, ".tar.xz") {
			t.Errorf("SRC_URL = %q, want the .tar.xz URL", url)
		}
	})
}

func TestPatchedTreeSplit(t *testing.T) {
	// Patched and unpatched builds of the same version must not share a
	// tree: alternating between them used to discard the mount each way.
	plain := baseSpec()
	patched := baseSpec()
	patched.ApplyPatches = true

	mountID := func(s Spec) string {
		t.Helper()
		for _, m := range mustGraph(t, s).compileExec(t).Mounts {
			if m.Dest == "/build" {
				return m.CacheOpt.ID
			}
		}
		t.Fatal("no /build cache mount")
		return ""
	}
	a, b := mountID(plain), mountID(patched)
	if a == b {
		t.Fatalf("patched and unpatched share cache mount %q", a)
	}
	if !strings.Contains(b, "-patched") {
		t.Errorf("patched mount ID %q lacks the -patched suffix", b)
	}

	// The seed splits the same way, or a patched push would clobber the
	// unpatched tree's seed (stamp self-heal would catch it, expensively).
	plain.SeedURL, patched.SeedURL = "https://h/b", "https://h/b"
	if string(plain.SeedCfg()) == string(patched.SeedCfg()) {
		t.Error("patched and unpatched share a seed key")
	}
}

func TestToolchainIdentityInMountAndSeed(t *testing.T) {
	// Anything that changes which compiler runs must change the mount id and
	// the seed key, or a build reuses another compiler's object tree (the
	// stamp only distinguishes source+patches). Found by the red-team pass.
	mountID := func(s Spec) string {
		for _, m := range mustGraph(t, s).compileExec(t).Mounts {
			if m.Dest == "/build" {
				return m.CacheOpt.ID
			}
		}
		t.Fatal("no /build mount")
		return ""
	}
	base := baseSpec()
	cross := baseSpec()
	cross.CrossCompile = "powerpc64-linux-gnu-"
	if mountID(base) == mountID(cross) {
		t.Error("differing CROSS_COMPILE shares the cache mount")
	}
	base.SeedURL, cross.SeedURL = "https://h/b", "https://h/b"
	if string(base.SeedCfg()) == string(cross.SeedCfg()) {
		t.Error("differing CROSS_COMPILE shares the seed key")
	}
	// A different base image must also split (already true; guard it).
	tux := baseSpec()
	tux.Base = "docker.io/tuxmake/x86_64_gcc@sha256:" + strings.Repeat("cd", 32)
	if mountID(baseSpec()) == mountID(tux) {
		t.Error("differing base image shares the cache mount")
	}
	// But an explicit x86_64 that equals the default must NOT split (correct
	// sharing, not over-keying).
	exp := baseSpec()
	exp.Arch = "x86_64"
	if mountID(baseSpec()) != mountID(exp) {
		t.Error("explicit x86_64 over-keyed against the default")
	}
}

func TestGraphExpectAndBaseMake(t *testing.T) {
	// Expect file: mounted read-only at /kernel.expect only when named, so the
	// expectation content keys the compile vertex exactly like the config.
	s := baseSpec()
	s.ExpectName = "kernel.expect"
	ce := mustGraph(t, s).compileExec(t)
	var expectMount bool
	for _, m := range ce.Mounts {
		if m.Dest == "/kernel.expect" && m.Readonly {
			expectMount = true
		}
	}
	if !expectMount {
		t.Error("ExpectName set but no read-only /kernel.expect mount")
	}
	for _, m := range mustGraph(t, baseSpec()).compileExec(t).Mounts {
		if m.Dest == "/kernel.expect" {
			t.Error("/kernel.expect mounted without ExpectName")
		}
	}

	// BASE_MAKE rides the env (always present — stable op shape) and is
	// therefore IN the cache key: same fragment over a different make base
	// must be a different vertex.
	bm := baseSpec()
	bm.BaseMake = "x86_64_defconfig kvm_guest.config"
	var got string
	for _, e := range mustGraph(t, bm).compileExec(t).Meta.Env {
		if strings.HasPrefix(e, "BASE_MAKE=") {
			got = e
		}
	}
	if got != "BASE_MAKE=x86_64_defconfig kvm_guest.config" {
		t.Errorf("BASE_MAKE env = %q", got)
	}
	plain := normalizedOps(t, mustGraph(t, baseSpec()))
	based := normalizedOps(t, mustGraph(t, bm))
	same := len(plain) == len(based)
	if same {
		for i := range plain {
			if plain[i] != based[i] {
				same = false
			}
		}
	}
	if same {
		t.Error("BaseMake did not change any cache-relevant op content (would alias distinct configs)")
	}
}
