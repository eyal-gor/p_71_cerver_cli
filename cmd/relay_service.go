package cmd

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// `cerver relay install` keeps this computer in your Cerver pool all the
// time: the relay (p_69, run with uvx) starts with the computer and restarts
// itself if it stops. Unlike `cerver net`, it runs as you, not as root: the
// agents it starts use your Claude/Codex logins and your files.
//
//	macOS: ~/Library/LaunchAgents/ai.cerver.relay.plist
//	Linux: ~/.config/systemd/user/cerver-relay.service (+ linger, so it runs without a login)
//
// The relay reads its sign-in from ~/.cerver/cerver.env itself, so the service
// holds no keys.

const (
	relayLabel     = "ai.cerver.relay"
	relayRepo      = "git+https://github.com/eyal-gor/p_69_cerver_relay.git"
	relayOldLabel  = "dev.cerver.relay" // set up by hand before this command existed
	relayLinuxUnit = "cerver-relay.service"
)

func Relay(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "install":
		return relayInstall(args[1:])
	case "uninstall":
		return relayUninstall()
	case "", "status":
		return relayStatus()
	default:
		return fmt.Errorf("unknown: cerver relay %s (try: install, uninstall, status)", sub)
	}
}

func home() string { h, _ := os.UserHomeDir(); return h }

func relayLog() string { return filepath.Join(home(), ".cerver", "relay.log") }
func relayPlist() string {
	return filepath.Join(home(), "Library", "LaunchAgents", relayLabel+".plist")
}
func relayOldPlist() string {
	return filepath.Join(home(), "Library", "LaunchAgents", relayOldLabel+".plist")
}
func relayUnitPath() string {
	return filepath.Join(home(), ".config", "systemd", "user", relayLinuxUnit)
}
func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func relayInstalled() bool  { _, err := os.Stat(relayServiceFile()); return err == nil }
func relayServiceFile() string {
	switch runtime.GOOS {
	case "darwin":
		return relayPlist()
	case "windows":
		return winRelayFile()
	}
	return relayUnitPath()
}

