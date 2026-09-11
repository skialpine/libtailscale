# libtailscale (Decenza fork)

This is `skialpine/libtailscale`, a fork of Tailscale's `libtailscale` maintained
**only** to serve [Decenza](https://github.com/Kulitorum/Decenza) (the Qt/C++
DE1 espresso controller). It embeds `tsnet` as a statically-linked C-ABI Go
library so Decenza can join a tailnet and expose its local MCP server publicly
over Funnel, without shelling out to a separate `tailscaled` process. See the
upstream `README.md` for what the library does; this file is about what's
different in this fork and why.

## Branch strategy

- **`main`** is *intended* to mirror upstream Tailscale's `libtailscale`
  history, kept clean so it's easy to pull upstream updates (`git fetch
  upstream && git merge upstream/main` or similar) without fighting
  Decenza-specific commits. Occasionally a fork commit gets merged into `main`
  via PR (e.g. PR #1, `66c63af`, the Android netmon-interface-getter fix) when
  it's the kind of thing upstream might plausibly want too — that's fine, but
  it's the exception, not the rule.

  **It does not currently hold.** As of 2026-09-11 `main` is 4 commits ahead
  of `upstream/main` with fork-only work: `f470665` (the original fork
  commit), `e97d912`, and the `f9667b5`/`14c1f78` strip-and-revert pair. So
  `main` is not a clean upstream mirror and a naive `git merge upstream/main`
  into it will not be a fast-forward. Sync upstream into `decenza-main`
  directly (that is what was done for the 2026-09-11 merge) rather than
  treating `main` as an upstream staging area, or first rebase those four
  commits off `main`.

- There is **no `upstream` remote by default** in a fresh clone — only
  `origin` (`skialpine/libtailscale`). Add it with
  `git remote add upstream https://github.com/tailscale/libtailscale.git`.
  Note the side effect: with both remotes present, bare `gh` commands resolve
  to `tailscale/libtailscale` and fail with `HTTP 404: workflow release.yml
  not found on the default branch`. Pass `--repo skialpine/libtailscale` to
  every `gh` invocation.
