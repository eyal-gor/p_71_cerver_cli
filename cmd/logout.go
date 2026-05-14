package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
)

// Logout revokes the local api_key server-side AND removes
// ~/.cerver/cerver.env so the next `cerver` command requires a fresh
// `cerver login`. Without this verb, deleting the env file alone left
// the key valid — anyone who had a copy could still use it.
//
// Flags:
//
//	--local-only   skip the server revoke; just delete cerver.env
//	--force        don't error if cerver.env is already gone
func Logout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	localOnly := fs.Bool("local-only", false, "Just delete ~/.cerver/cerver.env; leave the key valid server-side")
	force := fs.Bool("force", false, "Don't error if no key is found")
	if err := fs.Parse(args); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	envPath := filepath.Join(home, ".cerver", "cerver.env")
	apiKey := readEnvKey(envPath, "CERVER_API_KEY")

	if apiKey == "" {
		if *force {
			fmt.Println("Already logged out.")
			return nil
		}
		return errors.New("no api_key found in ~/.cerver/cerver.env — already logged out, or use --force")
	}

	// Revoke server-side first so a bricked-then-recreated env file
	// can't accidentally leave the old key live. If the revoke fails
	// (network, gateway down), we keep the env file in place so the
	// user can retry — otherwise they'd be locked out locally with no
	// key to revoke later.
	if !*localOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		gw := gateway.New(apiKey)
		if err := gw.RevokeKey(ctx, apiKey); err != nil {
			return fmt.Errorf("server-side revoke failed: %w (env file kept; try `cerver logout` again or use --local-only)", err)
		}
		fmt.Println("Revoked server-side.")
	}

	if err := os.Remove(envPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", envPath, err)
	}
	fmt.Printf("Logged out — removed %s\n", envPath)
	return nil
}
