# Kernelfile reference

[Back to the README](../README.md)

## Syntax and precedence

A Kernelfile contains one `KEY VALUE` pair per line. Whitespace separates the
key from its value. `#` starts a whole-line or trailing comment.

Unknown and duplicate keys are errors. Key order does not matter. An explicit
`SOURCE_URL` or `SHA256` takes precedence over values derived from `KERNEL`,
wherever it appears.

The `#syntax=` directive is required for `docker build` delegation. No parser
key is mandatory because the frontend starts from `DefaultSpec`. The config
file itself is required in the build context; without `CONFIG`, the frontend
loads `kernel.config`.

```Dockerfile
#syntax=ghcr.io/emirb/kernelbuild-buildkit
KERNEL   6.18.20
CONFIG   kernel.config
SHA256   a1415e257075c2fadf070f44bbb029469efbde5b6cf07d1433fe72207acff03c
EPOCH    1785542400
TARGETS  vmlinux image config
```

| Key | Required? | Value | Default |
| --- | :---: | --- | --- |
| `KERNEL` | No | kernel.org version such as `7.1-rc3` | `6.18.20` |
| `SOURCE_URL` | No | source tarball (`.tar.gz`, `.tar.xz`, or `.tar.zst`) | derived from `KERNEL` |
| `SHA256` | No | source tarball digest | pinned for the default source; empty after a source override |
| `EPOCH` | No | `SOURCE_DATE_EPOCH` | `1785542400` |
| `CONFIG` | No | config filename in the build context | `kernel.config` |
| `EXPECT` | No | post-`olddefconfig` assertion filename | disabled |
| `BASE_MAKE` | No | whitespace-separated make config targets applied before `CONFIG` | none |
| `TARGETS` | No | whitespace- or comma-separated artifact names | architecture default |
| `ARCH` | No | `x86_64` or `arm64` | `x86_64` |
| `CROSS_COMPILE` | No | cross-tool prefix | derived from `ARCH` |
| `TOOLCHAIN` | No | `apt` or `ready` | `apt` |
| `PATCHES` | No | `on` or `off` | `off` |
| `BASE` | No | base or ready-toolchain image | digest-pinned Ubuntu 24.04 |
| `PROXY_CA` | No | CA certificate filename in the context | none |

## Targets

| Target | Output | Notes |
| --- | --- | --- |
| `vmlinux` | `vmlinux` | uncompressed ELF |
| `image` | `bzImage` on x86_64; `Image` on arm64 | architecture boot image |
| `modules` | `modules.tar.zst` | stripped `modules_install`; requires `CONFIG_MODULES=y` |
| `config` | `config` | resolved post-`olddefconfig` config; no compile |
| `kconfigs` | `kconfig.txt.gz` | sorted `Kconfig*` catalog; no compile |

If `TARGETS` is omitted, x86_64 defaults to `vmlinux`; arm64 defaults to
`vmlinux image`.

`kconfig.txt.gz` contains one `### <path>` section per Kconfig file, sorted by
path. It is intended as input to services that build a symbol catalog.

## Config expectations

`olddefconfig` silently drops unknown symbols and options with unmet
dependencies. A `select` elsewhere can also force an option back on. `EXPECT`
names a file checked immediately after config resolution and before compilation
or seed publication.

```text
y CONFIG_KVM_GUEST      # must be y or m
n CONFIG_DRM            # must not be y or m
= CONFIG_HZ=100         # exact config line must be present
```

Blank lines and comments are ignored. A malformed line fails closed. Every
violation is printed as `KBF-EXPECT-FAIL <op> <arg>` in the build progress
stream.

## Base configs

`BASE_MAKE` runs config targets from the kernel tree before applying `CONFIG` as
a fragment:

```Dockerfile
BASE_MAKE x86_64_defconfig kvm_guest.config
CONFIG    kernel.config
TARGETS   config kconfigs
```

This answers “what does this kernel version's defconfig resolve to?” without a
local source checkout or compiler. `BASE_MAKE` is part of the compile vertex's
cache key.

