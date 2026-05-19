package cmd

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Update reinstalls the cerver CLI from the latest commit on `main`.
//
// Goes through `go install github.com/eyal-gor/p_71_cerver_cli/cmd/cerver@latest`
// so the binary lands on the same directory the running cerver lives in.
// That avoids a common gotcha — `go install` with the user's default
// GOBIN would land in ~/go/bin, but the running binary might be at
// ~/.cerver/bin or /opt/homebrew/bin, leaving the user wondering why
// `cerver --help` still shows the old commands.
//
// Falls back with a clear remediation when Go isn't on PATH — direct
// binary download is the manual alternative.
func Update(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	verbose := fs.Bool("verbose", false, "Stream go install output (default: only on failure)")
	dryRun := fs.Bool("dry-run", false, "Print what would happen, don't install")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Where is the *current* cerver binary running from? Install into
	// that directory so the upgrade replaces the in-flight binary in
	// place. macOS / Linux let us overwrite a running binary cleanly
	// — the running process keeps using its memory-mapped copy; the
	// next invocation picks up the new file.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("can't resolve current executable: %w", err)
	}
	// Follow a symlink (e.g. /opt/homebrew/bin/cerver → cellar) so we
	// install into the canonical directory, not the symlink's parent.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	installDir := filepath.Dir(exe)

	goBin, err := exec.LookPath("go")
	if err != nil {
		return errors.New(
			"Go isn't on PATH. Install Go from https://go.dev/dl/ or download a prebuilt cerver binary from " +
				"https://github.com/eyal-gor/p_71_cerver_cli/releases/latest and put it at " + exe,
		)
	}

	fmt.Printf("Current binary: %s\n", exe)
	fmt.Printf("Installing latest cerver into %s (via go install)…\n", installDir)
	if *dryRun {
		fmt.Println("(dry-run: stopping before invoking go install)")
		return nil
	}

	cmd := exec.Command(goBin, "install", "github.com/eyal-gor/p_71_cerver_cli/cmd/cerver@latest")
	// Force GOBIN to the same dir as the running binary. Otherwise
	// `go install` writes to $GOBIN or $GOPATH/bin, which often isn't
	// on PATH at all.
	env := append(os.Environ(), "GOBIN="+installDir)
	cmd.Env = env
	if *verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go install failed: %w", err)
		}
	} else {
		// Capture output so a clean install doesn't spam the terminal,
		// but surface the captured stream on failure so the user can
		// see *why* (compile error / network drop / proxy refusal).
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintln(os.Stderr, strings.TrimRight(string(out), "\n"))
			return fmt.Errorf("go install failed: %w", err)
		}
	}

	// Verify the new binary landed and is fresh.
	newExe := filepath.Join(installDir, "cerver")
	info, err := os.Stat(newExe)
	if err != nil {
		return fmt.Errorf("install reported success but %s isn't there: %w", newExe, err)
	}
	fmt.Printf("\nInstalled: %s\n", newExe)
	fmt.Printf("Modified:  %s\n", info.ModTime().Format("2006-01-02 15:04:05"))
	fmt.Println()
	fmt.Println("Verify with: cerver help")
	return nil
}