- **`decenza-main`** is the actual fork trunk. All Decenza-specific work
  happens here: Android build targets, status accessor exports, the release
  pipeline, and the build-tag trimming below. It branched off upstream history
  at `f470665` ("Decenza fork: status exports, Android build, release
  pipeline"). This branch was previously named `android-netmon-getter` before
  it accreted general fork-trunk responsibilities beyond that one feature —
  renamed 2026-07-20 to stop that name being misleading.
- Tagged releases (`decenza-v*`) are cut from `decenza-main`.

## Consumed by Decenza as

`cmake/tsnet.cmake` in the Decenza-Desktop repo downloads a `decenza-v*`
release's prebuilt artifacts (macOS `libtailscale.a`, iOS
`libtailscale.xcframework`, Android per-ABI `.so`s) — no Go toolchain needed
to build Decenza itself. `TSNET_TAG` there pins the exact release; bump it
(and the per-artifact SHA-256 hashes from that release's `manifest.json`)
whenever a new tag is cut here.

Decenza only ever exercises **tsnet Mode A**: embedded Tailscale + Funnel,
through `tailscale.go`'s exported C API —
`TsnetNewServer/Start/Up/Close/Listen/Dial/Loopback`,
`TsnetSetAuthKey/SetControlURL/SetEphemeral/SetHostname`,
`TsnetEnableFunnelToLocalhostPlaintextHttp1`, and the status accessors this
fork added (`TsnetGetCertDomain`, `TsnetGetAuthURL`, `TsnetGetBackendState`).
It does **not** use Tailscale SSH, Taildrop, subnet routes, exit nodes, App
Connectors, or MagicDNS/peer resolution — it only needs to expose *itself*
publicly, not resolve or route to other tailnet devices.

## Build tags (`TS_OMIT_TAGS` in the Makefile)

tailscale.com uses a `feature/buildfeatures` system: each optional subsystem
has an `_enabled.go`/`_disabled.go` pair gated by a `ts_omit_<name>` build
tag, letting embedders dead-code-eliminate what they don't use.

**`ts_omit_ssh` — load-bearing crash fix, not just a size optimization.**
Added 2026-07-20 after a Decenza crash: `ipn/ipnlocal/ssh.go`'s
`getSSHUsernames` shells out to `dscl` via `exec.Command`, and it's wired up
as a **C2N (control-to-node) HTTP handler** — the Tailscale coordination
server can trigger it at an arbitrary, remotely-initiated moment (e.g.
someone opening the admin console's SSH panel for the tailnet), completely
independent of anything the embedding app does locally. Decenza ships debug
builds with ASan auto-enabled (see Decenza's `CMakeLists.txt`), and `fork()`
without an immediate `exec()` in a multithreaded, ASan-instrumented process is
unsafe — a worker thread can be holding ASan's allocator lock at the instant
of `fork()`, and the child inherits it permanently locked. That produced
exactly this crash signature: `EXC_BREAKPOINT`/`SIGKILL`, "BUG IN CLIENT OF
LIBPLATFORM: os_unfair_lock is corrupt", "crashed on child side of fork
pre-exec". `-tags ts_omit_ssh` makes the C2N handler return `501` without
ever calling `exec`. Verified by diffing `strings` output of an archive built
with vs. without the tag — the `"dscl"` string is only present without it.
**Do not remove this tag without re-verifying `dscl` isn't reachable another
way.**

**Everything else in `TS_OMIT_TAGS` is a size optimization**, confirmed via
`go list -deps -json` against the actual dependency graph (not guessed from
feature names) — each either self-gates its own import chain the same way
`ssh` does, or is checked via `buildfeatures.Has*` inside always-linked core
files with no other reachable caller in a tsnet build:
`ts_omit_identityfederation`, `ts_omit_oauthkey`, `ts_omit_dns`,
`ts_omit_appconnectors`, `ts_omit_useroutes`, `ts_omit_advertiseroutes`,
`ts_omit_useexitnode`, `ts_omit_advertiseexitnode`, `ts_omit_cloud`,
`ts_omit_clientupdate`, `ts_omit_outboundproxy`. Net effect: macOS
`libtailscale.a` 55M → 49M unstripped.

**`ts_omit_dns` needs a real runtime check.** It's high-confidence
architecturally (net/dns/* isn't wired into tsnet's userspace engine) but
touches real surface area. Run a full connect+Funnel cycle on a build using
this tag before treating it as settled.

**Do not add these, ever:**
- `ts_omit_serve` — breaks Funnel outright. `TsnetEnableFunnelToLocalhostPlaintextHttp1`
  directly builds `ipn.ServeConfig{AllowFunnel: ...}`; this is the entire
  reason this library exists.
- `ts_omit_useproxy` — needed to reach Tailscale's control/DERP servers
  through corporate/system HTTP proxies.

**Investigated but deliberately left alone** (real but unverified risk, per
an earlier audit — don't add without dedicated testing):
`ts_omit_peerapiclient`/`ts_omit_peerapiserver` (PeerAPI — should be
orthogonal to Serve/Funnel but not proven), `ts_omit_portmapper` (NAT-PMP/PCP/
UPnP — likely dead weight since Funnel ingress is TLS-terminated through
Tailscale's network rather than this node's own NAT-mapped port, but affects
general DERP/peer connection quality too), `ts_omit_synology` (touches
`ipn/ipnlocal/cert.go`, which is central to Funnel's cert issuance — don't
touch without confirming the Synology branch is truly side-only),
`ts_omit_bakedroots` (embedded CA root fallback in `tlsdial.go` — a fallback
specifically for TLS, which is exactly what Funnel needs; don't drop without
confirming every target platform's system root store is always sufficient),
`ts_omit_unixsocketidentity` (low risk, low value, unverified).

**Most of the other 68 `ts_omit_*` tags upstream defines are no-ops for a
tsnet build** — features like `taildrop`, `aws`, `kube`, `tap`, `tpm`,
`wakeonlan`, `drive`, `posture`, `relayserver`, `capture`, `osrouter`,
`portlist`, `sdnotify` etc. are only reachable through
`tailscale.com/feature/condregister`, which `tsnet.go` never imports at all —
adding their tags would add tag-list noise with zero binary-size benefit.
Re-derive this with `go list -deps -json` against the pinned `tailscale.com`
version before trusting it again after a version bump; buildfeatures wiring
can change between releases.

## LocalAPI `OmitAuth` — second instance of the fork-under-ASan crash

`ts_omit_ssh` above fixed one `exec.Command` reachable on macOS. It was not the
only one. `local.Client.DoLocalRequest` attaches a Basic-Auth header on every
LocalAPI request unless `OmitAuth` is set, and on darwin that lookup resolves
to `safesocket_darwin.go`'s `readMacosSameUserProof()`, which runs
`lsof -n -a -u<uid> -c IPNExtension -F`. One fork+exec **per LocalAPI request**
— and `TsnetGetAuthURL`/`TsnetGetBackendState` are polled by Decenza during
connect, so it fired repeatedly. Same signature as the `dscl` crash:
`EXC_BREAKPOINT`/`SIGKILL`, "BUG IN CLIENT OF LIBPLATFORM: os_unfair_lock is
corrupt", "crashed on child side of fork pre-exec".

It was pure waste even when it didn't crash. That lookup exists so a
non-sandboxed CLI can find the **Mac App Store Tailscale GUI's** random
LocalAPI port and token. tsnet serves its own LocalAPI over an in-process
`memnet` listener and sets no `RequiredPassword` on that handler, so the token
is never checked; on a Mac with no Tailscale GUI installed `lsof` matches
nothing and the lookup fails anyway. `lsof -u` walks every fd of every process
the user owns.

Fixed by setting `OmitAuth` on the shared `*local.Client`. What justifies that
is the `RequiredPassword` point above — the token is never checked — and not
upstream's blessing. `OmitAuth`'s own doc comment describes something narrower
than our case: "meant for when Dial is set and the LocalAPI is being proxied
**to a different operating system, such as in integration tests**." Ours is
in-process and same-OS. Same mechanism, different scenario; don't cite that
comment as upstream having sanctioned this use.

**Routing our own call sites through a helper is NOT sufficient, and believing
it was is the mistake this section exists to prevent.** `tsnet.Server.Up`
obtains the same shared client itself and issues `WatchIPNBus`, `Status` and
`SetServeConfig` on it — and `Up` runs *before* anything polls status. The
first version of this fix converted only the three call sites in `tailscale.go`
and measured **zero** improvement: 6 `lsof` forks before, 6 after (see below).
So `OmitAuth` is primed by `(*server).primeLocalClient()` from `TsnetStart` and
`TsnetUp`, before the server can service any request. The `sync.Once` alone
would also be a genuine data race — it orders callers of `Do` against each
other, and tsnet's readers never call it. Priming is what supplies the
ordering; the Once only guarantees a single write.

**Verified at runtime, by measurement.** The `strings`-diff technique used for
`dscl` proves nothing here — this is a runtime fix, `safesocket_darwin.go` is
still linked and `"lsof"` is still in the archive. Instead, put a recording
shim named `lsof` first on `PATH` and run `go test -run TestConn`, which drives
a full `tailscale_up()` cycle against the in-process test control server:

| tree | `lsof` forks |
|---|---|
| before any fix | 6 |
| helper only, no priming | 6 |
| helper + priming | **0** |

`TestLocalAPIGoesThroughOmitAuthHelper` in `tailscale_test.go` is the
regression guard for the source side: it parses **every non-test `.go` file in
the package** (not just `tailscale.go` — package `main` has others, and build
tags are deliberately not evaluated) and fails on any `LocalClient()` reached
outside the helper, including via a method value or a package-level `var`.
Note its limit: it cannot see tsnet's own calls, which is exactly why the
priming exists. A green run is not proof the forks are gone; the shim is.

**Audit result, so this doesn't get re-derived from scratch:** every file
actually compiled into the macOS artifact was scanned (`go list -deps` with
`TS_OMIT_TAGS`, 530 packages / 2511 files) for `exec.Command`,
`exec.CommandContext`, `exec.LookPath`, `syscall.StartProcess` and
`syscall.ForkExec`. 16 hits (1 fixed + 2 near-misses + 13 guarded — do not
count `os/exec_posix.go`'s own `syscall.StartProcess`, which is the shared
primitive every `exec.Command` already routes through, not a distinct site).
After this fix **none are reachable on macOS**. Two are worth knowing about:

- `safesocket/unixsocket.go:77` runs `launchctl list com.tailscale.tailscaled`
  and *is* darwin-active — the GOOS guard passes. It is unreachable only
  because its sole caller is `safesocket.Listen`, which tsnet never calls (it
  uses `memnet.Listen`). If anything ever routes through `safesocket.Listen`,
  this forks.
- `util/osuser/user.go:142` runs `getent passwd` with no GOOS guard excluding
  darwin, and `ipn/ipnlocal` imports it. It doesn't fork only because macOS has
  no `getent`: `exec.Command` does `LookPath` at construction, fails, and
  `Start` returns without forking. Fragile-by-luck, not guarded.

The remaining 13: nine guarded to linux (`hostinfo.go:488`,
`ip_forward.go:322`, `local.go:1079`, `direct.go:98/105/114`,
`group_ids.go:48`, `logpolicy.go:384` — that last one linux/freebsd/openbsd),
one to illumos/solaris (`ip_forward.go:345`, `ipadm`), one to Synology
(`synology.go:28`), and three in `assets.go:85/90/126` (`yarn`/`node`/`git`)
which are gated by a `devMode` flag rather than by GOOS at all. **None is
Windows-guarded** — an earlier version of this paragraph said "windows" and
omitted illumos/solaris, which is the kind of unchecked detail this file exists
to stop. Re-run the audit after a `tailscale.com` bump.

## Do not reintroduce: `-s -w` symbol stripping

Tried in `f9667b5` ("Strip Go symbol table + DWARF (-s -w) from all builds"),
reverted same-day in `14c1f78` with no explanation recorded. Re-measured
2026-07-20: stripping saves real space in isolation (55M → 27M unstripped
vs. stripped, no omit tags) but the actual reason for the revert wasn't
"didn't save space" — do not re-add without first finding out *why* it broke
last time (crash symbolication on the Decenza side is the leading suspect,
since this archive links directly into an app that ships crash reports).

## CI gotcha: the macOS release job doesn't route through the Makefile

`release.yml`'s `macos` job builds a universal (arm64+amd64) binary via two
raw `go build` invocations + `lipo`, because the Makefile's `libtailscale.a`
target only builds for the host arch. This means it does **not**
automatically pick up `-tags` changes made to the Makefile — the `ts_omit_ssh`
crash fix itself would have silently missed the one platform it was fixing,
had this not been caught. Fixed by adding `make print-tags` (prints
`TS_OMIT_TAGS`) and having the macOS job read `TAGS="$(make print-tags)"`
instead of duplicating the tag list. **If you add a new `-tags` flag to any
Makefile target, verify `release.yml`'s macOS job still gets it** — either it
already does (via `print-tags`) or you've introduced the same drift again.
The `ios` and `ios-sim` and `android` release jobs call `make c-archive-ios`/
`make android` directly and pick up Makefile changes automatically; no
equivalent gotcha there.

**`MACOS_TARGET` is now load-bearing in `release.yml`, in the opposite
direction.** Upstream `59d4bb8` changed the Makefile's `MACOS_TARGET := 15.0`
to `?=`, so an environment variable now overrides it. `release.yml` sets
`MACOS_TARGET: "15.0"` as a workflow-level `env:`, which previously the
Makefile ignored and which is now what actually wins for every `make` call in
every release job. The two values agree today, so nothing changed — but the
comment above that line ("Keep in sync with the Makefile MACOS_TARGET") is no
longer merely advisory: **editing the Makefile default alone now silently has
no effect in CI**, because the workflow env var overrides it. Change both, or
delete the workflow env var and let the Makefile be the single source.

Other workflows (`test.yml`, `swift.yml`, `ruby.yml`, `sourcepkg.yml`) are
upstream's own CI for the Go test suite and the Swift/Ruby/Python language
bindings — none of them build the macOS/iOS/Android release artifacts Decenza
consumes, so they don't need `TS_OMIT_TAGS` and weren't touched by any of the
above.

## Cutting a release

1. Land changes on `decenza-main`, verify `make libtailscale.a` (or whichever
   target) builds clean locally first — see "Local builds need two env vars"
   below, this does not work with a bare `make` on a current macOS box.
2. `git tag decenza-v<tailscale-version>-<n>` (increment `n` for a re-release
   at the same tailscale.com version; see `git tag -l` for the existing
   sequence) and `git push origin <tag>`.
3. `release.yml` triggers on `decenza-v*` tag push, builds macOS/iOS/Android,
   and publishes a GitHub Release with `SHA256SUMS.txt` + `manifest.json`.
4. In Decenza-Desktop, bump `TSNET_TAG` in `cmake/tsnet.cmake` to the new tag
   and update the three per-platform SHA-256 hashes from the release's
   `manifest.json`.

## Local builds need two env vars (diagnosed 2026-09-11)

A bare `make libtailscale.a` or `go test` fails on a current macOS box, for two
independent reasons. Neither is caused by anything in this fork, and CI is
unaffected — `release.yml` pins `GO_VERSION: "1.25.5"` and runs on a
GitHub-hosted runner whose Xcode and SDK are consistent with each other.

**1. Go toolchain — affects every target.** `go.mod` says `go 1.25.5` with no
`toolchain` directive, which is a *minimum*, so a newer local Go is used
instead. Under Go 1.27 the pinned `github.com/go-json-experiment/json`
(`v0.0.0-20250813024750-ebf49471dced`) fails to compile:

    undefined: json.SkipFunc
    undefined: json.DiscardUnknownMembers

Fix: `GOTOOLCHAIN=go1.25.5`. Prefer the env var over adding a `toolchain` line
to `go.mod` — `go.mod`/`go.sum` are currently byte-identical to upstream's,
which is what makes it cheap to confirm after a merge that the `tailscale.com`
pin didn't move and the `ts_omit_*` / `exec.Command` audits still hold. Don't
give that up for a local convenience.

**2. Xcode/CLT SDK split — affects linking executables only, i.e. `go test`.**
`xcode-select -p` points at `/Applications/Xcode.app`, which ships `ld-1267`
(Jun 2026) and SDKs only up to `MacOSX26.5`. But `xcrun --show-sdk-path`
resolves to the *Command Line Tools* SDK, `MacOSX27.0`, whose `.tbd` stubs
declare `arm64e.x1` slices that the older `ld` cannot parse:

    ld: multiple errors: tapi error: malformed file
    .../MacOSX27.0.sdk/usr/lib/libresolv.9.tbd:4:20: error: unknown architecture
                       arm64e.x1-macos, arm64e.x1-maccatalyst ]

So the build mixes Xcode's old linker with the CLT's newer SDK. Fix by pinning
both halves to Xcode:

    export SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk

This affects `go test` and anything else that links a Mach-O executable.
`-buildmode=c-archive` does **not** link, so `make libtailscale.a` needs only
`GOTOOLCHAIN` — which is why a release can be cut without ever noticing this.

**Verified working combinations** (`make print-tags` set, arm64):

| command | env needed | result |
|---|---|---|
| `make libtailscale.a` | `GOTOOLCHAIN` only | 50368736-byte archive |
| `go test -run 'TestConn\|TestLocalAPIGoesThroughOmitAuthHelper'` | `GOTOOLCHAIN` + `SDKROOT` | `ok`, with `ld: warning: object file ... built for newer 'macOS' version (27.0) than being linked (26.5)` |
| same, plus `MACOSX_DEPLOYMENT_TARGET=15.0` | all three | `ok`, no warnings |

The `make` targets already set `MACOSX_DEPLOYMENT_TARGET` and the
`-mmacos-version-min` CGO flags themselves; that third variable is only needed
when invoking `go test`/`go build` by hand.

Two alternatives, neither taken: `sudo xcode-select -s
/Library/Developer/CommandLineTools` pairs the *newer* `ld-27037.1` with the
`MacOSX27.0` SDK and needs no env vars, but it's a global switch that also
changes what Decenza's own Qt builds compile against; or update Xcode so its
`ld` and SDK catch up to the CLT's.