func findUVX() string {
	if p, err := exec.LookPath("uvx"); err == nil {
		return p
	}
	for _, p := range []string{
		filepath.Join(home(), ".local", "bin", "uvx"+exeSuffix()),
		filepath.Join(home(), ".cargo", "bin", "uvx"+exeSuffix()),
		"/opt/homebrew/bin/uvx",
		"/usr/local/bin/uvx",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// readEnvFile reads KEY=VALUE lines (no values are printed anywhere).
func readEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && !strings.HasPrefix(k, "#") {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}

// relayDefaultName keeps the name this computer already has in the pool.
func relayDefaultName() string {
	for _, p := range []string{relayPlist(), relayOldPlist()} {
		if b, err := exec.Command("plutil", "-extract", "ProgramArguments", "json", "-o", "-", p).Output(); err == nil {
			s := string(b)
			if i := strings.Index(s, `"--name","`); i >= 0 {
				rest := s[i+len(`"--name","`):]
				if j := strings.Index(rest, `"`); j > 0 {
					return rest[:j]
				}
			}
		}
	}
	if b, err := os.ReadFile(relayUnitPath()); err == nil {
		if i := strings.Index(string(b), "--name "); i >= 0 {
			if f := strings.Fields(string(b)[i+len("--name "):]); len(f) > 0 {
				return strings.Trim(f[0], `'"`)
			}
		}
	}
	if b, err := os.ReadFile(winRelayFile()); err == nil {
		var s winRelaySpec
		if json.Unmarshal(b, &s) == nil && s.Name != "" {
			return s.Name
		}
	}
	if n := os.Getenv("CERVER_MACHINE_NAME"); n != "" {
		return n
	}
	h, _ := os.Hostname()
	return strings.TrimSuffix(h, ".local")
}

// relayCommand is what the service runs: the relay from its repo (always the
// latest on each start), wrapped in `infisical run` when install.sh set up
// Infisical through its CLI login (then the secrets come from there).
func relayCommand(uvx, name string) (args []string, dir string) {
	gw := os.Getenv("CERVER_GATEWAY_URL")
	if gw == "" {
		gw = readEnvFile(filepath.Join(home(), ".cerver", "cerver.env"))["CERVER_GATEWAY_URL"]
	}
	if gw == "" {
		gw = "https://gateway.cerver.ai"
	}
	args = []string{uvx, "--refresh", "--from", relayRepo, "cerver-relay", "--cerver-only", "--no-tui", "--cerver-url", gw, "--name", name}
	dir = home()
	inf := readEnvFile(filepath.Join(home(), ".cerver", "infisical.env"))
	infBin := filepath.Join(home(), ".cerver", "bin", "infisical")
	if inf["INFISICAL_LOGIN_VIA"] == "cli" {
		if _, err := os.Stat(infBin); err == nil {
			env := inf["INFISICAL_ENV"]
			if env == "" {
				env = "dev"
			}
			args = append([]string{infBin, "run", "--env=" + env, "--"}, args...)
			dir = filepath.Join(home(), ".cerver") // where `infisical init` left .infisical.json
		}
	}
	return args, dir
}

func relayInstall(args []string) error {
	fs := flag.NewFlagSet("relay install", flag.ContinueOnError)
	name := fs.String("name", relayDefaultName(), "This computer's name in your pool")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		return fmt.Errorf("run this without sudo: the relay runs as you, with your logins and files")
	}
	if _, err := os.Stat(filepath.Join(home(), ".cerver", "cerver.env")); err != nil {
		return fmt.Errorf("not signed in to Cerver on this computer: run curl -fsSL https://cerver.ai/install.sh | sh first")
	}
	uvx := findUVX()
	if uvx == "" {
		return fmt.Errorf("the relay needs uv: curl -LsSf https://astral.sh/uv/install.sh | sh, then run this again")
	}
	cmdArgs, dir := relayCommand(uvx, *name)
	os.MkdirAll(filepath.Dir(relayLog()), 0o700)
	path := os.Getenv("PATH") // the agents it starts (claude, codex) must be found the same way as in your terminal

	switch runtime.GOOS {
	case "darwin":
		// One relay per computer: retire the hand-made service and any copy
		// install.sh left running in a terminal.
		exec.Command("launchctl", "bootout", launchdDomain()+"/"+relayOldLabel).Run()
		os.Remove(relayOldPlist())
		stopStrayRelay()
		exec.Command("launchctl", "bootout", launchdDomain()+"/"+relayLabel).Run()
		waitGone(launchdDomain() + "/" + relayLabel)
		os.MkdirAll(filepath.Dir(relayPlist()), 0o755)
		if err := os.WriteFile(relayPlist(), []byte(relayLaunchdPlist(cmdArgs, dir, path)), 0o644); err != nil {
			return err
		}
		var last error
		for i := 0; i < 5; i++ {
			out, err := exec.Command("launchctl", "bootstrap", launchdDomain(), relayPlist()).CombinedOutput()
			if err == nil {
				last = nil
				break
			}
			last = fmt.Errorf("launchctl: %v: %s", err, strings.TrimSpace(string(out)))
			time.Sleep(time.Second)
		}
		if last != nil {
			return last
		}
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return fmt.Errorf("no systemd here; run the relay in a terminal instead (see cerver.ai/install.sh)")
		}
		stopStrayRelay()
		os.MkdirAll(filepath.Dir(relayUnitPath()), 0o755)
		if err := os.WriteFile(relayUnitPath(), []byte(relaySystemdUnit(cmdArgs, dir, path)), 0o644); err != nil {
			return err
		}
		for _, c := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", relayLinuxUnit}, {"--user", "restart", relayLinuxUnit}} {
			if out, err := exec.Command("systemctl", c...).CombinedOutput(); err != nil {
				return fmt.Errorf("systemctl %s: %v: %s", strings.Join(c, " "), err, strings.TrimSpace(string(out)))
			}
		}
		// Without linger, user services stop when you log out.
		if err := exec.Command("loginctl", "enable-linger").Run(); err != nil {
			fmt.Printf("To keep it running after you log out: sudo loginctl enable-linger %s\n", os.Getenv("USER"))
		}
	case "windows":
		if err := winRelayInstall(cmdArgs, dir, *name); err != nil {
			return err
		}
	default:
		return fmt.Errorf("the relay service runs on macOS, Linux and Windows")
	}
	if err := updaterInstall(); err != nil {
		fmt.Printf("Couldn't set up automatic updates (%v); update by hand with: cerver update\n", err)
	}
	fmt.Printf("%s now stays in your Cerver pool, also after restarts, and updates itself.\n", *name)
	fmt.Printf("Log: %s   ·   Stop: cerver relay uninstall\n", relayLog())
	return nil
}

