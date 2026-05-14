package cmd

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
)

// Login bootstraps ~/.cerver/cerver.env via the device-code flow.
// The CLI requests a device_code from the gateway and shows the user
// a short user_code + a URL to open in their browser. The user signs
// in via magic link (one email round-trip) and confirms the code in
// the browser; the CLI polls until approved and writes the returned
// api_key to disk.
//
// Why device-code: the legacy POST /v2/auth/login {email} let anyone
// who knew an email mint a key for that account. Locking that down
// broke `cerver login` for existing users until this verb learned
// the new flow.
//
// Idempotent: if ~/.cerver/cerver.env already has CERVER_API_KEY,
// this prints what's there and exits unless --force is set.
func Login(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	force := fs.Bool("force", false, "Re-login even if cerver.env already has a key")
	noBrowser := fs.Bool("no-browser", false, "Don't try to open the browser automatically")
	if err := fs.Parse(args); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cerverDir := filepath.Join(home, ".cerver")
	envPath := filepath.Join(cerverDir, "cerver.env")

	if !*force {
		if existing := readEnvKey(envPath, "CERVER_API_KEY"); existing != "" {
			fmt.Printf("Already logged in (%s). Use --force to re-login.\n", envPath)
			return nil
		}
	}

	hostName, _ := os.Hostname()
	if hostName == "" {
		hostName = "cerver CLI"
	}

	// Step 1 — request a device code. Unauthenticated; no bearer yet.
	startCtx, cancelStart := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelStart()
	gw := gateway.New("")
	start, err := gw.StartDeviceAuth(startCtx, hostName)
	if err != nil {
		return fmt.Errorf("starting device auth: %w", err)
	}

	// Step 2 — show the user where to go and try to open it.
	fmt.Println()
	fmt.Printf("  Open this URL in your browser to authorize this device:\n")
	fmt.Printf("    %s\n\n", start.VerificationURI)
	fmt.Printf("  Or enter the code manually at https://cerver.ai/approve:\n")
	fmt.Printf("    code: %s\n\n", start.UserCode)
	fmt.Printf("  Waiting for approval (expires in %d minutes)…\n", maxInt(start.ExpiresIn/60, 1))

	if !*noBrowser {
		_ = openInBrowser(start.VerificationURI)
	}

	// Step 3 — poll until approved or timeout. Server-supplied interval
	// (typically 5s); we clamp to [3, 10] so a misbehaving gateway
	// can't drag this into either DOS territory or hour-long waits.
	interval := time.Duration(clamp(start.Interval, 3, 10)) * time.Second
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	var apiKey string
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		pollCtx, cancelPoll := context.WithTimeout(context.Background(), 10*time.Second)
		poll, err := gw.PollDeviceAuth(pollCtx, start.DeviceCode)
		cancelPoll()
		if err != nil {
			// Transient network noise — keep polling unless the deadline hits.
			continue
		}
		if poll.Status == "approved" && poll.AccessToken != "" {
			apiKey = poll.AccessToken
			break
		}
		switch poll.Error {
		case "expired_token":
			return errors.New("the sign-in code expired before approval. Try `cerver login` again.")
		case "access_denied":
			return errors.New("approval was denied in the browser.")
		case "invalid_device_code":
			return errors.New("device code rejected by the gateway. Try `cerver login` again.")
		}
	}
	if apiKey == "" {
		return errors.New("timed out waiting for approval. Try `cerver login` again.")
	}

	// Step 4 — write the key. Same shape as the old flow so anything
	// reading ~/.cerver/cerver.env keeps working.
	if err := os.MkdirAll(cerverDir, 0o700); err != nil {
		return err
	}
	if err := writeCerverEnv(envPath, apiKey); err != nil {
		return err
	}
	if err := os.Chmod(envPath, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "warn: chmod %s: %v\n", envPath, err)
	}
	fmt.Printf("\nLogged in — key saved to %s\n", envPath)
	return nil
}

// readEnvKey returns the value of `key` from a simple KEY=VALUE env
// file. Empty string if missing.
func readEnvKey(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	prefix := key + "="
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, prefix) {
			return strings.Trim(line[len(prefix):], `"'`)
		}
	}
	return ""
}

// writeCerverEnv writes CERVER_API_KEY + CERVER_GATEWAY_URL to the
// env file. Truncates rather than appending — `--force` re-login
// should replace the old key entirely.
func writeCerverEnv(path, apiKey string) error {
	content := fmt.Sprintf("CERVER_API_KEY=%s\nCERVER_GATEWAY_URL=%s\n",
		apiKey, gateway.DefaultURL)
	return os.WriteFile(path, []byte(content), 0o600)
}

func openInBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "linux":
		cmd = "xdg-open"
		args = []string{url}
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", url}
	default:
		return fmt.Errorf("unsupported OS %s", runtime.GOOS)
	}
	return exec.Command(cmd, args...).Start()
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
