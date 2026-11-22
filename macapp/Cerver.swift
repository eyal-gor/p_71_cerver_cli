// Cerver — the Mac app. For now it connects this Mac to your other computers (`cerver net`).
//
// It doesn't run the network itself: the system service (launchd
// ai.cerver.net, the bundled `cerver` binary) does. The app turns that
// service on and off through the normal macOS password prompt, and shows
// what it's doing by reading /var/run/cerver-net.json, which the service
// rewrites every few seconds.
//
// A window (opens at launch and from the menu bar) and a menu bar icon.
// Build: ./build.sh  →  build/Cerver.app

import AppKit
import ServiceManagement
import SwiftUI

let statusFile = "/var/run/cerver-net.json"
let logFile = "/var/log/cerver-net.log"
let addDeviceCommand = "curl -fsSL https://cerver.ai/install.sh | sh && sudo ~/.cerver/bin/cerver net install"

// MARK: - The logo (Cerver's coloured squares, as on cerver.ai)

let logoColors: [[UInt32]] = [
	[0x667cf9, 0x857cf5, 0xb07cef, 0xc477b2],
	[0x578fe4, 0x667cf9, 0xc477b2, 0xd77275],
	[0x50b6b6, 0x61c996, 0xeca74b, 0xe8864b],
	[0x61c996, 0x9dc977, 0xf0c84c, 0xeca74b],
]

func rgb(_ v: UInt32, gray: Bool) -> NSColor {
	let r = CGFloat((v >> 16) & 0xff) / 255, g = CGFloat((v >> 8) & 0xff) / 255, b = CGFloat(v & 0xff) / 255
	if gray {
		let l = 0.3 * r + 0.59 * g + 0.11 * b
		return NSColor(white: l, alpha: 0.55)
	}
	return NSColor(red: r, green: g, blue: b, alpha: 1)
}

/// The grid as an image; gray when the network is off.
func logoImage(size: CGFloat, gray: Bool = false, gap: CGFloat = 0) -> NSImage {
	let img = NSImage(size: NSSize(width: size, height: size), flipped: true) { rect in
		let cell = (rect.width - gap * 3) / 4
		for (y, row) in logoColors.enumerated() {
			for (x, c) in row.enumerated() {
				rgb(c, gray: gray).setFill()
				NSBezierPath(rect: NSRect(x: CGFloat(x) * (cell + gap), y: CGFloat(y) * (cell + gap), width: cell, height: cell)).fill()
			}
		}
		return true
	}
	img.isTemplate = false
	return img
}

// MARK: - Status from the service

struct NetPeer: Decodable, Identifiable {
	let name: String
	let ip: String
	let connected: Bool
	let via: String
	var id: String { ip }
}

struct NetStatus: Decodable {
	let name: String
	let ip: String
	let relay_connected: Bool
	let updated: Double
	let peers: [NetPeer]
}

enum Phase: Equatable {
	case notSignedIn, off, starting, on, working(String), failed(String)
}

@MainActor
final class Model: ObservableObject {
	@Published var status: NetStatus?
	@Published var phase: Phase = .off
	@Published var openAtLogin = SMAppService.mainApp.status == .enabled
	@Published var copied: String?

	private var timer: Timer?
	private var wantOnUntil: Date?

	var deviceName: String {
		get { UserDefaults.standard.string(forKey: "name") ?? defaultName() }
		set { UserDefaults.standard.set(newValue, forKey: "name") }
	}

	var isOn: Bool { phase == .on }
	var busy: Bool {
		switch phase {
		case .working, .starting: return true
		default: return false
		}
	}

	init() {
		refresh()
		timer = Timer.scheduledTimer(withTimeInterval: 2, repeats: true) { _ in
			Task { @MainActor in self.refresh() }
		}
	}

	func refresh() {
		let signedIn = FileManager.default.fileExists(atPath: NSHomeDirectory() + "/.cerver/cerver.env")
		var fresh: NetStatus?
		if let data = FileManager.default.contents(atPath: statusFile),
			let s = try? JSONDecoder().decode(NetStatus.self, from: data),
			Date().timeIntervalSince1970 - s.updated < 30
		{
			fresh = s
		}
		status = fresh
		if case .working = phase { return }
		if let s = fresh, wantOnUntil == nil || s.name == deviceName {
			deviceName = s.name
			phase = .on
			wantOnUntil = nil
		} else if let until = wantOnUntil, Date() < until {
			phase = .starting
		} else if case .failed = phase {
			// keep showing the error until the next action
		} else {
			wantOnUntil = nil
			phase = signedIn ? .off : .notSignedIn
		}
	}

