#!/usr/bin/env bash
# Rebuild the gomobile AAR that android/app consumes.
#
# Run from the repository root. Everything here is derived state: the AAR is a
# build artifact and is not checked in.
set -euo pipefail

: "${ANDROID_HOME:?set ANDROID_HOME to your Android SDK}"
NDK_VERSION="${NDK_VERSION:-27.2.12479018}"
export ANDROID_NDK_HOME="${ANDROID_NDK_HOME:-$ANDROID_HOME/ndk/$NDK_VERSION}"

# gomobile refuses to bind inside a go.work workspace because the tool module
# resolution differs from a single-module build.
export GOWORK=off

if [ ! -d "$ANDROID_NDK_HOME" ]; then
  echo "NDK not found at $ANDROID_NDK_HOME" >&2
  echo "install it with: sdkmanager 'ndk;$NDK_VERSION'" >&2
  exit 1
fi

OUT="${1:-android/app/libs/tdl-bridge.aar}"
mkdir -p "$(dirname "$OUT")"

# Version stamped into the Go core.
#
# mobile.Version is what Status() reports and what the UI shows. It was never
# injected, so every build said "dev" while the APK's versionName (hardcoded in
# app/build.gradle) said something else — the two can disagree and there is no
# way to tell which binary a device is running. Derive it from the tag and let
# VERSION override it for a manual build.
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}"
echo "version: $VERSION"

# Licence verifier material.
#
# The master key never enters the repository or this script: it is read from the
# file named by TDL_LICENSE_MASTER_FILE, and only the derived HMAC key and public
# key are compiled in. Without that variable the AAR carries no verifier, which
# means the app ships ungated — correct for a development build, wrong for a
# release, so say so out loud instead of failing silently.
LICENSE_FLAGS=""
if [ -n "${TDL_LICENSE_LDFLAGS:-}" ]; then
  LICENSE_FLAGS="$TDL_LICENSE_LDFLAGS"
elif [ -n "${TDL_LICENSE_MASTER_FILE:-}" ]; then
  LICENSE_FLAGS="$(go run . license verifier --key-file "$TDL_LICENSE_MASTER_FILE" | tr '\n' ' ')"
  echo "licence: using master key from $TDL_LICENSE_MASTER_FILE"
else
  echo "licence: TDL_LICENSE_MASTER_FILE is unset — this AAR will NOT enforce licensing" >&2
fi

# Online activation endpoint baked into the app.
#
# Without it the customer has to be told the server address and type it; with it
# the activation screen only asks for the key. Offline-only builds leave it
# unset, and then the screen falls back to whatever the user types.
if [ -n "${TDL_LICENSE_SERVER:-}" ]; then
  LICENSE_FLAGS="$LICENSE_FLAGS -X github.com/iyear/tdl/archive.licenceServerDefault=${TDL_LICENSE_SERVER}"
  echo "licence: online activation via ${TDL_LICENSE_SERVER}"
fi

# -s -w strips DWARF: 104 MB -> 81 MB. Gradle strips the packaged copy again,
# but a smaller input keeps the build cache and uploads manageable.
gomobile bind \
  -target=android/arm64 \
  -androidapi 24 \
  -javapkg=com.tdl.archive \
  -ldflags="-s -w -X github.com/iyear/tdl/mobile.Version=$VERSION $LICENSE_FLAGS" \
  -o "$OUT" \
  ./mobile

ls -lh "$OUT"
