package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// The background updater runs `cerver update --auto` every 6 hours (and once
// at login), as you. `cerver relay install` sets it up; `cerver uninstall`
// removes it.
//
//	macOS: ~/Library/LaunchAgents/ai.cerver.update.plist
//	Linux: ~/.config/systemd/user/cerver-update.{service,timer}

const (
	updaterLabel = "ai.cerver.update"
	updaterUnit  = "cerver-update"
)

func updaterLog() string { return filepath.Join(home(), ".cerver", "update.log") }
func updaterPlist() string {
	return filepath.Join(home(), "Library", "LaunchAgents", updaterLabel+".plist")
}
func updaterUnitDir() string {
	return filepath.Join(home(), ".config", "systemd", "user")
}

// cerverBin is the copy of cerver the services should run: the one install.sh
// put in ~/.cerver/bin (it's the one updates replace).
func cerverBin() string {
	p := filepath.Join(home(), ".cerver", "bin", "cerver")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	exe, _ := os.Executable()
	return exe
}

func updaterInstall() error {
	bin := cerverBin()
	switch runtime.GOOS {
	case "darwin":
		plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + updaterLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(bin) + `</string>
		<string>update</string><string>--auto</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key><string>` + xmlEscape(home()) + `</string>
		<key>PATH</key><string>` + xmlEscape(os.Getenv("PATH")) + `</string>
	</dict>
	<key>StartInterval</key><integer>21600</integer>
	<key>RunAtLoad</key><true/>
	<key>StandardOutPath</key><string>` + xmlEscape(updaterLog()) + `</string>
	<key>StandardErrorPath</key><string>` + xmlEscape(updaterLog()) + `</string>
</dict>
</plist>
`
		exec.Command("launchctl", "bootout", launchdDomain()+"/"+updaterLabel).Run()
		waitGone(launchdDomain() + "/" + updaterLabel)
		os.MkdirAll(filepath.Dir(updaterPlist()), 0o755)
		if err := os.WriteFile(updaterPlist(), []byte(plist), 0o644); err != nil {
			return err
		}
		if out, err := exec.Command("launchctl", "bootstrap", launchdDomain(), updaterPlist()).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl: %v: %s", err, out)
		}
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return nil
		}
		dir := updaterUnitDir()
		os.MkdirAll(dir, 0o755)
		svc := "[Unit]\nDescription=Cerver updates\n\n[Service]\nType=oneshot\nExecStart=" + bin + " update --auto\nEnvironment=PATH=" + os.Getenv("PATH") +
			"\nStandardOutput=append:" + updaterLog() + "\nStandardError=append:" + updaterLog() + "\n"
		timer := "[Unit]\nDescription=Check for Cerver updates every 6 hours\n\n[Timer]\nOnBootSec=10min\nOnUnitActiveSec=6h\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n"
		if err := os.WriteFile(filepath.Join(dir, updaterUnit+".service"), []byte(svc), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, updaterUnit+".timer"), []byte(timer), 0o644); err != nil {
			return err
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", updaterUnit+".timer").CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl: %v: %s", err, out)
		}
	}
	return nil
}

func updaterUninstall() {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("launchctl", "bootout", launchdDomain()+"/"+updaterLabel).Run()
		os.Remove(updaterPlist())
	case "linux":
		exec.Command("systemctl", "--user", "disable", "--now", updaterUnit+".timer").Run()
		os.Remove(filepath.Join(updaterUnitDir(), updaterUnit+".service"))
		os.Remove(filepath.Join(updaterUnitDir(), updaterUnit+".timer"))
		exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
}
