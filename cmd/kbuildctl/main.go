// kbuildctl drives KernelLLB directly against a buildkitd and exports the
// requested artifacts to a local directory. It is the "run it here" form of
// the build.kernel.v0 frontend — a thin flag wrapper over kbuild.Build, which
// the Go integration suite drives directly.
//
// Remote cache uses BuildKit's standard surface: -export-cache/-import-cache
// take buildctl syntax (type=registry,ref=... | type=s3,... | type=gha | ...);
// -cache-s3 <https://host/bucket> expands to the full s3 entry. Credentials come
// from AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY. SEED credentials travel as
// BuildKit secrets — out of cache keys and logs. s3 CACHE-BACKEND credentials
// necessarily ride as solve-request attrs (BuildKit's s3 cache API; the
// daemon has no env of its own) — they stay out of vertex cache keys and
// build logs, but are visible to the daemon in the request.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	kbuild "github.com/emirb/kernelbuild-buildkit"
	"github.com/moby/buildkit/util/tracing/detect"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint(*m) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	addr := flag.String("addr", "tcp://127.0.0.1:1234", "buildkitd address")
	ctxDir := flag.String("context", ".", "build context (holds kernel.config, patches/)")
	proxyCA := flag.String("proxy-ca", "", "CA cert filename INSIDE the context to trust (MITM-proxy sandbox)")
	helperBin := flag.String("helper", "", "path to the kbuild-step binary (default: next to this executable)")
	srcDir := flag.String("src", "", "dir containing the kernel tarball (local source mode)")
	srcName := flag.String("src-name", "", "tarball filename inside --src")
	out := flag.String("out", "out", "artifact output directory")
	noExport := flag.Bool("no-export", false, "solve without exporting artifacts (bench: isolates solve+cache from the copy)")
	proxy := flag.String("https-proxy", "", "https_proxy for network steps (cache-neutral)")
	httpProxy := flag.String("http-proxy", "", "http_proxy for network steps; apt's stock mirrors are http (cache-neutral)")
	noProxy := flag.String("no-proxy", "", "extra no_proxy entries, comma-separated, e.g. an internal mirror (cache-neutral)")
	sha := flag.String("sha256", "", "sha256 of the source tarball")
	apply := flag.Bool("apply-patches", false, "apply patches/*.patch from the context")
	targets := flag.String("targets", "", "comma-separated artifacts: vmlinux,image,modules,config,kconfigs (default: arch default)")
	version := flag.String("kernel-version", "", "override kernel version")
	config := flag.String("config", "", "override config filename in the context")
	expect := flag.String("expect", "", "expectation filename in the context, validated after olddefconfig (y/n/= lines)")
	baseMake := flag.String("base-make", "", "make config targets run before the context config is applied as a fragment (e.g. \"x86_64_defconfig kvm_guest.config\")")
	baseImg := flag.String("base", "", "override base image (e.g. docker.io/tuxmake/x86_64_gcc)")
	tcReady := flag.Bool("toolchain-ready", false, "base image already has the kernel toolchain: skip the apt vertex")
	arch := flag.String("arch", "", "target architecture (x86_64 default, arm64 — needs -toolchain-ready cross image)")
	cross := flag.String("cross-compile", "", "cross prefix override (derived from -arch when empty)")
	seedURL := flag.String("seed-url", "", "S3-compatible bucket base URL for the object-tree seed")
	seedPrefix := flag.String("seed-prefix", "kbuild-seed", "key prefix for the seed object")
	seedRegion := flag.String("seed-region", "", "bucket region for the seed store and -cache-s3 (default auto; real AWS S3 needs the actual region)")
	seedPush := flag.Bool("seed-push", false, "push the object tree after a successful build")
	cacheS3 := flag.String("cache-s3", "", "S3-compatible bucket URL; expands to a full s3 cache entry for export+import")
	var exportCaches, importCaches multiFlag
	flag.Var(&exportCaches, "export-cache", "BuildKit cache export, buildctl syntax (repeatable)")
	flag.Var(&importCaches, "import-cache", "BuildKit cache import, buildctl syntax (repeatable)")
	noCache := flag.Bool("no-cache", false, "force the build vertices to execute; BuildKit then starts the object tree empty, so this is a full cold rebuild")
	timing := flag.Bool("timing", true, "print per-vertex timing summary")
	flag.Parse()

	spec := kbuild.DefaultSpec()
	if *version != "" {
		spec.KernelVersion = *version
		spec.SourceURL = kbuild.SourceURLFor(*version)
		spec.SourceSHA256 = "" // default pin is for the default tarball only
	}
	if *config != "" {
		spec.ConfigName = *config
	}
	spec.ExpectName = *expect
	spec.BaseMake = *baseMake
	if *baseImg != "" {
		spec.Base = *baseImg
	}
	spec.ToolchainReady = *tcReady
	spec.Arch = *arch
	spec.CrossCompile = *cross
	spec.HTTPSProxy = *proxy
	spec.HTTPProxy = *httpProxy
	spec.NoProxy = *noProxy
	if *sha != "" {
		spec.SourceSHA256 = *sha
	}
	spec.ApplyPatches = *apply
	if *targets != "" {
		spec.Targets = strings.Split(*targets, ",")
	}
	spec.SeedURL = *seedURL
	spec.SeedPrefix = *seedPrefix
	spec.SeedRegion = *seedRegion
	spec.SeedPush = *seedPush
	spec.ProxyCAFile = *proxyCA
	spec.IgnoreCache = *noCache
	if *srcName != "" {
		spec.SourceLocalName = *srcName
	}
	if spec.SeedPush && spec.SeedURL == "" {
		fmt.Fprintln(os.Stderr, "error: -seed-push requires -seed-url")
		os.Exit(1)
	}

	cfg := kbuild.BuildConfig{
		Addr:         *addr,
		ContextDir:   *ctxDir,
		HelperBin:    *helperBin,
		SrcDir:       *srcDir,
		OutDir:       *out,
		CacheExports: exportCaches,
		CacheImports: importCaches,
		AccessKey:    os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey:    os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	if *cacheS3 != "" {
		entry, err := kbuild.S3CacheURL(*cacheS3, *seedRegion)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		cfg.CacheExports = append(cfg.CacheExports, entry)
		cfg.CacheImports = append(cfg.CacheImports, entry)
	}

	// Stream the build log live: a buffered end-of-build dump makes a long
	// solve (a kernel compile) look hung.
	cfg.Progress = os.Stderr
	if *noExport {
		cfg.OutDir = ""
	}
	// OTEL: with OTEL_EXPORTER_OTLP_ENDPOINT set, the solve joins a trace and
	// buildkitd's per-vertex spans land in your collector (Jaeger etc.);
	// unset, detect returns a no-op exporter and this costs nothing.
	if exp, err := detect.NewSpanExporter(context.Background()); err == nil && !detect.IsNoneSpanExporter(exp) {
		tp := sdktrace.NewTracerProvider(sdktrace.WithResource(detect.Resource()), sdktrace.WithBatcher(exp))
		defer func() { _ = tp.Shutdown(context.Background()) }()
		cfg.TracerProvider = tp
	}
	res, err := kbuild.Build(context.Background(), spec, cfg)
	if res != nil {
		if *timing {
			fmt.Println("\n--- vertex timing ---")
			for _, v := range res.Vertices {
				switch {
				case v.Cached:
					fmt.Printf("%9s  %s\n", "CACHED", v.Name)
				case v.Duration > 0:
					fmt.Printf("%8.1fs  %s\n", v.Duration.Seconds(), v.Name)
				default:
					fmt.Printf("%9s  %s\n", "?", v.Name)
				}
			}
			fmt.Printf("%8.1fs  TOTAL wall\n", res.Wall.Seconds())
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if *noExport {
		fmt.Println("OK (no export)")
	} else {
		fmt.Printf("OK -> %s (targets: %s)\n", *out, *targets)
	}
}
