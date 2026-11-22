package cmd

import (
	"context"
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

	"github.com/eyal-gor/p_71_cerver_cli/internal/version"
)

// `cerver update` brings everything on this computer to the latest release:
//
//	the cerver program      replaced in place from the release (checksum-checked)
//	the Mac app            /Applications/Cerver.app, when it isn't open (or --app)
//	the relay              restarted when no agent is running (it fetches the newest on start)
//	the network service    updates itself within the hour (it runs as admin)
//
// --auto is what the background updater runs every 6 hours: quiet, and it
// never interrupts a running agent. --check only reports; --json for the app.
func Update(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "Only report what's installed and what's available")
	asJSON := fs.Bool("json", false, "With --check: print JSON (for the Mac app)")
	auto := fs.Bool("auto", false, "Background mode: quiet, never interrupts a running agent")
	app := fs.Bool("app", false, "Also replace the Mac app while it's open (the app asks for this)")
	source := fs.Bool("source", false, "Build the latest commit with Go instead (developers)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source {
		return updateFromSource(nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	say := func(f string, a ...any) {
		if !*asJSON {
			fmt.Printf(f+"\n", a...)
		}
	}

	st := updateState(ctx)
	if *check {
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(st)
		}
		printState(st)
		return nil
	}
	if *auto {
		fmt.Printf("— %s\n", time.Now().Format(time.RFC3339))
	}
	if st.Latest == "" {
		if *auto {
			say("Couldn't check for updates: %s", st.Problem)
			return nil
		}
		say("No release to update from (%s); building the latest commit instead.", st.Problem)
		return updateFromSource(nil)
	}

	// 1. This program.
	if st.CLI.Update {
		data, err := releaseAsset(ctx, st.Latest, cliAssetName())
		if err == nil {
			err = replaceFile(cliTarget(), data)
		}
		if err != nil {
			say("cerver: couldn't update: %v", err)
		} else {
			say("cerver: %s → %s", st.CLI.Current, st.Latest)
		}
	} else {
		say("cerver: %s, up to date", st.CLI.Current)
	}

	// 2. The Mac app.
	if st.App != nil && st.App.Update {
		if appOpen() && !*app {
			say("Cerver app: %s → %s available; it updates when you click Update in the app", st.App.Current, st.Latest)
		} else if err := updateApp(ctx, st.Latest); err != nil {
			say("Cerver app: couldn't update: %v", err)
		} else {
			say("Cerver app: %s → %s", st.App.Current, st.Latest)
		}
	}

	// 3. The relay: restart only between agent runs.
	if st.Relay != nil && st.Relay.Update {
		if n, err := agentsRunningHere(ctx); err != nil {
			say("relay: v%s → v%s available; couldn't tell if agents are running (%v), so not restarting it now", st.Relay.Current, st.Relay.Latest, err)
		} else if n > 0 {
			say("relay: v%s → v%s available; %d agent(s) running, so it updates after they finish", st.Relay.Current, st.Relay.Latest, n)
		} else if err := restartRelay(); err != nil {
			say("relay: couldn't restart: %v", err)
		} else {
			say("relay: restarting to pick up v%s", st.Relay.Latest)
		}
	}

	// 4. The network service updates itself (it runs as admin).
	if st.Net != nil && st.Net.Update {
		if st.Net.Current == "dev" {
			// From before releases: it can't update itself yet. One reinstall fixes that for good.
			say("network service: from before automatic updates; reinstall it once (admin password): sudo %s net install", cliTarget())
		} else {
			say("network service: %s → %s within the hour (it updates itself)", st.Net.Current, st.Latest)
		}
	}
	return nil
}

type part struct {
	Current string `json:"current"`
	Latest  string `json:"latest,omitempty"`
	Update  bool   `json:"update"`
}

type state struct {
	Latest  string `json:"latest"` // newest release of cerver + the app + the network service
	Problem string `json:"problem,omitempty"`
	CLI     part   `json:"cli"`
	App     *part  `json:"app,omitempty"`
	Relay   *part  `json:"relay,omitempty"`
	Net     *part  `json:"net,omitempty"`
}

