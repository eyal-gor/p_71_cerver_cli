// Draws the app icon (Cerver's coloured squares in a macOS rounded square)
// into an .iconset folder; build.sh turns it into AppIcon.icns.
// Usage: swift make-icon.swift <out.iconset>
import AppKit

let colors: [[UInt32]] = [
	[0x667cf9, 0x857cf5, 0xb07cef, 0xc477b2],
	[0x578fe4, 0x667cf9, 0xc477b2, 0xd77275],
	[0x50b6b6, 0x61c996, 0xeca74b, 0xe8864b],
	[0x61c996, 0x9dc977, 0xf0c84c, 0xeca74b],
]

func color(_ v: UInt32) -> NSColor {
	NSColor(red: CGFloat((v >> 16) & 0xff) / 255, green: CGFloat((v >> 8) & 0xff) / 255, blue: CGFloat(v & 0xff) / 255, alpha: 1)
}

func png(_ px: Int) -> Data {
	let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: px, pixelsHigh: px, bitsPerSample: 8, samplesPerPixel: 4,
		hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
	NSGraphicsContext.saveGraphicsState()
	NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
	let s = CGFloat(px)
	let inset = s * 0.1  // Apple's icon grid leaves a margin around the shape
	let box = NSRect(x: inset, y: inset, width: s - 2 * inset, height: s - 2 * inset)
	NSBezierPath(roundedRect: box, xRadius: box.width * 0.225, yRadius: box.width * 0.225).addClip()
	let cell = box.width / 4
	for (row, line) in colors.enumerated() {
		for (col, c) in line.enumerated() {
			color(c).setFill()
			// row 0 is the top; AppKit's y grows upward
			NSRect(x: box.minX + CGFloat(col) * cell, y: box.maxY - CGFloat(row + 1) * cell, width: cell + 0.5, height: cell + 0.5).fill()
		}
	}
	NSGraphicsContext.restoreGraphicsState()
	return rep.representation(using: .png, properties: [:])!
}

let out = CommandLine.arguments[1]
try? FileManager.default.createDirectory(atPath: out, withIntermediateDirectories: true)
for base in [16, 32, 128, 256, 512] {
	try! png(base).write(to: URL(fileURLWithPath: "\(out)/icon_\(base)x\(base).png"))
	try! png(base * 2).write(to: URL(fileURLWithPath: "\(out)/icon_\(base)x\(base)@2x.png"))
}
