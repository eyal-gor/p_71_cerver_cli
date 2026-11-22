#!/bin/sh
# Builds "Cerver.app" (menu bar app + the cerver program inside it) into
# macapp/build/. Works on Apple Silicon and Intel. Signed ad hoc: fine on the
# Mac that built it; giving it to others needs a Developer ID signature.
set -e
cd "$(dirname "$0")"
APP="build/Cerver.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp Info.plist "$APP/Contents/Info.plist"
# Releases pass VERSION (1.0.N); a local build keeps Info.plist's.
VERSION="${VERSION:-}"
if [ -n "$VERSION" ]; then
	plutil -replace CFBundleShortVersionString -string "$VERSION" "$APP/Contents/Info.plist"
	plutil -replace CFBundleVersion -string "$VERSION" "$APP/Contents/Info.plist"
fi
LDFLAGS="-X github.com/eyal-gor/p_71_cerver_cli/internal/version.Version=${VERSION:-dev}"
swift make-icon.swift build/AppIcon.iconset
iconutil -c icns build/AppIcon.iconset -o "$APP/Contents/Resources/AppIcon.icns"
rm -rf build/AppIcon.iconset

for arch in arm64 x86_64; do
	swiftc -parse-as-library -O -target "$arch-apple-macos13.0" Cerver.swift -o "build/app-$arch"
done
lipo -create build/app-arm64 build/app-x86_64 -output "$APP/Contents/MacOS/Cerver"

(cd .. && GOARCH=arm64 go build -ldflags "$LDFLAGS" -o macapp/build/cerver-arm64 ./cmd/cerver && GOARCH=amd64 go build -ldflags "$LDFLAGS" -o macapp/build/cerver-amd64 ./cmd/cerver)
lipo -create build/cerver-arm64 build/cerver-amd64 -output "$APP/Contents/Resources/cerver"
rm -f build/app-* build/cerver-*

codesign --force --sign - "$APP/Contents/Resources/cerver"
codesign --force --sign - "$APP"
echo "Built $APP"
