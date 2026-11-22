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
		refreshRelay()
		timer = Timer.scheduledTimer(withTimeInterval: 2, repeats: true) { _ in
			Task { @MainActor in
				self.refresh()
				self.refreshRelay()
				self.refreshPool()
			}
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

	// MARK: The relay (this Mac in your pool, so agents can run on it)

	@Published var relay: RelayState = .off
	@Published var relayName = ""
	private var relayBusy = false

	/// Reads the relay service's state from launchd (it runs as you, so no password).
	func refreshRelay() {
		if relayBusy { return }
		Task.detached {
			let uid = getuid()
			let new = launchctlState("gui/\(uid)/ai.cerver.relay")
			let old = launchctlState("gui/\(uid)/dev.cerver.relay")  // set up by hand before the app
			let name = relayPoolName()
			await MainActor.run {
				if self.relayBusy { return }
				self.relayName = name
				if new == "running" || old == "running" {
					self.relay = .running
				} else if new != nil {
					self.relay = .starting  // installed; launchd restarts it on its own
				} else if case .failed = self.relay {
				} else {
					self.relay = .off
				}
			}
		}
	}

	// What Cerver itself sees of this Mac (online, free or busy, agents running).
	@Published var poolMachine: PoolMachine?
	@Published var poolChecked = false
	private var lastPoolFetch = Date.distantPast

	func refreshPool(force: Bool = false) {
		if !force && Date().timeIntervalSince(lastPoolFetch) < 8 { return }
		lastPoolFetch = Date()
		let name = relayName
		Task.detached {
			let m = await fetchPoolMachine(named: name)
			await MainActor.run {
				self.poolChecked = true
				self.poolMachine = m
			}
		}
	}

	func relayOn() { runRelay(["relay", "install"], doing: "Adding this Mac to your pool…") }
	func relayOff() { runRelay(["relay", "uninstall"], doing: "Taking this Mac out of your pool…") }

	private func runRelay(_ args: [String], doing: String) {
		guard let bin = Bundle.main.path(forResource: "cerver", ofType: nil) else {
			relay = .failed("The app is missing its cerver program. Download the app again.")
			return
		}
		relayBusy = true
		relay = .working(doing)
		Task.detached {
			let p = Process()
			p.executableURL = URL(fileURLWithPath: bin)
			p.arguments = args
			var env = ProcessInfo.processInfo.environment
			env["PATH"] = loginShellPath()  // so the relay finds uv, claude and codex like your terminal does
			p.environment = env
			let out = Pipe()
			p.standardOutput = out
			p.standardError = out
			try? p.run()
			p.waitUntilExit()
			let msg = String(data: out.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
			let ok = p.terminationStatus == 0
			await MainActor.run {
				self.relayBusy = false
				self.relay = ok ? .off : .failed(msg.replacingOccurrences(of: "error: ", with: "").trimmingCharacters(in: .whitespacesAndNewlines))
				self.refreshRelay()
				self.refreshPool(force: true)
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

struct PoolMachine: Decodable {
	let name: String
	let online: Bool
	let state: String?  // free / busy / offline
	let running: Int?
	let max: Int?
	let mine: Bool?
}

/// Asks Cerver (GET /v2/pool, with your key from ~/.cerver/cerver.env) how it
/// sees the machine with this name.
func fetchPoolMachine(named name: String) async -> PoolMachine? {
	let envPath = NSHomeDirectory() + "/.cerver/cerver.env"
	guard let text = try? String(contentsOfFile: envPath, encoding: .utf8) else { return nil }
	var env: [String: String] = [:]
	for line in text.split(separator: "\n") {
		let parts = line.split(separator: "=", maxSplits: 1).map { String($0).trimmingCharacters(in: .whitespaces) }
		if parts.count == 2 { env[parts[0]] = parts[1].trimmingCharacters(in: CharacterSet(charactersIn: "\"'")) }
	}
	guard let key = env["CERVER_API_KEY"], !key.isEmpty else { return nil }
	let base = env["CERVER_GATEWAY_URL"].flatMap { $0.isEmpty ? nil : $0 } ?? "https://gateway.cerver.ai"
	guard let url = URL(string: base + "/v2/pool") else { return nil }
	var req = URLRequest(url: url, timeoutInterval: 15)
	req.setValue("Bearer " + key, forHTTPHeaderField: "Authorization")
	struct Pool: Decodable { let machines: [PoolMachine]? }
	guard let (data, resp) = try? await URLSession.shared.data(for: req),
		(resp as? HTTPURLResponse)?.statusCode == 200,
		let pool = try? JSONDecoder().decode(Pool.self, from: data)
	else { return nil }
	return pool.machines?.first { $0.name == name && ($0.mine ?? true) }
}

enum RelayState: Equatable {
	case off, starting, running, working(String), failed(String)
}

/// "running", "waiting" etc. for a launchd job, or nil when it isn't loaded.
func launchctlState(_ target: String) -> String? {
	let p = Process()
	p.executableURL = URL(fileURLWithPath: "/bin/launchctl")
	p.arguments = ["print", target]
	let out = Pipe()
	p.standardOutput = out
	p.standardError = Pipe()
	guard (try? p.run()) != nil else { return nil }
	let data = out.fileHandleForReading.readDataToEndOfFile()
	p.waitUntilExit()
	if p.terminationStatus != 0 { return nil }
	let text = String(data: data, encoding: .utf8) ?? ""
	for line in text.split(separator: "\n") {
		let t = line.trimmingCharacters(in: .whitespaces)
		if t.hasPrefix("state = ") { return String(t.dropFirst("state = ".count)) }
	}
	return "loaded"
}

/// The name this Mac has in your pool (from the relay service's settings).
func relayPoolName() -> String {
	for label in ["ai.cerver.relay", "dev.cerver.relay"] {
		let path = NSHomeDirectory() + "/Library/LaunchAgents/\(label).plist"
		if let plist = NSDictionary(contentsOfFile: path), let args = plist["ProgramArguments"] as? [String],
			let i = args.firstIndex(of: "--name"), i + 1 < args.count
		{
			return args[i + 1]
		}
	}
	return ProcessInfo.processInfo.hostName.replacingOccurrences(of: ".local", with: "")
}

/// The PATH your terminal has (apps started from the Dock get a bare one).
func loginShellPath() -> String {
	let shell = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
	let p = Process()
	p.executableURL = URL(fileURLWithPath: shell)
	p.arguments = ["-ilc", "printf '__PATH__%s' \"$PATH\""]
	let out = Pipe()
	p.standardOutput = out
	p.standardError = Pipe()
	p.standardInput = FileHandle.nullDevice
	if (try? p.run()) != nil {
		let data = out.fileHandleForReading.readDataToEndOfFile()
		p.waitUntilExit()
		if let s = String(data: data, encoding: .utf8), let r = s.range(of: "__PATH__", options: .backwards) {
			let path = String(s[r.upperBound...]).trimmingCharacters(in: .whitespacesAndNewlines)
			if !path.isEmpty { return path }
		}
	}
	let home = NSHomeDirectory()
	return "\(home)/.local/bin:\(home)/.cerver/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
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
		.frame(minWidth: 760, minHeight: 540)
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
					sidebarRow(name: m.relayName, dot: thisMacDot, icon: "laptopcomputer")
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
		}
	}

	/// Green when everything you turned on is working, orange while starting, gray when all off.
	var thisMacDot: Color {
		let relayOn = m.relay == .running
		if m.busy || m.relay == .starting { return .orange }
		if m.isOn || relayOn { return .green }
		return Color.secondary.opacity(0.4)
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
		if m.phase == .notSignedIn {
			VStack(spacing: 18) {
				Image(nsImage: logoImage(size: 56)).clipShape(RoundedRectangle(cornerRadius: 13))
				Text("Sign in to Cerver").font(.system(size: 26, weight: .semibold))
				Text("Install Cerver on this Mac and sign in, then come back here.")
					.font(.system(size: 15)).foregroundStyle(.secondary)
				CommandBox(cmd: "curl -fsSL https://cerver.ai/install.sh | sh", m: m).frame(maxWidth: 460)
			}
			.padding(40)
			.frame(maxWidth: .infinity, maxHeight: .infinity)
		} else {
			switch page ?? .thisMac {
			case .thisMac: ThisMacPage(m: m)
			case .peer(let ip):
				if let p = m.status?.peers.first(where: { $0.ip == ip }) { PeerPage(m: m, p: p) } else { ThisMacPage(m: m) }
			case .add: AddPage(m: m)
			}
		}
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

/// Everything about this Mac on one page: can your other computers reach it
/// (the network), and can your agents run on it (the relay, in your pool).
struct ThisMacPage: View {
	@ObservedObject var m: Model
	@State private var editing = false
	@State private var newName = ""

	var body: some View {
		PageLayout(title: m.relayName, subtitle: "This Mac") {
			if case .failed(let why) = m.phase { problem(why) }
			if case .failed(let why) = m.relay { problem(why) }

			section("Agents can run on it", detail: "Puts this Mac in your Cerver pool. Agents run as you, with your Claude and Codex logins.")
			Card {
				Row(label: "Agents can run here") {
					if case .working = m.relay { ProgressView().controlSize(.small) }
					Toggle("", isOn: Binding(get: { relayOn }, set: { $0 ? m.relayOn() : m.relayOff() }))
						.toggleStyle(.switch).labelsHidden()
						.disabled(relayWorking)
				}
				Divider().padding(.leading, 16)
				Row(label: "Status") { StatusPill(text: relayText.0, color: relayText.1) }
				if let pm = m.poolMachine, pm.online {
					Divider().padding(.leading, 16)
					Row(label: "Agents running now") {
						Text(pm.max.map { "\(pm.running ?? 0) of \($0)" } ?? "\(pm.running ?? 0)").foregroundStyle(.secondary)
					}
				}
				Divider().padding(.leading, 16)
				Row(label: "Log") {
					Button("Show") {
						NSWorkspace.shared.open(URL(fileURLWithPath: NSHomeDirectory() + "/.cerver/relay.log"))
					}.buttonStyle(.link)
				}
			}

			section("Your other computers can reach it", detail: "A private network between your computers. Reach this Mac from them by its name.")
			Card {
				Row(label: "Reachable") {
					if m.busy { ProgressView().controlSize(.small) }
					Toggle("", isOn: Binding(get: { m.isOn || m.phase == .starting }, set: { $0 ? m.turnOn() : m.turnOff() }))
						.toggleStyle(.switch).labelsHidden()
						.disabled(m.busy)
				}
				if let s = m.status {
					Divider().padding(.leading, 16)
					Row(label: "Name") {
						if editing {
							TextField("Name", text: $newName).textFieldStyle(.roundedBorder).frame(width: 180).onSubmit(save)
							Button("Save", action: save).disabled(cleanName(newName).isEmpty)
							Button("Cancel") { editing = false }
						} else {
							Text(s.name).font(.system(size: 13, design: .monospaced)).foregroundStyle(.secondary)
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
				} else if m.phase == .off {
					Divider().padding(.leading, 16)
					Row(label: "Turning it on asks for your Mac password once") { EmptyView() }
				}
			}

			Card {
				Row(label: "Open Cerver at login") {
					Toggle("", isOn: Binding(get: { m.openAtLogin }, set: { m.setOpenAtLogin($0) }))
						.toggleStyle(.switch).labelsHidden()
				}
			}
		}
	}

	var relayWorking: Bool { if case .working = m.relay { return true }; return false }
	var relayOn: Bool { m.relay == .running || m.relay == .starting }

	/// What the relay is doing, as Cerver sees it when it can tell.
	var relayText: (String, Color) {
		switch m.relay {
		case .working(let w): return (w, .orange)
		case .failed: return ("Couldn't do that", .red)
		case .off: return ("Off", .gray)
		case .starting: return ("Starting…", .orange)
		case .running:
			guard let pm = m.poolMachine else { return (m.poolChecked ? "Running, connecting to Cerver…" : "Running", .orange) }
			if !pm.online { return ("Running, connecting to Cerver…", .orange) }
			if pm.state == "busy" { return ("Online, busy", .green) }
			return ("Online, ready for agents", .green)
		}
	}

	func section(_ title: String, detail: String) -> some View {
		VStack(alignment: .leading, spacing: 3) {
			Text(title).font(.system(size: 15, weight: .semibold))
			Text(detail).font(.callout).foregroundStyle(.secondary)
		}
		.padding(.top, 6)
	}

	func problem(_ why: String) -> some View {
		HStack(spacing: 8) {
			Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
			Text(why).font(.callout).textSelection(.enabled)
			Spacer()
		}
		.padding(12)
		.background(RoundedRectangle(cornerRadius: 10).fill(Color.orange.opacity(0.1)))
	}

	func save() {
		let n = cleanName(newName)
		guard !n.isEmpty else { return }
		editing = false
		if n != m.status?.name { m.turnOn(name: n) }
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
		switch m.relay {
		case .running, .starting:
			Text(m.relay == .running ? "Agents can run on this Mac" : "Agents: starting…")
			Button("Stop Agents on This Mac") { m.relayOff() }
		case .off, .failed:
			Text("Agents can't run on this Mac")
			Button("Let Agents Run on This Mac") { m.relayOn() }
		case .working(let w):
			Text(w)
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