func updateState(ctx context.Context) state {
	var st state
	latest, err := latestRelease(ctx)
	if err != nil {
		st.Problem = err.Error()
	}
	st.Latest = latest
	newer := func(cur string) bool {
		if latest == "" {
			return false
		}
		if cur == "" || cur == "dev" {
			return true // a build from before releases: replace it
		}
		return version.Newer(latest, cur)
	}

	// The cerver to update is the one in ~/.cerver/bin (what you and the
	// services run), even when the app's own copy is the one asking. A
	// developer's build elsewhere, or the copy inside the app, is left alone
	// (the app's copy updates with the app).
	target := cliTarget()
	cli := binVersion(target)
	managed := strings.HasPrefix(target, filepath.Join(home(), ".cerver", "bin"))
	st.CLI = part{Current: cli, Latest: latest, Update: managed && newer(cli)}

	if runtime.GOOS == "darwin" {
		if v := appVersion(); v != "" {
			st.App = &part{Current: v, Latest: latest, Update: newer(v)}
		}
	}
	if relayInstalled() {
		r := &part{Current: relayRunningVersion()}
		if l, err := relayLatestVersion(ctx); err == nil {
			r.Latest = l
			a, _ := strconv.Atoi(r.Current)
			b, _ := strconv.Atoi(l)
			r.Update = r.Current != "" && b > a
		}
		st.Relay = r
	}
	if b, err := os.ReadFile(netStatusFile); err == nil {
		var s struct {
			Version string `json:"version"`
		}
		json.Unmarshal(b, &s)
		cur := s.Version
		if cur == "" {
			cur = "dev"
		}
		st.Net = &part{Current: cur, Latest: latest, Update: newer(cur)}
	}
	return st
}

func printState(st state) {
	line := func(name string, p *part) {
		if p == nil {
			return
		}
		cur := p.Current
		if cur == "" {
			cur = "?"
		}
		if p.Update {
			fmt.Printf("  %-17s %s → %s available\n", name, cur, p.Latest)
		} else {
			fmt.Printf("  %-17s %s, up to date\n", name, cur)
		}
	}
	if st.Latest == "" {
		fmt.Printf("Couldn't check for updates: %s\n", st.Problem)
	}
	line("cerver", &st.CLI)
	line("Cerver app", st.App)
	line("relay", st.Relay)
	line("network service", st.Net)
	if st.CLI.Update || (st.App != nil && st.App.Update) || (st.Relay != nil && st.Relay.Update) || (st.Net != nil && st.Net.Update) {
		fmt.Println("Update: cerver update")
	}
}

const appPath = "/Applications/Cerver.app"

func appVersion() string {
	out, err := exec.Command("/usr/bin/defaults", "read", appPath+"/Contents/Info", "CFBundleShortVersionString").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func appOpen() bool { return exec.Command("pgrep", "-x", "Cerver").Run() == nil }

// updateApp unpacks Cerver.zip from release v next to the old app, then
// swaps them. A running app keeps working until it restarts.
func updateApp(ctx context.Context, v string) error {
	data, err := releaseAsset(ctx, v, "Cerver.zip")
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "cerver-app-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	zip := filepath.Join(dir, "Cerver.zip")
	if err := os.WriteFile(zip, data, 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("/usr/bin/ditto", "-x", "-k", zip, dir).CombinedOutput(); err != nil {
		return fmt.Errorf("unzip: %v: %s", err, out)
	}
	fresh := filepath.Join(dir, "Cerver.app")
	if _, err := os.Stat(fresh); err != nil {
		return fmt.Errorf("Cerver.zip has no Cerver.app")
	}
	old := appPath + ".old"
	os.RemoveAll(old)
	if err := os.Rename(appPath, old); err != nil && !os.IsNotExist(err) {
		return err
	}
	if out, err := exec.Command("/usr/bin/ditto", fresh, appPath).CombinedOutput(); err != nil {
		os.Rename(old, appPath)
		return fmt.Errorf("copy: %v: %s", err, out)
	}
	os.RemoveAll(old)
	return nil
}

// agentsRunningHere asks Cerver how many agents run on this computer now.
func agentsRunningHere(ctx context.Context) (int, error) {
	gw, err := netGateway(ctx)
	if err != nil {
		return 0, err
	}
	var out struct {
		Machines []struct {
			Name    string `json:"name"`
			Mine    bool   `json:"mine"`
			Running int    `json:"running"`
		} `json:"machines"`
	}
	if err := gw.Do(ctx, "GET", "/v2/pool", nil, &out); err != nil {
		return 0, err
	}
	name := relayDefaultName()
	for _, m := range out.Machines {
		if m.Mine && strings.EqualFold(m.Name, name) {
			return m.Running, nil
		}
	}
	return 0, nil
}

func restartRelay() error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("launchctl", "kickstart", "-k", launchdDomain()+"/"+relayLabel).Run()
	case "linux":
		return exec.Command("systemctl", "--user", "restart", relayLinuxUnit).Run()
	}
	return nil
}

// cliTarget is the cerver program updates replace.
func cliTarget() string {
	p := filepath.Join(home(), ".cerver", "bin", "cerver")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return cerverExe()
}

// binVersion asks a cerver program its version; builds from before versions
// don't know the command and count as "dev".
func binVersion(path string) string {
	if path == cerverExe() {
		return version.Version
	}
	out, err := exec.Command(path, "version").Output()
	if err != nil {
		return "dev"
	}
	f := strings.Fields(string(out))
	if len(f) == 2 && f[0] == "cerver" {
		return f[1]
	}
	return "dev"
}
