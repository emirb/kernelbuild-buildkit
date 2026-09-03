package kbuild

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/moby/buildkit/client"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/entitlements"
	"github.com/tonistiigi/fsutil"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// BuildConfig is everything Build needs beyond the Spec: where the daemon is,
// which local directories feed the graph, where the artifacts land, and the
// remote-cache wiring. It is the programmatic form of kbuildctl's flags, and
// what the integration suite drives directly.
type BuildConfig struct {
	Addr       string // buildkitd address
	ContextDir string // kernel.config (+ patches/, + CA file)
	HelperBin  string // path to the kbuild-step binary ("": next to the executable)
	SrcDir     string // local-source mode: dir with the tarball
	OutDir     string // artifact destination; "" solves WITHOUT exporting (bench: isolates solve+cache from the artifact copy)
	// Remote cache, buildctl syntax ("type=registry,ref=..." / "type=s3,...").
	CacheExports []string
	CacheImports []string
	// AWS-style credentials for the seed secrets and s3 cache entries.
	AccessKey, SecretKey string

	// Progress, when set, receives the LIVE build log stream (vertex output,
	// ">> ..." notes, "KBF-PHASE <name> <ms>ms" markers) as it arrives —
	// what a service streams to its user mid-build. The full transcript is
	// still collected into BuildResult.Logs either way. Writes happen from
	// the solve's status goroutine; the writer must be safe for that.
	Progress io.Writer
	// OnStatus, when set, is called with every raw BuildKit status packet
	// (vertex state changes, log chunks, transfer progress) — the structured
	// feed for consumers that want more than a byte stream. Same goroutine
	// caveat as Progress.
	OnStatus func(*client.SolveStatus)
	// TracerProvider, when set, propagates OpenTelemetry trace context into
	// buildkitd — the solve joins the caller's trace and the daemon's
	// per-vertex spans (cache probe, exec, export) hang off it.
	TracerProvider trace.TracerProvider
}

// VertexTiming is one solved vertex's observed schedule.
type VertexTiming struct {
	Name     string
	Cached   bool
	Duration time.Duration
}

// BuildResult reports what the solve did, precisely enough for tests to
// assert on: wall time, per-vertex timings, and the kbuild activity counted
// from the captured build logs (no log files, no grep).
type BuildResult struct {
	Wall     time.Duration
	Vertices []VertexTiming
	CC       int // "  CC  ..." lines observed — objects compiled
	Logs     string
}

var ccLineRe = regexp.MustCompile(`(?m)^\s+CC\s+`)

