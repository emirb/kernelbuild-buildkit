# Security model

This is a build frontend. It turns a build description into a BuildKit graph
and runs it. What that means for security depends entirely on whether the
build description is trusted.

## The trust boundary is the input, not the output

The frontend guarantees that a caller's inputs never become a shell command
in its own orchestration: they are validated by `Spec.Validate` or passed as
secret files, never re-parsed by an interpreter, and tarball extraction is
confined by `os.Root`. This is fuzzed and tested.

It does not, and cannot, promise that a kernel build runs no shell. `make`
and the kernel's own Makefiles run shells and `$(shell ...)`, and a config
or patch reaches that. So a build description is code: whoever writes the
Kernelfile, config, and patches can run arbitrary commands inside the build.

Two deployment shapes follow.

**Single-tenant (you build your own kernels).** You already trust the input.
Nothing below applies; run it however you like.

**Multi-tenant (you build kernels from untrusted descriptions).** Treat each
build as untrusted code execution. The guidance:

- Run builds in isolated workers with no ambient credentials and no network
  position you would not hand an attacker. The compile vertex runs untrusted
  `make`; it must not sit inside a trusted perimeter.
- Give untrusted builds **read-only** seed and cache access. The seed-push
  credential belongs only to a trusted seeder job that builds from pinned,
  reviewed input. The frontend enforces part of this: `seed-push` requires a
  pinned source (`source-sha256`), so a seeder cannot publish a tree built
  from a mutable URL.
- Do not expose the frontend as an unauthenticated fetch proxy. A
  caller-supplied `source-url` is a server-side request. The primary control
  is that the URL and every redirect must be `https`, which the cloud
  metadata services do not speak. As defense in depth, the fetch dialer also
  refuses link-local destinations and the AWS IPv6 metadata address, and
  dials the address it vetted rather than resolving the name a second time.
  That dialer is bypassed by design when the build runs behind an egress
  proxy (`https_proxy`), because the proxy resolves the destination then;
  the proxy's policy, and the rest of the egress policy, are yours.
- Extraction is confined but not size-capped: a hostile tarball can fill the
  object-tree mount. That is the same capability `make` already has, so it
  is not a separate boundary.

## Source integrity

`source-sha256` pins the tarball. It is optional on purpose: a quick
`KERNEL 6.19-rc2` experiment should not require chasing a checksum first. An
unpinned fetch prints a warning and is content-addressed only by URL, so it
is reproducible only as long as that URL serves the same bytes. Changing the
kernel version or source URL clears any inherited pin, because a pin is
specific to one tarball. For anything you keep, or anything you publish as a
seed, pin the source. See [Source integrity](docs/kernelfile.md#source-integrity).

## Reproducibility and the toolchain

Byte-reproducibility holds for a fixed toolchain. The default `apt` path
installs whatever `gcc`/`binutils` the Ubuntu archive serves at build time,
so it is best-effort across time even under a digest-pinned base image (apt
fetches current packages). `TOOLCHAIN ready` with a fully pinned toolchain
image (for example a pinned `tuxmake/*` digest) is the reproducible path. The
cache-mount and seed identity distinguish the two modes and the effective
cross-compile prefix, so their object trees never mix; they do not encode the
apt package set, which is why the apt path is not reproducible enough to
share a seed across time.

## Talking to buildkitd

`kbuildctl -addr` and `kbuild.Build` connect to a buildkitd you point them
at, with the transport that address implies (`tcp://` is plaintext gRPC, like
`buildctl`). Securing a remote daemon connection (TLS, mTLS) is the daemon
operator's job; run buildkitd with `--tlscert`/`--tlskey` and use a `tcp://`
address you control, or keep it on a local socket.

## Reporting

Open a private security advisory on the repository (Security, then "Report a
vulnerability"), or email <beganovic.emir@gmail.com>. Please do not file a
public issue for an unfixed vulnerability.
