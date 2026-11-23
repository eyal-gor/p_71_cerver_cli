package cmd

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// `cerver vm` provisions a local VM for agents on an Apple computer: Apple's
// own `container` tool (each container is a small Linux VM), plus a Cerver
// image with the coding agents in it. An agent started in the VM sees only
// the folder it works in; nothing else on the Mac exists for it.
//
//   cerver vm                  what's provisioned here (--json for the app)
//   sudo cerver vm install     Apple's container tool (signed by Apple, checked)
//   cerver vm start            start it and build the Cerver agent image
//   cerver vm login            give agents in the VM your Claude login (a token
//                              from `claude setup-token`; saved only on this Mac)
//   cerver vm run "…"          try it: Claude in the VM, in this folder
//
// The relay reads the same state (branch_monkey_mcp/computer_runtime/vm_runtime.py)
// and runs agents in the VM when the Security page says "In a VM".

const (
	containerVersion = "1.7.1"
	containerPkgURL  = "https://github.com/apple/container/releases/download/" + containerVersion + "/container-" + containerVersion + "-installer-signed.pkg"
	containerBin     = "/usr/local/bin/container"
	// VMImage is the agent image's name; the relay uses the same one.
	VMImage = "cerver-agent:1"
)

// The agent image: the coding agents and the tools they reach for. Root with
// IS_SANDBOX=1 because the VM itself is the sandbox (Claude Code allows
// skipping its own permission prompts there).
const vmDockerfile = `FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends git python3 python3-pip curl ca-certificates ripgrep jq procps \
 && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code @openai/codex && npm cache clean --force
ENV IS_SANDBOX=1 HOME=/root
WORKDIR /work
`

func vmEnvFile() string { return filepath.Join(os.Getenv("HOME"), ".cerver", "vm.env") }