	func turnOn(name: String? = nil) {
		guard let bin = Bundle.main.path(forResource: "cerver", ofType: nil) else {
			phase = .failed("The app is missing its cerver program. Download the app again.")
			return
		}
		if let n = name { deviceName = n }
		let cmd = "SUDO_USER=\(shq(NSUserName())) \(shq(bin)) net install --name \(shq(deviceName))"
		runAsAdmin(cmd, doing: name == nil ? "Connecting…" : "Renaming…") { ok in
			if ok { self.wantOnUntil = Date().addingTimeInterval(45) }
		}
	}

	func turnOff() {
		let bin = FileManager.default.fileExists(atPath: "/usr/local/lib/cerver/cerver")
			? "/usr/local/lib/cerver/cerver" : (Bundle.main.path(forResource: "cerver", ofType: nil) ?? "")
		runAsAdmin("SUDO_USER=\(shq(NSUserName())) \(shq(bin)) net uninstall", doing: "Disconnecting…") { _ in }
	}

	/// Runs a shell command as admin through the standard macOS password prompt.
	private func runAsAdmin(_ shell: String, doing: String, then: @escaping (Bool) -> Void) {
		phase = .working(doing)
		let script = "do shell script \"\(shell.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\""))\" with administrator privileges"
		Task.detached {
			let p = Process()
			p.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
			p.arguments = ["-e", script]
			let err = Pipe()
			p.standardError = err
			p.standardOutput = Pipe()
			try? p.run()
			p.waitUntilExit()
			let msg = String(data: err.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
			let ok = p.terminationStatus == 0
			await MainActor.run {
				if ok || msg.contains("-128") {  // -128: the person pressed Cancel
					self.phase = .off
				} else {
					self.phase = .failed(cleanError(msg))
				}
				then(ok)
				self.refresh()
			}
		}
	}

	func setOpenAtLogin(_ on: Bool) {
		do {
			if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
		} catch {
			phase = .failed("Couldn't change Open at Login: \(error.localizedDescription)")
		}
		openAtLogin = SMAppService.mainApp.status == .enabled
	}

	func copy(_ s: String) {
		NSPasteboard.general.clearContents()
		NSPasteboard.general.setString(s, forType: .string)
		copied = s
		Task { @MainActor in
			try? await Task.sleep(nanoseconds: 1_500_000_000)
			if self.copied == s { self.copied = nil }
		}
	}
}

func shq(_ s: String) -> String { "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'" }

/// Device names become host names (ssh <name>): lowercase letters, digits, dashes.
func cleanName(_ raw: String) -> String {
	let mapped = raw.lowercased().map { $0.isASCII && ($0.isLetter || $0.isNumber) ? $0 : "-" }
	return String(mapped).split(separator: "-").joined(separator: "-")
}

func defaultName() -> String {
	// Installed before (by the app or `sudo cerver net install --name X`): keep that name.
	if let plist = NSDictionary(contentsOfFile: "/Library/LaunchDaemons/ai.cerver.net.plist"),
		let args = plist["ProgramArguments"] as? [String],
		let i = args.firstIndex(of: "--name"), i + 1 < args.count
	{
		return args[i + 1]
	}
	let s = cleanName(Host.current().localizedName ?? ProcessInfo.processInfo.hostName)
	return s.isEmpty ? "mac" : s
}

func cleanError(_ s: String) -> String {
	// osascript wraps the command's error: "...: execution error: <text> (1)"
	var t = s.trimmingCharacters(in: .whitespacesAndNewlines)
	if let r = t.range(of: "execution error: ") { t = String(t[r.upperBound...]) }
	if let r = t.range(of: " (", options: .backwards) { t = String(t[..<r.lowerBound]) }
	return t.isEmpty ? "Something went wrong." : t
}

func openSSH(_ host: String) {
	let script = "tell application \"Terminal\" to do script \"ssh \(host)\"\ntell application \"Terminal\" to activate"
	let p = Process()
	p.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
	p.arguments = ["-e", script]
	try? p.run()
}

