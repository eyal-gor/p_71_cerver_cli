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
let addDeviceCommand = "curl -fsSL https://cerver.ai/install.sh | sh"

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
	var direct: Bool? = nil   // nil: an older network service
}

/// Direct connections on/off, read by the network service every few seconds
/// (cmd/net.go netDirectOff). Off = everything through the backup route.
enum NetSettings {
	static let file = NSHomeDirectory() + "/.cerver/net-settings.json"
	static var direct: Bool {
		guard let d = FileManager.default.contents(atPath: file),
			  let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else { return true }
		return (o["direct"] as? Bool) ?? true
	}
	static func setDirect(_ on: Bool) {
		try? FileManager.default.createDirectory(atPath: NSHomeDirectory() + "/.cerver", withIntermediateDirectories: true)
		if let d = try? JSONSerialization.data(withJSONObject: ["direct": on]) {
			try? d.write(to: URL(fileURLWithPath: file), options: .atomic)
		}
	}
}

/// `cerver update --check --json`
struct UpdateInfo: Decodable {
	struct Part: Decodable { let current: String; let latest: String?; let update: Bool }
	let latest: String
	let problem: String?
	let cli: Part
	let app: Part?
	let relay: Part?
	let net: Part?
	var any: Bool { cli.update || app?.update == true || relay?.update == true || net?.update == true }
}

