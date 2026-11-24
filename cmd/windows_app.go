package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// On Windows there is no launchd or systemd for the relay. The Cerver app
// (CerverApp.exe, a tray icon) does that job: it starts with Windows (a "Run"
// entry for this user), runs the relay with no window, restarts it when it
// stops, and checks for updates every 6 hours.
//
// `cerver relay install` writes what the app should run into
// ~/.cerver/relay-windows.json; the app reads it each time it starts the relay.

const winRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// winAppPath: where install.ps1 puts the app. CerverApp.exe, not Cerver.exe:
// Windows ignores case, and Cerver.exe would be taken for cerver.exe.
func winAppPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = filepath.Join(home(), "AppData", "Local")
	}
	return filepath.Join(base, "Programs", "Cerver", "CerverApp.exe")
}

func winRelayFile() string { return filepath.Join(home(), ".cerver", "relay-windows.json") }
func relayPidFile() string { return filepath.Join(home(), ".cerver", "relay.pid") }

type winRelaySpec struct {
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
	Name string   `json:"name"`
}

func winRelayInstall(args []string, dir, name string) error {
	if _, err := os.Stat(winAppPath()); err != nil {
		return fmt.Errorf("the Cerver app isn't installed (%s); install it with: irm https://cerver.ai/install.ps1 | iex", winAppPath())
	}
	b, _ := json.MarshalIndent(winRelaySpec{Args: args, Dir: dir, Name: name}, "", "  ")
	if err := os.WriteFile(winRelayFile(), b, 0o600); err != nil {
		return err
	}
	if out, err := exec.Command("reg", "add", winRunKey, "/v", "Cerver", "/t", "REG_SZ", "/d", `"`+winAppPath()+`"`, "/f").CombinedOutput(); err != nil {
		return fmt.Errorf("couldn't make Cerver start with Windows: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if winAppOpen() {
		winKillRelay() // the app starts it again, with the new settings
	} else if err := exec.Command(winAppPath()).Start(); err != nil {
		return fmt.Errorf("couldn't start the Cerver app: %v", err)
	}
	return nil
}

func winRelayUninstall() {
	exec.Command("reg", "delete", winRunKey, "/v", "Cerver", "/f").Run()
	os.Remove(winRelayFile())
	exec.Command("taskkill", "/IM", "CerverApp.exe", "/F").Run()
	winKillRelay()
}

// winKillRelay stops the relay and the agents it started (/T: the whole tree).
func winKillRelay() error {
	b, err := os.ReadFile(relayPidFile())
	if err != nil {
		return fmt.Errorf("the relay isn't running")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return fmt.Errorf("the relay isn't running")
	}
	return exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}

func winAppOpen() bool {
	// Not "Cerver.exe": Windows ignores case, and that would match cerver.exe (this program).
	out, _ := exec.Command("tasklist", "/FI", "IMAGENAME eq CerverApp.exe", "/NH").Output()
	return strings.Contains(strings.ToLower(string(out)), "cerverapp.exe")
}

func winAppVersion() string {
	if _, err := os.Stat(winAppPath()); err != nil {
		return ""
	}
	out, err := exec.Command(winAppPath(), "version").Output()
	if err != nil {
		return "dev"
	}
	f := strings.Fields(string(out))
	if len(f) == 2 {
		return f[1]
	}
	return "dev"
}

// relayAnswers: the relay's local server is up.
func relayAnswers() bool {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:18081/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}