func howConnected(_ p: NetPeer) -> String {
	if !p.connected { return "Not reachable" }
	return p.via == "relay" ? "Through Cerver relay" : "Direct"
}

// MARK: - Window

struct MainWindow: View {
	@ObservedObject var m: Model
	@State private var editingName = false
	@State private var newName = ""

	var body: some View {
		VStack(alignment: .leading, spacing: 0) {
			header
			Divider()
			ScrollView {
				VStack(alignment: .leading, spacing: 18) {
					content
				}
				.padding(20)
				.frame(maxWidth: .infinity, alignment: .leading)
			}
		}
		.frame(minWidth: 460, idealWidth: 500, minHeight: 420, idealHeight: 480)
	}

	var header: some View {
		HStack(spacing: 12) {
			Image(nsImage: logoImage(size: 28, gray: !m.isOn))
				.clipShape(RoundedRectangle(cornerRadius: 6))
			VStack(alignment: .leading, spacing: 2) {
				Text("Cerver").font(.title2.weight(.semibold))
				Text(subtitle).font(.callout).foregroundStyle(.secondary)
			}
			Spacer()
			if m.busy {
				ProgressView().controlSize(.small)
			}
			if m.phase != .notSignedIn {
				Toggle("", isOn: Binding(get: { m.isOn || m.phase == .starting }, set: { $0 ? m.turnOn() : m.turnOff() }))
					.toggleStyle(.switch)
					.labelsHidden()
					.disabled(m.busy)
			}
		}
		.padding(20)
	}

	var subtitle: String {
		switch m.phase {
		case .notSignedIn: return "Sign in to Cerver first"
		case .off: return "Not connected. Your other computers can't reach this Mac."
		case .starting: return "Starting…"
		case .working(let w): return w
		case .failed: return "Something went wrong"
		case .on: return "Connected. Your other computers can reach this Mac."
		}
	}

	@ViewBuilder var content: some View {
		if case .failed(let why) = m.phase {
			Label(why, systemImage: "exclamationmark.triangle.fill")
				.foregroundStyle(.orange)
		}
		switch m.phase {
		case .notSignedIn:
			Text("Cerver connects your computers into one private network, so you can reach any of them by name from anywhere.")
			Text("To start, install Cerver on this Mac and sign in:")
			commandBox("curl -fsSL https://cerver.ai/install.sh | sh")
			Text("Then come back here and turn it on.").foregroundStyle(.secondary)
		case .on:
			if let s = m.status {
				thisMac(s)
				devices(s)
			}
		default:
			Text("Cerver connects your computers into one private network, so you can reach any of them by name from anywhere, at home or away.")
			Text("Connect this Mac to start. macOS will ask for your password once.")
				.foregroundStyle(.secondary)
			Button("Connect This Mac") { m.turnOn() }
				.buttonStyle(.borderedProminent)
				.disabled(m.busy)
		}
	}

	func thisMac(_ s: NetStatus) -> some View {
		GroupBox {
			VStack(alignment: .leading, spacing: 8) {
				HStack {
					if editingName {
						TextField("Name", text: $newName)
							.textFieldStyle(.roundedBorder)
							.frame(maxWidth: 220)
							.onSubmit(saveName)
						Button("Save", action: saveName).disabled(cleanName(newName).isEmpty)
						Button("Cancel") { editingName = false }
					} else {
						Text(s.name).font(.headline)
						Button("Rename") {
							newName = s.name
							editingName = true
						}
						.buttonStyle(.link)
					}
					Spacer()
				}
				HStack(spacing: 16) {
					Text(s.ip).font(.system(.body, design: .monospaced)).textSelection(.enabled)
					Text(s.relay_connected ? "Backup relay ready" : "Backup relay connecting…")
						.font(.caption).foregroundStyle(.secondary)
				}
			}
			.padding(6)
			.frame(maxWidth: .infinity, alignment: .leading)
		} label: {
			Text("This Mac")
		}
	}

	func saveName() {
		let n = cleanName(newName)
		guard !n.isEmpty else { return }
		editingName = false
		if n != m.status?.name { m.turnOn(name: n) }
	}

