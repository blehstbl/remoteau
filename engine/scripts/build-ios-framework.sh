#!/bin/sh
# Builds RemoteAU.xcframework (the Go v2 engine compiled for iOS) via gomobile.
# Run on macOS:  brew install go && ./engine/scripts/build-ios-framework.sh
set -e

cd "$(dirname "$0")/.."

go install golang.org/x/mobile/cmd/gomobile@latest
go install golang.org/x/mobile/cmd/gobind@latest
export PATH="$PATH:$(go env GOPATH)/bin"

# Mobile bindings require the bind package.
go get golang.org/x/mobile/bind
gomobile init

mkdir -p ../ios/Frameworks
gomobile bind \
  -target ios,iossimulator \
  -o ../ios/Frameworks/RemoteAU.xcframework \
  -prefix RemoteAU \
  ./mobile

echo "Built ../ios/Frameworks/RemoteAU.xcframework"
