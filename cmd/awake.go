package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Awake is `cerver awake [on|off]`: keep this computer awake for agents, and
// email its owner if it goes offline. The relay reads ~/.cerver/awake.json
// every minute. Without the file, desktops and servers are on, laptops off.
func Awake(args []string) error {
	path := filepath.Join(home(), ".cerver", "awake.json")
	if len(args) == 0 {
		on, set := awakeSetting(path)
		state := "off"
		if on {
			state = "on"
		}
		why := "your choice"
		if !set {
			why = "the default for a desktop"
			if hasBattery() {
				why = "the default for a laptop"
			}
		}
		fmt.Printf("Keep awake: %s (%s)\n", state, why)
		fmt.Println("On: this computer doesn't sleep while plugged in, and you're emailed if it goes offline.")
		fmt.Println("Change: cerver awake on | cerver awake off")
		return nil
	}
	var on bool
	switch args[0] {
	case "on":
		on = true
	case "off":
		on = false
	default:
		return fmt.Errorf("usage: cerver awake [on|off]")
	}
	b, _ := json.Marshal(map[string]bool{"keep_awake": on})
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	if on {
		fmt.Println("✓ This computer stays awake while plugged in, and you'll be emailed if it goes offline (within a minute).")
	} else {
		fmt.Println("✓ This computer may sleep again, and offline emails are off for it (within a minute).")
	}
	return nil
}

// awakeSetting: the saved choice, or the default (laptops off).
func awakeSetting(path string) (on bool, set bool) {
	var s struct {
		KeepAwake *bool `json:"keep_awake"`
	}
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &s) == nil && s.KeepAwake != nil {
		return *s.KeepAwake, true
	}
	return !hasBattery(), false
}

func hasBattery() bool {
	switch runtime.GOOS {
	case "darwin":
		out, _ := exec.Command("pmset", "-g", "batt").Output()
		return strings.Contains(string(out), "InternalBattery")
	case "windows":
		out, _ := exec.Command("powershell", "-NoProfile", "-Command", "(Get-CimInstance Win32_Battery | Measure-Object).Count").Output()
		return strings.TrimSpace(string(out)) != "" && strings.TrimSpace(string(out)) != "0"
	default:
		m, _ := filepath.Glob("/sys/class/power_supply/BAT*")
		return len(m) > 0
	}
}
