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
		checkUpdates()
		refreshAccount()
		timer = Timer.scheduledTimer(withTimeInterval: 2, repeats: true) { _ in
			Task { @MainActor in
				self.refresh()
				self.refreshRelay()
				self.refreshPool()
				self.checkUpdates()
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
			// 3. uv, which runs the relay.
			if shell("command -v uvx || ls ~/.local/bin/uvx").isEmpty {
				await MainActor.run { self.connecting = "Installing uv (runs the relay)…" }
				_ = shell("curl -LsSf https://astral.sh/uv/install.sh | sh")
			}
			// 4. The pool.
			await MainActor.run { self.connecting = "Joining your pool…" }
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

	var updateAvailable: Bool { updates?.any ?? false }

	/// Asks `cerver update --check --json` what's installed and what's new.
	/// On launch and every 6 hours (GitHub is asked at most that often).
	func checkUpdates(force: Bool = false) {
		if !force && Date().timeIntervalSince(lastUpdateCheck) < 6 * 3600 { return }
		lastUpdateCheck = Date()
		Task.detached {
			let (ok, out) = runCerver(["update", "--check", "--json"])
			let info = ok ? try? JSONDecoder().decode(UpdateInfo.self, from: Data(out.utf8)) : nil
			await MainActor.run { if let i = info { self.updates = i } }
		}
	}

	/// Updates everything, then reopens the (new) app.
	func updateNow() {
		updating = true
		updateProblem = nil
		Task.detached {
			let (ok, out) = runCerver(["update", "--app"])
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
					p.arguments = ["-c", "sleep 1; open /Applications/Cerver.app"]
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

func openWeb(_ path: String) {
	if let u = URL(string: "https://cerver.ai" + path) { NSWorkspace.shared.open(u) }
}

/// GET from the Cerver gateway with your key from ~/.cerver/cerver.env.
func gatewayGET(_ path: String) async -> Data? { await gatewayRequest("GET", path) }

func gatewayRequest(_ method: String, _ path: String) async -> Data? {
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
	case map, pool, permissions, thisMac, computer(String), add, settings, session
}

struct MainWindow: View {
	@ObservedObject var m: Model
	@StateObject private var session = SessionModel()
	// `-startPage thisMac` on launch opens that page (for screenshots).
	@State private var page: Page? = ["thisMac": Page.thisMac, "add": Page.add, "settings": Page.settings, "session": Page.session, "pool": Page.pool, "permissions": Page.permissions][UserDefaults.standard.string(forKey: "startPage") ?? ""] ?? .map

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
		.onChange(of: m.computers.map(\.id)) { ids in
			// The computer on screen left: go back to the list.
			if case .computer(let id) = page, !ids.contains(id) { page = .pool }
		}
	}

	// MARK: Sidebar

	var sidebar: some View {
		VStack(spacing: 0) {
			// The logo takes you to the map: the first page.
			Button { page = .map } label: {
				HStack(spacing: 9) {
					Image(nsImage: logoImage(size: 22))
						.clipShape(RoundedRectangle(cornerRadius: 5))
					Text("Cerver").font(.system(size: 15, weight: .semibold))
					Spacer()
				}
				.contentShape(Rectangle())
			}
			.buttonStyle(.plain)
			.help("Map")
			.padding(.horizontal, 16)
			.padding(.top, 6)
			.padding(.bottom, 10)

			List(selection: $page) {
				Label("Map", systemImage: "map")
					.tag(Page.map)
				Section("Cerver Compute") {
					Label("Ask your computers", systemImage: "bubble.left.and.text.bubble.right")
						.tag(Page.session)
					Label("Your pool", systemImage: "server.rack")
						.tag(Page.pool)
					ForEach(m.computers.filter { !$0.isPhone }) { c in
						sidebarRow(name: c.name, dot: c.isThisMac ? thisMacDot : computerDot(c), icon: c.icon, you: c.isThisMac)
							.padding(.leading, 14)
							.tag(c.isThisMac ? Page.thisMac : Page.computer(c.id))
					}
					if !m.computers.contains(where: { $0.isThisMac }) {
						sidebarRow(name: m.relayName, dot: thisMacDot, icon: "laptopcomputer", you: true)
							.padding(.leading, 14)
							.tag(Page.thisMac)
					}
					Label("Add a computer", systemImage: "plus")
						.foregroundStyle(.secondary)
						.padding(.leading, 14)
						.tag(Page.add)
				}
			}
			.listStyle(.sidebar)
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
			Button { page = .settings } label: {
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
			.help("Settings")
			Spacer(minLength: 0)

			if m.updateAvailable {
				Button {
					m.updateNow()
				} label: {
					if m.updating { ProgressView().controlSize(.mini) } else { Text("Update").font(.system(size: 11, weight: .semibold)) }
				}
				.buttonStyle(.borderedProminent).controlSize(.small).clipShape(Capsule())
				.disabled(m.updating)
				.help("Cerver \(m.updates?.latest ?? "") is out")
			}
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
				Text("Sign in in your browser. Then this Mac joins your pool, so agents can run on it, and your network, so your computers reach it.")
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
			switch page ?? .map {
			case .map: MapPage(m: m, open: { page = $0 })
			case .pool: PoolPage(m: m, open: { page = $0 })
			case .permissions: PermissionsPage(m: m)
			case .thisMac: ThisMacPage(m: m)
			case .computer(let id):
				if let c = m.computers.first(where: { $0.id == id }) { ComputerPage(m: m, c: c) } else { PoolPage(m: m, open: { page = $0 }) }
			case .add: AddPage(m: m)
			case .settings: SettingsPage(m: m)
			case .session: SessionPage(m: m, s: session)
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
	let open: (Page) -> Void

	var body: some View {
		PageLayout(title: "Map", subtitle: "Your agents, the pool they run in, and the network under it. Click anything to open it.") {
			if m.computers.isEmpty {
				Text(m.poolChecked ? "No computers yet." : "Loading…").foregroundStyle(.secondary)
			} else {
				NetworkMap(m: m, open: open)
			}
		}
	}
}

/// Your pool: every computer agents can run on, and how the pool is run.
struct PoolPage: View {
	@ObservedObject var m: Model
	let open: (Page) -> Void
	@State private var view = "Computers"
	@State private var help = false
	@State private var info: PoolInfo?
	@State private var copied = false

	var body: some View {
		PageLayout(title: info?.name ?? "Your pool", subtitle: "Every computer your agents can run on, and who can use them.") {
			HStack {
				Picker("", selection: $view) {
					Text("Computers").tag("Computers")
					Text("Coding agents").tag("Coding agents")
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
			} else {
				Card {
					Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 0) {
						GridRow {
							header("Computer")
							header("Agents")
							header("Network")
						}
						.padding(.horizontal, 16).padding(.vertical, 10)
						ForEach(m.computers) { c in
							Divider().gridCellColumns(3)
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
								StatusPill(text: networkStatus(c, peer: peer(c)).0.hasPrefix("On") ? "On" : "Off", color: networkStatus(c, peer: peer(c)).1)
							}
							.padding(.horizontal, 16).padding(.vertical, 9)
							.contentShape(Rectangle())
							.onTapGesture { open(c.isThisMac ? .thisMac : .computer(c.id)) }
						}
					}
				}
			}
			HStack(spacing: 18) {
				Button { open(.permissions) } label: { Label("Permissions", systemImage: "lock.shield") }.buttonStyle(.link)
				Button { open(.add) } label: { Label("Add a computer", systemImage: "plus") }.buttonStyle(.link)
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
			}
			Text("Your computers connect directly, computer to computer, and use Cerver's backup route only when a direct connection can't be made.")
				.font(.callout).foregroundStyle(.secondary)

			section("Cloud computers")
			Card {
				if let i = info {
					ForEach(Array(i.cloud.enumerated()), id: \.offset) { n, p in
						if n > 0 { Divider().padding(.leading, 16) }
						Row(label: p.name) {
							Text(p.connected ? (p.yours ? "On your own account" : "Free first computers") : "Not connected").foregroundStyle(.secondary)
							Button(p.connected && p.yours ? "Manage" : "Connect") { openWeb("/dashboard/connectors") }
						}
					}
				} else {
					Row(label: "Loading…") { ProgressView().controlSize(.small) }
				}
			}
			Text("Connect your own E2B, Daytona, Modal or Vercel account and agents can start computers there. The provider bills you at its own price; Cerver adds nothing.")
				.font(.callout).foregroundStyle(.secondary)

			section("People")
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
			Text("People you invite can run agents only on computers you allow, in Permissions → Advanced.")
				.font(.callout).foregroundStyle(.secondary)
		}
		.task { await load() }
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
	case overview = "Overview", agents = "Agents", network = "Network", software = "Software", hardware = "Hardware", settings = "Settings"
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
	// `-startTab Settings` opens that tab (for screenshots).
	@State private var tab: Tab = Tab(rawValue: UserDefaults.standard.string(forKey: "startTab") ?? "") ?? .overview
	@State private var editing = false
	@State private var newName = ""

	var me: Computer? { m.computers.first(where: { $0.isThisMac }) }

	var body: some View {
		PageLayout(title: m.relayName, subtitle: "This Mac") {
			Tabs(tab: $tab, tabs: [.overview, .agents, .network, .software, .hardware])
			if case .failed(let why) = m.phase { problem(why) }
			if case .failed(let why) = m.relay { problem(why) }
			switch tab {
			case .overview: overview
			case .agents: agents
			case .network: network
			case .software: SoftwareSection(spec: me?.spec)
			case .hardware: SpecSection(spec: me?.spec, showTitle: false)
			case .settings: EmptyView()
			}
		}
	}

	var overview: some View {
		Card {
			Row(label: "Agents can run here", note: "Your agents may run on it, as you.") {
				StatusPill(text: relayText.0, color: relayText.1)
				if relayWorking { ProgressView().controlSize(.small) }
				Toggle("", isOn: Binding(get: { relayOn }, set: { $0 ? m.relayOn() : m.relayOff() }))
					.toggleStyle(.switch).labelsHidden().disabled(relayWorking)
			}
			Divider().padding(.leading, 16)
			Row(label: "Your computers can reach it", note: "Your other computers can connect to it privately.") {
				StatusPill(text: m.isOn ? "On" : (m.busy ? "…" : "Off"), color: m.isOn ? .green : .gray)
				if m.busy { ProgressView().controlSize(.small) }
				Toggle("", isOn: Binding(get: { m.isOn || m.phase == .starting }, set: { $0 ? m.turnOn() : m.turnOff() }))
					.toggleStyle(.switch).labelsHidden().disabled(m.busy)
			}
			if let me = me, let p = me.pool {
				Divider().padding(.leading, 16)
				Row(label: "People you invite can use it", note: "Invited people's agents may run on it, fenced off from your files.") {
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
			Tabs(tab: $tab, tabs: [.overview, .agents, .network, .software, .hardware])
			switch tab {
			case .overview: overview
			case .agents: agents
			case .network: network
			case .software: SoftwareSection(spec: c.spec)
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
			Row(label: "People you invite can use it", note: "Invited people's agents may run on it, fenced off from your files.") { StatusPill(text: sharedStatus(c).0, color: sharedStatus(c).1) }
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
			Text("2.  It asks for that computer's password once, then shows up here within a few seconds. Reach it by its name.")
				.font(.system(size: 14))
			Text("It signs in with your Cerver account, so only your computers join.")
				.font(.callout).foregroundStyle(.secondary)

		}
	}
}

/// What each computer allows, one permission per card, each explained.
struct PermissionsPage: View {
	@ObservedObject var m: Model
	@State private var advanced = false

	var computers: [Computer] { m.computers.filter { !$0.isPhone } }

	var body: some View {
		PageLayout(title: "Permissions", subtitle: "What each of your computers allows.") {
			permission("Agents can run here",
				"Your agents may run on it, as you.") { c in
				cell(on: c.isThisMac ? (m.relay == .running || m.relay == .starting) : c.pool?.online == true,
					enabled: c.isThisMac && !relayBusy, remote: !c.isThisMac) { $0 ? m.relayOn() : m.relayOff() }
			}
			permission("Your computers can reach it",
				"Your other computers can connect to it privately.") { c in
				cell(on: c.isThisMac ? (m.isOn || m.phase == .starting) : c.net?.online == true,
					enabled: c.isThisMac && !m.busy, remote: !c.isThisMac) { $0 ? m.turnOn() : m.turnOff() }
			}

			DisclosureGroup(isExpanded: $advanced) {
				VStack(alignment: .leading, spacing: 14) {
					permission("People you invite can use it",
						"Invited people's agents may run on it, fenced off from your files.") { c in
						cell(on: c.pool?.shared == true,
							enabled: c.pool?.compute_id.map { !m.sharing.contains($0) } ?? false, remote: false) { m.setShared(c, $0) }
					}
				}
				.padding(.top, 10)
			} label: {
				Text("Advanced").font(.system(size: 15, weight: .semibold))
			}
		}
	}

	var relayBusy: Bool { if case .working = m.relay { return true }; return false }

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

/// Settings: your account, what this computer is part of, updates, and the rest.
struct SettingsPage: View {
	@ObservedObject var m: Model

	var body: some View {
		PageLayout(title: "Settings", subtitle: m.email.map { "Signed in as " + $0 } ?? "Not signed in") {
			Text("This computer").font(.system(size: 15, weight: .semibold))
			Card {
				Row(label: "Agents can run here", note: "Your agents may run on it, as you.") {
					Toggle("", isOn: Binding(get: { m.relay == .running || m.relay == .starting }, set: { $0 ? m.relayOn() : m.relayOff() }))
						.toggleStyle(.switch).labelsHidden()
				}
				Divider().padding(.leading, 16)
				Row(label: "Your computers can reach it", note: "Your other computers can connect to it privately.") {
					Text(m.isOn ? networkSummary : "Off").font(.callout).foregroundStyle(.secondary)
					Toggle("", isOn: Binding(get: { m.isOn || m.phase == .starting }, set: { $0 ? m.turnOn() : m.turnOff() }))
						.toggleStyle(.switch).labelsHidden().disabled(m.busy)
				}
			}
			rest
		}
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

	@ViewBuilder var rest: some View {
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
				Row(label: u.any ? "An update is available" : (u.latest.isEmpty ? "Couldn't check for updates" : "Up to date")) {
					if m.updating { ProgressView().controlSize(.small) }
					if u.any {
						Button("Update now") { m.updateNow() }.buttonStyle(.borderedProminent).disabled(m.updating)
					} else {
						Button("Check again") { m.checkUpdates(force: true) }.buttonStyle(.link)
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

		Text("This Mac").font(.system(size: 15, weight: .semibold)).padding(.top, 6)
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
			Text("This Mac leaves your pool and network. Its sign-in, services, files and this app are deleted. You'll be asked for your password once.")
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
			Text("Ask your computers").font(.system(size: 17, weight: .semibold))
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
			Text("An agent (\(s.cli == "codex" ? "Codex" : "Claude"), with your login) answers on that computer, as you, so it can look and act there. Try: what's using the disk? · is Node installed? · update Homebrew.")
				.foregroundStyle(.secondary)
		}
		.padding(.top, 30)
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
			TextField(target == nil ? "No computer is ready" : "Ask \(target?.name ?? "it")…", text: $draft, axis: .vertical)
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
	let open: (Page) -> Void

	let height: CGFloat = 560
	let agentsY: CGFloat = 62, computeY: CGFloat = 188, nodesY: CGFloat = 420
	let band1: CGFloat = 120, band2: CGFloat = 255  // where each layer ends

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
				let compute = CGPoint(x: w / 2, y: computeY)
				let backup = CGPoint(x: w - 92, y: band2 + 44)
				let pos: [String: CGPoint] = Dictionary(uniqueKeysWithValues: ordered.enumerated().map { i, c in
					(c.id, CGPoint(x: (w - 60) * CGFloat(i + 1) / CGFloat(ordered.count + 1) + 10, y: nodesY))
				})
				ZStack {
					// The three layers.
					VStack(spacing: 0) {
						band("CERVER AGENTS", "Agents running for you", height: band1, tint: .secondary)
						band("CERVER COMPUTE", "Your pool: where each agent run goes", height: band2 - band1, tint: .secondary)
						band("YOUR NETWORK", "Your computers and how they reach each other", height: height - band2, tint: .secondary)
					}

					Canvas { ctx, _ in
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
							path.addCurve(to: CGPoint(x: p.x, y: p.y - 46),
								control1: CGPoint(x: compute.x, y: compute.y + 110), control2: CGPoint(x: p.x, y: p.y - 140))
							ctx.stroke(path, with: .color(pool.online ? Color.secondary.opacity(0.6) : Color.secondary.opacity(0.3)),
								style: StrokeStyle(lineWidth: 1.5, dash: pool.online ? [] : [3, 4]))
						}
						guard let me = me, let mp = pos[me.id] else { return }
						// This computer's standby line to the network backup.
						if m.status?.relay_connected == true {
							var path = Path()
							path.move(to: CGPoint(x: mp.x + 40, y: mp.y - 46))
							path.addQuadCurve(to: CGPoint(x: backup.x - 60, y: backup.y + 8), control: CGPoint(x: mp.x + 60, y: backup.y + 20))
							ctx.stroke(path, with: .color(Color.secondary.opacity(0.45)), style: StrokeStyle(lineWidth: 1.5, dash: [5, 4]))
						}
						for c in others {
							guard let p = pos[c.id], let pr = peer(c), pr.connected else { continue }
							if pr.via == "relay" {
								var path = Path()
								path.move(to: CGPoint(x: p.x, y: p.y - 46))
								path.addQuadCurve(to: CGPoint(x: backup.x, y: backup.y + 16), control: CGPoint(x: p.x, y: backup.y + 40))
								ctx.stroke(path, with: .color(Color.secondary), style: StrokeStyle(lineWidth: 2.5, dash: [6, 4]))
							} else {
								var path = Path()
								let a = CGPoint(x: mp.x, y: mp.y + 46), b = CGPoint(x: p.x, y: p.y + 46)
								path.move(to: a)
								path.addQuadCurve(to: b, control: CGPoint(x: (a.x + b.x) / 2, y: a.y + 70))
								ctx.stroke(path, with: .color(Color.primary.opacity(0.7)), style: StrokeStyle(lineWidth: 3, lineCap: .round))
							}
						}
					}

					box(icon: "sparkles", title: "Agents", tint: .secondary,
						line: running == 0 ? "none running now" : "\(running) running now")
						.position(agents)
					box(icon: "server.rack", title: "Your pool", tint: .secondary,
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
									.position(x: (mp.x + p.x) / 2, y: nodesY + 46 + 35)
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
		}
		.padding(.horizontal, 8)
		.frame(width: 170, height: 86)
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
