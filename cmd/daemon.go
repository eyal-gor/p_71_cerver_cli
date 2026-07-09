package cmd

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The local bridge daemon. Claude Code is pointed at this (127.0.0.1) once,
// permanently — its config never changes again. The daemon decides, PER
// REQUEST, where to forward:
//
//   bridge OFF  → api.anthropic.com, request untouched (your subscription
//                 OAuth token is forwarded verbatim — flat-rate preserved)
//   bridge ON   → gateway.cerver.ai/v2/proxy/anthropic, OAuth swapped for
//                 your project key (metered, capped, redacted)
//
// So `cerver bridge` flips the flag file and the NEXT request routes
// differently — no Claude Code restart. This works because Claude Code
// forwards its subscription auth through a custom base URL (verified).
const daemonAddr = "127.0.0.1:8788"

func daemonBaseURL() string { return "http://" + daemonAddr }

// Daemon runs (or ensures) the local bridge proxy.
//
//	cerver daemon            run in the foreground (the server)
//	cerver daemon --ensure   start it detached if not already up, then return
//	cerver daemon stop       stop it
func Daemon(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "--ensure", "ensure":
		if daemonAlive() {
			return nil
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, "daemon")
		cmd.Stdout = nil
		cmd.Stderr = nil
		cmd.Stdin = nil
		if err := cmd.Start(); err != nil {
			return err
		}
		// don't wait — let it run detached
		_ = cmd.Process.Release()
		// give it a moment to bind
		for i := 0; i < 20 && !daemonAlive(); i++ {
			time.Sleep(50 * time.Millisecond)
		}
		return nil
	case "stop":
		return daemonStop()
	case "status":
		if daemonAlive() {
			fmt.Printf("● bridge daemon running on %s\n", daemonAddr)
		} else {
			fmt.Println("○ bridge daemon not running")
		}
		return nil
	default:
		return runDaemon()
	}
}

func daemonAlive() bool {
	c := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := c.Get(daemonBaseURL() + "/__cerver_health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func daemonStop() error {
	c := &http.Client{Timeout: 500 * time.Millisecond}
	req, _ := http.NewRequest("POST", daemonBaseURL()+"/__cerver_stop", nil)
	_, err := c.Do(req)
	if err == nil {
		fmt.Println("○ bridge daemon stopped")
	}
	return nil
}

func runDaemon() error {
	ln, err := net.Listen("tcp", daemonAddr)
	if err != nil {
		// already bound — another daemon is up, fine.
		if strings.Contains(err.Error(), "address already in use") {
			return nil
		}
		return err
	}

	anthropic := &httputil.ReverseProxy{
		FlushInterval: -1, // stream SSE immediately
		Director: func(req *http.Request) {
			if bridgeIsOn() {
				// Route through the Cerver gateway with the project key.
				req.URL.Scheme = "https"
				req.URL.Host = "gateway.cerver.ai"
				req.Host = "gateway.cerver.ai"
				req.URL.Path = "/v2/proxy/anthropic" + req.URL.Path
				key := daemonRoutingKey()
				req.Header.Set("x-api-key", key)
				req.Header.Del("Authorization") // drop the subscription OAuth
			} else {
				// Passthrough to Anthropic — subscription OAuth forwarded as-is.
				req.URL.Scheme = "https"
				req.URL.Host = "api.anthropic.com"
				req.Host = "api.anthropic.com"
			}
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/__cerver_health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/__cerver_stop", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		go func() { time.Sleep(50 * time.Millisecond); os.Exit(0) }()
	})
	mux.Handle("/", anthropic)

	srv := &http.Server{Handler: mux}
	return srv.Serve(ln)
}

func bridgeIsOn() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".cerver", "bridge"))
	return err == nil
}

// daemonRoutingKey reads the bound project key (or falls back to the account
// key). Same source the shims used to read, now read server-side per request
// so a re-bind takes effect without restarting anything.
func daemonRoutingKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if b, err := os.ReadFile(filepath.Join(home, ".cerver", "gateway.key")); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k
		}
	}
	return readEnvKey(filepath.Join(home, ".cerver", "cerver.env"), "CERVER_API_KEY")
}
