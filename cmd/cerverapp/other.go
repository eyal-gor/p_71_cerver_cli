//go:build !windows

// The Cerver app for Windows. On a Mac, the app is macapp/; on Linux, the
// relay runs as a systemd user service.
package main

import (
	"fmt"
	"os"

	"github.com/eyal-gor/p_71_cerver_cli/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("Cerver", version.Version)
		return
	}
	fmt.Println("This is the Cerver app for Windows.")
	os.Exit(1)
}
