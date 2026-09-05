#!/bin/sh
# Builds RemoteAU.xcframework (the Go v2 engine compiled for iOS) via gomobile.
#
# Default build includes Opus (libopus is fetched from a pinned source and
# compiled for device + simulator). Set OPUS=0 for a PCM-only dev build:
#
#   ./engine/scripts/build-ios-framework.sh              # Opus
#   OPUS=0 ./engine/scripts/build-ios-framework.sh       # PCM-only dev build
#
# Requirements on macOS: Xcode, go, pkg-config (brew install pkg-config).
set -e

OPUS_VERSION=1.4
OPUS_SHA256=c9b32b4253be5ae63d1ff16eea06b94b5f0f2951b7a02aceef58e3a3ce49c51f
OPUS_URL="https://github.com/xiph/opus/releases/download/v${OPUS_VERSION}/opus-${OPUS_VERSION}.tar.gz"

cd "$(dirname "$0")/.."

go install golang.org/x/mobile/cmd/gomobile@latest
go install golang.org/x/mobile/cmd/gobind@latest
export PATH="$PATH:$(go env GOPATH)/bin"

# Mobile bindings require the bind package.
go get golang.org/x/mobile/bind
gomobile init

mkdir -p ../ios/Frameworks

if [ "${OPUS:-1}" = "0" ]; then
  echo "PCM-only build (OPUS=0): no libopus."
  gomobile bind \
    -target ios,iossimulator \
    -o ../ios/Frameworks/RemoteAU.xcframework \
    -prefix RemoteAU \
    ./mobile
  echo "Built ../ios/Frameworks/RemoteAU.xcframework (PCM-only)"
  exit 0
fi

command -v pkg-config >/dev/null || { echo "pkg-config not found: brew install pkg-config"; exit 1; }

BUILD="$PWD/.build/opus-ios"
rm -rf "$BUILD"
mkdir -p "$BUILD"

# Fetch and verify the pinned Opus source.
if [ ! -f ".build/opus-$OPUS_VERSION.tar.gz" ]; then
  mkdir -p .build
  curl -sSL --retry 3 -o ".build/opus-$OPUS_VERSION.tar.gz" "$OPUS_URL"
fi
echo "$OPUS_SHA256  .build/opus-$OPUS_VERSION.tar.gz" | shasum -a 256 -c -
tar -xf ".build/opus-$OPUS_VERSION.tar.gz" -C .build
SRC="$PWD/.build/opus-$OPUS_VERSION"
CC="$(xcrun -f clang)"

build_opus() {
  # $1=prefix $2=host $3=cflags
  cd "$SRC"
  make distclean >/dev/null 2>&1 || true
  CC="$CC" CFLAGS="$3" ./configure \
    --prefix="$1" --host="$2" \
    --disable-doc --disable-extra-programs \
    --disable-shared --enable-static
  make -j
  make install
}

build_opus "$BUILD/device" aarch64-apple-darwin \
  "-arch arm64 -isysroot $(xcrun --sdk iphoneos --show-sdk-path) -miphoneos-version-min=13.0 -O2"
build_opus "$BUILD/sim" aarch64-apple-darwin \
  "-arch arm64 -isysroot $(xcrun --sdk iphonesimulator --show-sdk-path) -mios-simulator-version-min=13.0 -O2"
build_opus "$BUILD/sim-x86" x86_64-apple-darwin \
  "-arch x86_64 -isysroot $(xcrun --sdk iphonesimulator --show-sdk-path) -mios-simulator-version-min=13.0 -O2"

# Merge the two simulator archives into one fat static lib.
lipo -create "$BUILD/sim/lib/libopus.a" "$BUILD/sim-x86/lib/libopus.a" \
  -output "$BUILD/sim/lib/libopus-fat.a"
mv "$BUILD/sim/lib/libopus-fat.a" "$BUILD/sim/lib/libopus.a"

# Device slice.
PKG_CONFIG_PATH="$BUILD/device/lib/pkgconfig" \
CGO_CFLAGS="-I$BUILD/device/include/opus" \
CGO_LDFLAGS="-L$BUILD/device/lib -lopus" \
gomobile bind -target ios \
  -o "$BUILD/RemoteAU-ios.xcframework" -prefix RemoteAU \
  -tags "opus,nolibopusfile" ./mobile

# Simulator slice (arm64 + x86_64).
PKG_CONFIG_PATH="$BUILD/sim/lib/pkgconfig" \
CGO_CFLAGS="-I$BUILD/sim/include/opus" \
CGO_LDFLAGS="-L$BUILD/sim/lib -lopus" \
gomobile bind -target iossimulator \
  -o "$BUILD/RemoteAU-sim.xcframework" -prefix RemoteAU \
  -tags "opus,nolibopusfile" ./mobile

# Combine into one xcframework.
DEV_FW=$(find "$BUILD/RemoteAU-ios.xcframework" -maxdepth 2 -name RemoteAU.framework)
SIM_FW=$(find "$BUILD/RemoteAU-sim.xcframework" -maxdepth 2 -name RemoteAU.framework)
rm -rf ../ios/Frameworks/RemoteAU.xcframework
xcodebuild -create-xcframework \
  -framework "$DEV_FW" -framework "$SIM_FW" \
  -output ../ios/Frameworks/RemoteAU.xcframework

echo "Built ../ios/Frameworks/RemoteAU.xcframework (Opus $OPUS_VERSION)"
