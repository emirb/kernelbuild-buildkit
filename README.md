# kernelbuild-buildkit

[![CI](https://github.com/emirb/kernelbuild-buildkit/actions/workflows/ci.yml/badge.svg)](https://github.com/emirb/kernelbuild-buildkit/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/emirb/kernelbuild-buildkit?include_prereleases&sort=semver)](https://github.com/emirb/kernelbuild-buildkit/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/emirb/kernelbuild-buildkit.svg)](https://pkg.go.dev/github.com/emirb/kernelbuild-buildkit)
[![Coverage](https://codecov.io/gh/emirb/kernelbuild-buildkit/graph/badge.svg)](https://codecov.io/gh/emirb/kernelbuild-buildkit)
[![OpenSSF Scorecard](https://img.shields.io/badge/dynamic/json?url=https%3A%2F%2Fapi.scorecard.dev%2Fprojects%2Fgithub.com%2Femirb%2Fkernelbuild-buildkit&query=%24.score&label=openssf%20scorecard)](https://scorecard.dev/viewer/?uri=github.com/emirb/kernelbuild-buildkit)
[![License: MIT](https://img.shields.io/github/license/emirb/kernelbuild-buildkit)](LICENSE)

Build Linux kernels with stock `docker build`. No local compiler, source
checkout, or custom client is required.

`kernelbuild-buildkit` is a custom [BuildKit](https://github.com/moby/buildkit)
LLB frontend. It reads a small `Kernelfile`, resolves the source and toolchain,
and runs kbuild over a persistent object tree. An identical build is a full
cache hit. A config change re-runs one vertex, where kbuild recompiles only the
affected objects.

**Status:** pre-1.0. The Kernelfile format and Go API may change between minor
releases until v1.0.

## Requirements

- Docker with BuildKit enabled. The frontend path is tested with Docker 29.7.2
  and BuildKit 0.32.2 on native Linux/amd64 and Docker Desktop/arm64.
- Network access to GHCR, kernel.org, and the base image's package mirrors.
- Several gigabytes of free Docker storage. Allow about 3 GB per persisted
  kernel object tree, plus image layers and exported artifacts.

The default target is x86_64. Building arm64 kernels requires a ready
cross-toolchain image; the worker itself may be amd64 or arm64. Go is needed
only for the client and development workflows.

## Build your first kernel

1. Create a `Kernelfile`:

   ```Dockerfile
   #syntax=ghcr.io/emirb/kernelbuild-buildkit
   KERNEL   6.18.20
   CONFIG   kernel.config
   SHA256   a1415e257075c2fadf070f44bbb029469efbde5b6cf07d1433fe72207acff03c
   EPOCH    1785542400
   TARGETS  vmlinux image config
   ```

2. Create a file named `kernel.config` in the same directory as the
   `Kernelfile`; the `CONFIG` line above names it. A fragment is enough
   because the build runs `olddefconfig`, so this one line is a valid config:

   ```text
   CONFIG_OVERLAY_FS=y
   ```

   The directory now holds exactly two files:

   ```text
   .
   ├── Kernelfile
   └── kernel.config
   ```

3. Build from that directory:

   ```bash
   docker build -f Kernelfile --output type=local,dest=out .
   ```

4. Verify the result:

   ```bash
   file out/vmlinux out/bzImage
   grep '^CONFIG_OVERLAY_FS=y$' out/config
   ```

`out/vmlinux` is the uncompressed ELF, `out/bzImage` is the x86 boot image, and
`out/config` is the resolved config. Repeating the build should report every
vertex as cached.

The complete example is in [`examples/`](examples/). The bare frontend
reference tracks `latest`; use a release tag or digest in a committed
Kernelfile.

## Guest kernels

The artifacts boot directly in
[Firecracker](https://github.com/firecracker-microvm/firecracker) and
[Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor).
Firecracker normally takes `vmlinux`; Cloud Hypervisor accepts a PVH-enabled
`vmlinux` or `bzImage`. Both use the arm64 PE `Image` for arm64 guests.

Start from a VMM-maintained guest config rather than kbuild defaults:

```bash
curl -fsSLo kernel.config \
  https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config
docker build -f Kernelfile --output type=local,dest=out .
```

Append local overrides below the downloaded config; later lines win through
`olddefconfig`. Cloud Hypervisor needs `CONFIG_PVH=y`. The minimal tested boot
floor is [`testdata/boot.config`](testdata/boot.config).

An empty config builds but does not make a useful microVM guest: initramfs and
serial-console support are disabled by default, so it can boot to silence.

## How it works

```text
Kernelfile + config + patches
              │
              ▼
      BuildKit LLB frontend
              │
              ▼
  pinned base → kbuild-step → artifacts
                    ↕
           persistent object tree
```

The frontend resolves a tagged base image to a digest before generating the
graph. The compile vertex starts a static `kbuild-step` binary as plain argv.
Fetching, extraction, patching, config validation, seed transfer, and artifact
packing are implemented in Go; `make` is its only child process.

Values from the Kernelfile are validated before they reach the graph.
Extraction is confined with [`os.Root`](https://pkg.go.dev/os#Root), and the
frontend does not interpolate input into shell commands. Kernel Makefiles can
still execute shells, so a config or patch must be treated as code. See
[`SECURITY.md`](SECURITY.md) for the trust boundary and deployment guidance.

## Kernelfile essentials

Lines are `KEY VALUE`. Whitespace separates fields, `#` starts a comment, and
key order does not matter. Unknown and duplicate keys are errors.

The `#syntax=` directive is required for `docker build`. Individual keys are
optional because the frontend supplies defaults. The selected config file is
still required in the build context; omitting `CONFIG` selects `kernel.config`.

| Key | Required? | Value / default |
| --- | :---: | --- |
| `KERNEL` | No | kernel.org version; `6.18.20` |
| `SOURCE_URL` | No | derived from `KERNEL`; supports `.tar.gz`, `.tar.xz`, and `.tar.zst` |
| `SHA256` | No | pinned for the default source; cleared when the source changes |
| `EPOCH` | No | `SOURCE_DATE_EPOCH`; `1785542400` |
| `CONFIG` | No | config filename in the context; `kernel.config` |
| `EXPECT` | No | post-`olddefconfig` assertions; disabled |
| `BASE_MAKE` | No | in-tree config targets applied before `CONFIG`; none |
| `TARGETS` | No | arch default; accepts `vmlinux`, `image`, `modules`, `config`, `kconfigs` |
| `ARCH` | No | `x86_64` |
| `CROSS_COMPILE` | No | derived from `ARCH` |
| `TOOLCHAIN` | No | `apt`; use `ready` for a preinstalled toolchain |
| `PATCHES` | No | `off` |
| `BASE` | No | digest-pinned Ubuntu 24.04 image |
| `PROXY_CA` | No | CA certificate filename in the context; none |

`image` exports `bzImage` on x86_64 and `Image` on arm64. `modules` exports a
stripped `modules.tar.zst`. `config` and `kconfigs` do not compile a kernel.

See [Kernelfile reference](docs/kernelfile.md) for target behavior,
expectations, base configs, source pinning, Docker flags, and frontend image
verification.

## Caching

The build has three cache layers:

1. **BuildKit vertex cache.** An identical build is a full hit. The cache can
   be exported to a registry or S3-compatible store for fresh workers.
2. **Persistent object tree.** A config change re-enters the compile vertex but
   reuses kbuild's dependency state and compiled objects.
3. **Remote object-tree seed.** BuildKit cache exporters do not include cache
   mounts. A trusted seeder can publish the object tree to S3-compatible
   storage so a cold worker can hydrate it before compiling.

The persisted tree is keyed by kernel version, architecture, toolchain image,
cross prefix, and patched state. A source or patch-content mismatch discards
the tree and rebuilds it. Seed publication is forced without replacing the
warm cache mount.

No ccache or sccache is involved. See [Operations and design](docs/operations.md)
for cache identity, remote seeding, concurrency, garbage collection, proxies,
and toolchains.

## Measured

Linux 6.18.20, 4 vCPU worker, 26 August 2026:

| Scenario | Wall | Objects compiled |
| --- | ---: | --: |
| Cold build, including vertex-cache export | 329s | 1740 |
| Identical rebuild | 1s | 0 |
| One config option changed, warm worker | 16s | 8 |
| Fresh worker, same config, S3 vertex hit | 2s | 0 |
| Fresh worker, changed config, 283 MB seed hydrate | 49s | 0–19 |
| Seed publication | +19s | 0 |

On a 16 vCPU / 32 GB worker, the same suite measured 84s cold, 0.4s for an
identical request, and 11s for a one-option change. Client-side graph generation
and marshaling measured 22µs per solve on Apple M5.

Every release is gated on the same path a user takes: the exact image about to
be published builds a kernel from [`testdata/boot.config`](testdata/boot.config)
with stock `docker build` on a cold 4 vCPU runner and boots it in QEMU. For
[v0.1.0](https://github.com/emirb/kernelbuild-buildkit/actions/runs/33705397713)
that was 80s for the full build and 1.5s to reach userspace. That config is
minimal, so it is not comparable to the table above.

These numbers are workload and worker dependent. The important invariants are
zero compilation on a full hit, a small object delta for a local config change,
and zero or near-zero compilation after seed hydration.

## Reproducibility and security

`SHA256` pins the source bytes. `EPOCH` fixes the build timestamp. A committed
frontend digest and a `TOOLCHAIN ready` image pinned by digest fix the remaining
build inputs.

The default apt mode favors a one-command first build. Its Ubuntu base image is
pinned, but the archive can publish newer compiler and binutils packages, so it
is only best-effort reproducible across time. Use a complete, digest-pinned
toolchain image for byte reproducibility.

The published frontend image is multi-architecture, signed with keyless cosign,
and carries SLSA provenance and an SPDX SBOM. Verification commands are in the
[Kernelfile reference](docs/kernelfile.md#verifying-the-frontend-image).

## Limits

- Builds sharing one kernel, architecture, toolchain, and patch state serialize
  on a locked object-tree mount.
- BuildKit garbage collection can evict the tree. Size the daemon for roughly
  3 GB per active tree or configure a remote seed.
- arm64 kernels are cross-compiled in linux/amd64 build steps. On an arm64
  worker, those steps run under emulation.
- One invocation produces one target architecture, not a multi-platform result.
- The frontend applies in-tree `BASE_MAKE` targets and one config fragment. It
  does not implement arbitrary fragment merging or policy.

## Documentation

- [Kernelfile reference](docs/kernelfile.md)
- [Client and Go API](docs/client.md)
- [Operations and design](docs/operations.md)
- [Development, testing, and releases](docs/development.md)
- [Security model](SECURITY.md)

## Support

Use [GitHub issues](https://github.com/emirb/kernelbuild-buildkit/issues) for
bugs and usage questions. Report vulnerabilities privately as described in
[`SECURITY.md`](SECURITY.md).

## License

MIT
