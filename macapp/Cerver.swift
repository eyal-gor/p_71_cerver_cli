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

// MARK: - Window (sidebar + page, in the style of the ChatGPT Mac app)

enum Page: Hashable {
	case thisMac, peer(String), add
}

struct MainWindow: View {
	@ObservedObject var m: Model
	@State private var page: Page? = .thisMac

	var body: some View {
		NavigationSplitView {
			sidebar
				.navigationSplitViewColumnWidth(min: 210, ideal: 240, max: 300)
		} detail: {
			ZStack {
				Color(nsColor: .textBackgroundColor).ignoresSafeArea()
				detail
			}
		}
		.frame(minWidth: 760, minHeight: 500)
		.onChange(of: m.status?.peers.map(\.ip) ?? []) { ips in
			// The computer on screen left: go back to this Mac.
			if case .peer(let ip) = page, !ips.contains(ip) { page = .thisMac }
		}
	}

	// MARK: Sidebar

	var sidebar: some View {
		VStack(spacing: 0) {
			HStack(spacing: 9) {
				Image(nsImage: logoImage(size: 22))
					.clipShape(RoundedRectangle(cornerRadius: 5))
				Text("Cerver").font(.system(size: 15, weight: .semibold))
				Spacer()
			}
			.padding(.horizontal, 16)
			.padding(.top, 6)
			.padding(.bottom, 10)

			List(selection: $page) {
				Section("This Mac") {
					sidebarRow(name: m.status?.name ?? m.deviceName, dot: m.isOn ? .green : Color.secondary.opacity(0.4), icon: "laptopcomputer")
						.tag(Page.thisMac)
				}
				Section("Your computers") {
					ForEach(m.status?.peers ?? []) { p in
						sidebarRow(name: p.name, dot: dotColor(p), icon: "desktopcomputer")
							.tag(Page.peer(p.ip))
					}
					Label("Add a computer", systemImage: "plus")
						.foregroundStyle(.secondary)
						.tag(Page.add)
				}
			}
			.listStyle(.sidebar)

			Divider()
			HStack(spacing: 10) {
				Circle().fill(m.isOn ? Color.green : Color.secondary.opacity(0.4)).frame(width: 8, height: 8)
				VStack(alignment: .leading, spacing: 1) {
					Text(m.isOn ? "Connected" : "Not connected").font(.system(size: 13, weight: .medium))
					Text(m.status?.ip ?? "This Mac").font(.system(size: 11)).foregroundStyle(.secondary)
				}
				Spacer()
				if m.busy { ProgressView().controlSize(.small) }
				if m.phase != .notSignedIn {
					Toggle("", isOn: Binding(get: { m.isOn || m.phase == .starting }, set: { $0 ? m.turnOn() : m.turnOff() }))
						.toggleStyle(.switch).controlSize(.small).labelsHidden()
						.disabled(m.busy)
				}
			}
			.padding(.horizontal, 16)
			.padding(.vertical, 12)
		}
	}

	func sidebarRow(name: String, dot: Color, icon: String) -> some View {
		HStack(spacing: 8) {
			Image(systemName: icon).foregroundStyle(.secondary).frame(width: 18)
			Text(name).lineLimit(1)
			Spacer()
			Circle().fill(dot).frame(width: 7, height: 7)
		}
	}

	func dotColor(_ p: NetPeer) -> Color {
		p.connected ? (p.via == "relay" ? .orange : .green) : Color.secondary.opacity(0.4)
	}

	// MARK: Pages

	@ViewBuilder var detail: some View {
		if case .failed(let why) = m.phase {
			VStack {
				banner(why)
				Spacer()
			}
		}
		switch m.phase {
		case .notSignedIn:
			welcome(title: "Sign in to Cerver",
				text: "Cerver connects your computers into one private network, so you can reach any of them by name from anywhere.") {
				VStack(spacing: 10) {
					Text("Install Cerver on this Mac and sign in, then come back here.")
						.foregroundStyle(.secondary)
					CommandBox(cmd: "curl -fsSL https://cerver.ai/install.sh | sh", m: m)
				}
			}
		case .on, .working, .starting:
			if let s = m.status {
				switch page ?? .thisMac {
				case .thisMac: ThisMacPage(m: m, s: s)
				case .peer(let ip):
					if let p = s.peers.first(where: { $0.ip == ip }) { PeerPage(m: m, p: p) } else { ThisMacPage(m: m, s: s) }
				case .add: AddPage(m: m)
				}
			} else {
				welcome(title: m.phase == .starting ? "Connecting…" : (m.busy ? "One moment…" : "Connecting…"), text: "This takes a few seconds.") {
					ProgressView()
				}
			}
		default:
			if page == .add {
				AddPage(m: m)
			} else {
				welcome(title: "Reach all your computers from here",
					text: "Cerver connects your computers into one private network, so you can reach any of them by name, at home or away.") {
					VStack(spacing: 10) {
						Button {
							m.turnOn()
						} label: {
							Text("Connect This Mac").font(.system(size: 14, weight: .semibold))
								.padding(.horizontal, 18).padding(.vertical, 8)
						}
						.buttonStyle(.borderedProminent)
						.controlSize(.large)
						.clipShape(Capsule())
						.disabled(m.busy)
						Text("macOS asks for your password once.").font(.callout).foregroundStyle(.secondary)
					}
				}
			}
		}
	}

