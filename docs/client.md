# Client and Go API

[Back to the README](../README.md)

The packaged frontend is the simplest way to use the project. `kbuildctl` and
the Go package expose the same graph for services that connect directly to a
buildkitd.

## Build the client

`kbuildctl` runs on the caller's machine. `kbuild-step` runs inside the
linux/amd64 build container and must therefore be a static linux/amd64 binary,
even when the client runs on another OS or architecture.

```bash
go build -trimpath -o kbuildctl ./cmd/kbuildctl
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -o kbuild-step ./cmd/kbuild-step
```

The helper bytes are part of the compile vertex's cache key. `-trimpath` keeps
them stable across checkout paths.

## Run a build

```bash
./kbuildctl \
  -addr tcp://127.0.0.1:1234 \
  -context /path/to/context \
  -sha256 a1415e257075c2fadf070f44bbb029469efbde5b6cf07d1433fe72207acff03c \
  -targets vmlinux,image,config \
  -out out
```

The context contains `kernel.config` and any optional patches or CA
certificate. Pass `-helper` if `kbuild-step` is not next to `kbuildctl`.

`-no-cache` requests a cold build. `-no-export` solves without copying
artifacts, which is useful when measuring solver and cache performance.

## Remote cache

`-export-cache` and `-import-cache` accept BuildKit's `buildctl` syntax and are
repeatable:

```bash
./kbuildctl \
  -export-cache type=registry,ref=registry.example/kbuild-cache,mode=max \
  -import-cache type=registry,ref=registry.example/kbuild-cache \
  ...
```

`-cache-s3 https://host/bucket` expands one S3-compatible endpoint into both
an import and export. `-seed-region` supplies its region; the default `auto`
works with R2 and MinIO, while AWS S3 needs its actual region.

## Object-tree seeds

BuildKit's cache exporters do not include persistent cache-mount contents. A
seed stores the compiled object tree separately:

```bash
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
./kbuildctl \
  -seed-url https://host/bucket \
  -seed-push \
  ...
```

Use `-seed-push` only for a trusted seeder. Untrusted builds should receive
read-only seed and cache credentials. Credentials travel as BuildKit secrets;
the destination and credentials do not alter the compile vertex cache key.

The seed upload uses checksummed multipart S3 requests and works with AWS S3,
R2, MinIO, and compatible stores. One seed is kept per kernel version,
architecture, toolchain, and patched state.

## Proxies and tracing

`-https-proxy`, `-http-proxy`, and `-no-proxy` configure build-step networking
without invalidating cached vertices. `-proxy-ca` names a CA certificate inside
the build context.

When `OTEL_EXPORTER_OTLP_ENDPOINT` is set, `kbuildctl` joins the solve to an
OpenTelemetry trace. BuildKit's per-vertex spans appear beneath the client
solve.

## Go API

`kbuild.Build` is the programmatic form of `kbuildctl`:

```go
res, err := kbuild.Build(ctx, spec, kbuild.BuildConfig{
    Addr:       "tcp://127.0.0.1:1234",
    ContextDir: ctxDir,
    HelperBin:  "/opt/kbuild-step",
    OutDir:     outDir,
    Progress:   logSink,
    OnStatus:   trackVertices,
})
```

`Progress` receives the live byte stream. `OnStatus` receives raw
`*client.SolveStatus` packets for structured vertex, log, and transfer events.
`BuildResult` reports total wall time, vertex timing and cache state, compiler
activity, and the collected log transcript.

`BuildConfig.TracerProvider` propagates an existing OpenTelemetry trace context
into buildkitd.

## Transport security

The address uses BuildKit's normal transport semantics. `tcp://` without TLS is
plaintext gRPC. Securing a remote daemon with TLS or mTLS is the daemon
operator's responsibility; see [Security model](../SECURITY.md).
