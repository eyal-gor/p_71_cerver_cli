package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// What agents sent to this computer through Cerver may open: three lists of
// folders and a mode. The relay enforces them with the system sandbox.
//
//	cerver access                         show the rules
//	cerver access allow ~/Code            may open (besides the folder it works in)
//	cerver access ask ~/Documents         only after you say yes, per task (a dialog here)
//	cerver access block ~/.ssh            never
//	cerver access unlist ~/Code           take it off every list
//	cerver access mode own_folder         as_you | own_folder | vm
//	cerver access start ~/Code            where agents start when no folder is given (or "default")
//
// The rules file is root's, so changing it asks for this Mac's password: an
// agent can propose a change, only the person at the Mac can approve it.

const accessFile = "/Library/Application Support/Cerver/agent-rules.json"

type accessRules struct {
	Mode    string   `json:"agent_access"`
	Allowed []string `json:"allowed_folders"`
	Ask     []string `json:"ask_folders"`
	Blocked []string `json:"blocked_folders"`
	Start   string   `json:"start_folder,omitempty"`
}

func loadAccess(home string) (accessRules, bool) {
	r := accessRules{Mode: "as_you"}
	guarded := true
	b, err := os.ReadFile(accessFile)
	if err != nil {
		guarded = false
		b, err = os.ReadFile(filepath.Join(home, ".cerver", "security.json"))
	}
	if err == nil {
		_ = json.Unmarshal(b, &r)
	}
	if r.Mode == "" {
		r.Mode = "as_you"
	}
	return r, guarded
}

// tildePath: ~/… for folders in the home folder (the relay expands it).
func tildePath(home, p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~") {
		return filepath.Clean(p)
	}
	if !filepath.IsAbs(p) {
		if a, err := filepath.Abs(p); err == nil {
			p = a
		}
	}
	p = filepath.Clean(p)
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