// Build solves the Spec's graph against a buildkitd and exports vmlinux (and
// friends) to cfg.OutDir.
func Build(ctx context.Context, spec Spec, cfg BuildConfig) (*BuildResult, error) {
	if spec.SeedPush && spec.SeedURL == "" {
		// Validate cannot catch this (push is an action, not a value): the graph
		// would force an execution that cannot publish anything.
		return nil, errors.New("SeedPush requires SeedURL")
	}
	// Spec-level errors surface before any daemon dial (GatewaySolve validates
	// again; it is cheap and idempotent).
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	// The context is on disk here: answer "no kernel.config" or "no patches/"
	// before the dial, with a message naming the file, instead of BuildKit's
	// checksum error on the mount.
	if err := checkContextDir(cfg.ContextDir, spec); err != nil {
		return nil, err
	}
	dirs := map[string]string{"context": cfg.ContextDir}
	if spec.HelperRef == "" {
		// Image mode takes kbuild-step from HelperRef; staging a local
		// helper there would demand a binary the build never touches.
		helperDir, err := stageHelper(cfg.HelperBin)
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(helperDir) }()
		dirs["helper"] = helperDir
	}
	copts := []client.ClientOpt{}
	if cfg.TracerProvider != nil {
		copts = append(copts, client.WithTracerProvider(cfg.TracerProvider))
		// One root span per build: the session and control RPCs (and the
		// daemon's spans, which join through gRPC propagation) nest under it
		// instead of appearing as siblings in a multi-build service.
		var span trace.Span
		ctx, span = cfg.TracerProvider.Tracer("kernelbuild-buildkit").Start(ctx, "kbuild.solve",
			trace.WithAttributes(
				attribute.String("kernel.version", spec.KernelVersion),
				attribute.String("kernel.arch", spec.archOrDefault()),
				attribute.String("kernel.targets", strings.Join(spec.targetsOrDefault(), ",")),
			))
		defer span.End()
	}
	c, err := client.New(ctx, cfg.Addr, copts...)
	if err != nil {
		return nil, fmt.Errorf("connect buildkitd: %w", err)
	}
	defer func() { _ = c.Close() }()

	if cfg.SrcDir != "" {
		dirs["src"] = cfg.SrcDir
	}
	mounts := map[string]fsutil.FS{}
	for name, dir := range dirs {
		fs, err := fsutil.NewFS(dir)
		if err != nil {
			return nil, fmt.Errorf("local mount %q (%s): %w", name, dir, err)
		}
		mounts[name] = fs
	}

	var attachables []session.Attachable
	if spec.SeedURL != "" {
		if cfg.AccessKey == "" || cfg.SecretKey == "" {
			return nil, errors.New("seed needs AccessKey/SecretKey")
		}
		attachables = append(attachables, secretsprovider.FromMap(map[string][]byte{
			"seed_access_key": []byte(cfg.AccessKey),
			"seed_secret_key": []byte(cfg.SecretKey),
			"seed_cfg":        spec.SeedCfg(),
		}))
	}

	solveOpt := client.SolveOpt{
		LocalMounts:   mounts,
		Session:       attachables,
		FrontendAttrs: map[string]string{},
	}
	// BuildKit 0.32.2 merges cache attributes into FrontendAttrs without
	// allocating the destination map first. Keep it non-nil so any remote
	// cache import/export cannot panic in client.solve.
	if spec.NetworkHost {
		// The graph marshals NetModeHost ops; without the entitlement the
		// daemon rejects the solve outright.
		solveOpt.AllowedEntitlements = append(solveOpt.AllowedEntitlements, string(entitlements.EntitlementNetworkHost))
	}
	if cfg.OutDir != "" {
		solveOpt.Exports = []client.ExportEntry{{Type: client.ExporterLocal, OutputDir: cfg.OutDir}}
	}
	for _, e := range cfg.CacheExports {
		entry, err := ParseCacheEntry(e, cfg.AccessKey, cfg.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("export-cache %q: %w", e, err)
		}
		solveOpt.CacheExports = append(solveOpt.CacheExports, entry)
	}
	var gatewayCacheImports []gwclient.CacheOptionsEntry
	for _, e := range cfg.CacheImports {
		entry, err := ParseCacheEntry(e, cfg.AccessKey, cfg.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("import-cache %q: %w", e, err)
		}
		solveOpt.CacheImports = append(solveOpt.CacheImports, entry)
		// client.Build wraps our callback in gateway.v0. The outer solve needs
		// the import for the exported result, and the inner graph solve needs it
		// explicitly as well; otherwise a fresh daemon sees the remote manifest
		// but still executes every vertex.
		gatewayCacheImports = append(gatewayCacheImports, gwclient.CacheOptionsEntry{
			Type: entry.Type, Attrs: entry.Attrs,
		})
	}

	res := &BuildResult{}
	type vtx struct {
		name              string
		started, finished *time.Time
		cached            bool
	}
	seen := map[string]*vtx{}
	var order []string
	var logs strings.Builder

	ch := make(chan *client.SolveStatus)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for st := range ch {
			if cfg.OnStatus != nil {
				cfg.OnStatus(st)
			}
			for _, v := range st.Vertexes {
				d := v.Digest.String()
				e, ok := seen[d]
				if !ok {
					e = &vtx{name: v.Name}
					seen[d] = e
					order = append(order, d)
				}
				if v.Started != nil {
					e.started = v.Started
				}
				if v.Completed != nil {
					e.finished = v.Completed
				}
				e.cached = e.cached || v.Cached
			}
			for _, l := range st.Logs {
				logs.Write(l.Data)
				if cfg.Progress != nil {
					_, _ = cfg.Progress.Write(l.Data)
				}
			}
		}
	}()

	t0 := time.Now()
	// client.Build, not client.Solve: the graph is generated INSIDE a gateway
	// session so GatewaySolve can pin the base image by digest through the
	// daemon's resolver first — the same path the frontend takes. Solving a
	// pre-marshaled graph keyed the object tree by whatever tag -base named.
	_, solveErr := c.Build(ctx, solveOpt, "kbuild", func(ctx context.Context, gc gwclient.Client) (*gwclient.Result, error) {
		return GatewaySolve(ctx, gc, spec, GatewayOpts{CacheImports: gatewayCacheImports})
	}, ch)
	<-done
	res.Wall = time.Since(t0)
	res.Logs = logs.String()
	res.CC = len(ccLineRe.FindAllString(res.Logs, -1))
	for _, d := range order {
		v := seen[d]
		vt := VertexTiming{Name: VertexLabel(v.name), Cached: v.cached}
		if v.started != nil && v.finished != nil {
			vt.Duration = v.finished.Sub(*v.started)
		}
		res.Vertices = append(res.Vertices, vt)
	}
	if solveErr != nil {
		return res, fmt.Errorf("solve: %w", solveErr)
	}
	return res, nil
}

