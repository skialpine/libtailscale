# libtailscale (Decenza fork)

This is a **fork of libtailscale maintained for [Decenza](https://github.com/Kulitorum/Decenza)**,
the Qt/C++ controller for the Decent Espresso DE1. It embeds Tailscale into a
process as a C library, giving a program a userspace address on a tailnet and —
via Tailscale Funnel — a stable public HTTPS URL. Decenza uses it for the
"Remote MCP" connector so Claude/ChatGPT mobile apps can reach the on-device MCP
server without any developer-run infrastructure.

This is a standalone clone; it is not tracked against or linked to the original
project. Changes are made here to serve Decenza's needs.

## What this fork adds

- **Status accessors on the C API** — `tailscale_get_cert_domain` (the node's
  Funnel FQDN, used to compose the connector URL), `tailscale_get_auth_url` (the
  interactive login URL for first-time setup / QR), and
  `tailscale_get_backend_state`. See `tailscale.h`.
- **Android build targets** — per-ABI `c-shared` `.so` (arm64-v8a, armeabi-v7a,
  x86_64) built with the NDK. See `make android`.
- **A release pipeline** (`.github/workflows/release.yml`) that builds macOS,
  iOS, and Android artifacts and publishes them as a GitHub Release, so Decenza's
  build can pull in prebuilt binaries and needs **no Go toolchain**.

The upstream Funnel entrypoint (`tailscale_enable_funnel_to_localhost_plaintext_http1`)
is used as-is: it provisions Let's Encrypt certs, terminates TLS, and proxies
public HTTPS to a plaintext HTTP/1 server on `127.0.0.1:<port>` — which is
Decenza's loopback MCP listener.

## Prebuilt releases

Tagged releases (`decenza-v*`) publish:

| Artifact | Contents |
|---|---|
| `libtailscale-macos.zip` | universal (arm64+x86_64) `libtailscale.a` + `include/tailscale.h` |
| `libtailscale-ios.zip` | `libtailscale.xcframework` (device + simulator) wrapping the C static libs |
| `libtailscale-android.zip` | `jniLibs/<abi>/libtailscale.so` for each ABI + `include/tailscale.h` |

Each release also carries `SHA256SUMS.txt` and `manifest.json` (tag + per-file
checksums) for verified downloads.

## Building locally

Requires a recent Go toolchain (see `go.mod`). Link the resulting archive into
your binary and `#include "tailscale.h"`.

```sh
make c-archive        # libtailscale.a for the host platform
make shared           # libtailscale.so for the host platform
make c-archive-ios    # iOS device static archive
make c-archive-ios-sim# iOS simulator fat static archive
make android NDK=$ANDROID_NDK_HOME   # Android .so for all ABIs (NDK required)
```

## License

BSD 3-Clause for this repository, see LICENSE. The original copyright notices are
retained as required.
