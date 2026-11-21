package cmd

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/eyal-gor/p_71_cerver_cli/internal/wgnet"
)

var wgnetWriteHosts = wgnet.WriteHosts

// `sudo cerver net install` keeps this device on your network all the time:
// it starts with the computer and restarts itself if it stops.
// `sudo cerver net uninstall` takes it off again.

const (
	svcLabel  = "ai.cerver.net"
	svcPlist  = "/Library/LaunchDaemons/ai.cerver.net.plist"
	svcUnit   = "/etc/systemd/system/cerver-net.service"
	svcBinary = "/usr/local/lib/cerver/cerver"
	svcLog    = "/var/log/cerver-net.log"
)

func netInstall(args []string) error {
	fs := flag.NewFlagSet("net install", flag.ContinueOnError)
	host, _ := os.Hostname()
	name := fs.String("name", strings.TrimSuffix(host, ".local"), "This device's name in your network")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("installing a system service needs admin rights: sudo cerver net install")
	}
	home := ""
	if su := os.Getenv("SUDO_USER"); su != "" {
		if u, err := user.Lookup(su); err == nil {
			home = u.HomeDir
		}
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if _, err := os.Stat(filepath.Join(home, ".cerver", "cerver.env")); err != nil {
		return fmt.Errorf("no Cerver credentials in %s/.cerver — run cerver.ai/install.sh first", home)
	}
	// Run a copy in a fixed place, so the service doesn't depend on where
	// this binary happens to be (temp folders get cleaned). Install again after
	// `cerver update` to move the service to the new version.
	if err := copySelf(svcBinary); err != nil {
		return fmt.Errorf("copying cerver to %s: %w", svcBinary, err)
	}

	switch runtime.GOOS {
	case "darwin":
		exec.Command("launchctl", "bootout", "system/"+svcLabel).Run()
		if err := os.WriteFile(svcPlist, []byte(launchdPlist(*name, home)), 0644); err != nil {
			return err
		}
		if out, err := exec.Command("launchctl", "bootstrap", "system", svcPlist).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl: %v: %s", err, strings.TrimSpace(string(out)))
		}
	case "linux":
		if err := os.WriteFile(svcUnit, []byte(systemdUnit(*name, home)), 0644); err != nil {
			return err
		}
		for _, c := range [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "cerver-net"}, {"systemctl", "restart", "cerver-net"}} {
			if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
				return fmt.Errorf("%s: %v: %s", strings.Join(c, " "), err, strings.TrimSpace(string(out)))
			}
		}
	default:
		return fmt.Errorf("cerver net runs on macOS and Linux for now")
	}
	fmt.Printf("%s now stays on your Cerver network, also after restarts.\n", *name)
	fmt.Printf("See your devices: cerver net   ·   Log: %s   ·   Stop: sudo cerver net uninstall\n", svcLog)
	return nil
}

func netUninstall() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("needs admin rights: sudo cerver net uninstall")
	}
	switch runtime.GOOS {
	case "darwin":
		exec.Command("launchctl", "bootout", "system/"+svcLabel).Run()
		os.Remove(svcPlist)
	case "linux":
		exec.Command("systemctl", "disable", "--now", "cerver-net").Run()
		os.Remove(svcUnit)
		exec.Command("systemctl", "daemon-reload").Run()
	}
	os.Remove(svcBinary)
	// The service clears its names on a clean stop; make sure.
	updateNamesOff()
	fmt.Println("This device left your Cerver network and won't rejoin on restart.")
	return nil
}

func updateNamesOff() {
	if b, err := os.ReadFile(hostsFile); err == nil && strings.Contains(string(b), "cerver net") {
		_ = writeHostsEmpty()
	}
}

func launchdPlist(name, home string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + svcLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + svcBinary + `</string>
		<string>net</string><string>up</string>
		<string>--name</string><string>` + xmlEscape(name) + `</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict><key>HOME</key><string>` + xmlEscape(home) + `</string></dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>10</integer>
	<key>StandardOutPath</key><string>` + svcLog + `</string>
	<key>StandardErrorPath</key><string>` + svcLog + `</string>
</dict>
</plist>
`
}

func systemdUnit(name, home string) string {
	return `[Unit]
Description=Cerver net (private network between your devices)
After=network-online.target
Wants=network-online.target

[Service]
Environment=HOME=` + home + `
ExecStart=` + svcBinary + ` net up --name "` + strings.ReplaceAll(name, `"`, ``) + `"
Restart=always
RestartSec=10
StandardOutput=append:` + svcLog + `
StandardError=append:` + svcLog + `

[Install]
WantedBy=multi-user.target
`
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func copySelf(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	if src, err = filepath.EvalSymlinks(src); err != nil {
		return err
	}
	if src == dst {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func writeHostsEmpty() error { return wgnetWriteHosts(hostsFile, "") }
