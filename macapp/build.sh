#!/bin/sh
# Builds "Cerver Net.app" (menu bar app + the cerver program inside it) into
# macapp/build/. Works on Apple Silicon and Intel. Signed ad hoc: fine on the
# Mac that built it; giving it to others needs a Developer ID signature.
set -e
cd "$(dirname "$0")"
APP="build/Cerver Net.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp Info.plist "$APP/Contents/Info.plist"
swift make-icon.swift build/AppIcon.iconset
iconutil -c icns build/AppIcon.iconset -o "$APP/Contents/Resources/AppIcon.icns"
rm -rf build/AppIcon.iconset

for arch in arm64 x86_64; do
	swiftc -parse-as-library -O -target "$arch-apple-macos13.0" CerverNet.swift -o "build/CerverNet-$arch"
done
lipo -create build/CerverNet-arm64 build/CerverNet-x86_64 -output "$APP/Contents/MacOS/CerverNet"

(cd .. && GOARCH=arm64 go build -o macapp/build/cerver-arm64 ./cmd/cerver && GOARCH=amd64 go build -o macapp/build/cerver-amd64 ./cmd/cerver)
lipo -create build/cerver-arm64 build/cerver-amd64 -output "$APP/Contents/Resources/cerver"
rm -f build/CerverNet-* build/cerver-*

codesign --force --sign - "$APP/Contents/Resources/cerver"
codesign --force --sign - "$APP"
echo "Built $APP"
