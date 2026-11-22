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

	// Your computers as Cerver sees them: the pool (agents, shared) and the
	// network (reachable), joined by name.
	@Published var computers: [Computer] = []
	@Published var poolChecked = false
	@Published var sharing: Set<String> = []  // compute ids being switched right now
	private var lastPoolFetch = Date.distantPast

	/// This Mac in the pool, when Cerver knows it.
	var poolMachine: PoolMachine? { computers.first { $0.isThisMac }?.pool }

	func refreshPool(force: Bool = false) {
		if !force && Date().timeIntervalSince(lastPoolFetch) < 8 { return }
		lastPoolFetch = Date()
		let me = relayName
		let netMe = status?.name
		Task.detached {
			guard let list = await fetchComputers(thisMac: me, thisMacNet: netMe) else {
				await MainActor.run { self.poolChecked = true }
				return
			}
			await MainActor.run {
				self.poolChecked = true
				self.computers = list
			}
		}
	}

	/// Lets people in your pool use this computer (or stops it).
	func setShared(_ c: Computer, _ on: Bool) {
		guard let id = c.pool?.compute_id, let bin = Bundle.main.path(forResource: "cerver", ofType: nil) else { return }
		sharing.insert(id)
		Task.detached {
			let p = Process()
			p.executableURL = URL(fileURLWithPath: bin)
			p.arguments = ["pool", on ? "share" : "unshare", id]
			p.standardOutput = Pipe()
			p.standardError = Pipe()
			try? p.run()
			p.waitUntilExit()
			await MainActor.run {
				self.sharing.remove(id)
				self.refreshPool(force: true)
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
	let compute_id: String?
	let name: String
	let online: Bool
	let state: String?  // free / busy / offline
	let running: Int?
	let max: Int?
	let mine: Bool?
	let shared: Bool?
}

/// What a computer has, as its relay reported it to Cerver.
struct Spec: Decodable {
	struct GPU: Decodable { let name: String?; let memory_gb: Double? }
	let ref: String
	let os: String?
	let arch: String?
	let cpu_cores: Int?
	let ram_gb: Double?
	let disk_free_gb: Double?
	let gpus: [GPU]?
	let tools: [String]?
	let coding_agents: [String]?
	let desktop: Bool?
	let reported: Bool?

	var system: String? {
		guard let os = os else { return nil }
		let name = ["macos": "macOS", "linux": "Linux", "windows": "Windows"][os] ?? os
		guard let a = arch else { return name }
		return name + (a == "arm64" ? (os == "macos" ? " · Apple silicon" : " · ARM") : " · Intel/AMD")
	}

	/// "10 cores · 24 GB" for the table.
	var short: String? {
		var parts: [String] = []
		if let c = cpu_cores { parts.append("\(c) cores") }
		if let r = ram_gb { parts.append(gb(r)) }
		return parts.isEmpty ? nil : parts.joined(separator: " · ")
	}
}

func gb(_ v: Double) -> String { v >= 10 ? "\(Int(v.rounded())) GB" : String(format: "%.1f GB", v) }

struct NetNode: Decodable {
	let name: String
	let ip: String
	let online: Bool
}

/// One of your computers: its place in the pool and/or on the network.
struct Computer: Identifiable {
	let name: String
	let pool: PoolMachine?
	let net: NetNode?
	let isThisMac: Bool
	var spec: Spec? = nil
	var id: String { pool?.compute_id ?? "net:" + (net?.ip ?? name) }
}

/// Your computers: pool machines that are yours plus network devices, matched
/// by name (the network uses the lowercase form: MacBook-Air-2026 → macbook-air-2026).
func fetchComputers(thisMac: String, thisMacNet: String?) async -> [Computer]? {
	struct Pool: Decodable { let machines: [PoolMachine]?; let computers: [Spec]? }
	struct Net: Decodable { let nodes: [NetNode]? }
	guard let poolData = await gatewayGET("/v2/pool"), let pool = try? JSONDecoder().decode(Pool.self, from: poolData) else { return nil }
	let nodes = (await gatewayGET("/v2/net")).flatMap { try? JSONDecoder().decode(Net.self, from: $0) }?.nodes ?? []
	var out: [Computer] = []
	var used = Set<String>()
	for m in (pool.machines ?? []) where m.mine ?? true {
		let me = m.name == thisMac
		let node = nodes.first { !used.contains($0.ip) && ($0.name == cleanName(m.name) || (me && $0.name == thisMacNet)) }
		if let n = node { used.insert(n.ip) }
		let spec = pool.computers?.first { $0.ref == m.compute_id && $0.reported == true }
		out.append(Computer(name: m.name, pool: m, net: node, isThisMac: me, spec: spec))
	}
	for n in nodes where !used.contains(n.ip) {
		out.append(Computer(name: n.name, pool: nil, net: n, isThisMac: n.name == thisMacNet))
	}
	// This Mac first, then online ones, then by name.
	return out.sorted {
		if $0.isThisMac != $1.isThisMac { return $0.isThisMac }
		let a = ($0.pool?.online ?? false) || ($0.net?.online ?? false), b = ($1.pool?.online ?? false) || ($1.net?.online ?? false)
		if a != b { return a }
		return $0.name.lowercased() < $1.name.lowercased()
	}
}

/// GET from the Cerver gateway with your key from ~/.cerver/cerver.env.
func gatewayGET(_ path: String) async -> Data? {
	let envPath = NSHomeDirectory() + "/.cerver/cerver.env"
	guard let text = try? String(contentsOfFile: envPath, encoding: .utf8) else { return nil }
	var env: [String: String] = [:]
	for line in text.split(separator: "\n") {
		let parts = line.split(separator: "=", maxSplits: 1).map { String($0).trimmingCharacters(in: .whitespaces) }
		if parts.count == 2 { env[parts[0]] = parts[1].trimmingCharacters(in: CharacterSet(charactersIn: "\"'")) }
	}
	guard let key = env["CERVER_API_KEY"], !key.isEmpty else { return nil }
	let base = env["CERVER_GATEWAY_URL"].flatMap { $0.isEmpty ? nil : $0 } ?? "https://gateway.cerver.ai"
	guard let url = URL(string: base + path) else { return nil }
	var req = URLRequest(url: url, timeoutInterval: 15)
	req.setValue("Bearer " + key, forHTTPHeaderField: "Authorization")
	guard let (data, resp) = try? await URLSession.shared.data(for: req), (resp as? HTTPURLResponse)?.statusCode == 200 else { return nil }
	return data
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
	case all, thisMac, computer(String), add
}

struct MainWindow: View {
	@ObservedObject var m: Model
	// `-startPage thisMac` on launch opens that page (for screenshots).
	@State private var page: Page? = UserDefaults.standard.string(forKey: "startPage") == "thisMac" ? .thisMac : .all

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
		.onChange(of: m.computers.map(\.id)) { ids in
			// The computer on screen left: go back to the list.
			if case .computer(let id) = page, !ids.contains(id) { page = .all }
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
				Label("All computers", systemImage: "square.grid.2x2")
					.tag(Page.all)
				Section("This Mac") {
					sidebarRow(name: m.relayName, dot: thisMacDot, icon: "laptopcomputer")
						.tag(Page.thisMac)
				}
				Section("Your computers") {
					ForEach(m.computers.filter { !$0.isThisMac }) { c in
						sidebarRow(name: c.name, dot: computerDot(c), icon: "desktopcomputer")
							.tag(Page.computer(c.id))
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

	func computerDot(_ c: Computer) -> Color {
		if c.pool?.online == true || c.net?.online == true { return c.pool?.state == "busy" ? .orange : .green }
		return Color.secondary.opacity(0.4)
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
			switch page ?? .all {
			case .all: AllComputersPage(m: m, open: { page = $0 })
			case .thisMac: ThisMacPage(m: m)
			case .computer(let id):
				if let c = m.computers.first(where: { $0.id == id }) { ComputerPage(m: m, c: c) } else { AllComputersPage(m: m, open: { page = $0 }) }
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

// MARK: Computer status words (shared by the list and each computer's page)

func agentsStatus(_ c: Computer) -> (String, Color) {
	guard let p = c.pool else { return ("Not in pool", .gray) }
	if !p.online { return ("Offline", .gray) }
	if p.state == "busy" { return ("Busy", .orange) }
	return ("Ready", .green)
}

func networkStatus(_ c: Computer, peer: NetPeer?) -> (String, Color) {
	guard let n = c.net else { return ("Off", .gray) }
	if c.isThisMac { return n.online ? ("On", .green) : ("Off", .gray) }
	if let p = peer, p.connected { return p.via == "relay" ? ("On, via relay", .orange) : ("On, direct", .green) }
	return n.online ? ("On", .green) : ("Off", .gray)
}

func sharedStatus(_ c: Computer) -> (String, Color) {
	guard let p = c.pool else { return ("—", .gray) }
	return p.shared == true ? ("Yes", .blue) : ("Only you", .gray)
}

/// What's on a computer: hardware, coding agents, tools.
struct SpecSection: View {
	let spec: Spec?
	var showTitle = true

	var body: some View {
		if showTitle { Text("What's on it").font(.system(size: 15, weight: .semibold)).padding(.top, 6) }
		if let s = spec {
			Card {
				rows(s)
			}
		} else {
			Text("Not reported yet. A computer tells Cerver what it has when its relay starts.")
				.font(.callout).foregroundStyle(.secondary)
		}
	}

	@ViewBuilder func rows(_ s: Spec) -> some View {
		let items: [(String, String)] = [
			("System", s.system),
			("Processor", s.cpu_cores.map { "\($0) cores" }),
			("Memory", s.ram_gb.map(gb)),
			("Free disk", s.disk_free_gb.map(gb)),
			("Graphics", (s.gpus ?? []).compactMap { g in g.name.map { n in g.memory_gb.map { "\(n), \(gb($0))" } ?? n } }.joined(separator: ", ").nilIfEmpty),
			("Screen", s.desktop.map { $0 ? "Yes, agents can use the desktop" : "No, terminal only" }),
			("Coding agents", (s.coding_agents ?? []).joined(separator: ", ").nilIfEmpty),
			("Tools", (s.tools ?? []).joined(separator: ", ").nilIfEmpty),
		].compactMap { k, v in v.map { (k, $0) } }
		ForEach(Array(items.enumerated()), id: \.offset) { i, kv in
			if i > 0 { Divider().padding(.leading, 16) }
			HStack(alignment: .firstTextBaseline) {
				Text(kv.0).font(.system(size: 14))
				Spacer(minLength: 24)
				Text(kv.1).foregroundStyle(.secondary).multilineTextAlignment(.trailing).textSelection(.enabled)
			}
			.padding(.horizontal, 16).padding(.vertical, 11)
		}
	}
}

extension String {
	var nilIfEmpty: String? { isEmpty ? nil : self }
}

/// Every computer in one table: can agents run on it, is it on the network,
/// can people in your pool use it.
struct AllComputersPage: View {
	@ObservedObject var m: Model
	let open: (Page) -> Void
	@State private var view = UserDefaults.standard.string(forKey: "startView") ?? "Computers"
	@State private var help = false

	var body: some View {
		PageLayout(title: "Your computers", subtitle: "Where your agents can run.") {
			HStack {
				Picker("", selection: $view) {
					Text("Computers").tag("Computers")
					Text("Coding agents").tag("Coding agents")
					Text("Map").tag("Map")
				}
				.pickerStyle(.segmented).labelsHidden().fixedSize()
				Spacer()
				Button { help.toggle() } label: { Image(systemName: "questionmark.circle") }
					.buttonStyle(.borderless).help("What these mean")
					.popover(isPresented: $help) {
						VStack(alignment: .leading, spacing: 6) {
							legend("Agents", "Ready: agents can run on it now. Busy: running as many as it takes. Offline: its relay isn't running.")
							legend("Network", "On: your computers reach it by name. Direct means computer to computer; via relay means through Cerver's backup relay.")
							legend("Others can use it", "People you invited to your pool can run agents on it.")
						}
						.padding(16).frame(width: 360)
					}
			}
			if m.computers.isEmpty {
				Text(m.poolChecked ? "No computers yet." : "Loading…").foregroundStyle(.secondary)
			} else if view == "Map" {
				NetworkMap(m: m, open: open)
			} else if view == "Coding agents" {
				harnessTable
			} else {
				Card {
					Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 0) {
						GridRow {
							header("Computer")
							header("Agents")
							header("Network")
							header("Others can use it")
						}
						.padding(.horizontal, 16).padding(.vertical, 10)
						ForEach(m.computers) { c in
							Divider().gridCellColumns(4)
							GridRow {
								HStack(spacing: 8) {
									Image(systemName: c.isThisMac ? "laptopcomputer" : "desktopcomputer").foregroundStyle(.secondary)
									VStack(alignment: .leading, spacing: 1) {
										Text(c.name).font(.system(size: 14, weight: .medium)).lineLimit(1)
										let sub = ([c.isThisMac ? "This Mac" : nil, c.spec?.short].compactMap { $0 }).joined(separator: " · ")
										if !sub.isEmpty {
											Text(sub).font(.system(size: 11)).foregroundStyle(.secondary).lineLimit(1)
										}
									}
									.layoutPriority(1)
								}
								StatusPill(text: agentsStatus(c).0, color: agentsStatus(c).1)
								StatusPill(text: networkStatus(c, peer: peer(c)).0, color: networkStatus(c, peer: peer(c)).1)
								StatusPill(text: sharedStatus(c).0, color: sharedStatus(c).1)
							}
							.padding(.horizontal, 16).padding(.vertical, 9)
							.contentShape(Rectangle())
							.onTapGesture { open(c.isThisMac ? .thisMac : .computer(c.id)) }
						}
					}
				}
			}
		}
	}

	func peer(_ c: Computer) -> NetPeer? { m.status?.peers.first { $0.ip == c.net?.ip } }

	/// Which coding agents (harnesses) each computer has, as its relay reported.
	@ViewBuilder var harnessTable: some View {
		let all = Array(Set(m.computers.flatMap { $0.spec?.coding_agents ?? [] })).sorted { harnessOrder($0) < harnessOrder($1) }
		if all.isEmpty {
			Text("No computer has reported its coding agents yet.").font(.callout).foregroundStyle(.secondary)
		} else {
			Card {
				Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 0) {
					GridRow {
						header("Computer")
						ForEach(all, id: \.self) { h in header(h).gridColumnAlignment(.center) }
					}
					.padding(.horizontal, 16).padding(.vertical, 10)
					ForEach(m.computers) { c in
						Divider().gridCellColumns(all.count + 1)
						GridRow {
							Text(c.name).font(.system(size: 14, weight: .medium)).lineLimit(1).fixedSize()
							if let have = c.spec?.coding_agents {
								ForEach(all, id: \.self) { h in
									Image(systemName: have.contains(h) ? "checkmark.circle.fill" : "minus")
										.foregroundStyle(have.contains(h) ? Color.green : Color.secondary.opacity(0.5))
								}
							} else {
								Text("Not reported yet").font(.system(size: 12)).foregroundStyle(.secondary)
									.gridCellColumns(all.count)
							}
						}
						.padding(.horizontal, 16).padding(.vertical, 9)
					}
				}
			}
		}
	}

	func harnessOrder(_ h: String) -> String {
		let known = ["claude", "codex", "grok", "gemini", "gemma", "opencode"]
		return known.firstIndex(of: h).map { String(format: "%02d", $0) } ?? "99" + h
	}

	func header(_ t: String) -> some View {
		Text(t).font(.system(size: 12, weight: .semibold)).foregroundStyle(.secondary)
	}

	func legend(_ title: String, _ text: String) -> some View {
		(Text(title + ": ").fontWeight(.semibold) + Text(text)).font(.callout).foregroundStyle(.secondary)
	}
}

// MARK: Tabs for a computer's page

enum Tab: String, CaseIterable, Identifiable {
	case overview = "Overview", agents = "Agents", network = "Network", hardware = "Hardware", settings = "Settings"
	var id: String { rawValue }
}

/// Segmented tabs under the title (like System Settings), only the ones given.
struct Tabs: View {
	@Binding var tab: Tab
	let tabs: [Tab]
	var body: some View {
		Picker("", selection: $tab) {
			ForEach(tabs) { Text($0.rawValue).tag($0) }
		}
		.pickerStyle(.segmented)
		.labelsHidden()
		.fixedSize()
		.padding(.bottom, 4)
	}
}

func logRow(_ label: String, _ path: String) -> some View {
	Row(label: label) {
		Button("Show") { NSWorkspace.shared.open(URL(fileURLWithPath: path)) }.buttonStyle(.link)
	}
}

/// This Mac: the two switches up front; details in tabs.
struct ThisMacPage: View {
	@ObservedObject var m: Model
	@State private var tab: Tab = .overview
	@State private var editing = false
	@State private var newName = ""

	var me: Computer? { m.computers.first(where: { $0.isThisMac }) }

	var body: some View {
		PageLayout(title: m.relayName, subtitle: "This Mac") {
			Tabs(tab: $tab, tabs: [.overview, .agents, .network, .hardware, .settings])
			if case .failed(let why) = m.phase { problem(why) }
			if case .failed(let why) = m.relay { problem(why) }
			switch tab {
			case .overview: overview
			case .agents: agents
			case .network: network
			case .hardware: SpecSection(spec: me?.spec, showTitle: false)
			case .settings: settings
			}
		}
	}

	var overview: some View {
		Card {
			Row(label: "Agents can run here") {
				StatusPill(text: relayText.0, color: relayText.1)
				if relayWorking { ProgressView().controlSize(.small) }
				Toggle("", isOn: Binding(get: { relayOn }, set: { $0 ? m.relayOn() : m.relayOff() }))
					.toggleStyle(.switch).labelsHidden().disabled(relayWorking)
			}
			Divider().padding(.leading, 16)
			Row(label: "Your computers can reach it") {
				StatusPill(text: m.isOn ? "On" : (m.busy ? "…" : "Off"), color: m.isOn ? .green : .gray)
				if m.busy { ProgressView().controlSize(.small) }
				Toggle("", isOn: Binding(get: { m.isOn || m.phase == .starting }, set: { $0 ? m.turnOn() : m.turnOff() }))
					.toggleStyle(.switch).labelsHidden().disabled(m.busy)
			}
			if let me = me, let p = me.pool {
				Divider().padding(.leading, 16)
				Row(label: "Others can use it") {
					StatusPill(text: sharedStatus(me).0, color: sharedStatus(me).1)
					if let id = p.compute_id, m.sharing.contains(id) { ProgressView().controlSize(.small) }
					Toggle("", isOn: Binding(get: { p.shared == true }, set: { m.setShared(me, $0) }))
						.toggleStyle(.switch).labelsHidden()
						.disabled(p.compute_id.map { m.sharing.contains($0) } ?? true)
				}
			}
		}
	}

	@ViewBuilder var agents: some View {
		Text("Puts this Mac in your Cerver pool. Agents run as you, with your Claude and Codex logins.")
			.font(.callout).foregroundStyle(.secondary)
		Card {
			Row(label: "Status") { StatusPill(text: relayText.0, color: relayText.1) }
			if let pm = m.poolMachine, pm.online {
				Divider().padding(.leading, 16)
				Row(label: "Agents running now") {
					Text(pm.max.map { "\(pm.running ?? 0) of \($0)" } ?? "\(pm.running ?? 0)").foregroundStyle(.secondary)
				}
			}
			if let me = me, let p = me.pool {
				Divider().padding(.leading, 16)
				Row(label: "Others in your pool can use it") {
					if let id = p.compute_id, m.sharing.contains(id) { ProgressView().controlSize(.small) }
					Toggle("", isOn: Binding(get: { p.shared == true }, set: { m.setShared(me, $0) }))
						.toggleStyle(.switch).labelsHidden()
						.disabled(p.compute_id.map { m.sharing.contains($0) } ?? true)
				}
			}
		}
	}

	@ViewBuilder var network: some View {
		Text("A private network between your computers. Reach this Mac from them by its name.")
			.font(.callout).foregroundStyle(.secondary)
		if let s = m.status {
			Card {
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
				Row(label: "Backup relay") { Text(s.relay_connected ? "Ready" : "Connecting…").foregroundStyle(.secondary) }
			}
		} else {
			Text("Off. Turn it on in Overview.").font(.callout).foregroundStyle(.secondary)
		}
	}

	var settings: some View {
		Card {
			Row(label: "Open Cerver at login") {
				Toggle("", isOn: Binding(get: { m.openAtLogin }, set: { m.setOpenAtLogin($0) }))
					.toggleStyle(.switch).labelsHidden()
			}
			Divider().padding(.leading, 16)
			logRow("Agents log", NSHomeDirectory() + "/.cerver/relay.log")
			Divider().padding(.leading, 16)
			logRow("Network log", logFile)
		}
	}

	var relayWorking: Bool { if case .working = m.relay { return true }; return false }
	var relayOn: Bool { m.relay == .running || m.relay == .starting }

	var relayText: (String, Color) {
		switch m.relay {
		case .working: return ("…", .orange)
		case .failed: return ("Problem", .red)
		case .off: return ("Off", .gray)
		case .starting: return ("Starting", .orange)
		case .running:
			guard let pm = m.poolMachine, pm.online else { return ("Connecting", .orange) }
			return pm.state == "busy" ? ("Busy", .orange) : ("Ready", .green)
		}
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

/// One of your other computers: status and SSH up front; details in tabs.
struct ComputerPage: View {
	@ObservedObject var m: Model
	let c: Computer
	@State private var tab: Tab = .overview

	var peer: NetPeer? { m.status?.peers.first { $0.ip == c.net?.ip } }

	var body: some View {
		PageLayout(title: c.name, subtitle: subtitle) {
			Tabs(tab: $tab, tabs: [.overview, .agents, .network, .hardware])
			switch tab {
			case .overview: overview
			case .agents: agents
			case .network: network
			case .hardware, .settings: SpecSection(spec: c.spec, showTitle: false)
			}
		}
		.id(c.id)  // a different computer starts on Overview
	}

	@ViewBuilder var overview: some View {
		Card {
			Row(label: "Agents") { StatusPill(text: agentsStatus(c).0, color: agentsStatus(c).1) }
			Divider().padding(.leading, 16)
			Row(label: "Network") { StatusPill(text: networkStatus(c, peer: peer).0, color: networkStatus(c, peer: peer).1) }
			Divider().padding(.leading, 16)
			Row(label: "Others can use it") { StatusPill(text: sharedStatus(c).0, color: sharedStatus(c).1) }
		}
		if let n = c.net, let p = peer, p.connected {
			HStack(spacing: 10) {
				Button {
					openSSH(n.name)
				} label: {
					Label("Open SSH in Terminal", systemImage: "terminal").padding(.horizontal, 6).padding(.vertical, 3)
				}
				.buttonStyle(.borderedProminent).controlSize(.large).clipShape(Capsule())
				Button(m.copied == n.name ? "Copied" : "Copy Name") { m.copy(n.name) }
					.buttonStyle(.bordered).controlSize(.large).clipShape(Capsule())
			}
		}
	}

	@ViewBuilder var agents: some View {
		Card {
			Row(label: "Status") { StatusPill(text: agentsStatus(c).0, color: agentsStatus(c).1) }
			if let p = c.pool, p.online {
				Divider().padding(.leading, 16)
				Row(label: "Agents running now") {
					Text(p.max.map { "\(p.running ?? 0) of \($0)" } ?? "\(p.running ?? 0)").foregroundStyle(.secondary)
				}
			}
			if let p = c.pool {
				Divider().padding(.leading, 16)
				Row(label: "Others in your pool can use it") {
					if let id = p.compute_id, m.sharing.contains(id) { ProgressView().controlSize(.small) }
					Toggle("", isOn: Binding(get: { p.shared == true }, set: { m.setShared(c, $0) }))
						.toggleStyle(.switch).labelsHidden()
						.disabled(p.compute_id.map { m.sharing.contains($0) } ?? true)
				}
			}
		}
		if c.pool == nil {
			Text("Not in your pool. On that computer, run the Cerver install.").font(.callout).foregroundStyle(.secondary)
		} else if c.pool?.online == false {
			Text("Its relay isn't running. Turn the computer on, or run the Cerver install on it again.").font(.callout).foregroundStyle(.secondary)
		}
	}

	@ViewBuilder var network: some View {
		Card {
			Row(label: "Status") { StatusPill(text: networkStatus(c, peer: peer).0, color: networkStatus(c, peer: peer).1) }
			if let n = c.net {
				Divider().padding(.leading, 16)
				Row(label: "Name") { Text(n.name).font(.system(size: 13, design: .monospaced)).foregroundStyle(.secondary) }
				Divider().padding(.leading, 16)
				Row(label: "Address") {
					Text(n.ip).font(.system(size: 13, design: .monospaced)).foregroundStyle(.secondary).textSelection(.enabled)
					Button(m.copied == n.ip ? "Copied" : "Copy") { m.copy(n.ip) }.buttonStyle(.link)
				}
			}
		}
		if c.net == nil {
			Text("Not on your network. On that computer, run the Cerver install.").font(.callout).foregroundStyle(.secondary)
		}
	}

	var subtitle: String {
		if c.pool?.online == true || c.net?.online == true { return "Online" }
		return "Offline. It may be off or asleep."
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


// MARK: Network map

/// A picture of your network: your computers along the bottom, Cerver above.
/// Green between two computers: they talk directly. Orange dashed through
/// the backup relay: they talk through Cerver. Grey from a computer up to
/// Cerver: its agents connection (where agent runs arrive).
struct NetworkMap: View {
	@ObservedObject var m: Model
	let open: (Page) -> Void

	let height: CGFloat = 400

	var me: Computer? { m.computers.first { $0.isThisMac } }
	var others: [Computer] { m.computers.filter { !$0.isThisMac } }
	/// This Mac in the middle, the others around it.
	var ordered: [Computer] {
		var o = others
		if let me = me { o.insert(me, at: o.count / 2) }
		return o
	}

	func peer(_ c: Computer) -> NetPeer? { m.status?.peers.first { $0.ip == c.net?.ip } }

	var body: some View {
		VStack(alignment: .leading, spacing: 12) {
			GeometryReader { g in
				let w = g.size.width
				let cloud = CGPoint(x: w / 2, y: 62)
				let agentsPort = CGPoint(x: cloud.x - 70, y: cloud.y + 34)
				let relayPort = CGPoint(x: cloud.x + 70, y: cloud.y + 34)
				let row: CGFloat = 270
				let pos: [String: CGPoint] = Dictionary(uniqueKeysWithValues: ordered.enumerated().map { i, c in
					(c.id, CGPoint(x: w * CGFloat(i + 1) / CGFloat(ordered.count + 1), y: row))
				})
				ZStack {
					Canvas { ctx, _ in
						// Agents connection: every computer up to Cerver.
						for c in ordered {
							guard let p = pos[c.id], c.pool != nil else { continue }
							var path = Path()
							path.move(to: CGPoint(x: p.x, y: p.y - 44))
							path.addQuadCurve(to: agentsPort, control: CGPoint(x: p.x, y: agentsPort.y + 40))
							let on = c.pool?.online == true
							ctx.stroke(path, with: .color(on ? Color.secondary.opacity(0.55) : Color.secondary.opacity(0.25)),
								style: StrokeStyle(lineWidth: 1.5, dash: on ? [] : [3, 4]))
						}
						guard let me = me, let mp = pos[me.id] else { return }
						// This Mac's line to the backup relay (standing by, or carrying traffic).
						if m.status?.relay_connected == true {
							var path = Path()
							path.move(to: CGPoint(x: mp.x + 20, y: mp.y - 44))
							path.addQuadCurve(to: relayPort, control: CGPoint(x: mp.x + 20, y: relayPort.y + 40))
							ctx.stroke(path, with: .color(.orange.opacity(0.5)), style: StrokeStyle(lineWidth: 1.5, dash: [5, 4]))
						}
						// Each computer this Mac talks to on the network.
						for c in others {
							guard let p = pos[c.id], let pr = peer(c), pr.connected else { continue }
							if pr.via == "relay" {
								var path = Path()
								path.move(to: CGPoint(x: p.x, y: p.y - 44))
								path.addQuadCurve(to: relayPort, control: CGPoint(x: p.x, y: relayPort.y + 40))
								ctx.stroke(path, with: .color(.orange), style: StrokeStyle(lineWidth: 2.5, dash: [6, 4]))
							} else {
								var path = Path()
								let a = CGPoint(x: mp.x, y: mp.y + 44), b = CGPoint(x: p.x, y: p.y + 44)
								path.move(to: a)
								path.addQuadCurve(to: b, control: CGPoint(x: (a.x + b.x) / 2, y: a.y + 70))
								ctx.stroke(path, with: .color(.green), style: StrokeStyle(lineWidth: 3, lineCap: .round))
							}
						}
					}

					cloudNode.position(cloud)

					ForEach(ordered) { c in
						if let p = pos[c.id] {
							node(c).position(p)
								.onTapGesture { open(c.isThisMac ? .thisMac : .computer(c.id)) }
						}
					}

					// Labels on the direct links.
					if let me = me, let mp = pos[me.id] {
						ForEach(others) { c in
							if let p = pos[c.id], let pr = peer(c), pr.connected, pr.via != "relay" {
								Text("direct").font(.system(size: 11, weight: .semibold)).foregroundStyle(.green)
									.padding(.horizontal, 6).padding(.vertical, 1)
									.background(Capsule().fill(Color(nsColor: .windowBackgroundColor)))
									.position(x: (mp.x + p.x) / 2, y: row + 44 + 35)
							}
						}
					}
				}
			}
			.frame(height: height)
			.background(RoundedRectangle(cornerRadius: 14).fill(Color(nsColor: .controlBackgroundColor)))
			.overlay(RoundedRectangle(cornerRadius: 14).stroke(Color.primary.opacity(0.08)))

			HStack(spacing: 18) {
				key(Color.green, solid: true, "Direct")
				key(Color.orange, solid: false, "Through network backup")
				key(Color.secondary, solid: true, "Relay ↔ Cerver"
			}
			Text("Lines show what this Mac sees. Click a computer to open it.")
				.font(.callout).foregroundStyle(.secondary)
		}
	}

	var cloudNode: some View {
		VStack(spacing: 6) {
			HStack(spacing: 6) {
				Image(systemName: "cloud.fill").foregroundStyle(.blue)
				Text("Cerver").font(.system(size: 14, weight: .semibold))
			}
			HStack(spacing: 8) {
				chip("Agent runs", "bolt.horizontal.fill", .secondary)
				chip(m.status?.relay_connected == true ? "Network backup · ready" : "Network backup", "arrow.triangle.branch", .orange)
			}
		}
		.padding(.horizontal, 12).padding(.vertical, 9)
		.background(RoundedRectangle(cornerRadius: 12).fill(Color(nsColor: .windowBackgroundColor)))
		.overlay(RoundedRectangle(cornerRadius: 12).stroke(Color.blue.opacity(0.35)))
	}

	func chip(_ t: String, _ icon: String, _ color: Color) -> some View {
		HStack(spacing: 4) {
			Image(systemName: icon).font(.system(size: 10))
			Text(t).font(.system(size: 11))
		}
		.foregroundStyle(color)
		.padding(.horizontal, 7).padding(.vertical, 2)
		.background(Capsule().fill(color.opacity(0.1)))
	}

	func node(_ c: Computer) -> some View {
		let relayOn = c.pool?.online == true
		let netOn = c.net?.online == true
		return VStack(spacing: 4) {
			HStack(spacing: 5) {
				Image(systemName: c.isThisMac ? "laptopcomputer" : "desktopcomputer").font(.system(size: 13))
					.foregroundStyle(.secondary)
				Text(c.name).font(.system(size: 12.5, weight: .semibold)).lineLimit(1)
			}
			// The two programs that run on it.
			program("Relay", c.pool == nil ? "not installed" : (relayOn ? "connected" : "offline"), relayOn ? .green : .gray)
			program("Network", c.net.map { netOn ? $0.ip : "off" } ?? "not installed", netOn ? .green : .gray)
		}
		.padding(.horizontal, 8)
		.frame(width: 170, height: 86)
		.background(RoundedRectangle(cornerRadius: 12).fill(Color(nsColor: .windowBackgroundColor)))
		.overlay(RoundedRectangle(cornerRadius: 12).stroke(c.isThisMac ? Color.accentColor.opacity(0.6) : Color.primary.opacity(0.12), lineWidth: c.isThisMac ? 1.5 : 1))
		.contentShape(Rectangle())
	}

	func program(_ name: String, _ state: String, _ color: Color) -> some View {
		HStack(spacing: 5) {
			Circle().fill(color).frame(width: 6, height: 6)
			Text(name).font(.system(size: 11, weight: .medium))
			Spacer(minLength: 4)
			Text(state).font(.system(size: 10.5, design: state.contains(".") ? .monospaced : .default)).foregroundStyle(.secondary)
		}
		.padding(.horizontal, 7).padding(.vertical, 2)
		.background(RoundedRectangle(cornerRadius: 6).fill(Color.primary.opacity(0.04)))
	}

	func key(_ color: Color, solid: Bool, _ t: String) -> some View {
		HStack(spacing: 6) {
			Path { p in p.move(to: .init(x: 0, y: 5)); p.addLine(to: .init(x: 22, y: 5)) }
				.stroke(color, style: StrokeStyle(lineWidth: 2.5, dash: solid ? [] : [5, 3]))
				.frame(width: 22, height: 10)
			Text(t).font(.system(size: 12)).foregroundStyle(.secondary)
		}
	}
}
