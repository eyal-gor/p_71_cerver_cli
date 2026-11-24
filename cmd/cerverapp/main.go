//go:build windows

// CerverApp.exe: the Cerver app for Windows. A tray icon that
//
//   - keeps the relay running (no window), restarting it when it stops,
//   - starts with Windows (a Run entry, set by `cerver relay install`),
//   - shows this PC's Agents page (relay status, jobs, CPU/memory, its log)
//     at http://127.0.0.1:18090 in the browser,
//   - checks for updates every 6 hours (cerver update --auto) and restarts
//     itself when a new copy of it was installed.
//
// What the relay runs comes from ~/.cerver/relay-windows.json (written by
// `cerver relay install`).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
	"golang.org/x/sys/windows"

	"github.com/eyal-gor/p_71_cerver_cli/internal/version"
)

const (
	relayURL  = "http://127.0.0.1:18081"
	pageAddr  = "127.0.0.1:18090"
	dashboard = "https://cerver.ai/dashboard"
	runKey    = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	noWindow  = 0x08000000 // CREATE_NO_WINDOW
)

func home() string      { h, _ := os.UserHomeDir(); return h }
func cdir() string      { return filepath.Join(home(), ".cerver") }
func cerverBin() string { return filepath.Join(cdir(), "bin", "cerver.exe") }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("Cerver", version.Version)
		return
	}
	// One copy per user: a second start just opens the page.
	name, _ := windows.UTF16PtrFromString(`Local\CerverApp`)
	if _, err := windows.CreateMutex(nil, false, name); err == windows.ERROR_ALREADY_EXISTS {
		openURL("http://" + pageAddr)
		return
	}
	os.MkdirAll(cdir(), 0o700)
	// The app has no console: what it says, and any crash, goes to app.log.
	if f, err := os.OpenFile(filepath.Join(cdir(), "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		log.SetOutput(f)
		debug.SetCrashOutput(f, debug.CrashOptions{})
	}
	exe, _ := os.Executable()
	log.Printf("Cerver %s started (%s)", version.Version, exe)
	sup := &supervisor{}
	go sup.run()
	go servePage(sup)
	go updates(sup)
	systray.Run(func() { tray(sup) }, nil)
	log.Printf("tray loop ended (quit chosen: %v)", leaving)
	if !leaving {
		// No tray here (a PC with no desktop session): keep the relay and
		// the page running anyway.
		select {}
	}
	sup.stop()
}

// leaving: the person chose Quit (or a new copy is taking over).
var leaving bool

// ── The relay, kept running ──────────────────────────────────────────────

type relaySpec struct {
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
	Name string   `json:"name"`
}

type supervisor struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	quitting bool
	started  time.Time
	problem  string
}

