# Copyright (c) Tailscale Inc & AUTHORS
# SPDX-License-Identifier: BSD-3-Clause

# Detect GOOS if not set
ifeq ($(GOOS),)
	GOOS := $(shell go env GOOS)
endif

export CGO_ENABLED=1

# This should match the minimum target in the xCode project
# The wrapper lib currently requires features available in
# MacOS 15.0 (Sequoia)
#
# `?=` rather than `:=` so the environment can override it. With `:=`,
# `MACOS_TARGET=14.0 make c-archive` is silently ignored — make reports success
# and builds 15.0 anyway, because an environment variable does not override a
# makefile assignment. Only `make MACOS_TARGET=14.0 c-archive` worked, which is
# easy to get wrong and gives no signal when you do.
MACOS_TARGET ?= 15.0

# Set macOS-specific flags for darwin builds
ifeq ($(GOOS),darwin)
	DARWIN_CGO_CFLAGS := -mmacos-version-min=$(MACOS_TARGET)
	DARWIN_CGO_LDFLAGS := -mmacos-version-min=$(MACOS_TARGET)
	DARWIN_DEPLOYMENT_TARGET := MACOSX_DEPLOYMENT_TARGET=$(MACOS_TARGET)
endif

# --- Local developer environment -------------------------------------------
# Two things break a bare `make` / `go test` on a developer Mac. Both are
# environmental rather than anything in this tree, and both are skipped when
# CI is set (GitHub Actions always sets CI=true) so the release build keeps
# using exactly what the workflow pins. See CLAUDE.md, "Local builds need two
# env vars", for the full diagnosis.
ifndef CI

# 1. go.mod's `go` directive is a *minimum*, so a newer local Go gets used and
#    the pinned go-json-experiment/json fails with `undefined: json.SkipFunc`.
#    Pin the toolchain to what go.mod asks for -- read from go.mod rather than
#    hardcoded, so this cannot drift from the real pin the way a third copy of
#    the version number would.
GO_MOD_VERSION := $(shell awk '/^go /{print $$2; exit}' go.mod)
ifneq ($(GO_MOD_VERSION),)
GOTOOLCHAIN ?= go$(GO_MOD_VERSION)
export GOTOOLCHAIN
endif

# 2. Linking an executable (i.e. `go test`; c-archive builds don't link, which
#    is why `make libtailscale.a` never tripped over this) can pick up the
#    Command Line Tools' SDK while using Xcode's older ld, which then cannot
#    parse the newer .tbd stubs:
#        ld: multiple errors: tapi error: malformed file
#        ... error: unknown architecture arm64e.x1-macos
#    Pair the SDK with whichever developer dir is actually active. Derived from
#    xcode-select rather than hardcoding /Applications/Xcode.app. If the active
#    dir is the Command Line Tools this path doesn't exist and nothing is set --
#    correct, because the CLT's own ld and SDK already match each other.
ifeq ($(GOOS),darwin)
XCODE_DEVELOPER_DIR := $(shell xcode-select -p 2>/dev/null)
XCODE_SDKROOT := $(XCODE_DEVELOPER_DIR)/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk
ifneq ($(wildcard $(XCODE_SDKROOT)),)
SDKROOT ?= $(XCODE_SDKROOT)
export SDKROOT
endif
endif

endif # CI

# --- Android configuration -------------------------------------------------
# Android c-shared builds use the NDK's per-target clang as the cgo compiler.
# Override on the command line / in CI:
#   NDK              path to the Android NDK (default: $ANDROID_NDK_HOME)
#   ANDROID_API      min SDK to target (Decenza ships API 28+)
#   NDK_HOST         NDK prebuilt host tag (linux-x86_64 on CI, darwin-x86_64 locally)
NDK ?= $(ANDROID_NDK_HOME)
ANDROID_API ?= 28
NDK_HOST ?= linux-x86_64
ANDROID_TOOLCHAIN := $(NDK)/toolchains/llvm/prebuilt/$(NDK_HOST)/bin

# --- Feature trimming -------------------------------------------------------
# Decenza only ever uses tsnet for Mode A (embedded Tailscale + Funnel, plumbed
# through tailscale.go's Start/Up/Close/Listen/Dial/Loopback + SetServeConfig).
# It never uses SSH, Taildrop, subnet routes, exit nodes, App Connectors, the
# CLI-only update checker, cloud-provider detection, or the outbound SOCKS/HTTP
# proxy. Every tag below dead-code-eliminates one such feature from the linked
# binary. Do NOT add ts_omit_serve or ts_omit_useproxy here: Serve/Funnel is
# the feature this library exists for, and useproxy is needed to reach
# Tailscale's control/DERP servers through corporate HTTP proxies.
# ts_omit_dns in particular touches real surface area (net/dns/*) — verify a
# full connect+Funnel cycle after any binary built with these tags before
# shipping.
#
# Deliberately NOT stripping symbols (-s -w) here: tried in f9667b5 and
# reverted same-day in 14c1f78 — it didn't meaningfully shrink the binary and
# we'd rather keep symbols for crash symbolication.
TS_OMIT_TAGS := ts_omit_ssh,ts_omit_identityfederation,ts_omit_oauthkey,ts_omit_dns,ts_omit_appconnectors,ts_omit_useroutes,ts_omit_advertiseroutes,ts_omit_useexitnode,ts_omit_advertiseexitnode,ts_omit_cloud,ts_omit_clientupdate,ts_omit_outboundproxy

libtailscale.so:
	$(DARWIN_DEPLOYMENT_TARGET) CGO_CFLAGS="$(CGO_CFLAGS) $(DARWIN_CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS) $(DARWIN_CGO_LDFLAGS)" go build -v -tags $(TS_OMIT_TAGS) -buildmode=c-shared -o $@