	func welcome<Extra: View>(title: String, text: String, @ViewBuilder extra: () -> Extra) -> some View {
		VStack(spacing: 18) {
			Image(nsImage: logoImage(size: 56))
				.clipShape(RoundedRectangle(cornerRadius: 13))
			Text(title).font(.system(size: 26, weight: .semibold))
			Text(text)
				.font(.system(size: 15))
				.foregroundStyle(.secondary)
				.multilineTextAlignment(.center)
				.frame(maxWidth: 440)
			extra().padding(.top, 6)
		}
		.padding(40)
		.frame(maxWidth: .infinity, maxHeight: .infinity)
	}

	func banner(_ why: String) -> some View {
		HStack(spacing: 8) {
			Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
			Text(why).font(.callout)
			Spacer()
		}
		.padding(12)
		.background(RoundedRectangle(cornerRadius: 10).fill(Color.orange.opacity(0.1)))
		.padding(.horizontal, 24).padding(.top, 16)
	}
}

/// One page: big title, a line under it, then cards. Centered, readable width.
struct PageLayout<Content: View>: View {
	let title: String
	let subtitle: String
	@ViewBuilder let content: Content

	var body: some View {
		ScrollView {
			VStack(alignment: .leading, spacing: 22) {
				VStack(alignment: .leading, spacing: 6) {
					Text(title).font(.system(size: 28, weight: .semibold))
					Text(subtitle).font(.system(size: 15)).foregroundStyle(.secondary)
				}
				content
			}
			.frame(maxWidth: 620, alignment: .leading)
			.padding(.horizontal, 40)
			.padding(.vertical, 36)
			.frame(maxWidth: .infinity)
		}
	}
}

struct Card<Content: View>: View {
	@ViewBuilder let content: Content
	var body: some View {
		VStack(alignment: .leading, spacing: 0) { content }
			.background(RoundedRectangle(cornerRadius: 14).fill(Color(nsColor: .controlBackgroundColor)))
			.overlay(RoundedRectangle(cornerRadius: 14).stroke(Color.primary.opacity(0.08)))
	}
}

struct Row<Trailing: View>: View {
	let label: String
	@ViewBuilder let trailing: Trailing
	var body: some View {
		HStack {
			Text(label).font(.system(size: 14))
			Spacer()
			trailing
		}
		.padding(.horizontal, 16).padding(.vertical, 12)
	}
}

struct StatusPill: View {
	let text: String
	let color: Color
	var body: some View {
		HStack(spacing: 6) {
			Circle().fill(color).frame(width: 7, height: 7)
			Text(text).font(.system(size: 12, weight: .medium))
		}
		.padding(.horizontal, 10).padding(.vertical, 4)
		.background(Capsule().fill(color.opacity(0.12)))
	}
}

struct CommandBox: View {
	let cmd: String
	@ObservedObject var m: Model
	var body: some View {
		HStack(alignment: .center, spacing: 10) {
			Text(cmd)
				.font(.system(size: 12.5, design: .monospaced))
				.textSelection(.enabled)
				.frame(maxWidth: .infinity, alignment: .leading)
			Button(m.copied == cmd ? "Copied" : "Copy") { m.copy(cmd) }
				.buttonStyle(.bordered).clipShape(Capsule())
		}
		.padding(14)
		.background(RoundedRectangle(cornerRadius: 12).fill(Color.primary.opacity(0.05)))
	}
}

struct ThisMacPage: View {
	@ObservedObject var m: Model
	let s: NetStatus
	@State private var editing = false
	@State private var newName = ""

