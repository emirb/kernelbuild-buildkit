# Development, testing, and releases

[Back to the README](../README.md)

## Repository layout

| Path | Purpose |
| --- | --- |
| `spec.go` | build specification and validation |
| `llb.go` | LLB graph generation |
| `gateway.go` | image resolution and shared gateway solve path |
| `step.go` | Kernelfile parser and shared helpers |
| `client.go` | programmatic BuildKit client |
| `cmd/kbuild-frontend/` | packaged gateway entrypoint |
| `cmd/kbuild-step/` | in-build step runner |
| `cmd/kbuildctl/` | direct buildkitd client |
| `cmd/llbdump/` | marshaled graph inspection |
| `integration_test.go` | live-buildkitd suite |
| `e2e_test.go` | Docker `#syntax=` delegation suite |
| `boot_test.go` | Firecracker/QEMU boot smoke |
| `Dockerfile.frontend` | multi-architecture frontend image |

## Local checks

The module targets Go 1.26 and is also tested with Go 1.27.

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
go build ./...
go mod tidy -diff
golangci-lint run
govulncheck ./...
actionlint
npx --yes markdownlint-cli2@0.20.0 README.md SECURITY.md 'docs/*.md'
```

The golangci-lint binary must be built with a Go version at least as new as the
toolchain running the checks. CI pins a compatible release.

## Fuzz smoke

The normal test suite runs the checked-in seed corpora. CI also fuzzes each
target for ten seconds:

```bash
for f in FuzzValidate FuzzParseKernelfile FuzzSourceExt FuzzKbuildTimestamp; do
  go test -run '^$' -fuzz "^${f}$" -fuzztime 10s .
done

for f in FuzzUntar FuzzStripComponents; do
  go test -run '^$' -fuzz "^${f}$" -fuzztime 10s ./cmd/kbuild-step
done
```

## Integration suite

Start a live buildkitd:

```bash
docker run -d --name bkd --network=host --privileged \
  moby/buildkit:v0.32.2 --addr tcp://127.0.0.1:1234
```

Build the helper for the architecture of the compile container, not the host:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -o /tmp/kbuild-step ./cmd/kbuild-step
```

Prepare a context containing `kernel.config`, then run:

```bash
KBF_CONTEXT=/path/to/context \
KBF_HELPER=/tmp/kbuild-step \
KBF_SHA=a1415e257075c2fadf070f44bbb029469efbde5b6cf07d1433fe72207acff03c \
go test -tags integration -v -timeout 90m -run TestIntegration .
```

The ordered suite covers input rejection, checksum enforcement, a cold build,
an identical cache hit, a config-only incremental, patch application, patch
content invalidation, and concurrent builds on the locked tree.

Optional environment:

| Variable | Purpose |
| --- | --- |
| `KBF_ADDR` | buildkitd address; default `tcp://127.0.0.1:1234` |
| `KBF_GOLDEN` | expected `vmlinux` for byte comparison |
| `KBF_PROXY` | HTTPS proxy URL |
| `KBF_PROXY_CA` | CA filename inside the context |
| `KBF_SEED_URL` | S3-compatible bucket used for seed and vertex-cache tests |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | seed and S3 cache credentials |
| `KBF_QUICK=1` | skip patch and concurrency legs |

With `KBF_SEED_URL`, the suite publishes a warm tree, prunes the daemon,
hydrates a fresh mount, and verifies a separate S3 vertex-cache hit.

Docker Desktop does not expose a container's `--network=host` listener to the
macOS host. Use an explicit `-p 127.0.0.1:1234:1234` mapping there and start
buildkitd with `--addr tcp://0.0.0.0:1234`.

## Docker frontend end to end

The e2e suite builds the frontend image, invokes it through a real `#syntax=`
line, exports a resolved config, and checks an invalid Kernelfile:

```bash
go test -tags e2e -v -timeout 30m -run TestE2E .
```

Add a real kernel compile and boot smoke:

```bash
KBF_E2E_COMPILE=1 \
  go test -tags e2e -v -timeout 60m -run TestE2E .
```

The boot test prefers Firecracker with `/dev/kvm` and otherwise uses
`qemu-system-x86_64`. It requires the guest console to print `KBF-BOOT-OK`.

The local e2e image is single-platform. On a non-amd64 worker, set
`KBF_E2E_IMAGE` to a registry reference the builder can push and pull; the test
then publishes amd64 and arm64 variants before invoking the frontend.

An existing kernel can be boot-tested directly:

```bash
KBF_BOOT_VMLINUX=out/vmlinux \
  go test -tags e2e -run TestBootSmoke .
```

## Continuous integration

`.github/workflows/ci.yml` runs formatting, vet, race tests, build,
`go mod tidy -diff`, golangci-lint, govulncheck, and fuzz smoke on pushes to
`main` and pull requests.

The live buildkitd integration, Docker frontend e2e, and full build-and-boot jobs
are manually dispatched because they compile a real kernel:

```bash
gh workflow run ci.yml -f integration=true
gh workflow run ci.yml -f e2e=true
gh workflow run ci.yml -f boot=true
```

## Release

`.github/workflows/release.yml` publishes the frontend image for tags matching
`vMAJOR.MINOR.PATCH`:

```bash
git tag v0.1.0
git push origin v0.1.0
```

A stable tag publishes `:X.Y.Z`, `:X.Y`, `:X`, and `:latest`. A prerelease such
as `v0.2.0-rc1` publishes only its exact version.

The release builds amd64 and arm64 images, attaches SLSA provenance and an SPDX
SBOM, signs the index through GitHub OIDC, and verifies the published manifests,
attestations, and BuildKit frontend labels.

Before announcing the first release, make the GHCR package public and verify an
anonymous pull:

```bash
docker buildx imagetools inspect ghcr.io/emirb/kernelbuild-buildkit:latest
```
