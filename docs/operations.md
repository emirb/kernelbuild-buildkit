# Operations and design

[Back to the README](../README.md)

## Graph shape

The default graph has two executable vertices:

1. The toolchain vertex installs the kernel build dependencies into a
   digest-pinned Ubuntu base image.
2. The compile vertex runs `kbuild-step` over a locked persistent cache mount
   containing the source and object tree.

With `TOOLCHAIN ready`, the first vertex is omitted unless a proxy CA must be
installed.

The compile vertex mounts only the `kbuild-step` file from the frontend image.
Changing the gateway binary without changing the helper therefore preserves the
compile vertex cache key.

## Input boundary

The apt provisioning vertex uses a fixed shell script. The compile vertex
starts `kbuild-step` as argv, without a shell. Source fetching, hashing,
extraction, patch application, stamp management, config checks, seed transfer,
and artifact packing are implemented in Go. The only child process is `make`.

Every Kernelfile value is validated before graph generation. Tar extraction is
confined by `os.Root`; path traversal and escaping symlink members are rejected.

This boundary does not make a kernel build safe to run with credentials.
Kernel Makefiles execute shell commands, and config or patch input can affect
them. See [Security model](../SECURITY.md).

## Cache keys

Output-affecting values are part of the graph or environment and therefore the
cache key:

- source identity and URL
- config and expectation contents
- patches and patched state
- kernel version and architecture
- base image and cross prefix
- build epoch and requested targets
- helper binary contents

Network proxies use `llb.WithProxy`; seed destinations and credentials use
secrets. These inputs can change without invalidating the compile vertex.

## Cache layers

### Vertex cache

BuildKit caches each vertex by content. An identical request is a complete hit.
The cache can be exported with BuildKit's registry, S3, local, and other
backends.

Client mode forwards cache imports to both the outer gateway solve and the
inner graph solve. This matters on a fresh daemon: importing only at the wrapper
does not make the graph vertices available to the nested solve.

### Persistent object tree

The kernel source and object files live in a locked cache mount. The mount ID
contains the kernel version, target architecture, toolchain identity, and
patched/unpatched bit.

Changing a config re-runs the compile vertex against that tree. Kbuild's saved
command lines and dependency files determine which objects need rebuilding.

Patched and unpatched builds use separate mounts, so alternating between them
does not continuously discard one tree. Patch content is recorded in the tree
stamp; changing it discards and recreates the patched tree.

### Remote object-tree seed

BuildKit cache exporters do not include cache mounts. The optional seed packs
the tree as zstd-compressed tar and stores it in S3-compatible storage. A fresh
mount attempts to hydrate from that seed before fetching or compiling.

Seed publication has a unique action key. This forces execution even when the
compile result is cached but keeps the persistent mount warm. `llb.IgnoreCache`
is intentionally not used: on BuildKit 0.32.2 it replaces the cache mount with
an empty one.

The uploader uses 8 MiB multipart parts with two concurrent uploads. This stays
below MinIO's 16 MiB aws-chunked limit while retaining per-part checksums and
compatibility with AWS S3.

One seed is shared by configs with the same kernel, architecture, toolchain,
and patched state. The config is deliberately absent from the seed identity;
the receiving build applies its config and rebuilds the delta.

## Toolchains

### Apt mode

The default base is Ubuntu 24.04 pinned by digest. The toolchain vertex installs
gcc, binutils, and other kernel build packages from Ubuntu's archive.

This fixes the base filesystem but not the package set served by the archive.
Apt mode is suitable for convenient builds and local cache reuse, but not for
byte reproducibility across time.

When `PROXY_CA` is set, the base first installs `ca-certificates` from the stock
HTTP sources, appends the supplied CA, switches the apt sources to HTTPS, and
installs the remaining dependencies.

### Ready mode

`TOOLCHAIN ready` uses a base image that already contains the complete build
toolchain. Pin that image by digest for reproducible builds.

[TuxMake](https://tuxmake.org/) publishes maintained GCC, Clang, cross, and
kernel.org toolchain images. Older kernels often need compilers from the same
era rather than the current Ubuntu default.

The persistent mount and seed identity contain the base image, effective cross
prefix, and toolchain mode. Objects from different compilers cannot mix.

## Concurrency

Builds using the same mount serialize through `llb.CacheMountLocked`. The
second build waits, then sees the first build's completed tree.

For a workload that needs parallel builds of one kernel/toolchain combination,
use multiple workers or introduce a replica index into the mount identity.
Replica sharding is not implemented here. A shared seed can hydrate each
replica.

## Garbage collection and disk

BuildKit can evict cache mounts under its normal garbage-collection policy.
Allow approximately 3 GB per active kernel tree, plus toolchain layers and
artifacts. Patched and unpatched variants each consume a tree.

Configure the daemon's retained-storage budget when local incremental latency
matters:

```bash
buildkitd --oci-worker-gc-keepstorage <bytes>
```

Eviction does not corrupt a build. It changes the next request from a warm
incremental to a seed hydrate or cold build.

`kbuild-step` sizes `make -j` from the build container's CPU quota. A Docker or
Kubernetes CPU limit therefore controls compile parallelism without reference
to the host's total core count.

## Network and credentials

Source and apt traffic use the standard HTTP, HTTPS, and no-proxy settings.
Proxy values are cache-neutral.

Seed credentials are mounted as BuildKit secrets. A request with
`seed-push=true` but without the seed config and both credentials fails before
publication.

Treat the seed and remote vertex cache as build caches: untrusted builds should
receive read-only access. Only a trusted build should publish shared state.

## Failure behavior

- A wrong source digest downloads the complete tarball before failing because
  the hash is known only after the final byte. The existing warm tree remains.
- A source or patch stamp mismatch verifies the replacement source before
  deleting the current tree.
- A failing patch aborts the build; the frontend never continues unpatched.
- `EXPECT` failures happen immediately after `olddefconfig`, before compilation
  and seed publication.
- `--no-cache` creates a new empty object-tree mount on BuildKit 0.32.2 and is
  therefore a cold build.

## Troubleshooting

**The frontend image cannot be resolved.** Confirm that the GHCR package is
public or run `docker login ghcr.io` with an account that can read it. A
`#syntax=` failure happens before the Kernelfile reaches this frontend.

**`kbuildctl` reports that the helper is not a linux/amd64 ELF.** Rebuild it
with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`; the helper's architecture follows
the compile container, not the client host.

**The build cannot reach buildkitd.** Check the address with `buildctl debug
workers`. On Docker Desktop, publish the daemon port explicitly instead of
relying on `--network=host`.

**A built kernel boots with no console output.** Kbuild defaults disable the
usual microVM initramfs and serial-console options. Start from a Firecracker or
Cloud Hypervisor guest config, or compare against `testdata/boot.config`.

**A warm build became cold.** Check BuildKit garbage collection first. A tight
retained-storage budget can remove the object-tree mount while leaving normal
image layers intact.

## Rejected optimizations

On the measured workload, `make -j` at 1.5 times the available CPU count was
about 3% slower than one job per available CPU. `KCFLAGS=-pipe` changed the
output bytes relative to the golden artifact. Neither is enabled.

During a small incremental, compilation does not occupy every core for the
whole build. Config resolution, make's dependency walk, and the final link are
partly serial. If the input config is byte-identical, `kbuild-step` skips config
processing entirely.

A fully cached request with `-no-export` measured 0.18s on the 4 vCPU worker.
The remaining time is the BuildKit client session and control RPCs; exporting
the artifacts adds roughly 0.15s on that worker.

See the [README measurements](../README.md#measured) for recorded cold, cached,
incremental, seed, and client timings.