	@ViewBuilder func devices(_ s: NetStatus) -> some View {
		Text("Your other computers").font(.headline)
		if s.peers.isEmpty {
			VStack(alignment: .leading, spacing: 8) {
				Text("No other computers yet. To add one (Mac or Linux), run this on it:")
					.foregroundStyle(.secondary)
				commandBox(addDeviceCommand)
			}
		} else {
			VStack(spacing: 0) {
				ForEach(s.peers) { p in
					peerRow(p)
					if p.id != s.peers.last?.id { Divider() }
				}
			}
			.background(RoundedRectangle(cornerRadius: 8).fill(Color(nsColor: .controlBackgroundColor)))
			.overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.secondary.opacity(0.2)))
			Text("Reach them by name from this Mac, for example: ssh \(s.peers[0].name)")
				.font(.caption).foregroundStyle(.secondary)
		}
	}

	func peerRow(_ p: NetPeer) -> some View {
		HStack(spacing: 10) {
			Circle().fill(p.connected ? (p.via == "relay" ? Color.orange : Color.green) : Color.secondary.opacity(0.4))
				.frame(width: 9, height: 9)
			VStack(alignment: .leading, spacing: 2) {
				Text(p.name).font(.body.weight(.medium))
				Text("\(p.ip) · \(howConnected(p))").font(.caption).foregroundStyle(.secondary)
			}
			Spacer()
			Button(m.copied == p.name ? "Copied" : "Copy Name") { m.copy(p.name) }
			Button("SSH") { openSSH(p.name) }
		}
		.padding(.horizontal, 12)
		.padding(.vertical, 9)
	}

	func commandBox(_ cmd: String) -> some View {
		HStack(alignment: .top) {
			Text(cmd)
				.font(.system(.callout, design: .monospaced))
				.textSelection(.enabled)
				.frame(maxWidth: .infinity, alignment: .leading)
			Button(m.copied == cmd ? "Copied" : "Copy") { m.copy(cmd) }
		}
		.padding(10)
		.background(RoundedRectangle(cornerRadius: 8).fill(Color(nsColor: .textBackgroundColor)))
		.overlay(RoundedRectangle(cornerRadius: 8).stroke(Color.secondary.opacity(0.2)))
	}
}

// MARK: - Menu bar

struct MenuView: View {
	@ObservedObject var m: Model
	@Environment(\.openWindow) private var openWindow

	var body: some View {
		switch m.phase {
		case .on:
			if let s = m.status {
				Text("This Mac: \(s.name) · \(s.ip)")
				if s.peers.isEmpty {
					Text("No other computers yet")
				}
				ForEach(s.peers) { p in
					Menu("\(p.connected ? "●" : "○")  \(p.name)   \(howConnected(p))") {
						Button("Copy Name (\(p.name))") { m.copy(p.name) }
						Button("Copy Address (\(p.ip))") { m.copy(p.ip) }
						Button("Open SSH in Terminal") { openSSH(p.name) }
					}
				}
				Divider()
				Button("Disconnect This Mac") { m.turnOff() }
			}
		case .off, .failed:
			Text("This Mac isn’t connected")
			Button("Connect This Mac") { m.turnOn() }
		case .notSignedIn:
			Text("Cerver: sign in first")
		case .starting:
			Text("Starting…")
		case .working(let what):
			Text(what)
		}
		Divider()
		Button("Open Cerver…") { show() }
		Button("Show Log") { NSWorkspace.shared.open(URL(fileURLWithPath: logFile)) }
		Toggle("Open at Login", isOn: Binding(get: { m.openAtLogin }, set: { m.setOpenAtLogin($0) }))
		Button("Quit") { NSApp.terminate(nil) }
	}

	func show() {
		openWindow(id: "main")
		NSApp.activate(ignoringOtherApps: true)
	}
}

@main
struct CerverApp: App {
	@StateObject private var m = Model()

	var body: some Scene {
		Window("Cerver", id: "main") {
			MainWindow(m: m)
				.onAppear { NSApp.activate(ignoringOtherApps: true) }
		}
		.windowResizability(.contentSize)
		.defaultPosition(.center)

		MenuBarExtra {
			MenuView(m: m)
		} label: {
			Image(nsImage: logoImage(size: 16, gray: !m.isOn, gap: 1))
		}
		.menuBarExtraStyle(.menu)
	}
}