func (s *supervisor) run() {
	wait := 3 * time.Second
	for {
		s.mu.Lock()
		if s.quitting {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		b, err := os.ReadFile(filepath.Join(cdir(), "relay-windows.json"))
		var spec relaySpec
		if err != nil || json.Unmarshal(b, &spec) != nil || len(spec.Args) == 0 {
			s.setProblem("Not set up yet: run  cerver relay install")
			time.Sleep(5 * time.Second)
			continue
		}
		rlog, _ := os.OpenFile(filepath.Join(cdir(), "relay.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		c := exec.Command(spec.Args[0], spec.Args[1:]...)
		c.Dir = spec.Dir
		c.Stdout, c.Stderr = rlog, rlog
		c.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
		c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: noWindow}
		if err := c.Start(); err != nil {
			s.setProblem("Couldn't start the relay: " + err.Error())
			if rlog != nil {
				rlog.Close()
			}
			time.Sleep(wait)
			continue
		}
		os.WriteFile(filepath.Join(cdir(), "relay.pid"), []byte(strconv.Itoa(c.Process.Pid)), 0o600)
		log.Printf("relay started (pid %d)", c.Process.Pid)
		s.mu.Lock()
		s.cmd, s.started, s.problem = c, time.Now(), ""
		s.mu.Unlock()
		werr := c.Wait()
		log.Printf("relay stopped (%v)", werr)
		if rlog != nil {
			rlog.Close()
		}
		s.mu.Lock()
		s.cmd = nil
		ran := time.Since(s.started)
		s.mu.Unlock()
		// Back off when it keeps stopping right away; a restart (or an update
		// that killed it on purpose) comes back in 3 s.
		if ran > time.Minute {
			wait = 3 * time.Second
		} else if wait < time.Minute {
			wait *= 2
		}
		time.Sleep(wait)
	}
}

func (s *supervisor) setProblem(p string) {
	s.mu.Lock()
	if p != s.problem {
		log.Print(p)
	}
	s.problem = p
	s.mu.Unlock()
}

// restart stops the relay and the agents it started; run() starts it again.
func (s *supervisor) restart() {
	s.mu.Lock()
	c := s.cmd
	s.mu.Unlock()
	if c != nil && c.Process != nil {
		exec.Command("taskkill", "/PID", strconv.Itoa(c.Process.Pid), "/T", "/F").Run()
	}
}

func (s *supervisor) stop() {
	s.mu.Lock()
	s.quitting = true
	s.mu.Unlock()
	s.restart()
}

// ── What the relay reports ───────────────────────────────────────────────

type jobsReply struct {
	Running []json.RawMessage `json:"running"`
	Today   struct {
		Jobs, Done, Failed int
		Seconds            float64
	} `json:"today"`
}

var client = &http.Client{Timeout: 4 * time.Second}

func getJSON(path string, v any) error {
	resp, err := client.Get(relayURL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// state: "ready", "working" or "stopped", and a line for the tray.
func state(s *supervisor) (string, string) {
	var j jobsReply
	if getJSON("/api/cerver/jobs?n=1", &j) != nil {
		s.mu.Lock()
		p := s.problem
		s.mu.Unlock()
		if p != "" {
			return "stopped", p
		}
		return "stopped", "Relay starting…"
	}
	jobs := fmt.Sprintf("%d job", j.Today.Jobs)
	if j.Today.Jobs != 1 {
		jobs += "s"
	}
	if n := len(j.Running); n > 0 {
		return "working", fmt.Sprintf("Working on %d · %s today", n, jobs)
	}
	return "ready", "Ready · " + jobs + " today"
}

// ── Tray ─────────────────────────────────────────────────────────────────

func tray(s *supervisor) {
	systray.SetIcon(icon("stopped"))
	systray.SetTitle("Cerver")
	systray.SetTooltip("Cerver")
	status := systray.AddMenuItem("Relay starting…", "")
	status.Disable()
	systray.AddSeparator()
	open := systray.AddMenuItem("Open Cerver", "This PC's agents, jobs and relay")
	dash := systray.AddMenuItem("Open the dashboard", dashboard)
	systray.AddSeparator()
	restart := systray.AddMenuItem("Restart the relay", "")
	update := systray.AddMenuItem("Check for updates", "")
	login := systray.AddMenuItemCheckbox("Start with Windows", "", startsWithWindows())
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit Cerver", "Stops the relay: this PC leaves your pool until Cerver starts again")

	go func() {
		last := ""
		for {
			st, line := state(s)
			status.SetTitle(line)
			systray.SetTooltip("Cerver: " + line)
			if st != last {
				systray.SetIcon(icon(st))
				last = st
			}
			time.Sleep(3 * time.Second)
		}
	}()
	go func() {
		for {
			select {
			case <-open.ClickedCh:
				openURL("http://" + pageAddr)
			case <-dash.ClickedCh:
				openURL(dashboard)
			case <-restart.ClickedCh:
				s.restart()
			case <-update.ClickedCh:
				openURL("http://" + pageAddr + "/#update") // the page runs it and shows what changed
			case <-login.ClickedCh:
				if login.Checked() {
					exec.Command("reg", "delete", runKey, "/v", "Cerver", "/f").Run()
				} else {
					exe, _ := os.Executable()
					exec.Command("reg", "add", runKey, "/v", "Cerver", "/t", "REG_SZ", "/d", `"`+exe+`"`, "/f").Run()
				}
				if startsWithWindows() {
					login.Check()
				} else {
					login.Uncheck()
				}
			case <-quit.ClickedCh:
				leaving = true
				systray.Quit()
				return
			}
		}
	}()
}

func startsWithWindows() bool {
	c := exec.Command("reg", "query", runKey, "/v", "Cerver")
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: noWindow}
	return c.Run() == nil
}

func openURL(u string) {
	exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}

// ── Updates ──────────────────────────────────────────────────────────────

var updMu sync.Mutex

// runUpdate: cerver update --auto (it never restarts the relay mid-job).
func runUpdate() string {
	updMu.Lock()
	defer updMu.Unlock()
	c := exec.Command(cerverBin(), "update", "--auto")
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: noWindow}
	out, err := c.CombinedOutput()
	if err != nil {
		out = append(out, []byte("\n"+err.Error())...)
	}
	if f, e := os.OpenFile(filepath.Join(cdir(), "update.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); e == nil {
		f.Write(out)
		f.Close()
	}
	return string(out)
}

func updates(s *supervisor) {
	exe, _ := os.Executable()
	mine := modTime(exe)
	time.Sleep(2 * time.Minute)
	for {
		runUpdate()
		// A new CerverApp.exe was put in place: start it and step aside, between jobs.
		if t := modTime(exe); !t.IsZero() && t.After(mine) {
			if st, _ := state(s); st != "working" {
				relaunch(s, exe)
			}
		}
		time.Sleep(6 * time.Hour)
	}
}

func relaunch(s *supervisor, exe string) {
	c := exec.Command("cmd", "/c", "timeout /t 3 /nobreak >nul & start \"\" \""+exe+"\"")
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: noWindow}
	if c.Start() == nil {
		leaving = true
		s.stop()
		systray.Quit()
		os.Exit(0) // also when there's no tray to quit
	}
}

func modTime(p string) time.Time {
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// ── The page ─────────────────────────────────────────────────────────────

func servePage(s *supervisor) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, pageHTML)
	})
	// Read-only views of the relay's own local endpoints.
	for _, p := range []string{"/health", "/api/cerver/jobs", "/api/local-claude/stats", "/api/local-claude/logs/tail"} {
		path := p
		mux.HandleFunc("/relay"+path, func(w http.ResponseWriter, r *http.Request) {
			resp, err := client.Get(relayURL + path + "?" + r.URL.RawQuery)
			if err != nil {
				http.Error(w, "relay not answering", 502)
				return
			}
			defer resp.Body.Close()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
		})
	}
	mux.HandleFunc("/app/state", func(w http.ResponseWriter, r *http.Request) {
		st, line := state(s)
		json.NewEncoder(w).Encode(map[string]any{"state": st, "line": line, "version": version.Version, "relay": relayVersion()})
	})
	// Actions only from this page (same origin), never from another site.
	post := func(f func() any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.Header.Get("X-Cerver") != "1" {
				http.Error(w, "no", 403)
				return
			}
			json.NewEncoder(w).Encode(f())
		}
	}
	mux.HandleFunc("/app/restart", post(func() any { s.restart(); return map[string]bool{"ok": true} }))
	mux.HandleFunc("/app/update", post(func() any { return map[string]string{"output": runUpdate()} }))
	srv := &http.Server{Addr: pageAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	srv.ListenAndServe()
}
