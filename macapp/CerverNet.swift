// Cerver Net — the menu bar app for `cerver net`.
//
// It doesn't run the network itself: the system service (launchd
// ai.cerver.net, the bundled `cerver` binary) does. The app turns that
// service on and off through the normal macOS password prompt, and shows
// what it's doing by reading /var/run/cerver-net.json, which the service
// rewrites every few seconds.
//
// Build: ./build.sh  →  build/Cerver Net.app

import AppKit
import ServiceManagement
import SwiftUI

let statusFile = "/var/run/cerver-net.json"
let logFile = "/var/log/cerver-net.log"

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

	private var timer: Timer?
	private var wantOnUntil: Date?

	var deviceName: String {
		get { UserDefaults.standard.string(forKey: "name") ?? defaultName() }
		set { UserDefaults.standard.set(newValue, forKey: "name") }
	}

	init() {
		refresh()
		timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { _ in
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
			deviceName = s.name
		}
		status = fresh
		if case .working = phase { return }
		if fresh != nil {
			phase = .on
			wantOnUntil = nil
		} else if let until = wantOnUntil, Date() < until {
			phase = .starting
		} else if case .failed = phase {
			// keep showing the error until the next action
		} else {
			phase = signedIn ? .off : .notSignedIn
		}
	}

	func turnOn() {
		guard let bin = Bundle.main.path(forResource: "cerver", ofType: nil) else {
			phase = .failed("The app is missing its cerver program; reinstall it.")
			return
		}
		let cmd = "SUDO_USER=\(shq(NSUserName())) \(shq(bin)) net install --name \(shq(deviceName))"
		runAsAdmin(cmd, doing: "Turning on…") { ok in
			if ok { self.wantOnUntil = Date().addingTimeInterval(45) }
		}
	}

	func turnOff() {
		let bin = FileManager.default.fileExists(atPath: "/usr/local/lib/cerver/cerver")
			? "/usr/local/lib/cerver/cerver" : (Bundle.main.path(forResource: "cerver", ofType: nil) ?? "")
		runAsAdmin("SUDO_USER=\(shq(NSUserName())) \(shq(bin)) net uninstall", doing: "Turning off…") { _ in }
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
}

func shq(_ s: String) -> String { "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'" }

func defaultName() -> String {
	let raw = (Host.current().localizedName ?? ProcessInfo.processInfo.hostName).lowercased()
	let cleaned = raw.map { $0.isLetter || $0.isNumber ? $0 : "-" }
	let s = String(cleaned).split(separator: "-").joined(separator: "-")
	return s.isEmpty ? "mac" : s
}

func cleanError(_ s: String) -> String {
	// osascript wraps the command's error: "...: execution error: <text> (1)"
	var t = s.trimmingCharacters(in: .whitespacesAndNewlines)
	if let r = t.range(of: "execution error: ") { t = String(t[r.upperBound...]) }
	if let r = t.range(of: " (", options: .backwards) { t = String(t[..<r.lowerBound]) }
	return t.isEmpty ? "Something went wrong." : t
}

func copy(_ s: String) {
	NSPasteboard.general.clearContents()
	NSPasteboard.general.setString(s, forType: .string)
}

func openSSH(_ host: String) {
	let script = "tell application \"Terminal\" to do script \"ssh \(host)\"\ntell application \"Terminal\" to activate"
	let p = Process()
	p.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
	p.arguments = ["-e", script]
	try? p.run()
}

struct MenuView: View {
	@ObservedObject var m: Model

	var body: some View {
		switch m.phase {
		case .notSignedIn:
			Text("Cerver net: sign in first")
			Button("Get Cerver…") { NSWorkspace.shared.open(URL(string: "https://cerver.ai")!) }
			Text("Then run its installer, and come back.")
		case .off:
			Text("Cerver net is off")
			Button("Turn On") { m.turnOn() }
		case .starting:
			Text("Starting…")
		case .working(let what):
			Text(what)
		case .failed(let why):
			Text("Cerver net: couldn't do that")
			Text(why)
			Button("Turn On") { m.turnOn() }
		case .on:
			if let s = m.status {
				Text("This Mac: \(s.name) · \(s.ip)")
				Divider()
				if s.peers.isEmpty {
					Text("No other devices yet")
				} else {
					ForEach(s.peers) { p in
						Menu("\(p.connected ? "●" : "○")  \(p.name)   \(label(p))") {
							Button("Copy Name (\(p.name))") { copy(p.name) }
							Button("Copy Address (\(p.ip))") { copy(p.ip) }
							Button("Open SSH in Terminal") { openSSH(p.name) }
						}
					}
				}
				Divider()
				Button("Turn Off") { m.turnOff() }
			}
		}
		Divider()
		Button("Show Log") { NSWorkspace.shared.open(URL(fileURLWithPath: logFile)) }
		Toggle("Open at Login", isOn: Binding(get: { m.openAtLogin }, set: { m.setOpenAtLogin($0) }))
		Button("Quit") { NSApp.terminate(nil) }
	}

	func label(_ p: NetPeer) -> String {
		if !p.connected { return "not reachable" }
		return p.via == "relay" ? "via Cerver relay" : "direct"
	}
}

@main
struct CerverNetApp: App {
	@StateObject private var m = Model()

	var body: some Scene {
		MenuBarExtra {
			MenuView(m: m)
		} label: {
			Image(systemName: m.phase == .on ? "point.3.filled.connected.trianglepath.dotted" : "point.3.connected.trianglepath.dotted")
		}
		.menuBarExtraStyle(.menu)
	}
}
