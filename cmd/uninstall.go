package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// `cerver uninstall` takes Cerver off this computer completely, so a fresh
// install starts from nothing:
//
//	in your account   this computer leaves your network and your pool (--keep-account skips this)
//	services          relay, background updater, network service (admin password once)
//	files             ~/.cerver (sign-in, programs, logs), /Applications/Cerver.app,
//	                  the PATH line install.sh added to your shell
//
// Left alone: uv and Go (other programs may use them), ~/.kompany (Kompany's).
func Uninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "Don't ask to confirm")
	keep := fs.Bool("keep-account", false, "Keep this computer's entries in your Cerver account")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return fmt.Errorf("run it without sudo (it asks for the admin password itself, for the network part)")
	}
	name := relayDefaultName()
	netName := ""
	if b, err := os.ReadFile(netStatusFile); err == nil {
		var s struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(b, &s) == nil {
			netName = s.Name
		}
	}
	if !*yes {
		fmt.Printf("This removes Cerver from %s: it leaves your pool and network, and its sign-in, services and files are deleted.\nContinue? [y/N] ", name)
		tty, err := os.Open("/dev/tty")
		if err != nil {
			return fmt.Errorf("no terminal to confirm from; run: cerver uninstall --yes")
		}
		ans, _ := bufio.NewReader(tty).ReadString('\n')
		tty.Close()
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			fmt.Println("Nothing removed.")
			return nil
		}
	}

	// 1. Your account (while this computer still has its sign-in).
	if !*keep {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		forgetInAccount(ctx, name, netName)
		cancel()
	}

	// 2. The network service (runs as admin).
	if netServiceInstalled() {
		fmt.Println("Removing the network service (admin password)…")
		if err := runAsAdmin(cerverExe(), "net", "uninstall"); err != nil {
			fmt.Printf("  couldn't: %v\n  Finish it with: sudo %s net uninstall\n", err, cerverExe())
		}
	}

	// 3. Your services.
	if runtime.GOOS == "darwin" && appOpen() {
		exec.Command("osascript", "-e", `quit app "Cerver"`).Run()
		time.Sleep(time.Second)
		exec.Command("pkill", "-x", "Cerver").Run()
	}
	updaterUninstall()
	stopStrayRelay()
	switch runtime.GOOS {
	case "darwin":
		for _, l := range []string{relayLabel, relayOldLabel, "dev.kompany.relay"} {
			exec.Command("launchctl", "bootout", launchdDomain()+"/"+l).Run()
			os.Remove(filepath.Join(home(), "Library", "LaunchAgents", l+".plist"))
		}
	case "linux":
		exec.Command("systemctl", "--user", "disable", "--now", relayLinuxUnit).Run()
		os.Remove(relayUnitPath())
		exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	fmt.Println("✓ Relay and updates stopped")

	// 4. Files.
	if runtime.GOOS == "darwin" {
		if err := os.RemoveAll(appPath); err == nil {
			fmt.Println("✓ Removed " + appPath)
		}
	}
	removePathLine()
	if err := os.RemoveAll(filepath.Join(home(), ".cerver")); err != nil {
		return err
	}
	fmt.Println("✓ Removed ~/.cerver (sign-in, programs, logs)")
	fmt.Println("\nCerver is off this computer. To install again: curl -fsSL https://cerver.ai/install.sh | sh")
	return nil
}

func cerverExe() string {
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		return r
	}
	return exe
}

func netServiceInstalled() bool {
	for _, p := range []string{svcPlist, svcUnit, svcBinary} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// runAsAdmin runs one command as root: sudo in a terminal, or macOS's own
// password dialog when there's no terminal (from the app).
func runAsAdmin(args ...string) error {
	if tty, err := os.Open("/dev/tty"); err == nil {
		tty.Close()
		c := exec.Command("sudo", args...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("needs a terminal for sudo")
	}
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	script := `do shell script "` + strings.ReplaceAll(strings.Join(q, " "), `"`, `\"`) + `" with administrator privileges`
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// forgetInAccount removes this computer from your network and your pool.
func forgetInAccount(ctx context.Context, name, netName string) {
	gw, err := netGateway(ctx)
	if err != nil {
		fmt.Printf("  (not signed in, so nothing to remove from your account: %v)\n", err)
		return
	}
	if netName != "" {
		var out struct{}
		if err := gw.Do(ctx, "DELETE", "/v2/net/nodes/"+netName, nil, &out); err != nil {
			fmt.Printf("  couldn't take %s off your network: %v\n", netName, err)
		} else {
			fmt.Printf("✓ %s left your network\n", netName)
		}
	}
	var pool struct {
		Machines []struct {
			ComputeID string `json:"compute_id"`
			Name      string `json:"name"`
			Mine      bool   `json:"mine"`
		} `json:"machines"`
	}
	if err := gw.Do(ctx, "GET", "/v2/pool", nil, &pool); err != nil {
		fmt.Printf("  couldn't read your pool: %v\n", err)
		return
	}
	for _, m := range pool.Machines {
		if m.Mine && strings.EqualFold(m.Name, name) && m.ComputeID != "" {
			var out struct{}
			if err := gw.Do(ctx, "DELETE", "/v2/computes/"+m.ComputeID, nil, &out); err != nil {
				fmt.Printf("  couldn't take %s out of your pool: %v\n", name, err)
			} else {
				fmt.Printf("✓ %s left your pool\n", name)
			}
		}
	}
}

// removePathLine takes out the line install.sh added to your shell startup.
func removePathLine() {
	const line = `export PATH="$HOME/.cerver/bin:$PATH"`
	for _, f := range []string{".zshrc", ".bashrc", ".bash_profile", ".profile"} {
		p := filepath.Join(home(), f)
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), line) {
			continue
		}
		var keep []string
		for _, l := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(l) != line {
				keep = append(keep, l)
			}
		}
		if os.WriteFile(p, []byte(strings.Join(keep, "\n")), 0o644) == nil {
			fmt.Println("✓ Removed the cerver line from ~/" + f)
		}
	}
}