libtailscale.a:
	$(DARWIN_DEPLOYMENT_TARGET) CGO_CFLAGS="$(CGO_CFLAGS) $(DARWIN_CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS) $(DARWIN_CGO_LDFLAGS)" go build -tags $(TS_OMIT_TAGS) -buildmode=c-archive -o $@

libtailscale_ios.a:
	# TODO(raggi): setup a PREFIX in the libtailscale.a build, then delete these targets, the caller should be setting PREFIX and CC
	# that way the caller can also use the prefix, and not have to specialize target/link object names per build configuration.
	GOOS=ios GOARCH=arm64 CC=$(PWD)/swift/script/clangwrap-ios.sh go build -v -ldflags -w -tags ios,$(TS_OMIT_TAGS) -o $@ -buildmode=c-archive

libtailscale_ios_sim_arm64.a:
	GOOS=ios GOARCH=arm64 CC=$(PWD)/swift/script/clangwrap-ios-sim-arm.sh go build -v -ldflags -w -tags ios,$(TS_OMIT_TAGS) -o $@ -buildmode=c-archive

libtailscale_ios_sim_x86_64.a:
	GOOS=ios GOARCH=amd64 CC=$(PWD)/swift/script/clangwrap-ios-sim-x86.sh go build -v -ldflags -w -tags ios,$(TS_OMIT_TAGS) -o $@ -buildmode=c-archive

.PHONY: c-archive-ios
c-archive-ios: libtailscale_ios.a  ## Builds libtailscale_ios.a for iOS (iOS SDK required)

.PHONY: c-archive-ios-sim
c-archive-ios-sim: libtailscale_ios_sim_arm64.a libtailscale_ios_sim_x86_64.a ## Builds a fat binary for iOS (iOS SDK required)
	lipo -create -output libtailscale_ios_sim.a libtailscale_ios_sim_x86_64.a libtailscale_ios_sim_arm64.a

# --- Android (c-shared .so per ABI, built with the NDK) --------------------
# Each target emits libtailscale.so into an ABI-named dir so the outputs can be
# bundled straight into an Android app's jniLibs/<abi>/ layout. The generated
# header is identical across ABIs; consumers use the hand-written tailscale.h.
android/arm64-v8a/libtailscale.so:
	mkdir -p $(dir $@)
	CGO_ENABLED=1 GOOS=android GOARCH=arm64 \
	  CC=$(ANDROID_TOOLCHAIN)/aarch64-linux-android$(ANDROID_API)-clang \
	  go build -v -ldflags -w -tags $(TS_OMIT_TAGS) -buildmode=c-shared -o $@

android/armeabi-v7a/libtailscale.so:
	mkdir -p $(dir $@)
	CGO_ENABLED=1 GOOS=android GOARCH=arm GOARM=7 \
	  CC=$(ANDROID_TOOLCHAIN)/armv7a-linux-androideabi$(ANDROID_API)-clang \
	  go build -v -ldflags -w -tags $(TS_OMIT_TAGS) -buildmode=c-shared -o $@

android/x86_64/libtailscale.so:
	mkdir -p $(dir $@)
	CGO_ENABLED=1 GOOS=android GOARCH=amd64 \
	  CC=$(ANDROID_TOOLCHAIN)/x86_64-linux-android$(ANDROID_API)-clang \
	  go build -v -ldflags -w -tags $(TS_OMIT_TAGS) -buildmode=c-shared -o $@

.PHONY: android
android: android/arm64-v8a/libtailscale.so android/armeabi-v7a/libtailscale.so android/x86_64/libtailscale.so ## Builds Android c-shared .so for all shipped ABIs (NDK required)

.PHONY: c-archive
c-archive: libtailscale.a  ## Builds libtailscale.a for the target platform

.PHONY: shared
shared: libtailscale.so ## Builds libtailscale.so for the target platform

.PHONY: print-tags
print-tags: ## Print TS_OMIT_TAGS (for CI steps that can't route through this Makefile, e.g. release.yml's lipo'd macOS build)
	@echo $(TS_OMIT_TAGS)

.PHONY: print-env
print-env: ## Print the toolchain/SDK overrides this Makefile applies locally (empty under CI)
	@echo "GOTOOLCHAIN=$(GOTOOLCHAIN)"
	@echo "SDKROOT=$(SDKROOT)"

# Deliberately untagged, matching test.yml's `go test -v ./...`. Building
# ./... with TS_OMIT_TAGS does not compile: tsnetctest and tstestcontrol reach
# tailscale.com/ssh/tailssh, which under ts_omit_ssh fails with
# `*ipnlocal.LocalBackend does not implement ipnLocalBackend (missing method
# GetSSH_HostKeys)`. The tagged configuration is what the release jobs build,
# and that is where it gets exercised.
.PHONY: test
test: ## Run the Go test suite, as test.yml does (linking a test binary needs the SDKROOT pairing above)
	$(DARWIN_DEPLOYMENT_TARGET) CGO_CFLAGS="$(CGO_CFLAGS) $(DARWIN_CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS) $(DARWIN_CGO_LDFLAGS)" go test ./...

.PHONY: clean
clean: ## Clean up build artifacts
	rm -f libtailscale*.h
	rm -f libtailscale*.a
	rm -f libtailscale*.so
	rm -rf android


.PHONY: help
help: ## Show this help
	@echo "\nSpecify a command. The choices are:\n"
	@grep -hE '^[0-9a-zA-Z_-]+:.*?## .*$$' ${MAKEFILE_LIST} | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[0;36m%-12s\033[m %s\n", $$1, $$2}'
	@echo ""

.DEFAULT_GOAL := help