## Patches

`PATCHES on` applies `patches/*.patch` from the build context in lexical order.
A missing or empty `patches/` directory fails before compilation. A failing
patch aborts the build.

Patched and unpatched builds use separate persistent trees. The tree stamp also
contains a hash of the patch series; editing patch content invalidates the
patched tree before reuse.

## Source integrity

`SHA256` pins the source tarball. The build hashes the download before replacing
a warm tree and fails on a mismatch. The digest also participates in the cache
key.

kernel.org publishes signed checksum files per release directory:

```bash
curl -fsSL https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc \
  | grep 'linux-6.18.20.tar.gz$'
```

Omitting `SHA256` is allowed for short experiments. The build warns and keys the
source by URL, so it is reproducible only while that URL serves identical
bytes.

Stable releases resolve under `https://cdn.kernel.org/pub/linux/kernel/vX.x/`.
Release candidates resolve to kernel.org's generated Torvalds snapshots. Since
snapshot tarballs are generated on request, pin the bytes you fetched rather
than expecting a published checksum.

## Toolchains and architectures

The default `TOOLCHAIN apt` path starts from Ubuntu 24.04 by digest and installs
the kernel build packages. It is convenient, but the apt archive can change
independently of the base image.

For a complete preinstalled toolchain, set:

```Dockerfile
TOOLCHAIN ready
BASE      docker.io/tuxmake/x86_64_gcc@sha256:...
```

A tagged `BASE` is resolved to a digest before graph generation. Put the digest
in the Kernelfile itself when the toolchain must remain fixed across workers and
time.

arm64 requires `TOOLCHAIN ready` and an arm64 cross-toolchain image such as
`docker.io/tuxmake/arm64_gcc`. The build steps execute as linux/amd64 and use
`aarch64-linux-gnu-` unless `CROSS_COMPILE` overrides it.

## Proxies and private CAs

Standard Docker build arguments configure the network vertices without changing
their cache keys:

```bash
docker build -f Kernelfile \
  --build-arg http_proxy=http://proxy:3128 \
  --build-arg https_proxy=http://proxy:3128 \
  --build-arg no_proxy=mirror.internal \
  --output type=local,dest=out .
```

Pass both proxy schemes: Ubuntu's stock apt sources use HTTP. `PROXY_CA` names a
certificate in the build context. The toolchain installs the public CA bundle,
appends that certificate, and uses HTTPS package sources afterward.

## Docker flags

The frontend implements the standard gateway options relevant to this build:

| Flag | Effect |
| --- | --- |
| `--output type=local,dest=out` | exports the artifact directory |
| `--no-cache` | executes every vertex with a new empty object-tree mount; this is a cold build |
| `--pull` | re-resolves `BASE` instead of using cached image resolution |
| `--platform linux/arm64` | selects the target architecture; one platform per invocation |
| `--network=host` | gives build steps host networking |
| `--build-arg http_proxy=...` and related proxy args | configures network steps without changing cache keys |
| `--cache-from type=registry,ref=...` | imports standard BuildKit remote cache |
| `--print=subrequests.describe` | lists supported frontend subrequests |

Unsupported subrequests such as `--print=outline` and `--check` return an
explicit error instead of starting a kernel build.

The intended output is a local directory or tar stream. `-t` is legal, but the
resulting image filesystem contains only the selected kernel artifacts.

## Verifying the frontend image

The published image is signed keylessly by the release workflow and carries
SLSA provenance and an SPDX SBOM.

```bash
cosign verify ghcr.io/emirb/kernelbuild-buildkit:latest \
  --certificate-identity-regexp '^https://github.com/emirb/kernelbuild-buildkit/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

docker buildx imagetools inspect ghcr.io/emirb/kernelbuild-buildkit:latest \
  --format '{{ json .Provenance }}'
```

The provenance describes a build on this project's own runners; it is not SLSA
L3. The release workflow rejects missing amd64 or arm64 manifests, attestations,
and required BuildKit frontend labels.