func without(list []string, x string) []string {
	var out []string
	for _, v := range list {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

// applyAccess changes the rules (pure, for tests).
func applyAccess(r accessRules, verb string, args []string) (accessRules, string, error) {
	switch verb {
	case "allow", "ask", "block", "unlist":
		if len(args) == 0 {
			return r, "", fmt.Errorf("usage: cerver access %s <folder>…", verb)
		}
		for _, f := range args {
			r.Allowed, r.Ask, r.Blocked = without(r.Allowed, f), without(r.Ask, f), without(r.Blocked, f)
			switch verb {
			case "allow":
				r.Allowed = append(r.Allowed, f)
			case "ask":
				r.Ask = append(r.Ask, f)
			case "block":
				r.Blocked = append(r.Blocked, f)
			}
		}
		say := map[string]string{"allow": "Agents may open", "ask": "Agents must ask first for", "block": "Agents may never open", "unlist": "No rule for"}[verb]
		return r, say + " " + strings.Join(args, ", "), nil
	case "mode":
		if len(args) != 1 || (args[0] != "as_you" && args[0] != "own_folder" && args[0] != "vm") {
			return r, "", errors.New("usage: cerver access mode as_you|own_folder|vm")
		}
		r.Mode = args[0]
		return r, "Agents' access: " + args[0], nil
	case "set": // the whole rules at once (the Cerver app), as JSON
		if len(args) != 1 {
			return r, "", errors.New("usage: cerver access set '<json>'")
		}
		var n accessRules
		if err := json.Unmarshal([]byte(args[0]), &n); err != nil {
			return r, "", fmt.Errorf("bad rules: %v", err)
		}
		if n.Mode != "as_you" && n.Mode != "own_folder" && n.Mode != "vm" {
			return r, "", errors.New("bad mode")
		}
		return n, describeChange(r, n), nil
	case "start":
		if len(args) != 1 {
			return r, "", errors.New("usage: cerver access start <folder>|default")
		}
		if args[0] == "default" {
			r.Start = ""
			return r, "Agents start in the default folder", nil
		}
		r.Start = args[0]
		return r, "Agents start in " + args[0] + " when no folder is given", nil
	}
	return r, "", fmt.Errorf("unknown: cerver access %s", verb)
}

func Access(args []string) error {
	home := os.Getenv("CERVER_ACCESS_HOME") // set when re-run as admin
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	r, guarded := loadAccess(home)
	if len(args) == 0 || args[0] == "--json" {
		if len(args) > 0 {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"rules": r, "guarded": guarded, "file": accessFile})
		}
		fmt.Printf("Agents sent here through Cerver: %s\n", map[string]string{"as_you": "may open everything you can", "own_folder": "only the folder they work in", "vm": "a separate sandbox computer"}[r.Mode])
		if r.Start != "" {
			fmt.Printf("Start in:   %s (when no folder is given)\n", r.Start)
		}
		show := func(t string, l []string) {
			v := "-"
			if len(l) > 0 {
				s := append([]string(nil), l...)
				sort.Strings(s)
				v = strings.Join(s, ", ")
			}
			fmt.Printf("%-11s %s\n", t, v)
		}
		show("Allowed:", r.Allowed)
		show("Ask first:", r.Ask)
		show("Never:", r.Blocked)
		if !guarded {
			fmt.Println("(Not yet protected by your password: the next change moves the rules into an admin-only file.)")
		}
		return nil
	}
	verb, rest := args[0], args[1:]
	for i, f := range rest {
		if verb != "mode" && verb != "set" && !(verb == "start" && f == "default") {
			rest[i] = tildePath(home, f)
		}
	}
	next, say, err := applyAccess(r, verb, rest)
	if err != nil {
		return err
	}
	if runtime.GOOS != "darwin" && os.Geteuid() != 0 {
		return errors.New("changing the rules needs admin: run it with sudo")
	}
	if os.Geteuid() != 0 {
		// The guardrail: macOS asks the person at this Mac for its password.
		self, _ := os.Executable()
		q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
		cmdline := "CERVER_ACCESS_HOME=" + q(home) + " " + q(self) + " access " + q(verb)
		for _, a := range rest {
			cmdline += " " + q(a)
		}
		as := func(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }
		script := `do shell script "` + as(cmdline) + `" with prompt "` + as("Cerver: "+say+". Type your password to allow this change to what agents may open.") + `" with administrator privileges`
		out, err := exec.Command("/usr/bin/osascript", "-e", script).CombinedOutput()
		if err != nil {
			if strings.Contains(string(out), "-128") {
				return errors.New("not changed: the password prompt was cancelled")
			}
			return fmt.Errorf("not changed: %s", strings.TrimSpace(string(out)))
		}
		fmt.Print(string(out))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(accessFile), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(next, "", "  ")
	tmp := accessFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, accessFile); err != nil {
		return err
	}
	fmt.Println(say + ". From the next agent that starts.")
	return nil
}

// describeChange: the change in words, for the password prompt.
func describeChange(a, b accessRules) string {
	var parts []string
	if a.Mode != b.Mode {
		parts = append(parts, "access: "+b.Mode)
	}
	diff := func(label string, x, y []string) {
		in := map[string]bool{}
		for _, v := range x {
			in[v] = true
		}
		for _, v := range y {
			if !in[v] {
				parts = append(parts, label+" "+v)
			}
		}
	}
	diff("allow", a.Allowed, b.Allowed)
	diff("ask first for", a.Ask, b.Ask)
	diff("never open", a.Blocked, b.Blocked)
	diff("no longer allow", b.Allowed, a.Allowed)
	diff("no longer ask for", b.Ask, a.Ask)
	diff("may open again", b.Blocked, a.Blocked)
	if a.Start != b.Start {
		if b.Start == "" {
			parts = append(parts, "start in the default folder")
		} else {
			parts = append(parts, "start in "+b.Start)
		}
	}
	if len(parts) == 0 {
		return "No change to the rules"
	}
	return strings.Join(parts, "; ")
}