/// Runs a line in your login shell and returns what it printed.
func shell(_ line: String) -> String {
	let p = Process()
	p.executableURL = URL(fileURLWithPath: "/bin/zsh")
	p.arguments = ["-lc", line]
	let out = Pipe()
	p.standardOutput = out
	p.standardError = Pipe()
	do { try p.run() } catch { return "" }
	let data = out.fileHandleForReading.readDataToEndOfFile()
	p.waitUntilExit()
	return (String(data: data, encoding: .utf8) ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
}

/// Runs the app's own cerver as you (with your login PATH). Returns (ok, output).
func runCerver(_ args: [String]) -> (Bool, String) {
	guard let bin = Bundle.main.path(forResource: "cerver", ofType: nil) else { return (false, "The app is missing its cerver program.") }
	let p = Process()
	p.executableURL = URL(fileURLWithPath: bin)
	p.arguments = args
	var env = ProcessInfo.processInfo.environment
	env["PATH"] = loginShellPath()
	p.environment = env
	let out = Pipe()
	p.standardOutput = out
	p.standardError = out
	do { try p.run() } catch { return (false, error.localizedDescription) }
	let data = out.fileHandleForReading.readDataToEndOfFile()
	p.waitUntilExit()
	return (p.terminationStatus == 0, String(data: data, encoding: .utf8) ?? "")
}

enum Phase: Equatable {
	case notSignedIn, off, starting, on, working(String), failed(String)
}

@MainActor
final class Model: ObservableObject {
	/// Search computers by name (sidebar and Compute pool).
	@Published var search = UserDefaults.standard.string(forKey: "startSearch") ?? ""  // `-startSearch mini` for screenshots
	var shown: [Computer] {
		let q = search.trimmingCharacters(in: .whitespaces)
		return q.isEmpty ? computers : computers.filter { $0.name.localizedCaseInsensitiveContains(q) }
	}
	/// Your pool's name (PATCH /v2/pool); nil or "My pool" shows "Compute pool".
	@Published var poolName: String?
	var poolTitle: String { poolName.map { $0 == "My pool" ? "Compute pool" : $0 } ?? "Compute pool" }
	func loadPoolName() {
		Task { @MainActor in
			struct P: Decodable { let name: String? }
			if let d = await gatewayGET("/v2/pool"), let p = try? JSONDecoder().decode(P.self, from: d) { self.poolName = p.name }
		}
	}
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
		// Already-connected Macs get the skill too (once; it's skipped if there).
		if FileManager.default.fileExists(atPath: NSHomeDirectory() + "/.cerver/cerver.env") {
			Task.detached { installClaudeSkill(onlyIfMissing: true) }
		}
		refresh()
		refreshRelay()
		checkUpdates()
		refreshAccount()
		timer = Timer.scheduledTimer(withTimeInterval: 2, repeats: true) { _ in
			Task { @MainActor in
				self.refresh()
				self.refreshRelay()
				self.refreshPool()
				self.checkUpdates()
				self.refreshJobs()
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

	/// For the menu: what the relay is doing (its local service, every 5 s).
	@Published var relayJobs: RelayJobs?
	@Published var relayLoad: RelayStats.Compute?
	/// The menu asks the window to show a page.
	@Published var openPage: Page?
	private var lastJobsFetch = Date.distantPast
	func refreshJobs() {
		guard relay == .running else { relayJobs = nil; relayLoad = nil; return }
		guard Date().timeIntervalSince(lastJobsFetch) > 5 else { return }
		lastJobsFetch = Date()
		Task.detached {
			func get<T: Decodable>(_ path: String, _ t: T.Type) async -> T? {
				guard let u = URL(string: "http://127.0.0.1:18081" + path) else { return nil }
				var r = URLRequest(url: u); r.timeoutInterval = 2
				guard let (d, resp) = try? await URLSession.shared.data(for: r), (resp as? HTTPURLResponse)?.statusCode == 200 else { return nil }
				return try? JSONDecoder().decode(T.self, from: d)
			}
			let j = await get("/api/cerver/jobs?n=5", RelayJobs.self)
			let st = await get("/api/local-claude/stats", RelayStats.self)
			await MainActor.run { self.relayJobs = j; self.relayLoad = st?.compute }
		}
	}

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

	func relayOn() { runRelay(["relay", "install"], doing: "Adding this Mac to your compute pool…") }
	func relayOff() { runRelay(["relay", "uninstall"], doing: "Taking this Mac out of your compute pool…") }

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

	// MARK: Connect (first run): sign in, then pool, then network

	@Published var connecting: String?
	@Published var connectProblem: String?

	func connect() {
		connectProblem = nil
		connecting = "Waiting for you to sign in in your browser…"
		Task.detached {
			let home = NSHomeDirectory()
			// 1. Sign in (opens the browser; waits for your approval).
			if !FileManager.default.fileExists(atPath: home + "/.cerver/cerver.env") {
				let (ok, out) = runCerver(["login"])
				if !ok {
					await MainActor.run { self.connecting = nil; self.connectProblem = cleanError(out) }
					return
				}
			}
			// 2. The cerver program where the services and updates expect it.
			await MainActor.run { self.connecting = "Setting up…" }
			let bin = home + "/.cerver/bin/cerver"
			if !FileManager.default.fileExists(atPath: bin), let bundled = Bundle.main.path(forResource: "cerver", ofType: nil) {
				try? FileManager.default.createDirectory(atPath: home + "/.cerver/bin", withIntermediateDirectories: true)
				try? FileManager.default.copyItem(atPath: bundled, toPath: bin)
			}
			// The /cerver skill, so Claude Code knows it can run Codex (and the
			// rest) through Cerver. Skipped when Claude Code isn't installed.
			installClaudeSkill()
			// 3. uv, which runs the relay.
			if shell("command -v uvx || ls ~/.local/bin/uvx").isEmpty {
				await MainActor.run { self.connecting = "Installing uv (runs the relay)…" }
				_ = shell("curl -LsSf https://astral.sh/uv/install.sh | sh")
			}
			// 4. The pool.
			await MainActor.run { self.connecting = "Joining your compute pool…" }
			let (ok, out) = runCerver(["relay", "install"])
			await MainActor.run {
				self.connecting = nil
				if !ok { self.connectProblem = cleanError(out) }
				self.refresh()
				self.refreshRelay()
				self.refreshPool(force: true)
				// 5. The network (asks for this Mac's password).
				self.turnOn()
			}
		}
	}

	// MARK: Versions, updates, uninstall

	@Published var updates: UpdateInfo?
	/// Bumped by ⌘, to open the Settings page.
	@Published var openSettings = 0
	/// The account this Mac is signed in to, as Cerver knows it.
	@Published var email: String?
	func refreshAccount() {
		struct Account: Decodable { let email: String? }
		Task.detached {
			guard let d = await gatewayGET("/v2/auth/account"), let a = try? JSONDecoder().decode(Account.self, from: d) else { return }
			await MainActor.run { self.email = a.email }
		}
	}
	@Published var updating = false
	@Published var updateProblem: String?
	@Published var uninstalling = false
	private var lastUpdateCheck = Date.distantPast
	@Published var checking = false
	@Published var checkedAt: Date?

	var updateAvailable: Bool { updates?.any ?? false }

	/// Asks `cerver update --check --json` what's installed and what's new.
	/// On launch and every hour. When something is new and you haven't used
	/// the app for 10 minutes, it updates itself and reopens in the background.
	func checkUpdates(force: Bool = false) {
		if NSApp.isActive { lastActive = Date() }
		if updates?.any == true, !updating, !NSApp.isActive, Date().timeIntervalSince(lastActive) > 600,
		   Date().timeIntervalSince(lastAutoUpdate) > 3600 {
			lastAutoUpdate = Date()
			updateNow(quiet: true)
			return
		}
		if !force && Date().timeIntervalSince(lastUpdateCheck) < 3600 { return }
		lastUpdateCheck = Date()
		checking = true
		Task.detached {
			let (ok, out) = runCerver(["update", "--check", "--json"])
			let info = ok ? try? JSONDecoder().decode(UpdateInfo.self, from: Data(out.utf8)) : nil
			await MainActor.run {
				if let i = info { self.updates = i }
				self.checking = false
				self.checkedAt = Date()
			}
		}
	}
	private var lastActive = Date()
	private var lastAutoUpdate = Date.distantPast

	/// Updates everything, then reopens the (new) app.
	/// quiet: the automatic update while you're away. No password prompt (the
	/// network service updates itself), and the app reopens in the background.
	func updateNow(quiet: Bool = false) {
		updating = true
		updateProblem = nil
		let netName = status?.name ?? ""
		let netBehind = !quiet && updates?.net?.update == true
		Task.detached {
			let (ok, out) = runCerver(["update", "--app"])
			// The network service runs as admin, so updating it now needs the
			// password (macOS asks); otherwise it updates itself later.
			if ok, netBehind, !netName.isEmpty, let bin = Bundle.main.path(forResource: "cerver", ofType: nil) {
				let p = Process()
				p.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
				p.arguments = ["-e", "do shell script \"'\(bin)' net install --name '\(netName.replacingOccurrences(of: "'", with: ""))'\" with administrator privileges"]
				try? p.run(); p.waitUntilExit()
			}
			await MainActor.run {
				self.updating = false
				if !ok || out.contains("couldn't") {
					self.updateProblem = out.trimmingCharacters(in: .whitespacesAndNewlines)
					self.checkUpdates(force: true)
					return
				}
				if self.updates?.app?.update == true {
					let p = Process()
					p.executableURL = URL(fileURLWithPath: "/bin/sh")
					p.arguments = ["-c", quiet ? "sleep 1; open -g -j /Applications/Cerver.app" : "sleep 1; open /Applications/Cerver.app"]
					try? p.run()
					NSApp.terminate(nil)
				} else {
					self.checkUpdates(force: true)
				}
			}
		}
	}

	/// Takes Cerver off this Mac (asks for the admin password once), then quits.
	func uninstall() {
		uninstalling = true
		Task.detached {
			let (ok, out) = runCerver(["uninstall", "--yes"])
			await MainActor.run {
				self.uninstalling = false
				if ok {
					NSApp.terminate(nil)
				} else {
					self.updateProblem = out.trimmingCharacters(in: .whitespacesAndNewlines)
				}
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
	var relay_name: String? = nil   // the relay's own name; stays when you rename the computer
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
	var installs_tools: Bool? = nil
	let reported: Bool?
	var agent_access: AgentAccess? = nil
	var provisioning: Provisioning? = nil

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
	var os: String? = nil
}

/// One of your computers: its place in the pool and/or on the network.
struct Computer: Identifiable {
	let name: String
	let pool: PoolMachine?
	let net: NetNode?
	let isThisMac: Bool
	var spec: Spec? = nil
	var id: String { pool?.compute_id ?? "net:" + (net?.ip ?? name) }
	var isPhone: Bool { net?.os == "phone" }
	var icon: String { isThisMac ? "laptopcomputer" : (isPhone ? "iphone" : "desktopcomputer") }
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
		let own = m.relay_name ?? m.name
		let me = own == thisMac || m.name == thisMac
		let node = nodes.first { !used.contains($0.ip) && ($0.name == cleanName(own) || $0.name == cleanName(m.name) || (me && $0.name == thisMacNet)) }
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

/// The pool's own facts: name, invite link, people, cloud providers.
struct PoolInfo: Decodable {
	struct Person: Decodable { let account_id: String; let name: String?; let email: String?; let status: String? }
	struct Provider: Decodable { let name: String; let billed_by: String? }
	struct Cloud { let name: String; let connected: Bool; let yours: Bool }
	let name: String?
	let invite_url: String?
	let people: [Person]
	let providers: [Provider]?
	let can_add: [String]?

	/// The cloud providers you have, then the ones you can connect.
	var cloud: [Cloud] {
		let known = ["E2B", "Daytona", "Modal", "Vercel"]
		var out = (providers ?? []).map { Cloud(name: $0.name, connected: true, yours: ($0.billed_by ?? "").lowercased().contains("your own")) }
		for n in can_add ?? [] where known.contains(n) && !out.contains(where: { $0.name == n }) {
			out.append(Cloud(name: n, connected: false, yours: false))
		}
		return out
	}
}

/// Opens macOS Screen Sharing to a computer over your private network, with
/// your user name filled in (Screen Sharing remembers the password once you
/// tick Remember password).
func viewScreen(_ ip: String) {
	let user = NSUserName().addingPercentEncoding(withAllowedCharacters: .urlUserAllowed) ?? ""
	if let u = URL(string: "vnc://\(user)@\(ip)") { NSWorkspace.shared.open(u) }
}

/// Puts Cerver's skill into Claude Code (~/.claude/skills/cerver/SKILL.md),
/// as install.sh does, so Claude can ask Codex, compare agents, and use your
/// compute pool. Does nothing if Claude Code isn't on this Mac.
func installClaudeSkill(onlyIfMissing: Bool = false) {
	let claude = NSHomeDirectory() + "/.claude"
	let dir = claude + "/skills/cerver"
	let file = dir + "/SKILL.md"
	var isDir: ObjCBool = false
	guard FileManager.default.fileExists(atPath: claude, isDirectory: &isDir), isDir.boolValue else { return }
	if onlyIfMissing && FileManager.default.fileExists(atPath: file) { return }
	guard let url = URL(string: "https://cerver.ai/skills/cerver/SKILL.md"),
		let data = try? Data(contentsOf: url), data.count > 200,
		String(decoding: data.prefix(400), as: UTF8.self).contains("name:") else { return }
	try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
	try? data.write(to: URL(fileURLWithPath: file), options: .atomic)
}

func openWeb(_ path: String) {
	if let u = URL(string: "https://cerver.ai" + path) { NSWorkspace.shared.open(u) }
}

/// GET from the Cerver gateway with your key from ~/.cerver/cerver.env.
func gatewayGET(_ path: String) async -> Data? { await gatewayRequest("GET", path) }

func gatewayRequest(_ method: String, _ path: String, body: Data? = nil) async -> Data? {
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
	req.httpMethod = method
	req.setValue("Bearer " + key, forHTTPHeaderField: "Authorization")
	if let b = body { req.httpBody = b; req.setValue("application/json", forHTTPHeaderField: "Content-Type") }
	guard let (data, resp) = try? await URLSession.shared.data(for: req), let code = (resp as? HTTPURLResponse)?.statusCode, (200..<300).contains(code) else { return nil }
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
	case updates, agents
	case map, pool, permissions, addons, safety, focus, cleanup, exitNode, thisMac, computer(String), add, settings, session, profile, connectors, people
}

struct MainWindow: View {
	@ObservedObject var m: Model
	@StateObject private var session = SessionModel()
	@StateObject private var apps = AppsModel()
	@StateObject private var nav = SectionNav()
	@StateObject private var addons = AddonsModel()
	@AppStorage("poolFolded") private var poolFolded = false
	@State private var hoverPool = false
	@AppStorage("addonsFolded") private var addonsFolded = true
	@AppStorage("moreFolded") private var moreFolded = true
	/// Always foldable; going to one of its pages (e.g. -startPage) opens it once.
	var moreOpen: Bool { !moreFolded }
	static let morePages: [Page] = [.agents, .map, .connectors, .people, .permissions, .addons, .safety, .focus, .cleanup, .exitNode]
	@State private var hoverAddons = false
	// `-startPage thisMac` on launch opens that page (for screenshots).
	@State private var page: Page? = ["thisMac": Page.thisMac, "add": Page.add, "settings": Page.settings, "updates": Page.updates, "session": Page.session, "pool": Page.pool, "permissions": Page.permissions, "security": Page.permissions, "addons": Page.addons, "safety": Page.safety, "focus": Page.focus, "cleanup": Page.cleanup, "exit": Page.exitNode, "vpn": Page.exitNode, "profile": Page.profile, "connectors": Page.connectors, "people": Page.people, "map": Page.map, "agents": Page.agents][UserDefaults.standard.string(forKey: "startPage") ?? ""] ?? .pool

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
		.onChange(of: m.openSettings) { _ in page = .settings }
		.onChange(of: m.openPage) { p in if let p { page = p; m.openPage = nil } }
		.onAppear { if let p = m.openPage { page = p; m.openPage = nil } }
		.onChange(of: m.computers.map(\.id)) { ids in
			// The computer on screen left: go back to the list.
			if case .computer(let id) = page, !ids.contains(id) { page = .pool }
		}
	}

	/// Hand a task to an agent on a computer and show the conversation (Ask Compute Pool).
	var askAgent: AskAgent {
		AskAgent { prompt, name, isThisMac in
			session.reset()
			session.cli = "claude"
			session.computer = isThisMac ? "" : name
			session.send(prompt, on: isThisMac ? m.relayName : name)
			page = .session
		}
	}

	// MARK: Sidebar

	/// The open computer's parts, indented under it. Click to scroll there;
	/// the one you're reading is highlighted as you scroll.
	@ViewBuilder var sectionRows: some View {
		ForEach(Tab.page) { t in
			Button { nav.go(t) } label: {
				Text(t.rawValue)
					.font(.system(size: 12, weight: nav.current == t ? .semibold : .regular))
					.foregroundStyle(nav.current == t ? Color.primary : Color.secondary)
					.frame(maxWidth: .infinity, alignment: .leading)
					.padding(.leading, 44).padding(.vertical, 1)
					.overlay(alignment: .leading) {
						Rectangle().fill(nav.current == t ? Color.accentColor : Color.primary.opacity(0.12))
							.frame(width: 2).padding(.leading, 34)
					}
					.contentShape(Rectangle())
			}
			.buttonStyle(.plain)
		}
	}

	var sidebar: some View {
		VStack(spacing: 0) {
			// The logo takes you to the compute pool: the first page.
			Button { page = .pool } label: {
				HStack(spacing: 9) {
					Image(nsImage: logoImage(size: 22))
						.clipShape(RoundedRectangle(cornerRadius: 5))
					Text("Cerver").font(.system(size: 15, weight: .semibold))
					Spacer()
				}
				.contentShape(Rectangle())
			}
			.buttonStyle(.plain)
			.help("Compute pool")
			.padding(.horizontal, 16)
			.padding(.top, 6)
			.padding(.bottom, 10)

			SearchField(text: $m.search, prompt: "Search computers")
				.padding(.horizontal, 12).padding(.bottom, 6)

			List(selection: $page) {
				Section("Cerver Compute") {
					HStack(spacing: 4) {
						Label(m.poolTitle, systemImage: "server.rack").lineLimit(1)
						Spacer()
						// Fold the computers away; the button shows when the pointer is on the row.
						Button { withAnimation(.easeInOut(duration: 0.15)) { poolFolded.toggle() } } label: {
							Image(systemName: poolFolded ? "chevron.right" : "chevron.down")
								.font(.system(size: 11, weight: .semibold))
								.frame(width: 22, height: 18)
								.background(RoundedRectangle(cornerRadius: 5).fill(Color.primary.opacity(hoverPool ? 0.08 : 0)))
						}
						.buttonStyle(.plain)
						.foregroundStyle(.secondary)
						.opacity(hoverPool ? 1 : 0)
						.help(poolFolded ? "Show your computers" : "Hide your computers")
					}
					.onHover { hoverPool = $0 }
					.tag(Page.pool)
					if !poolFolded {
					Label("Ask Compute Pool", systemImage: "bubble.left.and.text.bubble.right")
						.padding(.leading, 14)
						.tag(Page.session)
					ForEach(m.shown.filter { !$0.isPhone }) { c in
						sidebarRow(name: c.name, dot: c.isThisMac ? thisMacDot : computerDot(c), icon: c.icon, you: c.isThisMac)
							.padding(.leading, 14)
							.tag(c.isThisMac ? Page.thisMac : Page.computer(c.id))
						if page == (c.isThisMac ? Page.thisMac : Page.computer(c.id)) { sectionRows }
					}
					if !m.computers.contains(where: { $0.isThisMac }) {
						sidebarRow(name: m.relayName, dot: thisMacDot, icon: "laptopcomputer", you: true)
							.padding(.leading, 14)
							.tag(Page.thisMac)
					}
					Label("Add Compute", systemImage: "plus")
						.foregroundStyle(.secondary)
						.padding(.leading, 14)
						.tag(Page.add)
					}
				}
				Section {
					// More: folded by default; opens by itself when one of its pages is showing.
					Button { withAnimation(.easeInOut(duration: 0.15)) { moreFolded.toggle() } } label: {
						HStack(spacing: 4) {
							Label("More", systemImage: "ellipsis.circle")
							Spacer()
							Image(systemName: moreOpen ? "chevron.down" : "chevron.right")
								.font(.system(size: 11, weight: .semibold)).foregroundStyle(.secondary)
						}
						.contentShape(Rectangle())
					}
					.buttonStyle(.plain)
					if moreOpen {
						Label("Agents", systemImage: "cpu")
							.padding(.leading, 14)
							.tag(Page.agents)
						Label("Map", systemImage: "map")
							.padding(.leading, 14)
							.tag(Page.map)
						Label("Connectors", systemImage: "app.connected.to.app.below.fill")
							.padding(.leading, 14)
							.tag(Page.connectors)
						Label("People", systemImage: "person.2")
							.padding(.leading, 14)
							.tag(Page.people)
						Label("Security", systemImage: "lock.shield")
							.padding(.leading, 14)
							.tag(Page.permissions)
						HStack(spacing: 4) {
							Label("Add-ons", systemImage: "puzzlepiece.extension")
							Spacer()
							Button { withAnimation(.easeInOut(duration: 0.15)) { addonsFolded.toggle() } } label: {
								Image(systemName: addonsFolded ? "chevron.right" : "chevron.down")
									.font(.system(size: 11, weight: .semibold))
									.frame(width: 22, height: 18)
									.background(RoundedRectangle(cornerRadius: 5).fill(Color.primary.opacity(hoverAddons ? 0.08 : 0)))
							}
							.buttonStyle(.plain)
							.foregroundStyle(.secondary)
							.opacity(hoverAddons ? 1 : 0)
							.help(addonsFolded ? "Show the add-ons" : "Hide the add-ons")
						}
						.padding(.leading, 14)
						.onHover { hoverAddons = $0 }
						.tag(Page.addons)
						if !addonsFolded {
							Label("Safety", systemImage: "checkmark.shield")
								.padding(.leading, 28)
								.tag(Page.safety)
							Label("Focus", systemImage: "moon.zzz")
								.padding(.leading, 28)
								.tag(Page.focus)
							Label("Cleanup", systemImage: "sparkles")
								.padding(.leading, 28)
								.tag(Page.cleanup)
							Label("VPN", systemImage: "network.badge.shield.half.filled")
								.padding(.leading, 28)
								.tag(Page.exitNode)
						}
					}
				}
			}
			.listStyle(.sidebar)
			.onChange(of: page) { p in
				nav.current = .overview
				if let p, Self.morePages.contains(p), moreFolded { moreFolded = false }
			}
			.onAppear { if let p = page, Self.morePages.contains(p) { moreFolded = false } }
			.task { m.loadPoolName() }
			Button { page = .updates } label: {
				HStack(spacing: 6) {
					Label("Updates", systemImage: "arrow.down.circle")
					Spacer(minLength: 0)
					if m.updating {
						ProgressView().controlSize(.mini)
					} else if m.updateAvailable {
						Text("New").font(.system(size: 10, weight: .semibold)).foregroundStyle(.white)
							.padding(.horizontal, 6).padding(.vertical, 1).background(Capsule().fill(Color.accentColor))
					} else {
						Text(appVersion).font(.system(size: 11)).foregroundStyle(.tertiary)
					}
				}
				.font(.system(size: 13))
				.frame(maxWidth: .infinity, alignment: .leading)
				.padding(.horizontal, 8).padding(.vertical, 6)
				.background(RoundedRectangle(cornerRadius: 6).fill(page == .updates ? Color.accentColor.opacity(0.15) : .clear))
				.contentShape(Rectangle())
			}
			.buttonStyle(.plain)
			.help(m.updateAvailable ? "Cerver \(m.updates?.latest ?? "") is out" : "Cerver \(appVersion)")
			.padding(.horizontal, 10)
			Button { page = .settings } label: {
				Label("Settings", systemImage: "gearshape")
					.font(.system(size: 13))
					.frame(maxWidth: .infinity, alignment: .leading)
					.padding(.horizontal, 8).padding(.vertical, 6)
					.background(RoundedRectangle(cornerRadius: 6).fill(page == .settings ? Color.accentColor.opacity(0.15) : .clear))
					.contentShape(Rectangle())
			}
			.buttonStyle(.plain)
			.padding(.horizontal, 10).padding(.bottom, 4)
			footer
		}
	}

	/// Bottom of the sidebar: who you're signed in as, how this computer is
	/// connected (with ways to disconnect), and the version (with Update).
	var footer: some View {
		HStack(spacing: 8) {
			Button { page = .profile } label: {
				HStack(spacing: 8) {
					ZStack(alignment: .bottomTrailing) {
						Circle().fill(Color.accentColor.opacity(0.85)).frame(width: 24, height: 24)
							.overlay(Text(String((m.email ?? "?").prefix(1)).uppercased()).font(.system(size: 11, weight: .semibold)).foregroundStyle(.white))
						Circle().fill(thisMacDot).frame(width: 8, height: 8)
							.overlay(Circle().stroke(Color(nsColor: .windowBackgroundColor), lineWidth: 1.5))
					}
					Text(m.email ?? "Not signed in").font(.system(size: 12.5)).lineLimit(1).truncationMode(.middle)
				}
				.contentShape(Rectangle())
			}
			.buttonStyle(.plain)
			.help("Your profile and plan")
			Spacer(minLength: 0)

		}
		.padding(.horizontal, 14).padding(.vertical, 10)
		.overlay(alignment: .top) { Divider().padding(.horizontal, 12) }
	}

	var appVersion: String { Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "" }

	/// "Network: direct to 2 · 1 via backup"
	var networkLine: String {
		guard m.isOn, let s = m.status else { return "Network: off" }
		let direct = s.peers.filter { $0.connected && $0.via == "direct" }.count
		let relayed = s.peers.filter { $0.connected && $0.via == "relay" }.count
		var parts: [String] = []
		if direct > 0 { parts.append("direct to \(direct)") }
		if relayed > 0 { parts.append("\(relayed) via backup") }
		if parts.isEmpty { parts.append("on, no one else yet") }
		return "Network: " + parts.joined(separator: " · ")
	}

	/// Green when everything you turned on is working, orange while starting, gray when all off.
	var thisMacDot: Color {
		let relayOn = m.relay == .running
		if m.busy || m.relay == .starting { return .orange }
		if m.isOn || relayOn { return .green }
		return Color.secondary.opacity(0.4)
	}

	func sidebarRow(name: String, dot: Color, icon: String, you: Bool = false) -> some View {
		HStack(spacing: 8) {
			Image(systemName: icon).foregroundStyle(.secondary).frame(width: 18)
			Text(name).lineLimit(1)
			if you { Text("you").font(.system(size: 10, weight: .semibold)).foregroundStyle(.secondary) }
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
				Text("Connect this Mac to Cerver").font(.system(size: 26, weight: .semibold))
				Text("Sign in in your browser. Then this Mac joins your compute pool, so agents can run on it, and your network, so your computers reach it.")
					.font(.system(size: 15)).foregroundStyle(.secondary).multilineTextAlignment(.center).frame(maxWidth: 460)
				Button {
					m.connect()
				} label: {
					Text(m.connecting ?? "Connect").font(.system(size: 15, weight: .semibold)).padding(.horizontal, 18).padding(.vertical, 6)
				}
				.buttonStyle(.borderedProminent).controlSize(.large).clipShape(Capsule())
				.disabled(m.connecting != nil)
				if let p = m.connectProblem {
					Text(p).font(.callout).foregroundStyle(.red).textSelection(.enabled).frame(maxWidth: 460)
				}
			}
			.padding(40)
			.frame(maxWidth: .infinity, maxHeight: .infinity)
		} else {
			switch page ?? .pool {
			case .map: MapPage(m: m, apps: apps, open: { page = $0 })
			case .connectors: ConnectorsPage(a: apps)
			case .people: PeoplePage(m: m)
			case .agents: AgentsPage(m: m)
			case .pool: PoolPage(m: m, open: { page = $0 })
			case .permissions: PermissionsPage(m: m)
			case .addons: AddonsHome(a: addons, open: { page = $0 })
			case .safety: SafetyPage(m: m, a: addons)
			case .exitNode: ExitNodePage(m: m, a: addons)
			case .focus: FocusPage(m: m, a: addons)
			case .cleanup: CleanupPage(m: m).environment(\.askAgent, askAgent)
			case .thisMac: ThisMacPage(m: m, nav: nav).environment(\.askAgent, askAgent).environment(\.openPage, OpenPage { page = $0 })
			case .computer(let id):
				if let c = m.computers.first(where: { $0.id == id }) { ComputerPage(m: m, c: c, nav: nav).environment(\.askAgent, askAgent).environment(\.openPage, OpenPage { page = $0 }) } else { PoolPage(m: m, open: { page = $0 }) }
			case .add: AddPage(m: m)
			case .settings: SettingsPage(m: m).id("settings")
			case .updates: SettingsPage(m: m, tab: "Updates").id("updates")
			case .profile: ProfilePage(m: m)
			case .session: SessionPage(m: m, s: session)
			}
		}
	}
}

/// One page: big title, a line under it, then cards. Centered, readable width.
struct PageLayout<Content: View>: View {
	let title: String
	let subtitle: String
	var rename: (() -> Void)? = nil
	@ViewBuilder let content: Content

	init(title: String, subtitle: String, rename: (() -> Void)? = nil, @ViewBuilder content: () -> Content) {
		self.title = title; self.subtitle = subtitle; self.rename = rename; self.content = content()
	}

	var body: some View {
		ScrollView {
			ScrollViewReader { proxy in
			VStack(alignment: .leading, spacing: 22) {
				VStack(alignment: .leading, spacing: 6) {
					HStack(spacing: 8) {
						Text(title).font(.system(size: 28, weight: .semibold))
						if let r = rename {
							Button(action: r) { Image(systemName: "pencil").font(.system(size: 15)) }
								.buttonStyle(.borderless).foregroundStyle(.secondary).help("Rename")
						}
					}
					Text(subtitle).font(.system(size: 15)).foregroundStyle(.secondary)
				}
				content
			}
			.frame(maxWidth: 620, alignment: .leading)
			.padding(.horizontal, 40)
			.padding(.vertical, 36)
			.frame(maxWidth: .infinity)
			.environment(\.pageScroller, PageScroller(proxy: proxy))
			}
		}
		.coordinateSpace(name: "page")
	}
}

/// Lets a section list inside a page scroll the page to one of its parts.
struct PageScroller { var proxy: ScrollViewProxy? }
private struct PageScrollerKey: EnvironmentKey { static let defaultValue = PageScroller(proxy: nil) }
extension EnvironmentValues {
	var pageScroller: PageScroller {
		get { self[PageScrollerKey.self] }
		set { self[PageScrollerKey.self] = newValue }
	}
}

/// Which part of a computer's page is on screen, shared with the sidebar.
@MainActor final class SectionNav: ObservableObject {
	@Published var current: Tab = .overview
	@Published var jump: Tab?
	func go(_ t: Tab) { current = t; jump = t }
}

private struct SectionTops: PreferenceKey {
	static var defaultValue: [Tab: CGFloat] = [:]
	static func reduce(value: inout [Tab: CGFloat], nextValue: () -> [Tab: CGFloat]) { value.merge(nextValue()) { $1 } }
}

/// A computer's parts one after another on a single page. Reports which one is
/// at the top while you scroll, and scrolls to one when the sidebar asks.
struct SectionedPage<Body: View>: View {
	@ObservedObject var nav: SectionNav
	@ViewBuilder let part: (Tab) -> Body
	@Environment(\.pageScroller) private var scroller
	@State private var jumping = false

	var body: some View {
		VStack(alignment: .leading, spacing: 34) {
			ForEach(Tab.page) { t in
				VStack(alignment: .leading, spacing: 12) {
					Text(t.rawValue).font(.system(size: 20, weight: .semibold))
					part(t)
				}
				.id(t)
				.background(GeometryReader { g in
					Color.clear.preference(key: SectionTops.self, value: [t: g.frame(in: .named("page")).minY])
				})
			}
			// Room so the last part can reach the top.
			Color.clear.frame(height: 320)
		}
		.onPreferenceChange(SectionTops.self) { tops in
			guard !jumping else { return }
			let reading = Tab.page.last(where: { (tops[$0] ?? .infinity) <= 90 }) ?? .overview
			if nav.current != reading { nav.current = reading }
		}
		.onChange(of: nav.jump) { t in
			guard let t = t else { return }
			jumping = true
			withAnimation(.easeInOut(duration: 0.25)) { scroller.proxy?.scrollTo(t, anchor: .top) }
			nav.jump = nil
			DispatchQueue.main.asyncAfter(deadline: .now() + 0.4) { jumping = false }
		}
		.onAppear {
			// `-startTab Software` opens there (for screenshots).
			if let s = UserDefaults.standard.string(forKey: "startTab"), let t = Tab(rawValue: s) {
				DispatchQueue.main.asyncAfter(deadline: .now() + 1.0) { nav.go(t) }
			}
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
	var note: String? = nil
	@ViewBuilder let trailing: Trailing
	var body: some View {
		HStack {
			VStack(alignment: .leading, spacing: 2) {
				Text(label).font(.system(size: 14))
				if let n = note { Text(n).font(.system(size: 11.5)).foregroundStyle(.secondary) }
			}
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

/// What software a computer has for agents: coding agents and tools, as its relay found them.
struct SoftwareSection: View {
	let spec: Spec?

	static let names: [String: String] = [
		"claude": "Claude Code", "codex": "Codex", "grok": "Grok", "gemini": "Gemini CLI", "gemma": "Gemma", "opencode": "OpenCode",
		"git": "Git", "docker": "Docker", "ffmpeg": "FFmpeg", "node": "Node.js", "npm": "npm", "bun": "Bun", "python3": "Python",
		"uv": "uv", "go": "Go", "cargo": "Rust (cargo)", "java": "Java", "psql": "PostgreSQL client", "kubectl": "kubectl", "gh": "GitHub CLI",
	]

	var body: some View {
		if let s = spec {
			group("Coding agents", "The agents Cerver can run here.", s.coding_agents ?? [])
			group("Tools", "Programs agents can use here.", s.tools ?? [])
			if let i = s.installs_tools {
				Text(i ? "Agents can install more software here as they need it." : "Agents use what's installed; to add software, install it on this computer.")
					.font(.callout).foregroundStyle(.secondary)
			}
		} else {
			Text("Not reported yet. A computer tells Cerver what it has when its relay starts.")
				.font(.callout).foregroundStyle(.secondary)
		}
	}

	func group(_ title: String, _ note: String, _ items: [String]) -> some View {
		VStack(alignment: .leading, spacing: 8) {
			Text(title).font(.system(size: 15, weight: .semibold)).padding(.top, 6)
			Text(note).font(.callout).foregroundStyle(.secondary)
			Card {
				if items.isEmpty {
					Row(label: "None found") { EmptyView() }
				} else {
					ForEach(Array(items.enumerated()), id: \.offset) { n, t in
						if n > 0 { Divider().padding(.leading, 16) }
						Row(label: Self.names[t] ?? t) {
							Text(t).font(.system(size: 12, design: .monospaced)).foregroundStyle(.tertiary)
							Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
						}
					}
				}
			}
		}
	}
}

/// Every computer in one table: can agents run on it, is it on the network,
/// can people in your pool use it.
/// The first page: agents on top, your pool in the middle, the network under it.
struct MapPage: View {
	@ObservedObject var m: Model
	@ObservedObject var apps: AppsModel
	let open: (Page) -> Void

	var body: some View {
		PageLayout(title: "Map", subtitle: "The apps that ask for work, the agents doing it, the pool they run in, and the network under it. Click anything to open it.") {
			if m.computers.isEmpty {
				Text(m.poolChecked ? "No computers yet." : "Loading…").foregroundStyle(.secondary)
			} else {
				NetworkMap(m: m, apps: apps.apps, open: open)
			}
		}
		.task { await apps.refresh() }
	}
}

/// Your pool: every computer agents can run on, and how the pool is run.
/// Average round trip in ms over the private network (3 pings), nil if none came back.
func pingMs(_ ip: String) async -> Double? {
	await withCheckedContinuation { cont in
		DispatchQueue.global().async {
			let p = Process()
			p.executableURL = URL(fileURLWithPath: "/sbin/ping")
			p.arguments = ["-c", "3", "-i", "0.2", "-t", "3", "-q", ip]
			let out = Pipe(); p.standardOutput = out; p.standardError = Pipe()
			guard (try? p.run()) != nil else { return cont.resume(returning: nil) }
			p.waitUntilExit()
			let text = String(data: out.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
			// "round-trip min/avg/max/stddev = 4.1/5.2/6.3/0.8 ms"
			guard let line = text.split(separator: "\n").first(where: { $0.contains("min/avg") }),
				  let nums = line.split(separator: "=").last?.trimmingCharacters(in: .whitespaces).split(separator: "/"),
				  nums.count >= 2, let avg = Double(nums[1]) else { return cont.resume(returning: nil) }
			cont.resume(returning: avg)
		}
	}
}

/// Time for a small HTTPS request to Cerver's gateway (best of 2, connection reused).
func cerverPing() async -> Double? {
	guard let url = URL(string: "https://gateway.cerver.ai/health") else { return nil }
	var best: Double?
	for _ in 0..<2 {
		var r = URLRequest(url: url); r.httpMethod = "HEAD"; r.timeoutInterval = 5
		let t = Date()
		guard (try? await URLSession.shared.data(for: r)) != nil else { continue }
		let ms = Date().timeIntervalSince(t) * 1000
		best = min(best ?? ms, ms)
	}
	return best
}

struct PoolPage: View {
	@ObservedObject var m: Model
	let open: (Page) -> Void
	@State private var view = "Computers"
	@State private var help = false
	@State private var info: PoolInfo?
	@State private var copied = false
	@State private var pings: [String: Double] = [:]      // net ip → ms; -1 = no reply
	@State private var cerverMs: Double?
	@State private var direct = NetSettings.direct
	@State private var renamingPool = false

	var body: some View {
		PageLayout(title: m.poolTitle, subtitle: "Every computer your agents can run on, and how they connect.", rename: { renamingPool = true }) {
			HStack {
				Picker("", selection: $view) {
					Text("Computers").tag("Computers")
					Text("Coding agents").tag("Coding agents")
					Text("Provisioning").tag("Provisioning")
				}
				.pickerStyle(.segmented).labelsHidden().fixedSize()
				Spacer()
				Button { help.toggle() } label: { Image(systemName: "questionmark.circle") }
					.buttonStyle(.borderless).help("What these mean")
					.popover(isPresented: $help) {
						VStack(alignment: .leading, spacing: 6) {
							legend("Agents", "Ready: agents can run on it now. Busy: running as many as it takes. Offline: its relay isn't running.")
							legend("Network", "On: your computers reach it by name. Direct means computer to computer; via relay means through Cerver's backup relay.")
						}
						.padding(16).frame(width: 360)
					}
			}
			if m.computers.isEmpty {
				Text(m.poolChecked ? "No computers yet." : "Loading…").foregroundStyle(.secondary)
			} else if view == "Coding agents" {
				harnessTable
			} else if view == "Provisioning" {
				provisioningTable
			} else {
				Card {
					Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 0) {
						GridRow {
							header("Computer")
							header("Agents")
							header("Network")
						}
						.padding(.horizontal, 16).padding(.vertical, 10)
						ForEach(m.shown) { c in
							Divider().gridCellColumns(3)
							GridRow {
								HStack(spacing: 8) {
									Image(systemName: c.isThisMac ? "laptopcomputer" : "desktopcomputer").foregroundStyle(.secondary)
									VStack(alignment: .leading, spacing: 1) {
										Text(c.name).font(.system(size: 14, weight: .medium)).lineLimit(1)
										let sub = ([c.isThisMac ? "This Mac" : nil, c.spec?.short, c.spec?.disk_free_gb.map { gb($0) + " free" }].compactMap { $0 }).joined(separator: " · ")
										if !sub.isEmpty {
											Text(sub).font(.system(size: 11)).foregroundStyle(.secondary).lineLimit(1)
										}
									}
									.layoutPriority(1)
								}
								VStack(alignment: .leading, spacing: 3) {
									StatusPill(text: agentsStatus(c).0, color: agentsStatus(c).1).fixedSize()
									Text(running(c)).font(.system(size: 11).monospacedDigit()).foregroundStyle(.secondary).fixedSize()
								}
								VStack(alignment: .leading, spacing: 3) {
									StatusPill(text: networkStatus(c, peer: peer(c)).0.hasPrefix("On") ? "On" : "Off", color: networkStatus(c, peer: peer(c)).1).fixedSize()
									pingView(c).fixedSize()
								}
							}
							.padding(.horizontal, 16).padding(.vertical, 9)
							.contentShape(Rectangle())
							.onTapGesture { open(c.isThisMac ? .thisMac : .computer(c.id)) }
						}
					}
				}
			}
			HStack(spacing: 18) {
				Button { open(.permissions) } label: { Label("Security", systemImage: "lock.shield") }.buttonStyle(.link)
				Button { open(.add) } label: { Label("Add Compute", systemImage: "plus") }.buttonStyle(.link)
			}

			section("Network between your computers")
			Card {
				Row(label: "Private network") {
					let on = m.computers.filter { $0.net?.online == true }.count
					Text("\(on) of \(m.computers.count) computers on it").foregroundStyle(.secondary)
				}
				Divider().padding(.leading, 16)
				Row(label: "Connections") {
					Text(connections).foregroundStyle(.secondary)
				}
				Divider().padding(.leading, 16)
				Row(label: "Direct connections", note: direct
					? "Fastest: this Mac talks to your other computers straight, and uses the backup route only when it must."
					: "Off: everything goes through Cerver's backup route. Slower, and this Mac's address isn't shared.") {
					if m.status != nil && m.status?.direct == nil {
						Text("update the network first").font(.system(size: 11)).foregroundStyle(.tertiary)
					}
					Toggle("", isOn: Binding(get: { direct }, set: { direct = $0; NetSettings.setDirect($0) }))
						.toggleStyle(.switch).labelsHidden()
						.disabled(m.status != nil && m.status?.direct == nil)
				}
				Divider().padding(.leading, 16)
				Row(label: "Ping to Cerver", note: "From this Mac to Cerver, which starts your agents.") {
					Text(cerverMs.map { "\(Int($0.rounded())) ms" } ?? "Measuring…").monospacedDigit()
						.foregroundStyle(cerverMs.map { pingColor($0) } ?? .secondary)
				}
			}
			Text("Your computers connect directly, computer to computer, and use Cerver's backup route only when a direct connection can't be made.")
				.font(.callout).foregroundStyle(.secondary)

		}
		.sheet(isPresented: $renamingPool) { PoolNameSheet(m: m, shown: $renamingPool) }
		.task { await load() }
		.task {
			// Measure every 10 s while the page is open.
			while !Task.isCancelled {
				await measure()
				try? await Task.sleep(nanoseconds: 10_000_000_000)
			}
		}
	}

	func running(_ c: Computer) -> String {
		guard let p = c.pool, p.online else { return "not running" }
		return "\(p.running ?? 0) of \(p.max ?? 1) running"
	}

	@ViewBuilder func pingView(_ c: Computer) -> some View {
		let route = peer(c).flatMap { $0.connected ? ($0.via == "direct" ? "direct" : "via backup") : nil }
		if c.isThisMac {
			Text("this Mac").font(.system(size: 11)).foregroundStyle(.secondary)
		} else if let ip = c.net?.ip, let ms = pings[ip] {
			if ms < 0 {
				Text("no reply").font(.system(size: 11)).foregroundStyle(.red)
			} else {
				(Text(ms < 10 ? String(format: "%.1f ms", ms) : "\(Int(ms.rounded())) ms").foregroundColor(pingColor(ms)).fontWeight(.medium)
				 + Text(route.map { " · " + $0 } ?? "").foregroundColor(.secondary))
					.font(.system(size: 11).monospacedDigit())
			}
		} else {
			Text(route ?? (c.net == nil ? "not on it" : "measuring…")).font(.system(size: 11)).foregroundStyle(.secondary)
		}
	}

	func pingColor(_ ms: Double) -> Color { ms < 40 ? .green : (ms < 150 ? .orange : .red) }

	func measure() async {
		if m.isOn {
			let ips = m.computers.filter { !$0.isThisMac }.compactMap { $0.net?.ip }
			await withTaskGroup(of: (String, Double).self) { g in
				for ip in ips { g.addTask { (ip, await pingMs(ip) ?? -1) } }
				for await (ip, ms) in g { pings[ip] = ms }
			}
		}
		cerverMs = await cerverPing()
	}

	var connections: String {
		guard m.isOn, let s = m.status else { return "This Mac is off the network" }
		let direct = s.peers.filter { $0.connected && $0.via == "direct" }.count
		let relayed = s.peers.filter { $0.connected && $0.via == "relay" }.count
		if direct + relayed == 0 { return "No other computer connected" }
		return [direct > 0 ? "\(direct) direct" : nil, relayed > 0 ? "\(relayed) via backup" : nil].compactMap { $0 }.joined(separator: " · ")
	}

	func section(_ t: String) -> some View {
		Text(t).font(.system(size: 15, weight: .semibold)).padding(.top, 8)
	}

	func load() async {
		if let d = await gatewayGET("/v2/pool"), let i = try? JSONDecoder().decode(PoolInfo.self, from: d) { info = i }
	}

	func act(_ method: String, _ path: String) {
		Task {
			_ = await gatewayRequest(method, path)
			await load()
			m.refreshPool(force: true)
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
					ForEach(m.shown) { c in
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

	/// What's provisioned on each computer. Set up on each computer's page → Provisioning.
	var provisioningTable: some View {
		Card {
			ForEach(Array(m.shown.enumerated()), id: \.element.id) { n, c in
				if n > 0 { Divider().padding(.leading, 16) }
				Row(label: c.name + (c.isThisMac ? "  (this Mac)" : ""), note: vmNote(c.spec?.provisioning?.vm)) {
					Button("Open") { open(c.isThisMac ? .thisMac : .computer(c.id)) }.buttonStyle(.link)
				}
			}
		}
	}

	func vmNote(_ v: VMState?) -> String {
		guard let v = v else { return "Local VM: not reported yet" }
		if !v.supported { return "Local VM: not available (\(v.why ?? "this computer"))" }
		if v.ready && v.signed_in { return "Local VM: ready" }
		if v.ready { return "Local VM: ready, needs your Claude login" }
		if v.installed { return "Local VM: installed, not started" }
		return "Local VM: available, not installed"
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
	case overview = "Overview", access = "Access", agents = "Agents", network = "Network", software = "Software", hardware = "Hardware", settings = "Settings"
	var id: String { rawValue }
	/// The parts of a computer's page, top to bottom.
	static let page: [Tab] = [.overview, .access, .agents, .network, .software, .hardware]
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
	@ObservedObject var nav: SectionNav
	@State private var editing = false
	@State private var newName = ""
	@State private var renaming = false
	@StateObject private var vm = VMModel()

	var me: Computer? { m.computers.first(where: { $0.isThisMac }) }

	var body: some View {
		PageLayout(title: me?.name ?? m.relayName, subtitle: "This Mac", rename: me?.pool == nil ? nil : { renaming = true }) {
			if case .failed(let why) = m.phase { problem(why) }
			if case .failed(let why) = m.relay { problem(why) }
			SectionedPage(nav: nav) { t in
				switch t {
				case .overview: overview
				case .access: AccessSection(c: me ?? Computer(name: m.relayName, pool: nil, net: nil, isThisMac: true), m: m)
				case .agents: agents
				case .network: network
				case .software:
					VMSection(vm: vm, reported: nil, computer: me?.name ?? m.relayName, isThisMac: true)
					SoftwareSection(spec: me?.spec)
				case .hardware, .settings: SpecSection(spec: me?.spec, showTitle: false)
				}
			}
		}
		.sheet(isPresented: $renaming) { RenameSheet(m: m, computeID: me?.pool?.compute_id ?? "", current: me?.name ?? m.relayName, shown: $renaming) }
	}

	var overview: some View {
		Card {
			Row(label: "Run agents", note: "Part of your compute pool: Cerver can start your agents here, as you.") {
				StatusPill(text: relayText.0, color: relayText.1)
				if relayWorking { ProgressView().controlSize(.small) }
				Toggle("", isOn: Binding(get: { relayOn }, set: { $0 ? m.relayOn() : m.relayOff() }))
					.toggleStyle(.switch).labelsHidden().disabled(relayWorking)
			}
			Divider().padding(.leading, 16)
			Row(label: "Private network", note: "Your other computers can reach it: SSH, files, its screen.") {
				StatusPill(text: m.isOn ? "On" : (m.busy ? "…" : "Off"), color: m.isOn ? .green : .gray)
				if m.busy { ProgressView().controlSize(.small) }
				Toggle("", isOn: Binding(get: { m.isOn || m.phase == .starting }, set: { $0 ? m.turnOn() : m.turnOff() }))
					.toggleStyle(.switch).labelsHidden().disabled(m.busy)
			}
			if let me = me, let p = me.pool {
				Divider().padding(.leading, 16)
				Row(label: "Share with people you invite", note: "Their agents can run here, fenced off from your files.") {
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
				Row(label: "Share with people you invite", note: "Their agents can run here, fenced off from your files.") {
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
	@ObservedObject var nav: SectionNav
	@State private var renaming = false

	var peer: NetPeer? { m.status?.peers.first { $0.ip == c.net?.ip } }

	var body: some View {
		PageLayout(title: c.name, subtitle: subtitle, rename: c.pool == nil ? nil : { renaming = true }) {
			SectionedPage(nav: nav) { t in
				switch t {
				case .overview: overview
				case .access: AccessSection(c: c, m: m)
				case .agents: agents
				case .network: network
				case .software:
					VMSection(vm: nil, reported: c.spec?.provisioning?.vm, computer: c.name, isThisMac: false, online: c.pool?.online == true)
					SoftwareSection(spec: c.spec)
				case .hardware, .settings: SpecSection(spec: c.spec, showTitle: false)
				}
			}
		}
		.id(c.id)  // a different computer starts on Overview
		.sheet(isPresented: $renaming) { RenameSheet(m: m, computeID: c.pool?.compute_id ?? "", current: c.name, shown: $renaming) }
	}

	@ViewBuilder var overview: some View {
		// Same rows as This Mac. Run agents and Private network switch on that
		// computer itself (turning them off from here would cut the way back).
		Card {
			Row(label: "Run agents", note: "Part of your compute pool: Cerver can start your agents here, as you.") {
				StatusPill(text: agentsStatus(c).0, color: agentsStatus(c).1)
				Toggle("", isOn: .constant(c.pool?.online == true)).toggleStyle(.switch).labelsHidden().disabled(true)
					.help("Switch this on \(c.name) itself")
			}
			Divider().padding(.leading, 16)
			Row(label: "Private network", note: "Your other computers can reach it: SSH, files, its screen.") {
				StatusPill(text: networkStatus(c, peer: peer).0, color: networkStatus(c, peer: peer).1)
				Toggle("", isOn: .constant(c.net?.online == true)).toggleStyle(.switch).labelsHidden().disabled(true)
					.help("Switch this on \(c.name) itself")
			}
			Divider().padding(.leading, 16)
			Row(label: "Share with people you invite", note: "Their agents can run here, fenced off from your files.") {
				StatusPill(text: sharedStatus(c).0, color: sharedStatus(c).1)
				if let id = c.pool?.compute_id, m.sharing.contains(id) { ProgressView().controlSize(.small) }
				Toggle("", isOn: Binding(get: { c.pool?.shared == true }, set: { m.setShared(c, $0) }))
					.toggleStyle(.switch).labelsHidden()
					.disabled(c.pool?.compute_id.map { m.sharing.contains($0) } ?? true)
			}
		}
		Text("Run agents and Private network switch on \(c.name) itself.")
			.font(.callout).foregroundStyle(.secondary)
		if let n = c.net, let p = peer, p.connected {
			HStack(spacing: 10) {
				Button {
					viewScreen(n.ip)
				} label: {
					Label("View screen", systemImage: "display").padding(.horizontal, 6).padding(.vertical, 3)
				}
				.buttonStyle(.borderedProminent).controlSize(.large).clipShape(Capsule())
				.help("Opens its screen in Screen Sharing. Needs Screen Sharing on in that Mac's System Settings → General → Sharing.")
				Button {
					openSSH(n.name)
				} label: {
					Label("Open SSH in Terminal", systemImage: "terminal").padding(.horizontal, 6).padding(.vertical, 3)
				}
				.buttonStyle(.bordered).controlSize(.large).clipShape(Capsule())
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
				Row(label: "Share with people you invite", note: "Their agents can run here, fenced off from your files.") {
					if let id = p.compute_id, m.sharing.contains(id) { ProgressView().controlSize(.small) }
					Toggle("", isOn: Binding(get: { p.shared == true }, set: { m.setShared(c, $0) }))
						.toggleStyle(.switch).labelsHidden()
						.disabled(p.compute_id.map { m.sharing.contains($0) } ?? true)
				}
			}
		}
		if c.pool == nil {
			Text("Not in your compute pool. On that computer, run the Cerver install.").font(.callout).foregroundStyle(.secondary)
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
	@State private var info: PoolInfo?

	var body: some View {
		PageLayout(title: "Add Compute", subtitle: "Your own computer, or computers from a cloud provider.") {
			Text("Your own computer").font(.system(size: 17, weight: .semibold))
			Text("Private. A Mac or Linux computer you own, at home, at work or a server you rent.")
				.font(.callout).foregroundStyle(.secondary)
			Card {
				VStack(alignment: .leading, spacing: 10) {
					Text("1.  Open a terminal on that computer and run:").font(.system(size: 14))
					CommandBox(cmd: addDeviceCommand, m: m)
					Text("2.  It asks for that computer's password once, then shows up in your compute pool within seconds.")
						.font(.system(size: 14))
				}
				.padding(16)
			}

			Text("Cloud computers").font(.system(size: 17, weight: .semibold)).padding(.top, 10)
			Text("Public. Agents start a fresh computer at a provider when they need one. Connect your own account and the provider bills you at its price; Cerver adds nothing.")
				.font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
			LazyVGrid(columns: [GridItem(.flexible(), spacing: 12), GridItem(.flexible(), spacing: 12)], spacing: 12) {
				ForEach(clouds, id: \.name) { p in vendor(p) }
			}
		}
		.task {
			if let d = await gatewayGET("/v2/pool"), let i = try? JSONDecoder().decode(PoolInfo.self, from: d) { info = i }
		}
	}

	var clouds: [PoolInfo.Cloud] {
		let order = ["E2B", "Daytona", "Modal", "Vercel"]
		var list = info?.cloud ?? []
		for n in order where !list.contains(where: { $0.name == n }) {
			list.append(PoolInfo.Cloud(name: n, connected: false, yours: false))
		}
		return list.sorted { (order.firstIndex(of: $0.name) ?? 9) < (order.firstIndex(of: $1.name) ?? 9) }
	}

	func vendor(_ p: PoolInfo.Cloud) -> some View {
		VStack(alignment: .leading, spacing: 10) {
			HStack(spacing: 10) {
				vendorLogo(p.name).frame(width: 30, height: 30).clipShape(RoundedRectangle(cornerRadius: 7))
				Text(p.name).font(.system(size: 15, weight: .semibold))
				Spacer()
			}
			Text(p.connected ? (p.yours ? "On your own account" : "Free first computers, on Cerver") : "Not connected")
				.font(.system(size: 12)).foregroundStyle(.secondary)
			Button(p.connected && p.yours ? "Manage" : (p.connected ? "Use my account" : "Connect")) { openWeb("/dashboard/connectors") }
				.buttonStyle(.bordered)
		}
		.padding(14)
		.frame(maxWidth: .infinity, alignment: .leading)
		.background(RoundedRectangle(cornerRadius: 12).fill(Color(nsColor: .controlBackgroundColor)))
		.overlay(RoundedRectangle(cornerRadius: 12).stroke(Color.primary.opacity(0.08)))
	}
}

/// A cloud provider's logo, bundled with the app.
@ViewBuilder func vendorLogo(_ name: String) -> some View {
	if let path = Bundle.main.path(forResource: name.lowercased(), ofType: "png"), let img = NSImage(contentsOfFile: path) {
		Image(nsImage: img).resizable().interpolation(.high).aspectRatio(contentMode: .fit)
	} else {
		Image(systemName: "cloud").resizable().aspectRatio(contentMode: .fit).foregroundStyle(.secondary)
	}
}

/// What each computer allows, one permission per card, each explained.
struct PermissionsPage: View {
	@ObservedObject var m: Model
	@State private var advanced = false

	var computers: [Computer] { m.computers.filter { !$0.isPhone } }
	var others: [Computer] { computers.filter { !$0.isThisMac } }

	struct Choice { let id, title, plain, example, icon: String }
	var choices: [Choice] { [
		Choice(id: "as_you", title: "Everything you can open",
		       plain: "The agent works like you at the keyboard: it can open any file you can.",
		       example: "Handy for \"clean my Desktop\" or \"why is my Mac slow\". Least safe.", icon: "person.fill"),
		Choice(id: "own_folder", title: "Only the folder it works in",
		       plain: "The agent sees the project folder it was given, and nothing else of yours.",
		       example: "Your Documents, Desktop, photos and keys are invisible to it. Good default.", icon: "folder.fill"),
		Choice(id: "vm", title: "A separate sandbox computer",
		       plain: "The agent runs in a throwaway Linux computer inside your Mac, with only its folder copied in.",
		       example: vmState.ready && vmState.signed_in ? "Safest. It can install anything without touching your Mac."
		                : "Safest. Set it up first: this Mac → Software → Provisioning.", icon: "shippingbox.fill"),
	] }

	var body: some View {
		PageLayout(title: "Security", subtitle: "What your agents can see on your computers, and who can use them.") {
			summary

			section("1. How much of this Mac can your agents see?")
			Text("Applies to agents sent here through Cerver: from this app, Ask Compute Pool, or your other computers. Claude Code you open yourself in Terminal follows its own settings. Other people's agents never see your files.")
				.font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
			Card {
				ForEach(Array(choices.enumerated()), id: \.element.id) { n, ch in
					if n > 0 { Divider().padding(.leading, 16) }
					let ready = ch.id != "vm" || (vmState.ready && vmState.signed_in)
					Button { if ready { sec.mode = ch.id; saveSec() } } label: {
						HStack(alignment: .top, spacing: 12) {
							Image(systemName: ch.icon).font(.system(size: 16)).frame(width: 22).foregroundStyle(ready ? Color.accentColor : Color.secondary).padding(.top, 2)
							VStack(alignment: .leading, spacing: 3) {
								Text(ch.title).font(.system(size: 14, weight: .semibold))
								Text(ch.plain).font(.callout).fixedSize(horizontal: false, vertical: true)
								Text(ch.example).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
							}
							Spacer()
							Image(systemName: sec.mode == ch.id ? "checkmark.circle.fill" : "circle")
								.font(.system(size: 18)).foregroundStyle(sec.mode == ch.id ? Color.accentColor : Color.secondary.opacity(0.5))
						}
						.padding(.horizontal, 16).padding(.vertical, 12).contentShape(Rectangle())
						.opacity(ready ? 1 : 0.55)
					}
					.buttonStyle(.plain)
				}
			}
			if !others.isEmpty {
				ForEach(others) { c in
					Text("\(c.name): \(modeTitle(c.spec?.agent_access?.mode)). To change it, open Cerver on \(c.name).")
						.font(.caption).foregroundStyle(.secondary)
				}
			}

			if sec.mode == "own_folder" {
				Text("Where agents work on this Mac").font(.system(size: 13, weight: .semibold)).padding(.top, 4)
				Card {
					Row(label: "The folder each agent is started in", note: "Usually a project, e.g. the folder you open in Claude Code.") { EmptyView() }
					Divider().padding(.leading, 16)
					Row(label: "When no folder is given, agents start in", note: sec.startShown + "  ·  Ask Compute Pool, cerver run") {
						Button("Change…") { if let f = SecurityRules.pick("Start here").first { sec.start = f; saveSec() } }.buttonStyle(.link)
						if !sec.start.isEmpty { Button("Default") { sec.start = ""; saveSec() }.buttonStyle(.link) }
					}
				}
			}

			section("2. Folders")
			Text("Three lists for every agent sent to this Mac through Cerver. A folder is on one list at most.")
				.font(.callout).foregroundStyle(.secondary)
			Card {
				if sec.mode == "own_folder" {
					FolderList(icon: "checkmark.circle.fill", color: .green, title: "Allowed", explain: "may open, besides the folder it works in",
					           folders: sec.allowed, editable: true, add: { put(\.allowed, "Allow") }, remove: { f in sec.allowed.removeAll { $0 == f }; saveSec() })
					Divider().padding(.leading, 16)
				}
				FolderList(icon: "hand.raised.fill", color: .orange, title: "Ask first", explain: "a dialog here asks you, per task",
				           folders: sec.ask, editable: true, add: { put(\.ask, "Ask first") }, remove: { f in sec.ask.removeAll { $0 == f }; saveSec() })
				Divider().padding(.leading, 16)
				FolderList(icon: "xmark.octagon.fill", color: .red, title: "Never", explain: "closed, whatever else is set",
				           folders: sec.blocked, editable: true, add: { put(\.blocked, "Never allow") }, remove: { f in sec.blocked.removeAll { $0 == f }; saveSec() })
			}
			HStack(spacing: 14) {
				ForEach(suggested.filter { !sec.blocked.contains($0) }, id: \.self) { f in
					Button("Never " + folderName(f)) { sec.ask.removeAll { $0 == f }; sec.allowed.removeAll { $0 == f }; sec.blocked.append(f); saveSec() }.buttonStyle(.link).font(.callout)
				}
			}
			Text(SecurityRules.guarded ? "Changes ask for this Mac's password, so an agent can't change them by itself." : "Your next change moves these rules into an admin-only file; from then on, changes ask for this Mac's password.")
				.font(.caption).foregroundStyle(.secondary)
			if let e = secError { Text(e).font(.callout).foregroundStyle(.red) }

			section("3. Who can use each computer")
				.task { vmState = await VMModel.fetch() ?? VMState() }
			Text("A grey switch belongs to another computer: change it in Cerver on that computer.")
				.font(.callout).foregroundStyle(.secondary)
			ForEach(computers) { c in
				Card {
					Text(c.name + (c.isThisMac ? "  (this Mac)" : "")).font(.system(size: 15, weight: .semibold))
						.padding(.horizontal, 16).padding(.top, 14).padding(.bottom, 4)
					Row(label: "Your agents can run here", note: "Cerver can start Claude or Codex on it for you.") {
						cell(on: c.isThisMac ? (m.relay == .running || m.relay == .starting) : c.pool?.online == true,
							enabled: c.isThisMac && !relayBusy, remote: !c.isThisMac) { $0 ? m.relayOn() : m.relayOff() }
					}
					Divider().padding(.leading, 16)
					Row(label: "Your other computers can connect to it", note: "Reach it by name: files, SSH, its screen.") {
						cell(on: c.isThisMac ? (m.isOn || m.phase == .starting) : c.net?.online == true,
							enabled: c.isThisMac && !m.busy, remote: !c.isThisMac) { $0 ? m.turnOn() : m.turnOff() }
					}
					Divider().padding(.leading, 16)
					Row(label: "People you invited can run agents here", note: "In a fenced-off space; they never see your files.") {
						cell(on: c.pool?.shared == true,
							enabled: c.pool?.compute_id.map { !m.sharing.contains($0) } ?? false, remote: false) { m.setShared(c, $0) }
					}
				}
			}
			Text("Rules apply from the next agent that starts, enforced by macOS's own sandbox (the one Claude Code and Codex use).")
				.font(.caption).foregroundStyle(.secondary)
		}
	}

	/// One line: where this Mac stands.
	var summary: some View {
		let shared = computers.first { $0.isThisMac }?.pool?.shared == true
		return HStack(spacing: 10) {
			Image(systemName: sec.mode == "as_you" ? "exclamationmark.shield" : "checkmark.shield.fill")
				.font(.system(size: 22)).foregroundStyle(sec.mode == "as_you" ? Color.orange : Color.green)
			VStack(alignment: .leading, spacing: 2) {
				Text("On this Mac, agents see: \(modeTitle(sec.mode).lowercased()).").font(.system(size: 14, weight: .medium))
				Text("\(sec.blocked.count) never · \(sec.ask.count) ask first · \(sec.allowed.count) allowed · \(shared ? "shared with people you invited" : "not shared with anyone")")
					.font(.callout).foregroundStyle(.secondary)
			}
		}
		.padding(14)
		.frame(maxWidth: .infinity, alignment: .leading)
		.background(RoundedRectangle(cornerRadius: 12).fill(Color.primary.opacity(0.04)))
	}

	func modeTitle(_ m: String?) -> String {
		switch m {
		case "own_folder": return "Only the folder it works in"
		case "vm": return "A separate sandbox computer"
		case "as_you": return "Everything you can open"
		default: return "not reported yet"
		}
	}

	func folderName(_ f: String) -> String {
		switch f {
		case "~/.ssh": return "SSH keys"
		case "~/Documents": return "Documents"
		case "~/Desktop": return "Desktop"
		default: return (f as NSString).lastPathComponent
		}
	}

	var relayBusy: Bool { if case .working = m.relay { return true }; return false }

	@State private var sec = SecurityRules.load()
	@State private var vmState = VMState()
	@State private var secError: String?
	let suggested = ["~/.ssh", "~/Documents", "~/Desktop"]

	func section(_ t: String) -> some View {
		Text(t).font(.system(size: 17, weight: .semibold)).padding(.top, 10)
	}

	func modeName(_ m: String?) -> String {
		switch m {
		case "own_folder": return "Own folder only"
		case "vm": return "In a VM"
		case "as_you": return "As you"
		default: return "Not reported yet"
		}
	}

	func accessNote(_ c: Computer) -> String {
		let mode = c.isThisMac ? sec.mode : (c.spec?.agent_access?.mode ?? "")
		switch mode {
		case "own_folder": return "Agents see only the folder they work in, plus their own settings."
		case "vm": return "Each agent runs in a fresh Linux VM that sees only the folder it works in."
		case "as_you": return "Agents can open what you can, except blocked folders."
		default: return "Its relay hasn't reported its rules yet."
		}
	}

	func put(_ list: WritableKeyPath<SecurityRules, [String]>, _ prompt: String) {
		let picked = SecurityRules.pick(prompt)
		guard !picked.isEmpty else { return }
		for f in picked {
			sec.allowed.removeAll { $0 == f }; sec.ask.removeAll { $0 == f }; sec.blocked.removeAll { $0 == f }
			sec[keyPath: list].append(f)
		}
		saveSec()
	}

	func saveSec() {
		sec.commit { err in
			secError = err?.contains("cancelled") == true ? nil : err
			sec = SecurityRules.load() // what's really saved (unchanged if cancelled)
		}
	}

	func addFolder() {
		let p = NSOpenPanel()
		p.canChooseDirectories = true; p.canChooseFiles = false; p.allowsMultipleSelection = true
		p.directoryURL = URL(fileURLWithPath: NSHomeDirectory())
		p.prompt = "Block"
		guard p.runModal() == .OK else { return }
		let home = NSHomeDirectory()
		for u in p.urls {
			let path = u.path.hasPrefix(home) ? "~" + u.path.dropFirst(home.count) : u.path
			if !sec.blocked.contains(path) { sec.blocked.append(path) }
		}
		saveSec()
	}

	func permission(_ title: String, _ explain: String, @ViewBuilder control: @escaping (Computer) -> some View) -> some View {
		VStack(alignment: .leading, spacing: 8) {
			Text(title).font(.system(size: 15, weight: .semibold)).padding(.top, 6)
			Text(explain).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
			Card {
				ForEach(Array(computers.enumerated()), id: \.element.id) { n, c in
					if n > 0 { Divider().padding(.leading, 16) }
					Row(label: c.name + (c.isThisMac ? "  (this Mac)" : "")) { control(c) }
				}
			}
		}
	}

	func cell(on: Bool, enabled: Bool, remote: Bool, set: @escaping (Bool) -> Void) -> some View {
		HStack(spacing: 8) {
			if remote { Text("switch on that computer").font(.system(size: 11)).foregroundStyle(.tertiary) }
			Toggle("", isOn: Binding(get: { on }, set: set))
				.toggleStyle(.switch).labelsHidden().disabled(!enabled)
		}
	}
}

/// Your profile: who you are on Cerver and your plan, with a way to upgrade.
struct ProfilePage: View {
	@ObservedObject var m: Model
	@State private var account: Account?
	@State private var plans: [Plan]?

	struct Account: Decodable { let name: String?; let email: String?; let created_at: String? }
	struct Plan: Decodable { let plan: String; let status: String; let renews_at: String?; let cancel_at_period_end: Bool? }
	struct PlansResp: Decodable { let plans: [Plan] }

	/// Same catalogue as the gateway's (billing/plan_requests.ts).
	static let catalogue: [String: (String, Int)] = [
		"compute-small": ("Cerver Compute Small", 20), "compute-builder": ("Cerver Compute Builder", 49),
		"compute-team": ("Cerver Compute Team", 99), "compute-scale": ("Cerver Compute Scale", 199),
		"agents-starter": ("Cerver Agents Starter", 29), "agents-team": ("Cerver Agents Team", 99),
		"agents-scale": ("Cerver Agents Scale", 399),
	]

	var body: some View {
		PageLayout(title: "Profile", subtitle: "Your Cerver account and plan.") {
			HStack(spacing: 16) {
				Circle().fill(Color.accentColor.opacity(0.85)).frame(width: 56, height: 56)
					.overlay(Text(String((account?.name ?? m.email ?? "?").prefix(1)).uppercased())
						.font(.system(size: 24, weight: .semibold)).foregroundStyle(.white))
				VStack(alignment: .leading, spacing: 3) {
					Text(account?.name ?? "—").font(.system(size: 18, weight: .semibold))
					Text(account?.email ?? m.email ?? "").foregroundStyle(.secondary)
					if let since = since { Text("On Cerver since " + since).font(.callout).foregroundStyle(.tertiary) }
				}
			}

			Text("Plan").font(.system(size: 15, weight: .semibold)).padding(.top, 6)
			Card {
				if let ps = plans {
					if ps.isEmpty {
						Row(label: "Free", note: "Your own computers, plus free first cloud computers.") {
							Button("Upgrade") { openWeb("/pricing") }.buttonStyle(.borderedProminent)
						}
					} else {
						ForEach(Array(ps.enumerated()), id: \.offset) { n, p in
							if n > 0 { Divider().padding(.leading, 16) }
							Row(label: Self.catalogue[p.plan].map { "\($0.0) · $\($0.1)/month" } ?? p.plan, note: planNote(p)) {
								Button("Change plan") { openWeb("/pricing") }.buttonStyle(.bordered)
							}
						}
					}
				} else {
					Row(label: "Loading…") { ProgressView().controlSize(.small) }
				}
			}
			Button { openWeb("/pricing") } label: {
				Label("See all plans on cerver.ai/pricing", systemImage: "arrow.up.right.square")
			}
			.buttonStyle(.link)
		}
		.task {
			if let d = await gatewayGET("/v2/auth/account") { account = try? JSONDecoder().decode(Account.self, from: d) }
			if let d = await gatewayGET("/v2/billing/plans") { plans = (try? JSONDecoder().decode(PlansResp.self, from: d))?.plans }
		}
	}

	var since: String? {
		guard let c = account?.created_at, let d = ISO8601DateFormatter.withFraction.date(from: c) ?? ISO8601DateFormatter().date(from: c) else { return nil }
		return d.formatted(.dateTime.month(.wide).year())
	}

	func planNote(_ p: Plan) -> String {
		var parts: [String] = []
		if p.status == "trialing" { parts.append("Trial") }
		if p.status == "past_due" { parts.append("Payment due") }
		if let r = p.renews_at, let d = ISO8601DateFormatter.withFraction.date(from: r) ?? ISO8601DateFormatter().date(from: r) {
			parts.append((p.cancel_at_period_end == true ? "Ends " : "Renews ") + d.formatted(date: .abbreviated, time: .omitted))
		}
		return parts.isEmpty ? "Active" : parts.joined(separator: " · ")
	}
}

extension ISO8601DateFormatter {
	static let withFraction: ISO8601DateFormatter = {
		let f = ISO8601DateFormatter()
		f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
		return f
	}()
}

/// Settings: your account, what this computer is part of, updates, and the rest.
struct SettingsPage: View {
	@ObservedObject var m: Model

	var body: some View {
		PageLayout(title: only ? "Updates" : "Settings", subtitle: only ? "Cerver \(m.updates?.latest ?? "") is the newest version." : (m.email.map { "Signed in as " + $0 } ?? "Not signed in")) {
			if !only {
				Picker("", selection: $tab) {
					ForEach(["General", "Terminal", "Your agent"], id: \.self) { Text($0).tag($0) }
				}
				.pickerStyle(.segmented).labelsHidden().fixedSize()
			}
			switch tab {
			case "Updates": updates
			case "Terminal": terminal
			case "Your agent": agent
			default: general
			}
		}
		.onAppear { skillInstalled = FileManager.default.fileExists(atPath: skillFile) }
	}

	@State var tab = "General"
	/// Opened from the sidebar's Updates item: just the updates.
	var only: Bool { tab == "Updates" }
	@State private var skillInstalled = false
	@State private var installingSkill = false
	let skillFile = NSHomeDirectory() + "/.claude/skills/cerver/SKILL.md"
	let onboardPrompt = "Read https://cerver.ai/llms.txt and https://cerver.ai/skills/cerver/SKILL.md. Then explain to me in plain words what Cerver is, and how it could help with the work I do on this computer. Look at my projects and give me three concrete examples, each with the exact cerver command to try."

	func heading(_ t: String) -> some View {
		Text(t).font(.system(size: 15, weight: .semibold)).padding(.top, 6)
	}
	func note(_ t: String) -> some View {
		Text(t).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
	}

	@ViewBuilder var terminal: some View {
		heading("Use Cerver from the terminal")
		note("The cerver command came with this app. Open Terminal and try:")
		VStack(alignment: .leading, spacing: 8) {
			cmd("cerver help", "Every command, with examples")
			cmd("cerver pool", "Your computers: free, busy or offline")
			cmd("cerver run \"what's using my disk?\"", "Ask an agent; it runs on a free computer")
			cmd("cerver run --on \(m.computers.first { !$0.isThisMac && !$0.isPhone }?.name ?? m.relayName) --cli codex \"run the tests\"", "Pick the computer and the agent")
			cmd("cerver compare \"explain this repo\"", "Claude and Codex answer side by side")
			cmd("cerver net", "Your private network: who's connected, and how")
		}
		note("If Terminal says \"command not found\", open a new Terminal window, or use ~/.cerver/bin/cerver.")
		heading("Docs")
		Card {
			Row(label: "Documentation", note: "Guides and the full API.") {
				Button("Open") { openWeb("/docs") }.buttonStyle(.link)
			}
			Divider().padding(.leading, 16)
			Row(label: "Quick reference for agents", note: "One page an AI agent can read to learn Cerver.") {
				Button("Open") { openWeb("/llms.txt") }.buttonStyle(.link)
			}
		}
	}

	func cmd(_ c: String, _ what: String) -> some View {
		VStack(alignment: .leading, spacing: 4) {
			Text(what).font(.system(size: 12)).foregroundStyle(.secondary)
			CommandBox(cmd: c, m: m)
		}
	}

	@ViewBuilder var agent: some View {
		heading("Let your own agent explain Cerver")
		note("Paste this into Claude Code, Codex or ChatGPT. It reads about Cerver, looks at your work and tells you how Cerver can help you.")
		CommandBox(cmd: onboardPrompt, m: m)
		heading("Cerver skill for Claude Code")
		Card {
			Row(label: skillInstalled ? "Installed" : "Not installed",
				note: "Teaches Claude Code to run agents on your computers, ask Codex, and compare answers. Lives in ~/.claude/skills/cerver.") {
				if installingSkill { ProgressView().controlSize(.small) }
				Button(skillInstalled ? "Update" : "Install") {
					installingSkill = true
					Task.detached {
						installClaudeSkill()
						await MainActor.run {
							installingSkill = false
							skillInstalled = FileManager.default.fileExists(atPath: skillFile)
						}
					}
				}
				.disabled(installingSkill)
			}
			Divider().padding(.leading, 16)
			Row(label: "Read the skill", note: "The same file, on the web.") {
				Button("Open") { openWeb("/skills/cerver/SKILL.md") }.buttonStyle(.link)
			}
		}
		heading("Tools for any app")
		note("To give Claude Desktop, Cursor, Hermes or OpenClaw your compute pool as tools, connect them in Connectors.")
	}

	var networkSummary: String {
		guard let s = m.status else { return "On" }
		let direct = s.peers.filter { $0.connected && $0.via == "direct" }.count
		let relayed = s.peers.filter { $0.connected && $0.via == "relay" }.count
		var parts: [String] = []
		if direct > 0 { parts.append("direct to \(direct)") }
		if relayed > 0 { parts.append("\(relayed) via backup") }
		return parts.isEmpty ? "On" : parts.joined(separator: " · ")
	}

	@State private var confirmUninstall = false

	func versionRow(_ label: String, _ p: UpdateInfo.Part?, prefix: String = "") -> some View {
		Row(label: label) {
			if let p = p {
				Text(p.current.isEmpty ? "?" : prefix + p.current).font(.system(size: 13, design: .monospaced)).foregroundStyle(.secondary)
				if p.update, let l = p.latest {
					StatusPill(text: "New: " + prefix + l, color: .blue)
				}
			} else {
				Text("not installed").foregroundStyle(.secondary)
			}
		}
	}

	/// When this copy of the app was installed (each update installs a fresh copy).
	var lastUpdated: String {
		let a = try? FileManager.default.attributesOfItem(atPath: Bundle.main.bundlePath)
		guard let d = (a?[.creationDate] as? Date) ?? (a?[.modificationDate] as? Date) else { return "Unknown" }
		return d.formatted(date: .abbreviated, time: .shortened) + " (" + RelativeDateTimeFormatter().localizedString(for: d, relativeTo: Date()) + ")"
	}

	@ViewBuilder var updates: some View {
		Text("Version").font(.system(size: 15, weight: .semibold))
		Card {
			if let u = m.updates {
				versionRow("Cerver app", u.app)
				Divider().padding(.leading, 16)
				versionRow("cerver program", u.cli)
				Divider().padding(.leading, 16)
				versionRow("Relay", u.relay, prefix: "v")
				Divider().padding(.leading, 16)
				versionRow("Network service", u.net)
				Divider().padding(.leading, 16)
				Row(label: "Last updated") {
					Text(lastUpdated).foregroundStyle(.secondary)
				}
				Divider().padding(.leading, 16)
				Row(label: u.any ? "An update is available" : (u.latest.isEmpty ? "Couldn't check for updates" : "Up to date")) {
					if m.updating || m.checking { ProgressView().controlSize(.small) }
					if !u.any, !m.checking, let t = m.checkedAt {
						Text("checked \(t.formatted(date: .omitted, time: .shortened)), latest \(u.latest)").foregroundStyle(.secondary)
					}
					if u.any {
						Button("Update now") { m.updateNow() }.buttonStyle(.borderedProminent).disabled(m.updating)
					} else {
						Button(m.checking ? "Checking…" : "Check again") { m.checkUpdates(force: true) }.buttonStyle(.link).disabled(m.checking)
					}
				}
			} else {
				Row(label: "Checking…") { ProgressView().controlSize(.small) }
			}
		}
		Text("Cerver updates itself every few hours, and never while an agent is running.")
			.font(.callout).foregroundStyle(.secondary)
		if let p = m.updateProblem {
			problem(p)
		}
	}

	@ViewBuilder var general: some View {
		Card {
			Row(label: "Open Cerver at login") {
				Toggle("", isOn: Binding(get: { m.openAtLogin }, set: { m.setOpenAtLogin($0) }))
					.toggleStyle(.switch).labelsHidden()
			}
			Divider().padding(.leading, 16)
			logRow("Agents log", NSHomeDirectory() + "/.cerver/relay.log")
			Divider().padding(.leading, 16)
			logRow("Network log", logFile)
			Divider().padding(.leading, 16)
			Row(label: "Remove Cerver from this Mac") {
				if m.uninstalling { ProgressView().controlSize(.small) }
				Button("Uninstall…", role: .destructive) { confirmUninstall = true }
					.disabled(m.uninstalling)
			}
		}
		.confirmationDialog("Remove Cerver from this Mac?", isPresented: $confirmUninstall) {
			Button("Uninstall", role: .destructive) { m.uninstall() }
		} message: {
			Text("This Mac leaves your compute pool and network. Its sign-in, services, files and this app are deleted. You'll be asked for your password once.")
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
}

// MARK: - Session: run an agent on one of your computers, from the app

/// One conversation with an agent (Claude or Codex) that Cerver runs on a
/// computer you pick. The same as `cerver run`, here as a chat.
final class SessionModel: ObservableObject {
	struct Message: Identifiable {
		let id = UUID()
		let fromYou: Bool
		let text: String
		var meta: String = ""
	}
	@Published var messages: [Message] = []
	@Published var working = false
	@Published var problem: String?
	@Published var computer = ""   // pool name; empty = this Mac
	@Published var cli = "claude"
	var sessionID: String?

	var started: Bool { sessionID != nil || !messages.isEmpty }

	func reset() {
		messages = []
		sessionID = nil
		problem = nil
	}

	func send(_ text: String, on: String) {
		let prompt = text.trimmingCharacters(in: .whitespacesAndNewlines)
		guard !prompt.isEmpty, !working else { return }
		messages.append(Message(fromYou: true, text: prompt))
		working = true
		problem = nil
		var args = ["run", "--json", "--timeout", "1800"]
		// A follow-up continues the session (it keeps its agent and computer).
		if let sid = sessionID { args += ["--resume", sid] } else { args += ["--cli", cli] + (on.isEmpty ? [] : ["--on", on]) }
		args.append(prompt)
		Task.detached {
			struct Reply: Decodable { let session_id: String; let seconds: Int; let reply: String }
			let (ok, out) = runCerver(args)
			let line = out.split(separator: "\n").last.map(String.init) ?? ""
			let r = ok ? try? JSONDecoder().decode(Reply.self, from: Data(line.utf8)) : nil
			await MainActor.run {
				self.working = false
				if let r = r {
					self.sessionID = r.session_id
					self.messages.append(Message(fromYou: false, text: r.reply, meta: "\(self.cli == "codex" ? "Codex" : "Claude") · \(r.seconds) s"))
				} else {
					self.problem = cleanError(out.isEmpty ? "No answer." : out)
				}
			}
		}
	}
}

struct SessionPage: View {
	@ObservedObject var m: Model
	@ObservedObject var s: SessionModel
	@State private var draft = ""
	@State private var tip = 0
	let tips = [
		"Why is my computer slow?",
		"Clean up my Desktop: sort the files into folders by type.",
		"What's using my disk? Show the 10 biggest folders.",
		"Organize my Downloads and delete installers I already used.",
		"Is anything running that I don't need?",
		"Update Homebrew and everything it installed.",
		"Which apps start when I log in?",
		"Find the photos from last week and put them in one folder.",
		"Is my battery healthy?",
		"Check that my backups are working.",
	]
	@FocusState private var focused: Bool

	/// Your computers agents can run on right now.
	var ready: [Computer] { m.computers.filter { $0.pool?.online == true } }
	var target: Computer? {
		ready.first { $0.name == s.computer } ?? ready.first { $0.isThisMac } ?? ready.first
	}

	var body: some View {
		VStack(spacing: 0) {
			header
			Divider()
			ScrollViewReader { proxy in
				ScrollView {
					VStack(alignment: .leading, spacing: 14) {
						if s.messages.isEmpty {
							empty
						}
						ForEach(s.messages) { msg in bubble(msg).id(msg.id) }
						if s.working {
							HStack(spacing: 8) {
								ProgressView().controlSize(.small)
								Text("Working on \(target?.name ?? "your computer")…").foregroundStyle(.secondary)
							}
							.id("working")
						}
						if let p = s.problem {
							Text(p).font(.callout).foregroundStyle(.red).textSelection(.enabled)
						}
					}
					.frame(maxWidth: 680, alignment: .leading)
					.padding(.horizontal, 32).padding(.vertical, 24)
					.frame(maxWidth: .infinity)
				}
				.onChange(of: s.messages.count) { _ in
					withAnimation {
						if s.working { proxy.scrollTo("working", anchor: .bottom) } else if let id = s.messages.last?.id { proxy.scrollTo(id, anchor: .bottom) }
					}
				}
				.onChange(of: s.working) { w in
					if w { withAnimation { proxy.scrollTo("working", anchor: .bottom) } }
				}
			}
			composer
		}
	}

	var header: some View {
		HStack(spacing: 12) {
			Text("Ask Compute Pool").font(.system(size: 17, weight: .semibold))
			Spacer()
			// Where it runs and which agent: chosen before the first message.
			Menu {
				ForEach(ready) { c in
					Button(c.name + (c.isThisMac ? " (this computer)" : "")) { s.computer = c.name }
				}
			} label: {
				Label(target?.name ?? "No computer ready", systemImage: target?.icon ?? "desktopcomputer")
			}
			.fixedSize().disabled(s.started)
			Picker("", selection: $s.cli) {
				Text("Claude").tag("claude")
				Text("Codex").tag("codex")
			}
			.pickerStyle(.segmented).labelsHidden().fixedSize().disabled(s.started)
			if s.started {
				Button("Start over") { s.reset(); draft = "" }.disabled(s.working)
			}
		}
		.padding(.horizontal, 24).padding(.top, 40).padding(.bottom, 12)
	}

	var empty: some View {
		VStack(alignment: .leading, spacing: 8) {
			Text("Ask \(target?.name ?? "your computer") anything").font(.system(size: 22, weight: .semibold))
			Text("An agent (\(s.cli == "codex" ? "Codex" : "Claude"), with your login) answers on that computer, as you, so it can look and act there.")
				.foregroundStyle(.secondary)
			// Rolling examples: a new one every few seconds; click to use it.
			Button { draft = tips[tip]; focused = true } label: {
				HStack(spacing: 8) {
					Text("Try").font(.callout).foregroundStyle(.secondary)
					Text(tips[tip]).font(.system(size: 14, weight: .medium))
						.id(tip)
						.transition(.asymmetric(insertion: .move(edge: .bottom).combined(with: .opacity), removal: .move(edge: .top).combined(with: .opacity)))
					Image(systemName: "arrow.up.left").font(.system(size: 10, weight: .semibold)).foregroundStyle(.secondary)
				}
				.padding(.horizontal, 12).padding(.vertical, 8)
				.background(RoundedRectangle(cornerRadius: 10).fill(Color.primary.opacity(0.05)))
				.clipped()
			}
			.buttonStyle(.plain)
			.help("Click to use this example")
			.padding(.top, 8)
		}
		.padding(.top, 30)
		.onReceive(Timer.publish(every: 3.5, on: .main, in: .common).autoconnect()) { _ in
			withAnimation(.easeInOut(duration: 0.4)) { tip = (tip + 1) % tips.count }
		}
	}

	func bubble(_ msg: SessionModel.Message) -> some View {
		HStack {
			if msg.fromYou { Spacer(minLength: 80) }
			VStack(alignment: .leading, spacing: 4) {
				Text(markdown(msg.text)).textSelection(.enabled).font(.system(size: 13.5))
				if !msg.meta.isEmpty {
					Text(msg.meta).font(.system(size: 10.5)).foregroundStyle(.tertiary)
				}
			}
			.padding(.horizontal, 14).padding(.vertical, 10)
			.background(RoundedRectangle(cornerRadius: 14).fill(msg.fromYou ? Color.accentColor.opacity(0.14) : Color.primary.opacity(0.05)))
			if !msg.fromYou { Spacer(minLength: 40) }
		}
	}

	func markdown(_ t: String) -> AttributedString {
		(try? AttributedString(markdown: t, options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace))) ?? AttributedString(t)
	}

	var composer: some View {
		HStack(alignment: .bottom, spacing: 10) {
			TextField(target == nil ? "No computer is ready" : (s.started ? "Ask \(target?.name ?? "it")…" : tips[tip]), text: $draft, axis: .vertical)
				.textFieldStyle(.plain)
				.lineLimit(1...8)
				.focused($focused)
				.onSubmit(send)
				.padding(.horizontal, 14).padding(.vertical, 10)
				.background(RoundedRectangle(cornerRadius: 16).fill(Color.primary.opacity(0.05)))
			Button(action: send) {
				Image(systemName: "arrow.up").font(.system(size: 13, weight: .bold)).foregroundStyle(.white)
					.frame(width: 30, height: 30)
					.background(Circle().fill(canSend ? Color.accentColor : Color.secondary.opacity(0.4)))
			}
			.buttonStyle(.plain).disabled(!canSend)
			.keyboardShortcut(.return, modifiers: .command)
		}
		.padding(.horizontal, 24).padding(.vertical, 14)
		.frame(maxWidth: 740)
		.frame(maxWidth: .infinity)
		.onAppear { focused = true }
	}

	var canSend: Bool { !s.working && target != nil && !draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }

	func send() {
		guard canSend, let t = target else { return }
		s.send(draft, on: t.isThisMac ? m.relayName : t.name)
		draft = ""
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
				if !m.computers.isEmpty {
					let online = m.computers.filter { ($0.pool?.online ?? false) || ($0.net?.online ?? false) }.count
					Button("Your Computers: \(m.computers.count), \(online) online…") { go(.pool) }
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
			if m.relay == .starting {
				Text("Agents: starting…")
			} else if let j = m.relayJobs {
				Text("Agents: \(j.running.isEmpty ? "ready" : "working") · \(jobsLine(j.today))")
				ForEach(j.running) { r in
					Button("    ▸ \(short(r.task ?? "A job"))  ·  \(since(r.at))") { go(.agents) }
				}
			} else {
				Text("Agents can run on this Mac")
			}
			if let c = m.relayLoad {
				Text([c.cpu_percent.map { "CPU \(Int($0))%" }, c.memory.map { "memory \(Int($0.percent))%" }, c.disk.map { "\(Int($0.free_bytes / 1e9)) GB free" }].compactMap { $0 }.joined(separator: " · "))
			}
			Button("Agents and Jobs…") { go(.agents) }
			Button("Stop Agents on This Mac") { m.relayOff() }
		case .off, .failed:
			Text("Agents can't run on this Mac")
			Button("Let Agents Run on This Mac") { m.relayOn() }
		case .working(let w):
			Text(w)
		}
		Divider()
		if m.updateAvailable, let u = m.updates {
			Button(m.updating ? "Updating…" : "Install Update (\(u.latest))") { m.updateNow() }.disabled(m.updating)
			Divider()
		}
		if let e = m.email { Text("Signed in as \(e)") }
		Button("Open Cerver…") { show() }
		Button("Settings…") { m.openSettings += 1; show() }
		Button("Web Dashboard…") { NSWorkspace.shared.open(URL(string: "https://cerver.ai/dashboard")!) }
		Button("Show Log") { NSWorkspace.shared.open(URL(fileURLWithPath: logFile)) }
		Toggle("Open at Login", isOn: Binding(get: { m.openAtLogin }, set: { m.setOpenAtLogin($0) }))
		Button("Quit") { NSApp.terminate(nil) }
	}

	func show() {
		openWindow(id: "main")
		NSApp.activate(ignoringOtherApps: true)
	}
	func go(_ p: Page) { m.openPage = p; show() }

	func jobsLine(_ t: RelayJobs.Today) -> String {
		if t.jobs == 0 { return "no jobs today" }
		var s = "\(t.jobs) job\(t.jobs == 1 ? "" : "s") today, \(t.done) done"
		if t.failed > 0 { s += ", \(t.failed) failed" }
		return s
	}
	func short(_ s: String) -> String {
		let one = s.split(whereSeparator: \.isNewline).first.map(String.init) ?? s
		return one.count > 40 ? String(one.prefix(39)) + "…" : one
	}
	func since(_ t: Double) -> String {
		let s = Int(Date().timeIntervalSince1970 - t)
		return s < 60 ? "\(s) s" : (s < 3600 ? "\(s / 60) min" : String(format: "%.1f h", Double(s) / 3600))
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
		.commands {
			CommandGroup(replacing: .appSettings) {
				Button("Settings…") { m.openSettings += 1 }.keyboardShortcut(",")
			}
		}

		MenuBarExtra {
			MenuView(m: m)
		} label: {
			Image(nsImage: logoImage(size: 16, gray: !m.isOn, gap: 1))
		}
		.menuBarExtraStyle(.menu)
	}
}


// MARK: Network map

/// Your setup in three layers, top to bottom:
///   Cerver Agents   the agents running for you right now
///   Cerver Compute  your pool: where Cerver sends each agent run (through each computer's relay)
///   Your network    your computers and phones, and how they reach each other
struct NetworkMap: View {
	@ObservedObject var m: Model
	let apps: [ConnectedApp]
	let open: (Page) -> Void

	let height: CGFloat = 700
	let appsY: CGFloat = 62, agentsY: CGFloat = 172, computeY: CGFloat = 298, nodesY: CGFloat = 530
	let band0: CGFloat = 110, band1: CGFloat = 230, band2: CGFloat = 365  // where each layer ends

	var me: Computer? { m.computers.first { $0.isThisMac } }
	var others: [Computer] { m.computers.filter { !$0.isThisMac } }
	var ordered: [Computer] {
		var o = others
		if let me = me { o.insert(me, at: o.count / 2) }
		return o
	}
	var pooled: [Computer] { m.computers.filter { $0.pool != nil } }
	var running: Int { pooled.reduce(0) { $0 + ($1.pool?.running ?? 0) } }
	var free: Int { pooled.filter { $0.pool?.state == "free" }.count }
	var busy: Int { pooled.filter { $0.pool?.state == "busy" }.count }

	func peer(_ c: Computer) -> NetPeer? { m.status?.peers.first { $0.ip == c.net?.ip } }

	var body: some View {
		VStack(alignment: .leading, spacing: 12) {
			GeometryReader { g in
				let w = g.size.width
				let agents = CGPoint(x: w / 2, y: agentsY)
				let appSlots = max(apps.count, 1)
				let appPos: [CGPoint] = (0..<appSlots).map { i in
					CGPoint(x: w / 2 + (CGFloat(i) - CGFloat(appSlots - 1) / 2) * min(190, (w - 40) / CGFloat(appSlots)), y: appsY)
				}
				let compute = CGPoint(x: w / 2, y: computeY)
				let backup = CGPoint(x: w - 92, y: band2 + 44)
				let pos: [String: CGPoint] = Dictionary(uniqueKeysWithValues: ordered.enumerated().map { i, c in
					(c.id, CGPoint(x: (w - 60) * CGFloat(i + 1) / CGFloat(ordered.count + 1) + 10, y: nodesY))
				})
				ZStack {
					// The three layers.
					VStack(spacing: 0) {
						band("CONNECTED APPS", "Apps that ask your pool for work", height: band0, tint: .secondary)
						band("CERVER AGENTS", "Agents running for you", height: band1 - band0, tint: .secondary)
						band("CERVER COMPUTE", "Compute pool: where each agent run goes", height: band2 - band1, tint: .secondary)
						band("YOUR NETWORK", "Your computers and how they reach each other", height: height - band2, tint: .secondary)
					}

					Canvas { ctx, _ in
						// Each app → Agents
						for (i, p) in appPos.enumerated() {
							var l = Path()
							l.move(to: CGPoint(x: p.x, y: p.y + 24))
							l.addCurve(to: CGPoint(x: agents.x, y: agents.y - 26),
								control1: CGPoint(x: p.x, y: p.y + 60), control2: CGPoint(x: agents.x, y: agents.y - 60))
							let recent = i < apps.count && Date().timeIntervalSince(apps[i].lastUsed ?? .distantPast) < 86400
							ctx.stroke(l, with: .color(Color.secondary.opacity(apps.isEmpty ? 0.3 : 0.6)), style: StrokeStyle(lineWidth: 1.5, dash: recent ? [] : [4, 4]))
						}
						// Agents → Compute
						var a = Path()
						a.move(to: CGPoint(x: agents.x, y: agents.y + 26))
						a.addLine(to: CGPoint(x: compute.x, y: compute.y - 30))
						ctx.stroke(a, with: .color(Color.secondary.opacity(0.6)), style: StrokeStyle(lineWidth: 2, dash: running > 0 ? [] : [4, 4]))
						// Compute → each computer's relay
						for c in ordered {
							guard let p = pos[c.id], let pool = c.pool else { continue }
							var path = Path()
							path.move(to: CGPoint(x: compute.x, y: compute.y + 30))
							path.addCurve(to: CGPoint(x: p.x, y: p.y - 58),
								control1: CGPoint(x: compute.x, y: compute.y + 110), control2: CGPoint(x: p.x, y: p.y - 140))
							ctx.stroke(path, with: .color(pool.online ? Color.secondary.opacity(0.6) : Color.secondary.opacity(0.3)),
								style: StrokeStyle(lineWidth: 1.5, dash: pool.online ? [] : [3, 4]))
						}
						guard let me = me, let mp = pos[me.id] else { return }
						// This computer's standby line to the network backup.
						if m.status?.relay_connected == true {
							var path = Path()
							path.move(to: CGPoint(x: mp.x + 40, y: mp.y - 58))
							path.addQuadCurve(to: CGPoint(x: backup.x - 60, y: backup.y + 8), control: CGPoint(x: mp.x + 60, y: backup.y + 20))
							ctx.stroke(path, with: .color(Color.secondary.opacity(0.45)), style: StrokeStyle(lineWidth: 1.5, dash: [5, 4]))
						}
						for c in others {
							guard let p = pos[c.id], let pr = peer(c), pr.connected else { continue }
							if pr.via == "relay" {
								var path = Path()
								path.move(to: CGPoint(x: p.x, y: p.y - 58))
								path.addQuadCurve(to: CGPoint(x: backup.x, y: backup.y + 16), control: CGPoint(x: p.x, y: backup.y + 40))
								ctx.stroke(path, with: .color(Color.secondary), style: StrokeStyle(lineWidth: 2.5, dash: [6, 4]))
							} else {
								var path = Path()
								let a = CGPoint(x: mp.x, y: mp.y + 58), b = CGPoint(x: p.x, y: p.y + 58)
								path.move(to: a)
								path.addQuadCurve(to: b, control: CGPoint(x: (a.x + b.x) / 2, y: a.y + 70))
								ctx.stroke(path, with: .color(Color.primary.opacity(0.7)), style: StrokeStyle(lineWidth: 3, lineCap: .round))
							}
						}
					}

					if apps.isEmpty {
						box(icon: "plus", title: "Connect an app", tint: .secondary, line: "Claude, Cursor, Hermes…")
							.position(appPos[0])
							.onTapGesture { open(.connectors) }
					} else {
						ForEach(Array(apps.enumerated()), id: \.element.id) { i, app in
							box(icon: app.icon, title: app.name, tint: .secondary, line: agoText(app.lastUsed))
								.position(appPos[i])
								.onTapGesture { open(.connectors) }
						}
					}
					Button { open(.connectors) } label: {
						Label("Connect apps", systemImage: "plus")
							.font(.system(size: 11.5, weight: .medium))
					}
					.buttonStyle(.bordered).controlSize(.small)
					.position(x: w - 70, y: 22)
					box(icon: "sparkles", title: "Agents", tint: .secondary,
						line: running == 0 ? "none running now" : "\(running) running now")
						.position(agents)
					box(icon: "server.rack", title: "Compute pool", tint: .secondary,
						line: "\(free) free · \(busy) busy · \(pooled.count - free - busy) offline")
						.position(compute)
						.onTapGesture { open(.pool) }
					backupNode.position(backup)

					ForEach(ordered) { c in
						if let p = pos[c.id] {
							node(c).position(p)
								.onTapGesture { open(c.isThisMac ? .thisMac : .computer(c.id)) }
						}
					}
					if let me = me, let mp = pos[me.id] {
						ForEach(others) { c in
							if let p = pos[c.id], let pr = peer(c), pr.connected, pr.via != "relay" {
								Text("direct").font(.system(size: 11, weight: .semibold)).foregroundStyle(.secondary)
									.padding(.horizontal, 6).padding(.vertical, 1)
									.background(Capsule().fill(Color(nsColor: .windowBackgroundColor)))
									.position(x: (mp.x + p.x) / 2, y: nodesY + 58 + 35)
							}
						}
					}
				}
			}
			.frame(height: height)
			.clipShape(RoundedRectangle(cornerRadius: 14))
			.overlay(RoundedRectangle(cornerRadius: 14).stroke(Color.primary.opacity(0.08)))

			HStack(spacing: 16) {
				key(Color.primary.opacity(0.7), solid: true, "Direct")
				key(.secondary, solid: false, "Network backup")
			}
			Text("Network lines show what this computer sees. Click a computer to open it.")
				.font(.callout).foregroundStyle(.secondary)
		}
	}

	func band(_ title: String, _ sub: String, height: CGFloat, tint: Color) -> some View {
		ZStack(alignment: .topLeading) {
			Rectangle().fill(Color.primary.opacity(0.025))
			VStack(alignment: .leading, spacing: 1) {
				Text(title).font(.system(size: 10.5, weight: .bold)).tracking(0.8).foregroundStyle(tint.opacity(0.85))
				Text(sub).font(.system(size: 10.5)).foregroundStyle(.secondary)
			}
			.padding(.leading, 12).padding(.top, 9)
			VStack { Spacer(); Divider() }
		}
		.frame(height: height)
	}

	func box(icon: String, title: String, tint: Color, line: String) -> some View {
		VStack(spacing: 3) {
			HStack(spacing: 6) {
				Image(systemName: icon).foregroundStyle(tint)
				Text(title).font(.system(size: 13.5, weight: .semibold))
			}
			Text(line).font(.system(size: 11)).foregroundStyle(.secondary)
		}
		.padding(.horizontal, 14).padding(.vertical, 8)
		.background(RoundedRectangle(cornerRadius: 12).fill(Color(nsColor: .windowBackgroundColor)))
		.overlay(RoundedRectangle(cornerRadius: 12).stroke(tint.opacity(0.4)))
	}

	var backupNode: some View {
		HStack(spacing: 5) {
			Image(systemName: "cloud.fill").font(.system(size: 11)).foregroundStyle(.secondary)
			VStack(alignment: .leading, spacing: 0) {
				Text("Network backup").font(.system(size: 11, weight: .semibold))
				Text(m.status?.relay_connected == true ? "ready, on standby" : "not connected").font(.system(size: 10)).foregroundStyle(.secondary)
			}
		}
		.padding(.horizontal, 9).padding(.vertical, 5)
		.background(RoundedRectangle(cornerRadius: 10).fill(Color(nsColor: .windowBackgroundColor)))
		.overlay(RoundedRectangle(cornerRadius: 10).stroke(Color.primary.opacity(0.12)))
	}

	func node(_ c: Computer) -> some View {
		let relayOn = c.pool?.online == true
		let netOn = c.net?.online == true || peer(c)?.connected == true  // a phone never checks in; this computer sees it
		return VStack(spacing: 4) {
			HStack(spacing: 5) {
				Image(systemName: c.icon).font(.system(size: 13)).foregroundStyle(.secondary)
				Text(c.name).font(.system(size: 12.5, weight: .semibold)).lineLimit(1)
			}
			if c.isPhone {
				program("Relay", "none (a phone)", .gray)
			} else {
				program("Relay", c.pool == nil ? "not installed" : (relayOn ? "connected" : "offline"), relayOn ? .green : .gray)
			}
			program("Network", c.net.map { netOn ? $0.ip : "off" } ?? "not installed", netOn ? .green : .gray)
			if !c.isThisMac, !c.isPhone, netOn, let n = c.net {
				Button { viewScreen(n.ip) } label: {
					Label("View screen", systemImage: "display")
						.font(.system(size: 11.5, weight: .medium))
						.frame(maxWidth: .infinity)
				}
				.buttonStyle(.bordered).controlSize(.small)
				.padding(.top, 2)
			}
		}
		.padding(.horizontal, 8)
		.frame(width: 170, height: 112)
		.background(RoundedRectangle(cornerRadius: 12).fill(Color(nsColor: .windowBackgroundColor)))
		.overlay(RoundedRectangle(cornerRadius: 12).stroke(c.isThisMac ? Color.primary.opacity(0.75) : Color.primary.opacity(0.12), lineWidth: c.isThisMac ? 2 : 1))
		.overlay(alignment: .top) {
			if c.isThisMac {
				Text("THIS COMPUTER").font(.system(size: 9.5, weight: .bold)).tracking(0.6).foregroundStyle(.white)
					.padding(.horizontal, 7).padding(.vertical, 2)
					.background(Capsule().fill(Color.primary.opacity(0.75)))
					.offset(y: -9)
			}
		}
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

// MARK: - Connectors: other apps that use your compute pool

/// An app that can use your compute pool through Cerver's MCP server
/// (npx @cerver/mcp), with its own key.
struct AppKind: Identifiable {
	let id: String      // also the key's label: "connector:<id>"
	let name: String
	let icon: String
	let note: String
	let auto: Bool      // the app writes its settings for you; otherwise you paste a line
}

let appKinds: [AppKind] = [
	AppKind(id: "claude-code", name: "Claude Code", icon: "terminal", note: "Claude Code hands work to your computers and asks Codex.", auto: true),
	AppKind(id: "claude-desktop", name: "Claude Desktop", icon: "bubble.left", note: "Claude's chat app gets your computers as tools.", auto: true),
	AppKind(id: "cursor", name: "Cursor", icon: "cursorarrow.rays", note: "Cursor's agent runs tasks on your computers.", auto: true),
	AppKind(id: "hermes", name: "Hermes", icon: "message", note: "Your personal agent on WhatsApp or Telegram does real work on your computers.", auto: false),
	AppKind(id: "openclaw", name: "OpenClaw", icon: "message.badge", note: "Your personal agent on WhatsApp or Telegram does real work on your computers.", auto: false),
	AppKind(id: "other", name: "Other app", icon: "app.connected.to.app.below.fill", note: "Anything that can use MCP tools.", auto: false),
]

/// One app connected to your pool, as the map and the Connectors page show it.
struct ConnectedApp: Identifiable {
	let id: String
	let name: String
	let icon: String
	let lastUsed: Date?
	let removable: Bool   // Kompany manages its own keys
}

/// What this Mac remembers about the keys it made: the key's start and end, never
/// the key (the gateway shows only 6 characters, too few to revoke by safely).
struct SavedConnector: Codable { let prefix: String; let last4: String }

let connectorsFile = NSHomeDirectory() + "/.cerver/connectors.json"
func savedConnectors() -> [String: SavedConnector] {
	guard let d = FileManager.default.contents(atPath: connectorsFile) else { return [:] }
	return (try? JSONDecoder().decode([String: SavedConnector].self, from: d)) ?? [:]
}
func saveConnectors(_ s: [String: SavedConnector]) {
	if let d = try? JSONEncoder().encode(s) { try? d.write(to: URL(fileURLWithPath: connectorsFile), options: .atomic) }
}

struct KeyRow: Decodable { let key_masked: String; let label: String?; let last_used_at: String?; let project_slug: String? }

func parseISO(_ s: String?) -> Date? {
	guard let s = s else { return nil }
	let f = ISO8601DateFormatter()
	f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
	return f.date(from: s) ?? ISO8601DateFormatter().date(from: s)
}

func agoText(_ d: Date?) -> String {
	guard let d = d else { return "not used yet" }
	if Date().timeIntervalSince(d) < 60 { return "used just now" }
	let f = RelativeDateTimeFormatter()
	f.unitsStyle = .full
	return "used " + f.localizedString(for: d, relativeTo: Date())
}

@MainActor
final class AppsModel: ObservableObject {
	@Published var apps: [ConnectedApp] = []
	@Published var loaded = false
	@Published var busy: String?
	@Published var error: String?
	/// A key and the line to paste, shown once, for apps the Mac can't set up itself.
	@Published var paste: (name: String, text: String)?

	func refresh() async {
		guard let d = await gatewayGET("/v2/auth/keys"),
			let rows = (try? JSONDecoder().decode([String: [KeyRow]].self, from: d))?["keys"] else { loaded = true; return }
		var out: [ConnectedApp] = []
		let saved = savedConnectors()
		for k in appKinds {
			guard let s = saved[k.id], let row = rows.first(where: { r in
				r.label == "connector:" + k.id && r.key_masked.hasPrefix(String(s.prefix.prefix(6))) && r.key_masked.hasSuffix(s.last4)
			}) else { continue }
			out.append(ConnectedApp(id: k.id, name: k.name, icon: k.icon, lastUsed: parseISO(row.last_used_at), removable: true))
		}
		// Kompany calls Cerver with keys it makes itself.
		let kompany = rows.filter { ($0.label ?? "").hasPrefix("cron:kompany") || ($0.project_slug ?? "").hasPrefix("kompany") }
		if !kompany.isEmpty {
			out.append(ConnectedApp(id: "kompany", name: "Kompany", icon: "building.2",
				lastUsed: kompany.compactMap { parseISO($0.last_used_at) }.max(), removable: false))
		}
		apps = out.sorted { ($0.lastUsed ?? .distantPast) > ($1.lastUsed ?? .distantPast) }
		loaded = true
	}

	func connect(_ k: AppKind) async {
		busy = k.id; error = nil
		defer { busy = nil }
		// One key per app: replace this app's old key, if this Mac made one.
		if let old = savedConnectors()[k.id] { _ = await gatewayRequest("DELETE", "/v2/auth/keys/" + old.prefix) }
		let body = try? JSONSerialization.data(withJSONObject: ["label": "connector:" + k.id])
		guard let d = await gatewayRequest("POST", "/v2/auth/keys", body: body),
			let key = (try? JSONSerialization.jsonObject(with: d) as? [String: Any])?["key"] as? String, key.count > 20 else {
			error = "Couldn't make a key for \(k.name). Check that you're signed in."
			return
		}
		var s = savedConnectors()
		s[k.id] = SavedConnector(prefix: String(key.prefix(16)), last4: String(key.suffix(4)))
		saveConnectors(s)
		let problem = await Task.detached { setUpApp(k.id, key: key) }.value
		if let p = problem {
			if p.isEmpty { paste = (k.name, pasteText(k.id, key: key)) } else { error = p }
		}
		await refresh()
	}

	func remove(_ a: ConnectedApp) async {
		busy = a.id; error = nil
		defer { busy = nil }
		var s = savedConnectors()
		if let old = s[a.id] { _ = await gatewayRequest("DELETE", "/v2/auth/keys/" + old.prefix) }
		s[a.id] = nil
		saveConnectors(s)
		await Task.detached { tearDownApp(a.id) }.value
		await refresh()
	}
}

/// Where npx is, so apps opened from the Dock (with a short PATH) can start the server.
func npxPath() -> String {
	let p = shell("command -v npx")
	return p.isEmpty ? "npx" : p
}

/// The MCP server entry for an app's settings file.
func mcpEntry(_ key: String) -> [String: Any] {
	["command": npxPath(), "args": ["-y", "@cerver/mcp"], "env": ["CERVER_KEY": key]]
}

/// Adds (or removes, with key nil) the "cerver" server in a JSON settings file
/// shaped { "mcpServers": { … } }, keeping everything else in it.
func editMCPFile(_ path: String, key: String?) -> Bool {
	var root: [String: Any] = [:]
	if let d = FileManager.default.contents(atPath: path) {
		guard let j = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else { return false }  // don't overwrite a file we can't read
		root = j
	} else if key == nil { return true }
	var servers = root["mcpServers"] as? [String: Any] ?? [:]
	servers["cerver"] = key.map { mcpEntry($0) }
	root["mcpServers"] = servers
	try? FileManager.default.createDirectory(atPath: (path as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
	guard let out = try? JSONSerialization.data(withJSONObject: root, options: [.prettyPrinted, .sortedKeys]) else { return false }
	return (try? out.write(to: URL(fileURLWithPath: path), options: .atomic)) != nil
}

let claudeDesktopConfig = NSHomeDirectory() + "/Library/Application Support/Claude/claude_desktop_config.json"
let cursorConfig = NSHomeDirectory() + "/.cursor/mcp.json"

/// Sets the app up with its key. nil = done; "" = the person pastes a line; other = what went wrong.
func setUpApp(_ id: String, key: String) -> String? {
	switch id {
	case "claude-code":
		if shell("command -v claude").isEmpty { return "Claude Code isn't installed on this Mac." }
		_ = shell("claude mcp remove cerver -s user")
		let p = Process()
		p.executableURL = URL(fileURLWithPath: "/bin/zsh")
		p.arguments = ["-lc", "claude mcp add cerver -s user -e CERVER_KEY=\"$CERVER_CONNECT_KEY\" -- \"$CERVER_NPX\" -y @cerver/mcp"]
		var env = ProcessInfo.processInfo.environment
		env["CERVER_CONNECT_KEY"] = key
		env["CERVER_NPX"] = npxPath()
		p.environment = env
		p.standardOutput = Pipe(); p.standardError = Pipe()
		guard (try? p.run()) != nil else { return "Couldn't run Claude Code." }
		p.waitUntilExit()
		return p.terminationStatus == 0 ? nil : "Claude Code didn't accept the server."
	case "claude-desktop":
		return editMCPFile(claudeDesktopConfig, key: key) ? nil : "Couldn't update Claude Desktop's settings file."
	case "cursor":
		return editMCPFile(cursorConfig, key: key) ? nil : "Couldn't update Cursor's settings file."
	default:
		return ""
	}
}

func tearDownApp(_ id: String) {
	switch id {
	case "claude-code": _ = shell("claude mcp remove cerver -s user")
	case "claude-desktop": _ = editMCPFile(claudeDesktopConfig, key: nil)
	case "cursor": _ = editMCPFile(cursorConfig, key: nil)
	default: break
	}
}

func pasteText(_ id: String, key: String) -> String {
	switch id {
	case "hermes":
		return "Add to ~/.hermes/config.yaml:\n\nmcp_servers:\n  cerver:\n    command: npx\n    args: [\"-y\", \"@cerver/mcp\"]\n    env:\n      CERVER_KEY: \(key)"
	case "openclaw":
		return "Add to mcp.servers in ~/.openclaw/openclaw.json:\n\n\"cerver\": { \"command\": \"npx\", \"args\": [\"-y\", \"@cerver/mcp\"], \"env\": { \"CERVER_KEY\": \"\(key)\" } }"
	default:
		return "{ \"mcpServers\": { \"cerver\": { \"command\": \"npx\", \"args\": [\"-y\", \"@cerver/mcp\"], \"env\": { \"CERVER_KEY\": \"\(key)\" } } } }"
	}
}

struct ConnectorsPage: View {
	@ObservedObject var a: AppsModel
	@State private var copied = false

	var body: some View {
		PageLayout(title: "Connectors", subtitle: "Apps that use your compute pool. Each one asks Cerver for work; it runs on your computers, with your own Claude and Codex logins.") {
			Text("Connected").font(.system(size: 17, weight: .semibold))
			Card {
				if a.apps.isEmpty {
					Text(a.loaded ? "No apps connected yet. Connect one below." : "Loading…")
						.foregroundStyle(.secondary).padding(16)
				}
				ForEach(Array(a.apps.enumerated()), id: \.element.id) { i, app in
					if i > 0 { Divider().padding(.leading, 16) }
					HStack(spacing: 12) {
						Image(systemName: app.icon).frame(width: 22).foregroundStyle(.secondary)
						VStack(alignment: .leading, spacing: 2) {
							Text(app.name).font(.system(size: 14))
							Text(app.removable ? agoText(app.lastUsed) : agoText(app.lastUsed) + " · managed in Kompany")
								.font(.system(size: 11.5)).foregroundStyle(.secondary)
						}
						Spacer()
						if app.removable {
							if a.busy == app.id { ProgressView().controlSize(.small) }
							Button("Remove") { Task { await a.remove(app) } }.disabled(a.busy != nil)
						}
					}
					.padding(.horizontal, 16).padding(.vertical, 12)
				}
			}

			if let e = a.error { Text(e).font(.callout).foregroundStyle(.red) }

			if let p = a.paste {
				Card {
					VStack(alignment: .leading, spacing: 10) {
						Text("Finish connecting \(p.name)").font(.system(size: 14, weight: .semibold))
						Text("Paste this into \(p.name). It holds the app's own key, shown only now.")
							.font(.system(size: 12)).foregroundStyle(.secondary)
						Text(p.text).font(.system(size: 11.5, design: .monospaced)).textSelection(.enabled)
							.padding(10).frame(maxWidth: .infinity, alignment: .leading)
							.background(RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(0.05)))
						HStack {
							Button(copied ? "Copied" : "Copy") {
								NSPasteboard.general.clearContents()
								NSPasteboard.general.setString(p.text, forType: .string)
								copied = true
							}
							Button("Done") { a.paste = nil; copied = false }
						}
					}
					.padding(16)
				}
			}

			Text("Connect an app").font(.system(size: 17, weight: .semibold)).padding(.top, 10)
			Text("The app gets its own key. Remove it here any time and the app loses access.")
				.font(.callout).foregroundStyle(.secondary)
			LazyVGrid(columns: [GridItem(.flexible(), spacing: 12), GridItem(.flexible(), spacing: 12)], spacing: 12) {
				ForEach(appKinds) { k in tile(k) }
			}
		}
		.task { await a.refresh() }
	}

	func tile(_ k: AppKind) -> some View {
		let on = a.apps.contains { $0.id == k.id }
		return VStack(alignment: .leading, spacing: 8) {
			HStack(spacing: 8) {
				Image(systemName: k.icon).foregroundStyle(.secondary)
				Text(k.name).font(.system(size: 15, weight: .semibold))
				Spacer()
				if on { Text("Connected").font(.system(size: 11, weight: .medium)).foregroundStyle(.green) }
			}
			Text(k.note).font(.system(size: 12)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
			HStack {
				Button(on ? "Reconnect" : "Connect") { Task { await a.connect(k) } }
					.buttonStyle(.bordered).disabled(a.busy != nil)
				if a.busy == k.id { ProgressView().controlSize(.small) }
				Spacer()
				Text(k.auto ? "Sets it up for you" : "You paste one line").font(.system(size: 11)).foregroundStyle(.tertiary)
			}
		}
		.padding(14)
		.frame(maxWidth: .infinity, minHeight: 128, alignment: .topLeading)
		.background(RoundedRectangle(cornerRadius: 12).fill(Color(nsColor: .controlBackgroundColor)))
		.overlay(RoundedRectangle(cornerRadius: 12).stroke(Color.primary.opacity(0.08)))
	}
}

/// The people in your pool: who's in, who asked to join, and the invite link.
struct PeoplePage: View {
	@ObservedObject var m: Model
	@State private var info: PoolInfo?
	@State private var copied = false

	var body: some View {
		PageLayout(title: "People", subtitle: "Who is in your pool, and how to invite someone.") {
			Card {
				if let i = info {
					ForEach(Array(i.people.enumerated()), id: \.offset) { n, x in
						if n > 0 { Divider().padding(.leading, 16) }
						Row(label: x.email ?? x.name ?? "Someone") {
							if x.status == "active" {
								Text(n == 0 ? "Owner" : "Can run agents").foregroundStyle(.secondary)
							} else {
								Text("Asked to join").foregroundStyle(.orange)
								Button("Approve") { act("POST", "/v2/pool/members/\(x.account_id)/approve") }
							}
						}
					}
					if let url = i.invite_url {
						Divider().padding(.leading, 16)
						Row(label: "Invite someone") {
							Text(url).font(.system(size: 12, design: .monospaced)).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
							Button(copied ? "Copied" : "Copy link") {
								NSPasteboard.general.clearContents()
								NSPasteboard.general.setString(url, forType: .string)
								copied = true
							}
						}
					}
				} else {
					Row(label: "Loading…") { ProgressView().controlSize(.small) }
				}
			}
			Text("People you invite can run agents only on computers you allow, in Security → People you invite.")
				.font(.callout).foregroundStyle(.secondary)
		}
		.task { await load() }
	}

	func load() async {
		if let d = await gatewayGET("/v2/pool"), let i = try? JSONDecoder().decode(PoolInfo.self, from: d) { info = i }
	}

	func act(_ method: String, _ path: String) {
		Task {
			_ = await gatewayRequest(method, path)
			await load()
			m.refreshPool(force: true)
		}
	}
}

/// Renames a computer everywhere you see it (the app, cerver.ai, `cerver pool`).
/// Its relay keeps its own name, so nothing on the computer changes.
/// Name your pool: shown in the sidebar, on its page and in invite links.
struct PoolNameSheet: View {
	@ObservedObject var m: Model
	@Binding var shown: Bool
	@State private var name = ""
	@State private var saving = false
	@State private var failed = false

	var body: some View {
		VStack(alignment: .leading, spacing: 12) {
			Text("Name your pool").font(.headline)
			Text("People you invite see this name.").font(.callout).foregroundStyle(.secondary)
			TextField("e.g. Eyal's computers", text: $name).textFieldStyle(.roundedBorder).frame(width: 300).onSubmit(save)
			if failed { Text("Couldn't save it. Try again.").font(.callout).foregroundStyle(.red) }
			HStack {
				Spacer()
				if saving { ProgressView().controlSize(.small) }
				Button("Cancel") { shown = false }.keyboardShortcut(.cancelAction)
				Button("Save", action: save).keyboardShortcut(.defaultAction)
					.disabled(name.trimmingCharacters(in: .whitespaces).isEmpty || saving)
			}
		}
		.padding(20)
		.onAppear { name = m.poolName == "My pool" ? "" : (m.poolName ?? "") }
	}

	func save() {
		let n = String(name.trimmingCharacters(in: .whitespaces).prefix(60))
		guard !n.isEmpty, !saving else { return }
		saving = true
		Task {
			let body = try? JSONSerialization.data(withJSONObject: ["name": n])
			let ok = await gatewayRequest("PATCH", "/v2/pool", body: body) != nil
			saving = false
			if ok { m.poolName = n; shown = false } else { failed = true }
		}
	}
}

struct RenameSheet: View {
	@ObservedObject var m: Model
	let computeID: String
	let current: String
	@Binding var shown: Bool
	@State private var name = ""
	@State private var saving = false
	@State private var failed = false

	var body: some View {
		VStack(alignment: .leading, spacing: 12) {
			Text("Rename computer").font(.headline)
			TextField("Name", text: $name).textFieldStyle(.roundedBorder).frame(width: 280).onSubmit(save)
			if failed { Text("Couldn't rename it. Try again.").font(.callout).foregroundStyle(.red) }
			HStack {
				Spacer()
				if saving { ProgressView().controlSize(.small) }
				Button("Cancel") { shown = false }.keyboardShortcut(.cancelAction)
				Button("Rename", action: save).keyboardShortcut(.defaultAction)
					.disabled(name.trimmingCharacters(in: .whitespaces).isEmpty || saving)
			}
		}
		.padding(20)
		.onAppear { name = current }
	}

	func save() {
		let n = name.trimmingCharacters(in: .whitespaces)
		guard !n.isEmpty, !saving else { return }
		if n == current { shown = false; return }
		saving = true
		Task {
			let body = try? JSONSerialization.data(withJSONObject: ["label": n])
			let ok = await gatewayRequest("PATCH", "/v2/computes/" + computeID, body: body) != nil
			saving = false
			if ok { shown = false; m.refreshPool(force: true) } else { failed = true }
		}
	}
}


// MARK: - Security rules for your own agents (read by the relay: agent_policy.py)

struct AgentAccess: Decodable {
	let mode: String?
	let blocked_folders: [String]?
	let allowed_folders: [String]?
	let ask_folders: [String]?
	let guarded: Bool?
	let ask_folder: String?
	let start_folder: String?
	let enforced_by: String?
}

struct SecurityRules {
	var mode = "as_you"
	var blocked: [String] = []
	var allowed: [String] = [] // own_folder: folders agents may also open
	var ask: [String] = []     // only after the owner says yes, per task
	static let rulesFile = "/Library/Application Support/Cerver/agent-rules.json"
	/// True once the rules sit in the admin-only file (changes need the password).
	static var guarded: Bool { FileManager.default.fileExists(atPath: rulesFile) }
	var start = ""             // where agents start when no folder is given ("" = default)
	/// The start folder in effect on this Mac.
	var startShown: String { !start.isEmpty ? start : (mode == "as_you" ? "~ (your home folder)" : SecurityRules.askFolder) }
	static let file = NSHomeDirectory() + "/.cerver/security.json"
	static let askFolder = "~/.cerver/work/ask"

	/// Folders picked in a standard Open panel, as ~/… paths.
	static func pick(_ prompt: String) -> [String] {
		let p = NSOpenPanel()
		p.canChooseDirectories = true; p.canChooseFiles = false; p.allowsMultipleSelection = true
		p.directoryURL = URL(fileURLWithPath: NSHomeDirectory())
		p.prompt = prompt
		guard p.runModal() == .OK else { return [] }
		let home = NSHomeDirectory()
		return p.urls.map { $0.path.hasPrefix(home) ? "~" + $0.path.dropFirst(home.count) : $0.path }
	}

	static func load() -> SecurityRules {
		var r = SecurityRules()
		guard let d = FileManager.default.contents(atPath: guarded ? rulesFile : file),
			  let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else { return r }
		if let m = o["agent_access"] as? String, ["as_you", "own_folder", "vm"].contains(m) { r.mode = m }
		r.blocked = (o["blocked_folders"] as? [String]) ?? []
		r.allowed = (o["allowed_folders"] as? [String]) ?? []
		r.ask = (o["ask_folders"] as? [String]) ?? []
		r.start = (o["start_folder"] as? String) ?? ""
		return r
	}

	/// Saves through `cerver access set`: macOS asks for this Mac's password
	/// (the guardrail: an agent can't change the rules by itself). Calls back
	/// on the main thread with nil when saved, else why not.
	func commit(_ done: @escaping (String?) -> Void) {
		var o: [String: Any] = ["agent_access": mode, "blocked_folders": blocked, "allowed_folders": allowed, "ask_folders": ask]
		if !start.isEmpty { o["start_folder"] = start }
		let json = String(data: (try? JSONSerialization.data(withJSONObject: o)) ?? Data(), encoding: .utf8) ?? "{}"
		Task.detached {
			let (ok, out) = runCerver(["access", "set", json])
			await MainActor.run { done(ok ? nil : out.trimmingCharacters(in: .whitespacesAndNewlines)) }
		}
	}

	/// nil when saved, else what went wrong. (The older per-user file.)
	func save() -> String? {
		var o: [String: Any] = ["agent_access": mode, "blocked_folders": blocked, "allowed_folders": allowed, "ask_folders": ask]
		if !start.isEmpty { o["start_folder"] = start }
		do {
			try FileManager.default.createDirectory(atPath: NSHomeDirectory() + "/.cerver", withIntermediateDirectories: true)
			let d = try JSONSerialization.data(withJSONObject: o, options: [.prettyPrinted, .sortedKeys])
			try d.write(to: URL(fileURLWithPath: SecurityRules.file), options: .atomic)
			return nil
		} catch {
			return "Couldn't save: \(error.localizedDescription)"
		}
	}
}


// MARK: - Provisioning: the local VM (cerver vm, Apple's container tool)

struct Provisioning: Decodable { let vm: VMState? }

struct VMState: Decodable {
	var supported = false
	var why: String? = nil
	var installed = false
	var version: String? = nil
	var running = false
	var image = false
	var signed_in = false
	var ready = false
}

@MainActor final class VMModel: ObservableObject {
	@Published var state: VMState?
	@Published var busy: String?
	@Published var problem: String?
	@Published var token = ""

	static func fetch() async -> VMState? {
		await Task.detached { () -> VMState? in
			let (ok, out) = runCerver(["vm", "--json"])
			guard ok, let d = out.data(using: .utf8) else { return nil }
			return try? JSONDecoder().decode(VMState.self, from: d)
		}.value
	}

	func refresh() async { state = await Self.fetch() }

	/// Apple's container tool: needs admin, so macOS asks for the password.
	func install() {
		guard let bin = Bundle.main.path(forResource: "cerver", ofType: nil) else { return }
		busy = "Installing Apple's container tool…"; problem = nil
		let script = "do shell script \"'\(bin)' vm install\" with administrator privileges"
		Task {
			let (ok, msg) = await Task.detached { () -> (Bool, String) in
				let p = Process()
				p.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
				p.arguments = ["-e", script]
				let out = Pipe(); p.standardOutput = out; p.standardError = out
				do { try p.run() } catch { return (false, error.localizedDescription) }
				let d = out.fileHandleForReading.readDataToEndOfFile()
				p.waitUntilExit()
				return (p.terminationStatus == 0, String(data: d, encoding: .utf8) ?? "")
			}.value
			busy = nil
			if !ok && !msg.contains("-128") { problem = cleanError(msg) }
			await refresh()
		}
	}

	/// Start the VM service and build the agent image (a few minutes the first time).
	func start() {
		busy = "Starting the VM and building the agent image… (a few minutes the first time)"; problem = nil
		Task {
			let (ok, out) = await Task.detached { runCerver(["vm", "start"]) }.value
			busy = nil
			if !ok { problem = out.split(separator: "\n").suffix(3).joined(separator: "\n") }
			await refresh()
		}
	}

	func saveToken() {
		let t = token.trimmingCharacters(in: .whitespacesAndNewlines)
		guard let bin = Bundle.main.path(forResource: "cerver", ofType: nil), !t.isEmpty else { return }
		problem = nil
		let p = Process()
		p.executableURL = URL(fileURLWithPath: bin)
		p.arguments = ["vm", "login", "--stdin"]
		let inp = Pipe(), out = Pipe()
		p.standardInput = inp; p.standardOutput = out; p.standardError = out
		do { try p.run() } catch { problem = error.localizedDescription; return }
		inp.fileHandleForWriting.write((t + "\n").data(using: .utf8)!)
		try? inp.fileHandleForWriting.close()
		p.waitUntilExit()
		if p.terminationStatus != 0 {
			problem = String(data: out.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8)
		}
		token = ""
		Task { await refresh() }
	}
}

/// The local VM, step by step. `vm` set: this Mac, with the buttons.
/// `reported`: another computer, as its relay reported.
struct VMSection: View {
	var vm: VMModel?
	var reported: VMState?
	var computer = ""
	var isThisMac = true
	var online = true
	@Environment(\.askAgent) private var ask

	var body: some View {
		VStack(alignment: .leading, spacing: 8) {
			Text("Provisioning").font(.system(size: 15, weight: .semibold)).padding(.top, 6)
			Text("Local VM: a small Linux computer inside this Mac, made with Apple's own container tool. An agent there sees only the folder it works in, and can install anything without touching the Mac.")
				.font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
			if let vm = vm { ThisMacVM(vm: vm) } else { remote }
			if (vm?.state ?? reported)?.supported != false {
				HStack(spacing: 10) {
					Button { ask.run(vmSetupPrompt(computer), computer, isThisMac) } label: {
						Label("Set up with an agent", systemImage: "sparkles")
					}
					.disabled(!online)
					Text("An agent on \(isThisMac ? "this Mac" : computer) installs it, checks it works, and fixes problems on the way. You only type your password when macOS asks\(isThisMac ? "" : " on that computer").")
						.font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
				}
				.padding(.top, 4)
				// What the agent will be told, word for word.
				DisclosureGroup("See the agent's instructions") {
					Text(vmSetupPrompt(computer))
						.font(.system(size: 12, design: .monospaced))
						.textSelection(.enabled)
						.frame(maxWidth: .infinity, alignment: .leading)
						.padding(12)
						.background(RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(0.04)))
						.padding(.top, 6)
				}
				.font(.callout)
			}
		}
	}

	@ViewBuilder var remote: some View {
		Card {
			if let r = reported {
				if !r.supported {
					Row(label: "Not on this computer", note: r.why) { EmptyView() }
				} else {
					step("Apple's container tool", r.installed, r.version)
					Divider().padding(.leading, 16)
					step("VM running, agent image built", r.running && r.image, nil)
					Divider().padding(.leading, 16)
					step("Claude login for the VM", r.signed_in, nil)
				}
			} else {
				Row(label: "Not reported yet", note: "Set it up on that computer: its Cerver app → This Mac → Provisioning.") { EmptyView() }
			}
		}
	}

	func step(_ label: String, _ done: Bool, _ note: String?) -> some View {
		Row(label: label, note: note) {
			Image(systemName: done ? "checkmark.circle.fill" : "circle").foregroundStyle(done ? Color.green : Color.secondary)
		}
	}
}

struct ThisMacVM: View {
	@ObservedObject var vm: VMModel

	var body: some View {
		Group {
			if let s = vm.state {
				if !s.supported {
					Card { Row(label: "Not on this Mac", note: s.why) { EmptyView() } }
				} else {
					Card {
						Row(label: "1. Apple's container tool", note: s.installed ? s.version : "Downloaded from Apple, checked to be signed by Apple. macOS asks for your password once.") {
							if s.installed { done } else { Button("Install") { vm.install() }.disabled(vm.busy != nil) }
						}
						Divider().padding(.leading, 16)
						Row(label: "2. Start the VM and build the agent image", note: "Claude Code, Codex, git and Python inside. About 1–2 GB, a few minutes the first time.") {
							if s.running && s.image { done } else { Button("Start") { vm.start() }.disabled(!s.installed || vm.busy != nil) }
						}
						Divider().padding(.leading, 16)
						Row(label: "3. Your Claude login for the VM", note: s.signed_in ? "Saved on this Mac only." : "Run claude setup-token in Terminal, then paste the token here. It stays on this Mac.") {
							if s.signed_in { done } else {
								SecureField("sk-ant-…", text: $vm.token).textFieldStyle(.roundedBorder).frame(width: 180)
								Button("Save") { vm.saveToken() }.disabled(vm.token.isEmpty)
							}
						}
					}
					if s.ready && s.signed_in {
						Text("Ready. To run agents in it, choose \"In a VM\" on the Security page.").font(.callout).foregroundStyle(.secondary)
					}
				}
			} else {
				ProgressView().controlSize(.small)
			}
			if let b = vm.busy {
				HStack(spacing: 8) { ProgressView().controlSize(.small); Text(b).font(.callout).foregroundStyle(.secondary) }
			}
			if let p = vm.problem { Text(p).font(.callout).foregroundStyle(.red).textSelection(.enabled) }
		}
		.task { await vm.refresh() }
	}

	var done: some View { Image(systemName: "checkmark.circle.fill").foregroundStyle(.green) }
}


/// Opens another page of the app — set by the main window.
struct OpenPage { var go: (Page) -> Void = { _ in } }
private struct OpenPageKey: EnvironmentKey { static let defaultValue = OpenPage() }
extension EnvironmentValues {
	var openPage: OpenPage {
		get { self[OpenPageKey.self] }
		set { self[OpenPageKey.self] = newValue }
	}
}

/// One list of folders (Allowed / Ask first / Never), editable or view-only.
struct FolderList: View {
	let icon: String
	let color: Color
	let title: String
	let explain: String
	let folders: [String]
	let editable: Bool
	var add: () -> Void = {}
	var remove: (String) -> Void = { _ in }
	var body: some View {
		HStack(alignment: .firstTextBaseline, spacing: 10) {
			Image(systemName: icon).foregroundStyle(color).frame(width: 16)
			VStack(alignment: .leading, spacing: 6) {
				HStack(spacing: 6) {
					Text(title).font(.system(size: 13, weight: .semibold))
					Text(explain).font(.callout).foregroundStyle(.secondary)
				}
				Flow(spacing: 6) {
					if folders.isEmpty { Text("none").font(.callout).foregroundStyle(.tertiary) }
					ForEach(folders, id: \.self) { f in
						HStack(spacing: 5) {
							Image(systemName: "folder").font(.system(size: 11))
							Text(f).font(.system(size: 12, design: .monospaced))
							if editable {
								Button { remove(f) } label: { Image(systemName: "xmark").font(.system(size: 9, weight: .bold)) }
									.buttonStyle(.plain).foregroundStyle(.secondary).help("Take it off this list")
							}
						}
						.padding(.horizontal, 8).padding(.vertical, 4)
						.background(Capsule().fill(color.opacity(0.12)))
					}
					if editable {
						Button { add() } label: { Label("Add…", systemImage: "plus").font(.callout) }.buttonStyle(.link)
					}
				}
			}
			Spacer()
		}
		.padding(.horizontal, 16).padding(.vertical, 8)
	}
}

/// One computer's rules in plain words: what agents may and may not touch,
/// and what's allowed or blocked for the people using it.
struct AccessSection: View {
	let c: Computer
	@ObservedObject var m: Model
	@StateObject private var a = AddonsModel()
	@Environment(\.openPage) private var openPage

	@State private var sec = SecurityRules.load()
	func commit() { sec.commit { _ in sec = SecurityRules.load() } }
	var mode: String? { c.isThisMac ? sec.mode : c.spec?.agent_access?.mode }
	var blocked: [String] { c.isThisMac ? sec.blocked : (c.spec?.agent_access?.blocked_folders ?? []) }
	var allowed: [String] { c.isThisMac ? sec.allowed : (c.spec?.agent_access?.allowed_folders ?? []) }
	var askList: [String] { c.isThisMac ? sec.ask : (c.spec?.agent_access?.ask_folders ?? []) }

	/// Put picked folders on one list (and off the others), then save (password).
	func put(_ list: WritableKeyPath<SecurityRules, [String]>, _ prompt: String) {
		let picked = SecurityRules.pick(prompt)
		guard !picked.isEmpty else { return }
		for f in picked {
			sec.allowed.removeAll { $0 == f }; sec.ask.removeAll { $0 == f }; sec.blocked.removeAll { $0 == f }
			sec[keyPath: list].append(f)
		}
		commit()
	}

	@ViewBuilder var folderLists: some View {
		if mode == "own_folder" {
			FolderList(icon: "checkmark.circle.fill", color: .green, title: "Allowed", explain: "may open, besides the folder it works in",
			           folders: allowed, editable: c.isThisMac, add: { put(\.allowed, "Allow") }, remove: { f in sec.allowed.removeAll { $0 == f }; commit() })
		}
		FolderList(icon: "hand.raised.fill", color: .orange, title: "Ask first", explain: "a dialog here asks you, per task",
		           folders: askList, editable: c.isThisMac, add: { put(\.ask, "Ask first") }, remove: { f in sec.ask.removeAll { $0 == f }; commit() })
		FolderList(icon: "xmark.octagon.fill", color: .red, title: "Never", explain: "closed, whatever else is set",
		           folders: blocked, editable: c.isThisMac, add: { put(\.blocked, "Never allow") }, remove: { f in sec.blocked.removeAll { $0 == f }; commit() })
		if c.isThisMac {
			Text(SecurityRules.guarded ? "Changes ask for this Mac's password, so an agent can't change them by itself." : "Your next change moves these rules into an admin-only file; from then on, changes ask for this Mac's password.")
				.font(.caption).foregroundStyle(.secondary).padding(.horizontal, 16)
		}
	}
	func tilde(_ p: String) -> String { p.replacingOccurrences(of: #"^/Users/[^/]+"#, with: "~", options: .regularExpression) }
	var startFolder: String { c.isThisMac ? sec.startShown : (c.spec?.agent_access?.start_folder.map(tilde) ?? "not reported yet") }

	/// Where agents start when the request names no folder; editable on this Mac.
	var startLine: some View {
		HStack(alignment: .firstTextBaseline, spacing: 10) {
			Image(systemName: "folder.fill").foregroundStyle(Color.accentColor).frame(width: 16)
			VStack(alignment: .leading, spacing: 3) {
				Text("Starts in " + startFolder).font(.system(size: 13, weight: .medium))
				Text("When a request names no folder (Ask Compute Pool, cerver run). A request can name a project folder instead.")
					.font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
			}
			Spacer()
			if c.isThisMac {
				Button("Change…") { if let f = SecurityRules.pick("Start here").first { sec.start = f; commit() } }.buttonStyle(.link)
				if !sec.start.isEmpty { Button("Default") { sec.start = ""; commit() }.buttonStyle(.link) }
			}
		}
		.padding(.horizontal, 16).padding(.vertical, 8)
	}
	var node: NetNodeAddons? { a.nodes.first { $0.ip == c.net?.ip || $0.name.lowercased() == c.name.lowercased() } }

	func line(_ ok: Bool?, _ title: String, _ detail: String) -> some View {
		HStack(alignment: .firstTextBaseline, spacing: 10) {
			Image(systemName: ok == nil ? "minus.circle" : (ok! ? "checkmark.circle.fill" : "xmark.octagon.fill"))
				.foregroundStyle(ok == nil ? Color.secondary : (ok! ? Color.green : Color.red))
				.frame(width: 16)
			VStack(alignment: .leading, spacing: 2) {
				Text(title).font(.system(size: 13, weight: .medium))
				if !detail.isEmpty { Text(detail).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
			}
			Spacer()
		}
		.padding(.horizontal, 16).padding(.vertical, 8)
	}

	func header(_ title: String, _ icon: String, change: String, _ to: Page) -> some View {
		HStack {
			Label(title, systemImage: icon).font(.system(size: 14, weight: .semibold))
			Spacer()
			if !change.isEmpty { Button(change) { openPage.go(to) }.buttonStyle(.link).font(.callout) }
		}
		.padding(.horizontal, 16).padding(.top, 14).padding(.bottom, 4)
	}

	var body: some View {
		VStack(alignment: .leading, spacing: 14) {
			let others = m.computers.filter { $0.id != c.id && !$0.isPhone }.map(\.name)
			Card {
				header("Agents you open yourself on \(c.isThisMac ? "this Mac" : c.name)", "terminal", change: "", .permissions)
				line(nil, "Cerver doesn't limit these", "Claude Code or Codex that you start in Terminal here follow their own permission settings, as you.")
				Spacer().frame(height: 6)
			}
			Card {
				header("Tasks sent here through Cerver", "arrow.down.circle", change: c.isThisMac ? "Change in Security" : "", .permissions)
				Text("From " + (others.isEmpty ? "your other computers" : others.joined(separator: ", ")) + ", Ask Compute Pool, or `cerver run`. \(c.isThisMac ? "This Mac" : c.name)'s rules decide, wherever the task came from.")
					.font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
					.padding(.horizontal, 16).padding(.bottom, 4)
				switch mode {
				case "as_you":
					startLine
					line(true, "May open everything you can open", "Your files, apps and logins on \(c.name).")
				case "own_folder":
					startLine
					line(true, "May open the folder it works in", "The start folder above, or the project folder the task names.")
					line(false, "Can't open the rest of your files", "Documents, Desktop, other projects: hidden from it.")
				case "vm":
					line(true, "Works in a separate sandbox computer", "A small Linux VM that sees only its folder.")
					line(false, "Can't touch \(c.isThisMac ? "this Mac" : c.name) itself", "")
				default:
					line(nil, "Not reported yet", "\(c.name) hasn't said what tasks may open there.")
				}
				Divider().padding(.leading, 16).padding(.vertical, 4)
				folderLists
				if !c.isThisMac {
					Text("Change these in Cerver on \(c.name).").font(.callout).foregroundStyle(.secondary).padding(.horizontal, 16)
				}
				Spacer().frame(height: 6)
			}
			Card {
				header("Tasks from people you invite", "person.2", change: "", .people)
				if c.pool?.shared == true {
					line(true, "Allowed", "People you approved in your pool can send tasks here.")
					line(false, "Always fenced off from your files", "Each task gets its own empty folder (and ~/CerverShared, if you made one). Your home folder, logins and keys are hidden. Your rules above can't loosen this.")
				} else {
					line(false, "Not allowed", "Only your own computers can send tasks here. Turn on \"Share with people you invite\" in Overview to allow it.")
				}
				Spacer().frame(height: 6)
			}
			Card {
				header("People using it", "person", change: "Change in Add-ons", .addons)
				if let n = node {
					let lvl = a.level(n)
					line(lvl == "off" ? nil : false, lvl == "off" ? "Websites: nothing filtered" : "Blocked: " + ["malware": "malware and phishing", "adult": "adult sites, malware", "kids": "adult sites, proxies and VPNs; SafeSearch forced"][lvl, default: lvl], "Safety")
					let rules = a.rules(n).filter { $0.policy != "off" && !$0.sites.isEmpty }
					if rules.isEmpty {
						line(nil, "No distracting sites blocked", "Focus")
					} else {
						let on = Dictionary((n.addons_applied?.focus?.rules ?? []).map { ($0.name ?? "", $0.on) }, uniquingKeysWith: { x, _ in x })
						ForEach(Array(rules.enumerated()), id: \.offset) { _, r in
							line((on[r.name ?? ""] ?? false) ? false : nil,
							     ((on[r.name ?? ""] ?? false) ? "Blocked now: " : "Blocked at set times: ") + FocusRule.names(r.sites),
							     "Focus · " + (r.name ?? "Focus") + " · " + ["always": "always", "work_hours": "Mon–Fri 9:00–18:00", "until": "for an hour", "schedule": "on a schedule"][r.policy, default: r.policy])
						}
					}
					if let via = n.exit_via, let ex = a.nodes.first(where: { $0.ip == via }) {
						line(true, "Internet goes through \(ex.name)", "VPN: encrypted to there; the local network sees nothing.")
					} else if n.exit_offer == true {
						line(true, "Your other computers may use it as their VPN", "VPN")
					} else {
						line(nil, "Internet goes directly", "VPN off")
					}
				} else if c.net == nil {
					line(nil, "Not on your private network", "Add-ons (Safety, Focus, VPN) need Cerver's network on this computer.")
				} else {
					line(nil, "Loading…", "")
				}
				Spacer().frame(height: 6)
			}
		}
		.task { await a.load() }
	}
}

/// "Hand this task to an agent on that computer" — set by the main window.
struct AskAgent { var run: (String, String, Bool) -> Void = { _, _, _ in } }
private struct AskAgentKey: EnvironmentKey { static let defaultValue = AskAgent() }
extension EnvironmentValues {
	var askAgent: AskAgent {
		get { self[AskAgentKey.self] }
		set { self[AskAgentKey.self] = newValue }
	}
}

/// The agent's instructions for provisioning the local VM. Diligent: verify
/// every step, fix what fails, retry; never handle a password itself.
func vmSetupPrompt(_ computer: String) -> String {
	"""
	Set up the Cerver local VM on this computer (\(computer)) and make sure it really works. Be diligent: check every step, and when something fails, find out why, fix it and try again. Stop only when it works or you're sure it can't on this computer.

	Work quietly: don't write any message until you're done; then write one short report.

	1. Find cerver: `command -v cerver`, else ~/.cerver/bin/cerver, else /Applications/Cerver.app/Contents/Resources/cerver. Run `cerver vm --json`. If it says `vm` is unknown, run `cerver update` and use the newer one.
	2. If "supported" is false, stop and report the reason.
	3. If "installed" is false, run exactly: osascript -e 'do shell script "<full path to cerver> vm install" with administrator privileges'. That opens macOS's own password box on this computer for me. Never ask me for a password and never type one. If I cancel (error -128), stop and say so. On any other error, read it, fix the cause (network, disk space, an earlier half install) and run it again.
	4. If "running" or "image" is false, run `cerver vm start` (it can take several minutes the first time). If it fails: read `container system logs` if it helps, try `container system stop` then start again, check free disk space (it needs about 5 GB), and retry.
	5. Verify for real: `/usr/local/bin/container run --rm cerver-agent:1 claude --version` must print a version, and `cerver vm --json` must show "ready": true.
	6. Report in a few short bullets: what you did, what you checked and its result, anything you fixed, and what's left for me. Usually that's only the Claude sign-in: run `claude setup-token` in Terminal and paste the token in the Cerver app → \(computer) → Software → Provisioning.
	"""
}


/// A small search box: magnifier, text, clear button.
struct SearchField: View {
	@Binding var text: String
	var prompt: String
	var body: some View {
		HStack(spacing: 6) {
			Image(systemName: "magnifyingglass").font(.system(size: 12)).foregroundStyle(.secondary)
			TextField(prompt, text: $text).textFieldStyle(.plain).font(.system(size: 13))
			if !text.isEmpty {
				Button { text = "" } label: { Image(systemName: "xmark.circle.fill").font(.system(size: 12)) }
					.buttonStyle(.plain).foregroundStyle(.secondary).help("Clear")
			}
		}
		.padding(.horizontal, 8).padding(.vertical, 5)
		.background(RoundedRectangle(cornerRadius: 7).fill(Color.primary.opacity(0.06)))
	}
}


// MARK: Add-ons

/// One device on your network, with the add-ons chosen for it and what it
/// last said it applied (GET /v2/net).
struct NetNodeAddons: Decodable, Identifiable {
	struct Wanted: Decodable {
		struct Focus: Decodable { let sites: [String]; let policy: String; let until: Double?; let rules: [FocusRule]? }
		let level: String
		let focus: Focus
		let set_at: String?
	}
	struct Applied: Decodable {
		struct Rule: Decodable { let name: String?; let policy: String; let sites: [String]; let on: Bool }
		struct Focus: Decodable { let policy: String?; let sites: [String]?; let blocking: Bool?; let until: Double?; let rules: [Rule]? }
		struct Exit: Decodable { let offering: Bool?; let via: String?; let active: Bool?; let why: String? }
		let level: String?
		let focus: Focus?
		let exit: Exit?
		let at: Double?
	}
	let ip: String
	let name: String
	let online: Bool
	let os: String?
	let addons: Wanted?
	let addons_applied: Applied?
	let exit_offer: Bool?
	let exit_via: String?
	var id: String { ip }
}

/// One Focus rule: sites, and when they're blocked. A computer can have several.
struct FocusRule: Codable, Equatable, Identifiable {
	struct Entry: Codable, Equatable, Hashable { var at: String; var block: Bool }
	var name: String?
	var sites: [String]
	var policy: String
	var until: Double?
	var schedule: [Entry]?
	var days: [Int]?
	var id: String { name ?? "Focus" }

	/// "facebook.com, m.facebook.com, x" → "Facebook, X".
	static func names(_ sites: [String]) -> String {
		let labels = ["facebook": "Facebook", "instagram": "Instagram", "youtube": "YouTube", "x": "X", "tiktok": "TikTok", "reddit": "Reddit", "netflix": "Netflix", "linkedin": "LinkedIn"]
		var out: [String] = []
		for s in sites {
			let d = s.lowercased()
			let g = groupDomains.first { $0.value.contains(d) }?.key ?? d
			let l = labels[g] ?? g
			if !out.contains(l) { out.append(l) }
		}
		return out.joined(separator: ", ")
	}

	/// The same groups as cerver's focusGroups.
	static let groupDomains: [String: [String]] = [
		"facebook": ["facebook.com", "www.facebook.com", "m.facebook.com", "web.facebook.com", "fb.com"],
		"instagram": ["instagram.com", "www.instagram.com"],
		"youtube": ["youtube.com", "www.youtube.com", "m.youtube.com", "youtu.be"],
		"x": ["x.com", "www.x.com", "twitter.com", "www.twitter.com", "mobile.twitter.com"],
		"tiktok": ["tiktok.com", "www.tiktok.com"],
		"reddit": ["reddit.com", "www.reddit.com", "old.reddit.com"],
		"netflix": ["netflix.com", "www.netflix.com"],
		"linkedin": ["linkedin.com", "www.linkedin.com"],
	]

	/// Work hours 08:00–18:00, every day, with the last 5 minutes of each hour open.
	static var workdayWithBreaks: [Entry] {
		(8...17).flatMap { h in [Entry(at: String(format: "%02d:00", h), block: true), Entry(at: String(format: "%02d:55", h), block: false)] }
	}
}

/// Add-ons for every computer on your network (GET /v2/net). You choose per
/// computer; that computer's Cerver network service applies it (no password)
/// at its next check-in, about 20 seconds, and reports back.
@MainActor final class AddonsModel: ObservableObject {
	@Published var nodes: [NetNodeAddons] = []
	@Published var problem: String?
	@Published var saving: Set<String> = []

	func load() async {
		struct R: Decodable { let nodes: [NetNodeAddons] }
		guard let d = await gatewayGET("/v2/net"), let r = try? JSONDecoder().decode(R.self, from: d) else {
			if nodes.isEmpty { problem = "Couldn't load your computers. Is Cerver's private network on?" }
			return
		}
		nodes = r.nodes.filter { ($0.os ?? "darwin") == "darwin" } + r.nodes.filter { ($0.os ?? "darwin") != "darwin" }
		problem = nil
	}

	func level(_ n: NetNodeAddons) -> String { n.addons?.level ?? n.addons_applied?.level ?? "off" }
	func sites(_ n: NetNodeAddons) -> [String] { n.addons?.focus.sites ?? n.addons_applied?.focus?.sites ?? [] }
	func policy(_ n: NetNodeAddons) -> String { n.addons?.focus.policy ?? n.addons_applied?.focus?.policy ?? "off" }

	/// The computer's Focus rules (what you chose; else what it reports).
	func rules(_ n: NetNodeAddons) -> [FocusRule] {
		if let f = n.addons?.focus {
			if let r = f.rules, !r.isEmpty { return r }
			return [FocusRule(name: nil, sites: f.sites, policy: f.policy, until: f.until)]
		}
		if let r = n.addons_applied?.focus?.rules, !r.isEmpty {
			return r.map { FocusRule(name: $0.name, sites: $0.sites, policy: $0.policy) }
		}
		return [FocusRule(name: nil, sites: [], policy: "off")]
	}

	func saveRules(_ n: NetNodeAddons, _ rules: [FocusRule]) {
		guard let r = try? JSONEncoder().encode(rules), let list = try? JSONSerialization.jsonObject(with: r) else { return }
		send(n, ["focus": ["rules": list]])
	}

	/// Exit node: {offer: Bool} or {via: ip or NSNull()}.
	func saveExit(_ n: NetNodeAddons, _ body: [String: Any]) {
		let data = try? JSONSerialization.data(withJSONObject: body)
		saving.insert(n.ip)
		Task {
			let ok = await gatewayRequest("PUT", "/v2/net/nodes/\(n.ip)/exit", body: data) != nil
			saving.remove(n.ip)
			problem = ok ? nil : "Couldn't change it for \(n.name). Try again."
			await load()
		}
	}

	/// Safety only (Focus stays as it is).
	func save(_ n: NetNodeAddons, level: String) { send(n, ["level": level]) }

	private func send(_ n: NetNodeAddons, _ obj: [String: Any]) {
		let body = try? JSONSerialization.data(withJSONObject: obj)
		saving.insert(n.ip)
		Task {
			let ok = await gatewayRequest("PUT", "/v2/net/nodes/\(n.ip)/addons", body: body) != nil
			saving.remove(n.ip)
			problem = ok ? nil : "Couldn't save it for \(n.name). Try again."
			await load()
		}
	}

	/// Status of one computer: (text, color).
	func status(_ n: NetNodeAddons, thisMac: Bool) -> (String, Color) {
		if (n.os ?? "darwin") != "darwin" { return ("Macs only for now", .secondary) }
		if !n.online { return ("Offline · applies when it's back", .secondary) }
		guard let a = n.addons_applied else { return ("Needs a Cerver update (automatic, within the hour)", .orange) }
		if let w = n.addons {
			let want = rules(n).map { "\($0.name ?? "")|\($0.policy)|\(Set($0.sites).sorted())" }
			let got = (a.focus?.rules ?? []).map { "\($0.name ?? "")|\($0.policy)|\(Set($0.sites).sorted())" }
			if a.level != w.level || (!got.isEmpty && want != got) || (got.isEmpty && (a.focus?.policy != w.focus.policy || Set(a.focus?.sites ?? []) != Set(w.focus.sites))) {
				return ("Applying…", .orange)
			}
		}
		return ((a.focus?.blocking ?? false) ? "In effect · blocking now" : "In effect", .green)
	}
}

struct AddonStatus: View {
	let text: String
	let color: Color
	var body: some View {
		HStack(spacing: 5) {
			Circle().fill(color).frame(width: 7, height: 7)
			Text(text).font(.caption).foregroundStyle(.secondary)
		}
	}
}

/// The Add-ons page: what each add-on is, with how many computers use it.
struct AddonsHome: View {
	@ObservedObject var a: AddonsModel
	let open: (Page) -> Void
	var body: some View {
		PageLayout(title: "Add-ons", subtitle: "Extra protection for whole computers: every app and browser on them, not only agents. Each computer gets its own settings.") {
			Card {
				item("checkmark.shield", "Safety", "Block malware, adult sites, or turn on kids safety.",
				     a.nodes.filter { a.level($0) != "off" }.count, .safety)
				Divider().padding(.leading, 16)
				item("sparkles", "Cleanup", "An agent finds what's filling a computer's disk and what slows it down, and cleans the safe things when you say so.",
				     -1, .cleanup)
				Divider().padding(.leading, 16)
				item("moon.zzz", "Focus", "Block distracting sites like Facebook, Instagram and YouTube: always, in work hours, or for an hour.",
				     a.nodes.filter { a.rules($0).contains { $0.policy != "off" && !$0.sites.isEmpty } }.count, .focus)
				Divider().padding(.leading, 16)
				item("lock.shield", "VPN", "On café Wi-Fi, send your laptop's internet through your own computer at home, encrypted. The café sees nothing.",
				     a.nodes.filter { $0.exit_via != nil }.count, .exitNode)
			}
		}
		.task { await a.load() }
	}
	func item(_ icon: String, _ name: String, _ what: String, _ on: Int, _ p: Page) -> some View {
		Button { open(p) } label: {
			HStack(alignment: .top, spacing: 12) {
				Image(systemName: icon).font(.system(size: 18)).frame(width: 24).foregroundStyle(Color.accentColor).padding(.top, 2)
				VStack(alignment: .leading, spacing: 3) {
					Text(name).font(.system(size: 15, weight: .semibold))
					Text(what).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
					if on >= 0 {
						Text(on == 0 ? "Off on all your computers" : "On for \(on) of \(a.nodes.count) computers").font(.caption).foregroundStyle(.secondary)
					}
				}
				Spacer()
				Image(systemName: "chevron.right").foregroundStyle(.secondary)
			}
			.padding(16).contentShape(Rectangle())
		}
		.buttonStyle(.plain)
	}
}

/// Safety: one row per computer, each with its own level.
struct SafetyPage: View {
	@ObservedObject var m: Model
	@ObservedObject var a: AddonsModel
	let levels: [(String, String, String)] = [
		("off", "Off", "Nothing filtered."),
		("malware", "Block malware", "Malware and phishing sites. By Cloudflare."),
		("adult", "Block adult sites", "Adult sites, plus malware. Cloudflare for Families."),
		("kids", "Kids safety", "Adult sites, filter-dodging proxies and VPNs; SafeSearch forced on Google, Bing and YouTube. By CleanBrowsing."),
	]

	var body: some View {
		PageLayout(title: "Safety", subtitle: "Choose for each computer. It filters that computer's whole internet through a safe DNS.") {
			if a.nodes.isEmpty { Text(a.problem ?? "Loading your computers…").foregroundStyle(.secondary) }
			ForEach(a.nodes) { n in
				Card {
					HStack {
						Text(n.name + (n.ip == m.status?.ip ? "  (this Mac)" : "")).font(.system(size: 15, weight: .semibold))
						Spacer()
						let st = a.status(n, thisMac: n.ip == m.status?.ip)
						if a.saving.contains(n.ip) { ProgressView().controlSize(.small) }
						AddonStatus(text: st.0, color: st.1)
					}
					.padding(.horizontal, 16).padding(.top, 14).padding(.bottom, 10)
					Picker("", selection: Binding(get: { a.level(n) }, set: { a.save(n, level: $0) })) {
						ForEach(levels, id: \.0) { Text($0.1).tag($0.0) }
					}
					.pickerStyle(.segmented).labelsHidden()
					.padding(.horizontal, 16)
					.disabled((n.os ?? "darwin") != "darwin")
					Text(levels.first { $0.0 == a.level(n) }?.2 ?? "")
						.font(.callout).foregroundStyle(.secondary)
						.padding(.horizontal, 16).padding(.top, 8).padding(.bottom, 14)
				}
			}
			if let p = a.problem, !a.nodes.isEmpty { Text(p).font(.callout).foregroundStyle(.red) }
			Text("Each level includes the ones before it. Cerver's names for your computers keep working.")
				.font(.caption).foregroundStyle(.secondary)
		}
		.task { while !Task.isCancelled { await a.load(); try? await Task.sleep(nanoseconds: 4_000_000_000) } }
	}
}

/// Exit node: a computer (at home) your other computers can send their
/// internet through, encrypted, e.g. from café Wi-Fi.
struct ExitNodePage: View {
	@ObservedObject var m: Model
	@ObservedObject var a: AddonsModel

	func now(_ n: NetNodeAddons) -> (String, Color) {
		let e = n.addons_applied?.exit
		if (n.os ?? "darwin") != "darwin" { return ("Macs only for now", .secondary) }
		if !n.online { return ("Offline", .secondary) }
		if let why = e?.why, !why.isEmpty { return (why, .orange) }
		if n.exit_via != nil { return e?.active == true ? ("VPN on", .green) : ("Switching…", .orange) }
		if n.exit_offer == true { return e?.offering == true ? ("Ready as a VPN", .green) : ("Setting up… (needs a Cerver update)", .orange) }
		return ("Uses its own connection", .secondary)
	}

	var body: some View {
		PageLayout(title: "VPN, through your own computers", subtitle: "Send a computer's internet through another one of yours, encrypted. From café Wi-Fi, the café sees only one scrambled connection, and websites see your home's address. Unlike a VPN company, nobody else's servers are in between.") {
			if let me = a.nodes.first(where: { $0.ip == m.status?.ip }) {
				let via = a.nodes.first { $0.ip == me.exit_via }
				let home = via?.name ?? a.nodes.first { $0.exit_offer == true && $0.ip != me.ip }?.name ?? a.nodes.first { $0.ip != me.ip && $0.os != "phone" }?.name ?? "Your home computer"
				Card { VPNPicture(on: me.exit_via != nil, me: me.name, home: home) }
			}
			if a.nodes.isEmpty { Text(a.problem ?? "Loading your computers…").foregroundStyle(.secondary) }
			ForEach(a.nodes) { n in
				let exits = a.nodes.filter { $0.exit_offer == true && $0.ip != n.ip }
				Card {
					HStack {
						Text(n.name + (n.ip == m.status?.ip ? "  (this Mac)" : "")).font(.system(size: 15, weight: .semibold))
						Spacer()
						if a.saving.contains(n.ip) { ProgressView().controlSize(.small) }
						let st = now(n)
						AddonStatus(text: st.0, color: st.1)
					}
					.padding(.horizontal, 16).padding(.top, 14).padding(.bottom, 10)
					Divider().padding(.leading, 16)
					Row(label: "Use as VPN for my other computers") {
						Toggle("", isOn: Binding(get: { n.exit_offer == true }, set: { a.saveExit(n, ["offer": $0]) }))
							.toggleStyle(.switch).labelsHidden()
							.disabled(n.exit_via != nil)
					}
					.help(n.exit_via != nil ? "It sends its own internet through a VPN; turn that off first." : "Your other computers can send their internet through this one. Best on a computer that stays home and on.")
					Divider().padding(.leading, 16)
					Row(label: "Internet goes") {
						Picker("", selection: Binding(get: { n.exit_via ?? "" }, set: { v in a.saveExit(n, ["via": v.isEmpty ? NSNull() : v]) })) {
							Text("Directly").tag("")
							ForEach(exits) { e in Text("Through \(e.name)").tag(e.ip) }
						}
						.labelsHidden().frame(maxWidth: 260)
						.disabled(n.exit_offer == true || (exits.isEmpty && n.exit_via == nil))
					}
				}
			}
			if let p = a.problem, !a.nodes.isEmpty { Text(p).font(.callout).foregroundStyle(.red) }
			Text("Speed is limited by that computer's home upload. If it stops answering, the computer goes back to its own connection by itself. Your Safety and Focus settings keep working.")
				.font(.caption).foregroundStyle(.secondary)
		}
		.task { while !Task.isCancelled { await a.load(); try? await Task.sleep(nanoseconds: 4_000_000_000) } }
	}
}

/// The VPN, drawn: this Mac's internet going straight out, or through the
/// tunnel to your computer at home. Follows the real setting.
struct VPNPicture: View {
	let on: Bool
	let me: String
	let home: String
	@Environment(\.accessibilityReduceMotion) private var still
	let tunnel = Color(red: 0.16, green: 0.62, blue: 0.47)

	var body: some View {
		VStack(alignment: .leading, spacing: 10) {
			HStack(spacing: 8) {
				Circle().fill(on ? tunnel : Color.orange).frame(width: 10, height: 10)
				Text(on ? "VPN on" : "VPN off").font(.system(size: 15, weight: .semibold))
			}
			TimelineView(.animation(paused: still)) { tl in
				Canvas { ctx, size in draw(&ctx, size, t: still ? 0 : tl.date.timeIntervalSinceReferenceDate) }
			}
			.frame(height: 230)
			Text(on ? "Websites see your home. The network you're on only sees scrambled data going to \(home)."
			        : "Websites see wherever you are. The café, hotel or phone carrier can see which sites you visit.")
				.font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
		}
		.padding(16)
	}

	func draw(_ c: inout GraphicsContext, _ size: CGSize, t: Double) {
		let s = min(size.width / 520, size.height / 230)
		c.translateBy(x: (size.width - 520 * s) / 2, y: 0)
		c.scaleBy(x: s, y: s)
		let fg = GraphicsContext.Shading.foreground
		let dim = GraphicsContext.Shading.color(.secondary)
		let phase = CGFloat(t.truncatingRemainder(dividingBy: 1.2) / 1.2 * -12)
		func text(_ str: String, _ x: CGFloat, _ y: CGFloat, bold: Bool = true, color: Color? = nil) {
			var tx = Text(str).font(.system(size: bold ? 13 : 12, weight: bold ? .semibold : .regular))
			if let color { tx = tx.foregroundColor(color) } else if !bold { tx = tx.foregroundColor(.secondary) }
			c.draw(tx, at: CGPoint(x: x, y: y), anchor: .center)
		}
		func arrow(_ a: CGPoint, _ b: CGPoint, _ color: Color, flow: Bool) {
			var p = Path(); p.move(to: a); p.addLine(to: b)
			c.stroke(p, with: .color(color), lineWidth: 3)
			if flow { c.stroke(p, with: .color(Color(nsColor: .windowBackgroundColor)), style: StrokeStyle(lineWidth: 3, lineCap: .round, dash: [2, 10], dashPhase: phase)) }
			let ang = atan2(b.y - a.y, b.x - a.x), l: CGFloat = 11
			var h = Path()
			h.move(to: CGPoint(x: b.x + 4 * cos(ang), y: b.y + 4 * sin(ang)))
			h.addLine(to: CGPoint(x: b.x - l * cos(ang - 0.45), y: b.y - l * sin(ang - 0.45)))
			h.addLine(to: CGPoint(x: b.x - l * cos(ang + 0.45), y: b.y - l * sin(ang + 0.45)))
			h.closeSubpath()
			c.fill(h, with: .color(color))
		}
		// Websites
		let g = CGPoint(x: 260, y: 58)
		c.stroke(Path(ellipseIn: CGRect(x: g.x - 26, y: g.y - 26, width: 52, height: 52)), with: fg, lineWidth: 2)
		c.stroke(Path(ellipseIn: CGRect(x: g.x - 11, y: g.y - 26, width: 22, height: 52)), with: fg, lineWidth: 1.5)
		var eq = Path(); eq.move(to: CGPoint(x: g.x - 26, y: g.y)); eq.addLine(to: CGPoint(x: g.x + 26, y: g.y))
		c.stroke(eq, with: fg, lineWidth: 1.5)
		text("Websites", g.x, 18)
		text(on ? "see: your home" : "see: where you are", g.x, 100, bold: false)
		// This Mac (laptop)
		c.stroke(Path(roundedRect: CGRect(x: 48, y: 136, width: 64, height: 42), cornerRadius: 5), with: fg, lineWidth: 2)
		var base = Path()
		base.move(to: CGPoint(x: 38, y: 182)); base.addLine(to: CGPoint(x: 122, y: 182))
		base.addLine(to: CGPoint(x: 116, y: 189)); base.addLine(to: CGPoint(x: 44, y: 189)); base.closeSubpath()
		c.fill(base, with: fg)
		text(me, 80, 206)
		text("where you are", 80, 222, bold: false)
		// Home computer (Mac mini)
		c.stroke(Path(roundedRect: CGRect(x: 400, y: 166, width: 80, height: 22), cornerRadius: 7), with: fg, lineWidth: 2)
		c.fill(Path(ellipseIn: CGRect(x: 462, y: 175, width: 4, height: 4)), with: fg)
		text(home, 440, 206)
		text("at home", 440, 222, bold: false)
		if on {
			var direct = Path(); direct.move(to: CGPoint(x: 100, y: 130)); direct.addLine(to: CGPoint(x: 236, y: 76))
			c.stroke(direct, with: dim, style: StrokeStyle(lineWidth: 1.5, dash: [3, 5]))
			let cap = Path(roundedRect: CGRect(x: 128, y: 164, width: 264, height: 26), cornerRadius: 13)
			c.fill(cap, with: .color(tunnel.opacity(0.15)))
			c.stroke(cap, with: .color(tunnel), lineWidth: 2)
			arrow(CGPoint(x: 142, y: 177), CGPoint(x: 380, y: 177), tunnel, flow: true)
			text("encrypted tunnel", 260, 206, color: tunnel)
			arrow(CGPoint(x: 428, y: 158), CGPoint(x: 284, y: 72), tunnel, flow: true)
			text("home internet", 395, 104, color: tunnel)
		} else {
			arrow(CGPoint(x: 100, y: 130), CGPoint(x: 236, y: 76), .orange, flow: true)
			text("straight out", 132, 92, color: .orange)
			var net = Path(); net.move(to: CGPoint(x: 130, y: 177)); net.addLine(to: CGPoint(x: 392, y: 177))
			c.stroke(net, with: .color(tunnel), style: StrokeStyle(lineWidth: 2, dash: [6, 6]))
			text("private network · connected", 260, 160, bold: false)
		}
	}
}

/// Focus: one card per computer, each with its own sites and schedule.
struct FocusPage: View {
	@ObservedObject var m: Model
	@ObservedObject var a: AddonsModel
	@State private var custom: [String: String] = [:]
	@State private var names: [String: String] = [:]
	@State private var open: Set<String> = []
	@State private var newTime = ""
	let groups: [(String, String)] = [("facebook", "Facebook"), ("instagram", "Instagram"), ("youtube", "YouTube"), ("x", "X"), ("tiktok", "TikTok"), ("reddit", "Reddit"), ("netflix", "Netflix"), ("linkedin", "LinkedIn")]
	let policies: [(String, String)] = [("off", "Off"), ("work_hours", "Work hours"), ("until", "For an hour"), ("always", "Always"), ("schedule", "Schedule")]

	var body: some View {
		PageLayout(title: "Focus", subtitle: "Block distracting sites. Each computer has its own sites and its own schedule.") {
			if a.nodes.isEmpty { Text(a.problem ?? "Loading your computers…").foregroundStyle(.secondary) }
			ForEach(a.nodes) { n in card(n) }
			if let p = a.problem, !a.nodes.isEmpty { Text(p).font(.callout).foregroundStyle(.red) }
			Text("Work hours: Monday to Friday, 9:00 to 18:00. A browser using its own secure DNS can skip Focus; turn that off in the browser if a site still opens.")
				.font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
		}
		.task { while !Task.isCancelled { await a.load(); try? await Task.sleep(nanoseconds: 4_000_000_000) } }
	}

	func card(_ n: NetNodeAddons) -> some View {
		let mac = (n.os ?? "darwin") == "darwin"
		let rules = a.rules(n)
		let on = Dictionary((n.addons_applied?.focus?.rules ?? []).map { ($0.name ?? "", $0.on) }, uniquingKeysWith: { x, _ in x })
		return Card {
			HStack {
				Text(n.name + (n.ip == m.status?.ip ? "  (this Mac)" : "")).font(.system(size: 15, weight: .semibold))
				Spacer()
				let st = a.status(n, thisMac: n.ip == m.status?.ip)
				if a.saving.contains(n.ip) { ProgressView().controlSize(.small) }
				AddonStatus(text: st.0.replacingOccurrences(of: " · blocking now", with: ""), color: st.1)
			}
			.padding(.horizontal, 16).padding(.top, 14).padding(.bottom, 6)
			ForEach(Array(rules.enumerated()), id: \.offset) { i, r in
				let key = "\(n.ip)#\(i)"
				Divider().padding(.leading, 16)
				Button { withAnimation(.easeInOut(duration: 0.15)) { if open.contains(key) { open.remove(key) } else { open.insert(key) } } } label: {
					HStack(spacing: 10) {
						Image(systemName: open.contains(key) ? "chevron.down" : "chevron.right")
							.font(.system(size: 10, weight: .semibold)).foregroundStyle(.secondary).frame(width: 12)
						Circle().fill((on[r.name ?? ""] ?? false) ? Color.green : Color.secondary.opacity(0.3)).frame(width: 7, height: 7)
							.help((on[r.name ?? ""] ?? false) ? "Blocking now" : "Not blocking now")
						Text(r.name ?? "Focus").font(.system(size: 14, weight: .semibold))
						Text(whenSummary(r)).font(.system(size: 13)).foregroundStyle(.secondary)
						Spacer(minLength: 12)
						Text(sitesSummary(r)).font(.system(size: 13)).foregroundStyle(.secondary).lineLimit(1).truncationMode(.tail)
					}
					.contentShape(Rectangle())
				}
				.buttonStyle(.plain)
				.padding(.horizontal, 16).padding(.vertical, 11)
				if open.contains(key) {
					ruleView(n, rules, i, r, blockingNow: on[r.name ?? ""] ?? false)
						.padding(14)
						.background(RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(0.03)))
						.padding(.horizontal, 16).padding(.bottom, 12)
				}
			}
			Divider().padding(.leading, 16)
			Button {
				let new = rules.count
				a.saveRules(n, rules + [FocusRule(name: "Rule \(rules.count + 1)", sites: [], policy: "off")])
				open.insert("\(n.ip)#\(new)")
			} label: {
				Label("Add a rule", systemImage: "plus")
			}
			.buttonStyle(.link).padding(.horizontal, 16).padding(.vertical, 10)
		}
		.disabled(!mac)
	}

	@ViewBuilder func ruleView(_ n: NetNodeAddons, _ rules: [FocusRule], _ i: Int, _ r: FocusRule, blockingNow: Bool) -> some View {
		let key = "\(n.ip)#\(i)"
		let set: (FocusRule) -> Void = { nr in var c = nr; c.sites = canonSites(nr.sites); var all = rules; all[i] = c; a.saveRules(n, all) }
		let sites = canonSites(r.sites)
		VStack(alignment: .leading, spacing: 10) {
			HStack {
				TextField("Rule name", text: Binding(get: { names[key] ?? (r.name ?? "Focus") }, set: { names[key] = $0 }))
					.textFieldStyle(.plain).font(.system(size: 14, weight: .semibold)).frame(maxWidth: 220)
					.onSubmit { var nr = r; nr.name = names[key]; set(nr) }
				Spacer()
				if rules.count > 1 {
					Button("Remove rule") { var all = rules; all.remove(at: i); a.saveRules(n, all) }.buttonStyle(.link).font(.caption)
				}
			}
			HStack {
				Text("When").font(.system(size: 13, weight: .medium)).frame(width: 46, alignment: .leading)
				Picker("", selection: Binding(get: { r.policy }, set: { p in
					var nr = r; nr.policy = p
					nr.until = p == "until" ? Date().timeIntervalSince1970 + 3600 : nil
					if p == "schedule" && (nr.schedule ?? []).isEmpty { nr.schedule = FocusRule.workdayWithBreaks }
					set(nr)
				})) {
					ForEach(policies, id: \.0) { Text($0.1).tag($0.0) }
				}
				.pickerStyle(.segmented).labelsHidden()
			}
			if r.policy == "until", let u = r.until, u > Date().timeIntervalSince1970 {
				Text("Blocked until \(Date(timeIntervalSince1970: u).formatted(date: .omitted, time: .shortened)).").font(.caption).foregroundStyle(.secondary)
			}
			if r.policy == "schedule" { scheduleEditor(r, set) }
			FlowChips(items: groups + sites.filter { s in !groups.contains { $0.0 == s } }.map { ($0, $0) }, on: Set(sites)) { id in
				var nr = r
				nr.sites = sites
				if let k = nr.sites.firstIndex(of: id) { nr.sites.remove(at: k) } else { nr.sites.append(id) }
				set(nr)
			}
			HStack {
				TextField("Another site, e.g. lichess.org", text: Binding(get: { custom[key] ?? "" }, set: { custom[key] = $0 }))
					.textFieldStyle(.roundedBorder).onSubmit { addCustom(key, r, set) }
				Button("Add") { addCustom(key, r, set) }.disabled((custom[key] ?? "").trimmingCharacters(in: .whitespaces).isEmpty)
			}
		}
	}

	/// Domains that belong to a group become the group ("m.facebook.com" → Facebook).
	func canonSites(_ sites: [String]) -> [String] {
		var out: [String] = []
		for s in sites {
			let d = s.lowercased()
			let g = FocusRule.groupDomains.first { $0.value.contains(d) }?.key ?? d
			if !out.contains(g) { out.append(g) }
		}
		return out
	}

	func sitesSummary(_ r: FocusRule) -> String {
		let names = canonSites(r.sites).map { id in groups.first { $0.0 == id }?.1 ?? id }
		return names.isEmpty ? "No sites" : names.joined(separator: ", ")
	}

	func whenSummary(_ r: FocusRule) -> String {
		switch r.policy {
		case "always": return "Always"
		case "work_hours": return "Mon–Fri 9:00–18:00"
		case "until":
			if let u = r.until, u > Date().timeIntervalSince1970 { return "Until \(Date(timeIntervalSince1970: u).formatted(date: .omitted, time: .shortened))" }
			return "Hour ended"
		case "schedule":
			let e = (r.schedule ?? []).sorted { $0.at < $1.at }
			guard let first = e.first(where: { $0.block }), let last = e.last(where: { !$0.block }) else { return "Schedule" }
			let breaks = e.filter { !$0.block }.count - 1
			let days = (r.days ?? []).isEmpty ? "" : " · " + (r.days ?? []).map { ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"][$0] }.joined(separator: " ")
			return "\(first.at)–\(last.at)" + (breaks > 0 ? ", \(breaks) breaks" : "") + days
		default: return "Off"
		}
	}

	/// Times of day: from each one on, the sites are blocked or open.
	@ViewBuilder func scheduleEditor(_ r: FocusRule, _ set: @escaping (FocusRule) -> Void) -> some View {
		let entries = (r.schedule ?? []).sorted { $0.at < $1.at }
		VStack(alignment: .leading, spacing: 6) {
			HStack(spacing: 6) {
				Text("Days").font(.system(size: 13, weight: .medium)).frame(width: 46, alignment: .leading)
				ForEach(Array(["S", "M", "T", "W", "T", "F", "S"].enumerated()), id: \.offset) { d, l in
					let all = (r.days ?? []).isEmpty
					let on = all || (r.days ?? []).contains(d)
					Button(l) {
						var days = all ? Array(0...6) : (r.days ?? [])
						if let k = days.firstIndex(of: d) { days.remove(at: k) } else { days.append(d) }
						var nr = r; nr.days = days.count == 7 ? nil : days.sorted(); set(nr)
					}
					.buttonStyle(.plain).font(.system(size: 12, weight: .semibold))
					.frame(width: 24, height: 22)
					.background(RoundedRectangle(cornerRadius: 5).fill(on ? Color.accentColor.opacity(0.2) : Color.primary.opacity(0.06)))
					.foregroundStyle(on ? Color.accentColor : Color.secondary)
				}
			}
			Text(scheduleSummary(entries)).font(.caption).foregroundStyle(.secondary)
			LazyVGrid(columns: [GridItem(.adaptive(minimum: 150), spacing: 8, alignment: .leading)], alignment: .leading, spacing: 6) {
				ForEach(entries, id: \.self) { e in
					HStack(spacing: 4) {
						Text(e.at).font(.system(size: 12, design: .monospaced))
						Button(e.block ? "block" : "open") {
							var nr = r; nr.schedule = entries.map { $0 == e ? FocusRule.Entry(at: e.at, block: !e.block) : $0 }; set(nr)
						}
						.buttonStyle(.plain).font(.system(size: 12, weight: .medium))
						.foregroundStyle(e.block ? Color.red : Color.green)
						Button { var nr = r; nr.schedule = entries.filter { $0 != e }; set(nr) } label: { Image(systemName: "xmark").font(.system(size: 9)) }
							.buttonStyle(.plain).foregroundStyle(.secondary)
					}
					.padding(.horizontal, 8).padding(.vertical, 4)
					.background(Capsule().fill(Color.primary.opacity(0.05)))
				}
			}
			HStack {
				TextField("HH:MM", text: $newTime).textFieldStyle(.roundedBorder).frame(width: 70)
				Button("Block from then") { addTime(r, entries, true, set) }
				Button("Open from then") { addTime(r, entries, false, set) }
				Spacer()
				Button("Workday with breaks") { var nr = r; nr.schedule = FocusRule.workdayWithBreaks; set(nr) }.buttonStyle(.link).font(.caption)
					.help("08:00–18:00, with the last 5 minutes of each hour open")
			}
		}
		.padding(10)
		.background(RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(0.03)))
	}

	func scheduleSummary(_ e: [FocusRule.Entry]) -> String {
		guard let first = e.first(where: { $0.block }), let last = e.last(where: { !$0.block }) else { return "Add the times when blocking starts and stops." }
		let breaks = e.filter { !$0.block }.count - 1
		return "Blocked from \(first.at), open from \(last.at)" + (breaks > 0 ? ", with \(breaks) break\(breaks == 1 ? "" : "s") in between." : ".")
	}

	func addTime(_ r: FocusRule, _ entries: [FocusRule.Entry], _ block: Bool, _ set: (FocusRule) -> Void) {
		let t = newTime.trimmingCharacters(in: .whitespaces)
		guard t.range(of: #"^([01]\d|2[0-3]):[0-5]\d$"#, options: .regularExpression) != nil else { return }
		var nr = r; nr.schedule = entries.filter { $0.at != t } + [FocusRule.Entry(at: t, block: block)]
		newTime = ""
		set(nr)
	}

	func addCustom(_ key: String, _ r: FocusRule, _ set: (FocusRule) -> Void) {
		let d = (custom[key] ?? "").trimmingCharacters(in: .whitespaces).lowercased()
			.replacingOccurrences(of: "https://", with: "").replacingOccurrences(of: "http://", with: "")
			.components(separatedBy: "/").first ?? ""
		guard !d.isEmpty, !r.sites.contains(d) else { return }
		custom[key] = ""
		var nr = r; nr.sites.append(d); set(nr)
	}
}

/// Toggle chips that wrap onto new lines.
struct FlowChips: View {
	let items: [(String, String)]
	let on: Set<String>
	let tap: (String) -> Void
	var body: some View {
		Flow(spacing: 8) {
			ForEach(items, id: \.0) { id, label in
				Button { tap(id) } label: {
					HStack(spacing: 5) {
						Image(systemName: on.contains(id) ? "checkmark" : "plus").font(.system(size: 10, weight: .bold))
						Text(label).font(.system(size: 13)).lineLimit(1)
					}
					.padding(.horizontal, 10).padding(.vertical, 5)
					.background(Capsule().fill(on.contains(id) ? Color.accentColor.opacity(0.18) : Color.primary.opacity(0.06)))
					.foregroundStyle(on.contains(id) ? Color.accentColor : Color.primary)
				}
				.buttonStyle(.plain)
			}
		}
	}
}


/// Lays children out left to right, wrapping onto new lines; each keeps its own width.
struct Flow: Layout {
	var spacing: CGFloat = 8
	func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
		let width = proposal.width ?? 600
		var x: CGFloat = 0, y: CGFloat = 0, row: CGFloat = 0, widest: CGFloat = 0
		for v in subviews {
			let s = v.sizeThatFits(.unspecified)
			if x > 0 && x + s.width > width { y += row + spacing; x = 0; row = 0 }
			x += s.width + spacing; row = max(row, s.height); widest = max(widest, x - spacing)
		}
		return CGSize(width: proposal.width ?? widest, height: y + row)
	}
	func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
		var x = bounds.minX, y = bounds.minY, row: CGFloat = 0
		for v in subviews {
			let s = v.sizeThatFits(.unspecified)
			if x > bounds.minX && x + s.width > bounds.maxX { y += row + spacing; x = bounds.minX; row = 0 }
			v.place(at: CGPoint(x: x, y: y), proposal: ProposedViewSize(s))
			x += s.width + spacing; row = max(row, s.height)
		}
	}
}

// MARK: Cleanup (an agentic add-on)

/// Cleanup: an agent on the chosen computer finds what fills its disk and
/// what slows it down, and cleans only the safe things, only when asked.
/// Each button hands the task to Claude on that computer (Ask Compute Pool).
struct CleanupPage: View {
	@ObservedObject var m: Model
	@Environment(\.askAgent) private var ask
	@State private var confirm: Computer?

	var computers: [Computer] { m.computers.filter { !$0.isPhone && ($0.pool != nil || $0.isThisMac) } }

	var body: some View {
		PageLayout(title: "Cleanup", subtitle: "An agent on each computer finds what's filling its disk and what's slowing it down. It only deletes when you say so, and never your own files.") {
			ForEach(computers) { c in
				let online = c.isThisMac || c.pool?.online == true
				Card {
					HStack {
						Text(c.name + (c.isThisMac ? "  (this Mac)" : "")).font(.system(size: 15, weight: .semibold))
						Spacer()
						if let f = c.spec?.disk_free_gb {
							Text("\(Int(f)) GB free").font(.callout).foregroundStyle(f < 30 ? Color.orange : Color.secondary)
						}
					}
					.padding(.horizontal, 16).padding(.top, 14).padding(.bottom, 10)
					HStack(spacing: 10) {
						Button { ask.run(cleanupFindPrompt, c.name, c.isThisMac) } label: { Label("Find what's taking space", systemImage: "magnifyingglass") }
						Button { ask.run(cleanupSlowPrompt, c.name, c.isThisMac) } label: { Label("Why is it slow?", systemImage: "gauge.with.needle") }
						Button { confirm = c } label: { Label("Clean the safe things…", systemImage: "trash") }
					}
					.disabled(!online)
					.padding(.horizontal, 16).padding(.bottom, 14)
					if !online {
						Text("Offline: it needs to be on to run an agent.").font(.caption).foregroundStyle(.secondary)
							.padding(.horizontal, 16).padding(.bottom, 12)
					}
				}
			}
			DisclosureGroup("See the agents' instructions") {
				VStack(alignment: .leading, spacing: 10) {
					ForEach([("Find what's taking space", cleanupFindPrompt), ("Why is it slow?", cleanupSlowPrompt), ("Clean the safe things", cleanupCleanPrompt)], id: \.0) { t, p in
						Text(t).font(.system(size: 13, weight: .semibold))
						Text(p).font(.system(size: 12, design: .monospaced)).textSelection(.enabled)
							.frame(maxWidth: .infinity, alignment: .leading).padding(10)
							.background(RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(0.04)))
					}
				}
				.padding(.top, 6)
			}
			.font(.callout)
			Text("Each button opens the conversation in Ask Compute Pool, where you can ask follow-ups. Agents can do the same from Claude Code with cerver run.")
				.font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
		}
		.alert("Clean the safe things on \(confirm?.name ?? "")?", isPresented: Binding(get: { confirm != nil }, set: { if !$0 { confirm = nil } }), presenting: confirm) { c in
			Button("Clean") { ask.run(cleanupCleanPrompt, c.name, c.isThisMac) }
			Button("Cancel", role: .cancel) {}
		} message: { _ in
			Text("The agent empties caches, old logs, unused Docker data and the Trash, and reports how much it freed. It never touches your documents, photos, code or app data.")
		}
	}
}

let cleanupFindPrompt = """
Find what's taking the most disk space on this computer. Read only: don't delete, move or change anything.

1. Free and total space: df -h / (and diskutil info / on a Mac).
2. The biggest folders in the home folder: du -xhd1 ~ | sort -hr | head -15, then inside the 3 biggest.
3. Check the usual suspects: ~/Library/Caches, ~/.cache, ~/Library/Developer (Xcode), Docker, node_modules folders under ~/Code or ~/Projects, big log files (find ~ -name '*.log' -size +500M), ~/Downloads, the Trash, local Time Machine snapshots (tmutil listlocalsnapshots /).

Reply short: free/total first, then a table of the top items (size, what it is), then two lists: safe to clean (rebuilds or re-downloads by itself) with the GB it would free, and personal data to review yourself. End with the total you could free safely.
"""

let cleanupSlowPrompt = """
Find out why this computer is slow. Read only: don't quit, kill or change anything.

Check: load and uptime (uptime), the top CPU and memory users (top -l 1 -o cpu -n 10 on a Mac, ps aux --sort=-%cpu | head on Linux), memory pressure (memory_pressure on a Mac, free -h on Linux), swap use, free disk space (under ~10% free slows a Mac down), login items and background agents that use a lot, and very old uptime.

Reply short: the one or two main causes first, with the numbers that show it, then what to do about each (and which of those an agent could do for you).
"""

let cleanupCleanPrompt = """
Free disk space on this computer, safely. The owner asked for this.

Allowed (each one rebuilds or re-downloads by itself), measure each before and after:
- caches: ~/Library/Caches/*, ~/.cache/* (uv, pip, puppeteer, huggingface), npm cache clean --force, pnpm store prune, brew cleanup
- Xcode: ~/Library/Developer/Xcode/DerivedData/*
- Docker: docker system prune -f (NOT -a, NOT volumes)
- log files over 500 MB in ~/Library/Logs or a dot-folder of a tool: empty them (truncate), don't delete
- the Trash: ~/.Trash/*

Never touch: Documents, Desktop, Pictures, Movies, Music, Downloads, ~/Code or any project, app data (WhatsApp, Mail, browser profiles), keys and settings (~/.ssh, ~/.config), anything you're unsure about. Don't use sudo.

Reply short: free space before → after, then each item you cleaned with the GB it freed, and anything big you left for the owner to decide.
"""

// MARK: - Agents: what this Mac's relay is doing (jobs received, work done)

/// The relay's local service (127.0.0.1:18081): status, jobs, load, log.
struct RelayJob: Decodable, Identifiable {
	let id: String
	let at: Double
	let task: String?
	let cli: String?
	let from: String?
	let project: String?
	let status: String
	let seconds: Double?
	let error: String?
}
struct RelayJobs: Decodable {
	struct Today: Decodable { let jobs: Int; let done: Int; let failed: Int; let seconds: Int }
	let running: [RelayJob]
	let jobs: [RelayJob]
	let today: Today
	let total: Int
}
struct RelayStats: Decodable {
	struct Compute: Decodable {
		struct Mem: Decodable { let percent: Double }
		struct Disk: Decodable { let free_bytes: Double; let percent: Double }
		let cpu_percent: Double?
		let memory: Mem?
		let disk: Disk?
	}
	let home_directory: String?
	let compute: Compute?
}
struct RelayLogLine: Decodable { let timestamp: String?; let line: String }
struct RelayLog: Decodable { let lines: [RelayLogLine] }

@MainActor final class RelayWatch: ObservableObject {
	@Published var up: Bool?
	@Published var jobs: RelayJobs?
	@Published var stats: RelayStats?
	@Published var log: [RelayLogLine] = []
	@Published var restarting = false
	private var timer: Timer?
	let base = "http://127.0.0.1:18081"

	func start() {
		refresh()
		timer?.invalidate()
		timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in Task { @MainActor in self?.refresh() } }
	}
	func stop() { timer?.invalidate(); timer = nil }

	func get<T: Decodable>(_ path: String, _ t: T.Type) async -> T? {
		guard let u = URL(string: base + path) else { return nil }
		var r = URLRequest(url: u); r.timeoutInterval = 2
		guard let (d, resp) = try? await URLSession.shared.data(for: r), (resp as? HTTPURLResponse)?.statusCode == 200 else { return nil }
		return try? JSONDecoder().decode(T.self, from: d)
	}

	func refresh() {
		Task {
			struct H: Decodable { let status: String }
			let h = await get("/health", H.self)
			up = h != nil
			guard h != nil else { return }
			jobs = await get("/api/cerver/jobs?n=40", RelayJobs.self)
			stats = await get("/api/local-claude/stats", RelayStats.self)
			if let l = await get("/api/local-claude/logs/tail?n=40", RelayLog.self) { log = l.lines }
		}
	}

	/// launchd restarts it (KeepAlive); no password.
	func restart() {
		restarting = true
		let p = Process()
		p.executableURL = URL(fileURLWithPath: "/bin/launchctl")
		p.arguments = ["kickstart", "-k", "gui/\(getuid())/ai.cerver.relay"]
		try? p.run()
		DispatchQueue.main.asyncAfter(deadline: .now() + 6) { self.restarting = false; self.refresh() }
	}
}

struct AgentsPage: View {
	@ObservedObject var m: Model
	@StateObject private var w = RelayWatch()
	@State private var showLog = false

	func ago(_ t: Double) -> String {
		let s = Int(Date().timeIntervalSince1970 - t)
		if s < 60 { return "just now" }
		if s < 3600 { return "\(s / 60) min ago" }
		if s < 86400 { return "\(s / 3600) h ago" }
		return Date(timeIntervalSince1970: t).formatted(date: .abbreviated, time: .shortened)
	}
	func dur(_ s: Double) -> String { s < 60 ? "\(Int(s)) s" : (s < 3600 ? "\(Int(s / 60)) min" : String(format: "%.1f h", s / 3600)) }
	func clean(_ s: String) -> String { s.replacingOccurrences(of: "\u{1b}\\[[0-9;]*m", with: "", options: .regularExpression) }

	var body: some View {
		PageLayout(title: "Agents", subtitle: "What Cerver's agent runner (the relay) on this Mac is doing: jobs it received and the work it did.") {
			Card {
				Row(label: "Relay", note: w.up == false ? "Not running, so this Mac can't take agent jobs." : (w.stats?.home_directory).map { "Works in " + (($0 as NSString).abbreviatingWithTildeInPath) + " when a job names no folder" }) {
					HStack(spacing: 10) {
						if let v = m.updates?.relay?.current { Text("v" + v).font(.system(size: 12, design: .monospaced)).foregroundStyle(.secondary) }
						switch w.up {
						case .some(true): StatusPill(text: (w.jobs?.running.isEmpty ?? true) ? "Ready" : "Working", color: .green)
						case .some(false): StatusPill(text: "Stopped", color: .red)
						case .none: ProgressView().controlSize(.small)
						}
						Button(w.restarting ? "Restarting…" : "Restart") { w.restart() }.buttonStyle(.link).disabled(w.restarting)
					}
				}
				if let c = w.stats?.compute {
					Divider().padding(.leading, 16)
					Row(label: "This Mac now") {
						Text([c.cpu_percent.map { "CPU \(Int($0))%" }, c.memory.map { "memory \(Int($0.percent))%" }, c.disk.map { "\(Int($0.free_bytes / 1e9)) GB free" }].compactMap { $0 }.joined(separator: " · "))
							.foregroundStyle(.secondary)
					}
				}
			}

			if let j = w.jobs {
				HStack(spacing: 12) {
					stat("\(j.today.jobs)", "jobs today")
					stat("\(j.today.done)", "done")
					stat("\(j.today.failed)", "failed", j.today.failed > 0 ? .red : .primary)
					stat(dur(Double(j.today.seconds)), "of work")
				}

				if !j.running.isEmpty {
					Text("Working now").font(.system(size: 15, weight: .semibold)).padding(.top, 6)
					Card { ForEach(j.running) { job in jobRow(job); if job.id != j.running.last?.id { Divider().padding(.leading, 16) } } }
				}

				Text("Recent jobs").font(.system(size: 15, weight: .semibold)).padding(.top, 6)
				if j.jobs.isEmpty {
					Card { Row(label: "No jobs yet", note: "Send one: Ask Compute Pool, or `cerver run --on this-mac \"…\"` from another computer.") { EmptyView() } }
				} else {
					Card { ForEach(j.jobs) { job in jobRow(job); if job.id != j.jobs.last?.id { Divider().padding(.leading, 16) } } }
				}
			} else if w.up == true {
				Text("This relay doesn't keep a job list yet: update it (Updates).").font(.callout).foregroundStyle(.secondary)
			}

			DisclosureGroup(isExpanded: $showLog) {
				ScrollView {
					Text(w.log.map { clean($0.line) }.joined(separator: "\n"))
						.font(.system(size: 11, design: .monospaced))
						.textSelection(.enabled)
						.frame(maxWidth: .infinity, alignment: .leading)
						.padding(10)
				}
				.frame(height: 260)
				.background(RoundedRectangle(cornerRadius: 10).fill(Color(nsColor: .textBackgroundColor)))
			} label: {
				Text("What the relay prints").font(.system(size: 15, weight: .semibold))
			}
			.padding(.top, 6)
		}
		.onAppear { w.start() }
		.onDisappear { w.stop() }
	}

	func stat(_ v: String, _ label: String, _ color: Color = .primary) -> some View {
		VStack(alignment: .leading, spacing: 2) {
			Text(v).font(.system(size: 22, weight: .semibold)).foregroundStyle(color)
			Text(label).font(.callout).foregroundStyle(.secondary)
		}
		.frame(maxWidth: .infinity, alignment: .leading)
		.padding(12)
		.background(RoundedRectangle(cornerRadius: 12).fill(Color(nsColor: .controlBackgroundColor)))
	}

	func jobRow(_ j: RelayJob) -> some View {
		HStack(alignment: .firstTextBaseline, spacing: 10) {
			Image(systemName: j.status == "running" ? "circle.dotted" : (j.status == "done" ? "checkmark.circle.fill" : "xmark.circle.fill"))
				.foregroundStyle(j.status == "running" ? .blue : (j.status == "done" ? .green : .red))
			VStack(alignment: .leading, spacing: 2) {
				Text((j.task ?? "").isEmpty ? "(no text)" : j.task!).lineLimit(2)
				Text([ago(j.at), j.cli.flatMap { $0.isEmpty ? nil : $0 }, j.from.flatMap { $0.isEmpty ? nil : "from " + $0 }, j.project.flatMap { $0.isEmpty ? nil : $0 }, j.error].compactMap { $0 }.joined(separator: " · "))
					.font(.system(size: 11.5)).foregroundStyle(.secondary).lineLimit(1)
			}
			Spacer()
			Text(j.status == "running" ? dur(Date().timeIntervalSince1970 - j.at) : (j.seconds.map(dur) ?? "")).font(.system(size: 12)).foregroundStyle(.secondary)
		}
		.padding(.horizontal, 16).padding(.vertical, 10)
	}
}