func relayUninstall() error {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("launchctl", "bootout", launchdDomain()+"/"+relayLabel).Run()
		os.Remove(relayPlist())
	case "linux":
		exec.Command("systemctl", "--user", "disable", "--now", relayLinuxUnit).Run()
		os.Remove(relayUnitPath())
		exec.Command("systemctl", "--user", "daemon-reload").Run()
	case "windows":
		winRelayUninstall()
	}
	fmt.Println("The relay stopped. This computer left your pool until you install it again.")
	return nil
}

func relayStatus() error {
	if !relayInstalled() {
		fmt.Println("○ The relay isn't installed as a service here. Start it: cerver relay install")
		return nil
	}
	if relayRunning() {
		fmt.Printf("● The relay is running and starts with this computer. Log: %s\n", relayLog())
	} else {
		fmt.Printf("◐ Installed but not running right now (it retries on its own). Log: %s\n", relayLog())
	}
	return nil
}

func relayRunning() bool {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "print", launchdDomain()+"/"+relayLabel).Output()
		return err == nil && strings.Contains(string(out), "state = running")
	case "linux":
		return exec.Command("systemctl", "--user", "is-active", "--quiet", relayLinuxUnit).Run() == nil
	case "windows":
		return relayAnswers()
	}
	return false
}

func waitGone(target string) {
	for i := 0; i < 20; i++ {
		if exec.Command("launchctl", "print", target).Run() != nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// stopStrayRelay stops a relay install.sh started in a terminal (its pid is
// in ~/.cerver/relay.pid), so two copies don't fight over the same machine.
func stopStrayRelay() {
	if runtime.GOOS == "windows" {
		return
	}
	b, err := os.ReadFile(filepath.Join(home(), ".cerver", "relay.pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return
	}
	out, _ := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if strings.Contains(string(out), "cerver-relay") {
		if p, err := os.FindProcess(pid); err == nil {
			p.Signal(os.Interrupt)
		}
	}
}

func relayLaunchdPlist(args []string, dir, path string) string {
	var a strings.Builder
	for _, s := range args {
		a.WriteString("\t\t<string>" + xmlEscape(s) + "</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + relayLabel + `</string>
	<key>ProgramArguments</key>
	<array>
` + a.String() + `	</array>
	<key>WorkingDirectory</key>
	<string>` + xmlEscape(dir) + `</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>` + xmlEscape(home()) + `</string>
		<key>PATH</key>
		<string>` + xmlEscape(path) + `</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(relayLog()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(relayLog()) + `</string>
</dict>
</plist>
`
}

func relaySystemdUnit(args []string, dir, path string) string {
	q := make([]string, len(args))
	for i, s := range args {
		q[i] = strconv.Quote(s)
	}
	return `[Unit]
Description=Cerver relay (this computer in your Cerver pool)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=` + strings.Join(q, " ") + `
WorkingDirectory=` + dir + `
Environment=PATH=` + path + `
Restart=always
RestartSec=10
StandardOutput=append:` + relayLog() + `
StandardError=append:` + relayLog() + `

[Install]
WantedBy=default.target
`
}