// Prune wipes ALL local buildkitd state — vertex cache and cache mounts.
// The integration suite uses it to simulate a fresh worker.
func Prune(ctx context.Context, addr string) error {
	c, err := client.New(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	ch := make(chan client.UsageInfo)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch { //nolint:revive // drains the usage stream buildkit writes until Prune returns
		}
	}()
	err = c.Prune(ctx, ch, client.PruneAll)
	// buildkit's Prune does not close the channel it writes to; close it
	// ourselves or the drain goroutine leaks per call.
	close(ch)
	<-done
	return err
}

// ParseCacheEntry parses buildctl's cache syntax ("type=registry,ref=...",
// "type=s3,bucket=...") into a CacheOptionsEntry — the standard BuildKit
// remote-cache surface, backend-agnostic. For type=s3 entries without
// explicit credentials, the given creds are injected (the daemon makes the
// S3 calls and has no env of its own).
func ParseCacheEntry(spec, ak, sk string) (client.CacheOptionsEntry, error) {
	entry := client.CacheOptionsEntry{Attrs: map[string]string{}}
	// CSV split, not strings.Split: an attr value can legitimately contain a
	// comma (endpoint_url, a quoted list), which a naive split mangles.
	r := csv.NewReader(strings.NewReader(spec))
	fields, err := r.Read()
	if err != nil {
		return entry, fmt.Errorf("cache entry %q: %w", spec, err)
	}
	for _, kv := range fields {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return entry, fmt.Errorf("want k=v, got %q", kv)
		}
		if k == "type" {
			entry.Type = v
		} else {
			entry.Attrs[k] = v
		}
	}
	if entry.Type == "" {
		return entry, errors.New("missing type=")
	}
	if entry.Type == "s3" && entry.Attrs["access_key_id"] == "" && ak != "" {
		entry.Attrs["access_key_id"] = ak
		entry.Attrs["secret_access_key"] = sk
	}
	return entry, nil
}

var regionRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// S3CacheURL expands an https://host/bucket URL into a full s3 cache
// entry spec in buildctl syntax. region is the bucket's region; empty means
// "auto", which region-less stores (R2, MinIO) accept and real AWS S3 rejects
// for SigV4 — so it is a parameter, not a constant.
func S3CacheURL(bucketURL, region string) (string, error) {
	rest, ok := strings.CutPrefix(strings.TrimSuffix(bucketURL, "/"), "https://")
	host, bucket, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || bucket == "" || strings.Contains(bucket, "/") {
		return "", fmt.Errorf("cache-s3 %q: want https://host/bucket", bucketURL)
	}
	if region == "" {
		region = "auto"
	}
	if !regionRe.MatchString(region) {
		return "", fmt.Errorf("cache-s3 region %q: bad region", region)
	}
	return "type=s3,bucket=" + bucket + ",region=" + region + ",prefix=kbuild-vertex/,endpoint_url=https://" + host +
		",use_path_style=true,mode=max", nil
}

// VertexLabel compresses a vertex name (which for exec ops is the whole
// embedded script) to a single readable line.
func VertexLabel(name string) string {
	if i := strings.IndexByte(name, '\n'); i > 0 {
		name = name[:i] + " ..."
	}
	if len(name) > 100 {
		name = name[:100] + "..."
	}
	return name
}

// stageHelper puts the kbuild-step binary alone in a temp dir so the "helper"
// local mount syncs exactly one file. Default: kbuild-step next to argv[0].
func stageHelper(path string) (string, error) {
	if path == "" {
		self, err := os.Executable()
		if err != nil {
			return "", err
		}
		path = filepath.Join(filepath.Dir(self), "kbuild-step")
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("kbuild-step helper not found (build it with `GOOS=linux GOARCH=amd64 go build ./cmd/kbuild-step`): %w", err)
	}
	// The helper executes inside the linux/amd64 build container. A host-arch
	// binary (a Mach-O from a Mac, an arm64 ELF) would fail deep inside the
	// build with an unhelpful exec error — reject it here with a real message.
	if ef, err := elf.NewFile(bytes.NewReader(src)); err != nil || ef.Machine != elf.EM_X86_64 {
		return "", fmt.Errorf("kbuild-step at %s is not a linux/amd64 ELF binary; rebuild it with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath ./cmd/kbuild-step`", path)
	}
	dir, err := os.MkdirTemp("", "kbuild-helper-")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "kbuild-step"), src, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