type vmStatus struct {
	Supported bool   `json:"supported"`
	Why       string `json:"why,omitempty"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Running   bool   `json:"running"`
	Image     bool   `json:"image"`
	SignedIn  bool   `json:"signed_in"`
	Ready     bool   `json:"ready"`
}

func macMajor() int {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.SplitN(strings.TrimSpace(string(out)), ".", 2)[0])
	return n
}

func getVMStatus() vmStatus {
	var s vmStatus
	switch {
	case runtime.GOOS != "darwin":
		s.Why = "the local VM uses Apple's container tool, which runs on Macs"
	case runtime.GOARCH != "arm64":
		s.Why = "needs an Apple silicon Mac (M1 or later)"
	case macMajor() < 26:
		s.Why = "needs macOS 26 or later"
	default:
		s.Supported = true
	}
	if _, err := os.Stat(containerBin); err == nil {
		s.Installed = true
		if out, err := exec.Command(containerBin, "--version").Output(); err == nil {
			s.Version = strings.TrimSpace(string(out))
		}
		if exec.Command(containerBin, "system", "status").Run() == nil {
			s.Running = true
			if out, err := exec.Command(containerBin, "image", "list").Output(); err == nil {
				name, tag, _ := strings.Cut(VMImage, ":")
				for _, line := range strings.Split(string(out), "\n") {
					f := strings.Fields(line)
					if len(f) >= 2 && strings.HasSuffix(f[0], name) && f[1] == tag {
						s.Image = true
					}
				}
			}
		}
	}
	if b, err := os.ReadFile(vmEnvFile()); err == nil && strings.Contains(string(b), "CLAUDE_CODE_OAUTH_TOKEN=") {
		s.SignedIn = true
	}
	s.Ready = s.Supported && s.Installed && s.Running && s.Image
	return s
}

func VM(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "", "status":
		fs := flag.NewFlagSet("vm", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "JSON for the app")
		if err := fs.Parse(args); err != nil {
			return err
		}
		s := getVMStatus()
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(s)
		}
		yes := func(b bool) string {
			if b {
				return "yes"
			}
			return "no"
		}
		if !s.Supported {
			fmt.Println("Local VM: not on this computer (" + s.Why + ").")
			return nil
		}
		fmt.Printf("Apple container tool  %s %s\nRunning               %s\nCerver agent image    %s\nClaude login          %s\n",
			yes(s.Installed), s.Version, yes(s.Running), yes(s.Image), yes(s.SignedIn))
		switch {
		case !s.Installed:
			fmt.Println("Next: sudo cerver vm install")
		case !s.Running || !s.Image:
			fmt.Println("Next: cerver vm start")
		case !s.SignedIn:
			fmt.Println("Next: cerver vm login")
		default:
			fmt.Println("Ready. Choose \"In a VM\" on the Security page of the Cerver app.")
		}
		return nil
	case "install":
		return vmInstall()
	case "start":
		return vmStart()
	case "login":
		return vmLogin(args)
	case "run":
		return vmRun(args)
	default:
		return fmt.Errorf("unknown: cerver vm %s (status | install | start | login | run)", sub)
	}
}

// vmInstall downloads Apple's signed installer, checks it is Apple's, and
// installs it. Needs admin (the app asks for the password with macOS's own
// prompt).
func vmInstall() error {
	if s := getVMStatus(); !s.Supported {
		return errors.New("this computer can't run the local VM: " + s.Why)
	} else if s.Installed {
		fmt.Println("Apple's container tool is already installed (" + s.Version + ").")
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("installing needs admin: sudo cerver vm install")
	}
	dir, err := os.MkdirTemp("", "cerver-vm-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	pkg := filepath.Join(dir, "container.pkg")
	fmt.Println("Downloading Apple's container " + containerVersion + "…")
	sum, err := download(containerPkgURL, pkg)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	// Apple's signature, checked twice: the certificate chain names Apple,
	// and Gatekeeper accepts it as an installer.
	out, err := exec.Command("pkgutil", "--check-signature", pkg).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Developer ID Installer: Apple Inc.") {
		return fmt.Errorf("the download isn't signed by Apple; not installing it (sha256 %s)", sum)
	}
	if out, err := exec.Command("spctl", "--assess", "--type", "install", pkg).CombinedOutput(); err != nil {
		return fmt.Errorf("macOS doesn't accept the installer: %s", strings.TrimSpace(string(out)))
	}
	fmt.Println("Signed by Apple. Installing…")
	if out, err := exec.Command("installer", "-pkg", pkg, "-target", "/").CombinedOutput(); err != nil {
		return fmt.Errorf("install failed: %s", strings.TrimSpace(string(out)))
	}
	fmt.Println("Installed Apple's container tool. Next: cerver vm start")
	return nil
}

func download(url, to string) (string, error) {
	c := &http.Client{Timeout: 10 * time.Minute}
	r, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return "", fmt.Errorf("%s: %s", url, r.Status)
	}
	f, err := os.Create(to)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), r.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// vmStart starts the container system (first time: installs Apple's Linux
// kernel for it) and builds the agent image. As you, not admin.
func vmStart() error {
	s := getVMStatus()
	if !s.Installed {
		return errors.New("install Apple's container tool first: sudo cerver vm install")
	}
	if os.Geteuid() == 0 {
		return errors.New("run this as you, not with sudo: cerver vm start")
	}
	if !s.Running {
		fmt.Println("Starting the VM service (the first time it downloads Apple's Linux kernel)…")
		c := exec.Command(containerBin, "system", "start", "--enable-kernel-install")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("container system start: %w", err)
		}
	}
	if !getVMStatus().Image {
		dir, err := os.MkdirTemp("", "cerver-vm-image-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(vmDockerfile), 0o644); err != nil {
			return err
		}
		fmt.Println("Building the Cerver agent image (Claude Code, Codex, git, Python; a few minutes the first time)…")
		c := exec.Command(containerBin, "build", "--tag", VMImage, "--file", filepath.Join(dir, "Dockerfile"), dir)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("building the image: %w", err)
		}
	}
	fmt.Println("The VM is ready. Next, if you haven't: cerver vm login")
	return nil
}

// vmLogin saves a Claude token for agents in the VM (they can't read the
// Mac's keychain). The token comes from `claude setup-token`; it stays in
// ~/.cerver/vm.env, readable only by you.
func vmLogin(args []string) error {
	fs := flag.NewFlagSet("vm login", flag.ContinueOnError)
	token := fs.String("token", "", "The token from `claude setup-token` (or paste it when asked)")
	stdin := fs.Bool("stdin", false, "Read the token from standard input (for the app)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	t := strings.TrimSpace(*token)
	if t == "" {
		if !*stdin {
			fmt.Println("In another Terminal window run: claude setup-token")
			fmt.Print("Then paste the token here: ")
		}
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		t = strings.TrimSpace(line)
	}
	if !strings.HasPrefix(t, "sk-ant-") || len(t) < 40 {
		return errors.New("that doesn't look like a Claude token (it starts with sk-ant-)")
	}
	if err := os.MkdirAll(filepath.Dir(vmEnvFile()), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(vmEnvFile(), []byte("CLAUDE_CODE_OAUTH_TOKEN="+t+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Println("Saved. Agents in the VM use your Claude plan.")
	return nil
}

func vmToken() string {
	b, err := os.ReadFile(vmEnvFile())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "CLAUDE_CODE_OAUTH_TOKEN="); ok {
			return v
		}
	}
	return ""
}

// vmRun tries it: Claude in a fresh VM that sees only this folder.
func vmRun(args []string) error {
	if len(args) == 0 {
		return errors.New(`usage: cerver vm run "what to do"`)
	}
	s := getVMStatus()
	if !s.Ready {
		return errors.New("the VM isn't ready; run: cerver vm")
	}
	if vmToken() == "" {
		return errors.New("no Claude login for the VM yet: cerver vm login")
	}
	cwd, _ := os.Getwd()
	c := exec.Command(containerBin, "run", "--rm",
		"--volume", cwd+":/work", "--workdir", "/work",
		"--env-file", vmEnvFile(), // the token never appears in a process list
		VMImage, "claude", "-p", strings.Join(args, " "), "--dangerously-skip-permissions")
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}
