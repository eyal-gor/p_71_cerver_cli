package cmd

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
)

// Login bootstraps ~/.cerver/cerver.env from an email-only login flow.
// Posts the email to gateway.cerver.ai/v2/auth/login (creates account
// on first call) and writes the returned api_key to disk. Idempotent:
// if cerver.env already has CERVER_API_KEY, this prints what's there
// and exits unless --force is set.
func Login(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	emailFlag := fs.String("email", "", "Email to log in with (otherwise prompts)")
	force := fs.Bool("force", false, "Re-login even if cerver.env already has a key")
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

	email := strings.TrimSpace(*emailFlag)
	if email == "" {
		fmt.Print("Email (used to create or log into your cerver account): ")
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read email: %w", err)
		}
		email = strings.TrimSpace(line)
	}
	if email == "" {
		return errors.New("email is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Login uses an empty bearer — the call is unauthenticated.
	gw := gateway.New("")
	resp, err := gw.Login(ctx, email)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(cerverDir, 0o700); err != nil {
		return err
	}
	if err := writeCerverEnv(envPath, resp.APIKey); err != nil {
		return err
	}
	if err := os.Chmod(envPath, 0o600); err != nil {
		// non-fatal; just warn
		fmt.Fprintf(os.Stderr, "warn: chmod %s: %v\n", envPath, err)
	}

	verb := "Logged in"
	if resp.IsNew {
		verb = "Account created"
	}
	fmt.Printf("%s as %s — key saved to %s\n", verb, email, envPath)
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