	var body: some View {
		PageLayout(title: s.name, subtitle: "This Mac. Your other computers reach it at this name.") {
			HStack { StatusPill(text: "Connected", color: .green); Spacer() }
			Card {
				Row(label: "Name") {
					if editing {
						TextField("Name", text: $newName).textFieldStyle(.roundedBorder).frame(width: 180).onSubmit(save)
						Button("Save", action: save).disabled(cleanName(newName).isEmpty)
						Button("Cancel") { editing = false }
					} else {
						Text(s.name).foregroundStyle(.secondary)
						Button("Rename") { newName = s.name; editing = true }.buttonStyle(.link)
					}
				}
				Divider().padding(.leading, 16)
				Row(label: "Address") {
					Text(s.ip).font(.system(size: 13, design: .monospaced)).foregroundStyle(.secondary).textSelection(.enabled)
					Button(m.copied == s.ip ? "Copied" : "Copy") { m.copy(s.ip) }.buttonStyle(.link)
				}
				Divider().padding(.leading, 16)
				Row(label: "Backup relay") {
					Text(s.relay_connected ? "Ready" : "Connecting…").foregroundStyle(.secondary)
				}
			}
			Text("When a network blocks a direct connection, traffic goes through the backup relay. It stays encrypted end to end.")
				.font(.callout).foregroundStyle(.secondary)
			Card {
				Row(label: "Open at login") {
					Toggle("", isOn: Binding(get: { m.openAtLogin }, set: { m.setOpenAtLogin($0) }))
						.toggleStyle(.switch).labelsHidden()
				}
				Divider().padding(.leading, 16)
				Row(label: "Log") {
					Button("Show") { NSWorkspace.shared.open(URL(fileURLWithPath: logFile)) }.buttonStyle(.link)
				}
			}
		}
	}

	func save() {
		let n = cleanName(newName)
		guard !n.isEmpty else { return }
		editing = false
		if n != s.name { m.turnOn(name: n) }
	}
}

struct PeerPage: View {
	@ObservedObject var m: Model
	let p: NetPeer

	var body: some View {
		PageLayout(title: p.name, subtitle: p.connected ? "Reach it from this Mac by its name." : "Cerver is trying to reach it. It may be off or asleep.") {
			HStack {
				StatusPill(text: howConnected(p), color: p.connected ? (p.via == "relay" ? .orange : .green) : .gray)
				Spacer()
			}
			HStack(spacing: 10) {
				Button {
					openSSH(p.name)
				} label: {
					Label("Open SSH in Terminal", systemImage: "terminal").padding(.horizontal, 6).padding(.vertical, 3)
				}
				.buttonStyle(.borderedProminent).controlSize(.large).clipShape(Capsule())
				.disabled(!p.connected)
				Button(m.copied == p.name ? "Copied" : "Copy Name") { m.copy(p.name) }
					.buttonStyle(.bordered).controlSize(.large).clipShape(Capsule())
			}
			Card {
				Row(label: "Name") { Text(p.name).foregroundStyle(.secondary) }
				Divider().padding(.leading, 16)
				Row(label: "Address") {
					Text(p.ip).font(.system(size: 13, design: .monospaced)).foregroundStyle(.secondary).textSelection(.enabled)
					Button(m.copied == p.ip ? "Copied" : "Copy") { m.copy(p.ip) }.buttonStyle(.link)
				}
				Divider().padding(.leading, 16)
				Row(label: "Connection") { Text(howConnected(p)).foregroundStyle(.secondary) }
			}
			Text("Use the name anywhere you'd use an address: ssh \(p.name), http://\(p.name):3000, and so on.")
				.font(.callout).foregroundStyle(.secondary)
		}
	}
}

struct AddPage: View {
	@ObservedObject var m: Model
	var body: some View {
		PageLayout(title: "Add a computer", subtitle: "Any Mac or Linux computer, at home, at work or in the cloud.") {
			VStack(alignment: .leading, spacing: 10) {
				Text("1.  Open a terminal on that computer and run:").font(.system(size: 14))
				CommandBox(cmd: addDeviceCommand, m: m)
			}
			Text("2.  It shows up here within a few seconds, and you can reach it by its name.")
				.font(.system(size: 14))
			Text("It signs in with your Cerver account, so only your computers join.")
				.font(.callout).foregroundStyle(.secondary)
		}
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
		.windowStyle(.hiddenTitleBar)
		.windowToolbarStyle(.unified(showsTitle: false))
		.defaultSize(width: 920, height: 620)
		.defaultPosition(.center)

		MenuBarExtra {
			MenuView(m: m)
		} label: {
			Image(nsImage: logoImage(size: 16, gray: !m.isOn, gap: 1))
		}
		.menuBarExtraStyle(.menu)
	}
}
