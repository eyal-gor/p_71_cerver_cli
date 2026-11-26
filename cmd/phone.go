package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Phone asks your phone for photos or files, and waits for you to approve.
//
//	cerver phone pull --photos --reason "receipts for the expense report"
//	cerver phone pull --files  --reason "the signed contract PDF" --out ./in
//
// This computer opens a one-time upload address on its Cerver network address
// and posts the ask to the gateway. The Cerver app on your phone notifies you;
// Approve opens the phone's own picker and sends only what you pick, straight
// to this computer over the encrypted network. The files never pass through
// Cerver's gateway. Made for agents: --json prints the saved paths.
//
// The other way:
//
//	cerver phone send report.pdf photo.jpg --reason "the signed contract"
//
// The phone shows the names and sizes; Accept downloads them straight from
// this computer into the phone's Downloads/Cerver.
func Phone(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "pull":
		return phonePull(args)
	case "send":
		return phoneSend(args)
	default:
		return fmt.Errorf("usage:\n  cerver phone pull --photos|--files --reason \"why\" [--max 10] [--out dir] [--device name] [--json]\n  cerver phone send <file>... --reason \"why\" [--device name] [--json]")
	}
}

// myNetAddress is this computer's address on your Cerver network (100.96.x.y).
func myNetAddress() (string, error) {
	pfx := netip.MustParsePrefix("100.96.0.0/16")
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && pfx.Contains(p.Addr()) {
				return p.Addr().String(), nil
			}
		}
	}
	return "", fmt.Errorf("this computer isn't on your Cerver network; run: sudo cerver net up")
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._ -]+`)

// saveName keeps a phone's file name safe and unique inside dir.
func saveName(dir, name string, n int) string {
	base := strings.TrimSpace(unsafeName.ReplaceAllString(filepath.Base(name), "_"))
	if base == "" || base == "." || base == ".." {
		base = fmt.Sprintf("file-%d", n)
	}
	p := filepath.Join(dir, base)
	ext := filepath.Ext(base)
	for k := 2; ; k++ {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s-%d%s", strings.TrimSuffix(base, ext), k, ext))
	}
}

func phonePull(args []string) error {
	fs := flag.NewFlagSet("phone pull", flag.ContinueOnError)
	photos := fs.Bool("photos", false, "Ask for photos or videos (the phone's photo picker)")
	files := fs.Bool("files", false, "Ask for any files (the phone's file picker)")
	reason := fs.String("reason", "", "Why: shown on the phone (required)")
	max := fs.Int("max", 10, "At most this many")
	out := fs.String("out", "", "Where to save (default ./phone-<time>)")
	device := fs.String("device", "", "Which phone, by name (default: any of your phones)")
	wait := fs.Duration("wait", 10*time.Minute, "How long to wait for the phone")
	asJSON := fs.Bool("json", false, "Print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	kind := "photos"
	if *files && !*photos {
		kind = "files"
	}
	if strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("--reason is required: it's what you see on the phone before approving")
	}
	ip, err := myNetAddress()
	if err != nil {
		return err
	}
	dir := *out
	if dir == "" {
		dir = "phone-" + time.Now().Format("20060102-150405")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *wait+30*time.Second)
	defer cancel()
	gw, err := netGateway(ctx)
	if err != nil {
		return err
	}

	// Which phone: by name, or any.
	deviceID := ""
	if *device != "" {
		var st struct {
			Nodes []struct{ NodeID, Name, Label, OS string } `json:"nodes"`
		}
		if err := gw.Do(ctx, "GET", "/v2/net", nil, &st); err != nil {
			return err
		}
		for _, n := range st.Nodes {
			if strings.EqualFold(n.Name, *device) || strings.EqualFold(n.Label, *device) {
				deviceID = n.NodeID
			}
		}
		if deviceID == "" {
			return fmt.Errorf("no device named %q in your network (see: cerver net)", *device)
		}
	}

	// The one-time upload address, on this computer's network address only.
	tb := make([]byte, 16)
	rand.Read(tb)
	token := hex.EncodeToString(tb)
	ln, err := net.Listen("tcp", ip+":0")
	if err != nil {
		return fmt.Errorf("couldn't open the upload address: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	type result struct {
		paths []string
		err   error
	}
	got := make(chan result, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/"+token, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST files here", 405)
			return
		}
		used := true
		once.Do(func() { used = false })
		if used {
			http.Error(w, "already used", 410)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "send multipart/form-data", 400)
			got <- result{err: err}
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			http.Error(w, err.Error(), 500)
			got <- result{err: err}
			return
		}
		var paths []string
		for len(paths) < *max {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, "upload broke off", 400)
				got <- result{paths: paths, err: err}
				return
			}
			if part.FileName() == "" {
				continue
			}
			p := saveName(dir, part.FileName(), len(paths)+1)
			f, err := os.Create(p)
			if err == nil {
				_, err = io.Copy(f, part)
				f.Close()
			}
			if err != nil {
				http.Error(w, "couldn't save", 500)
				got <- result{paths: paths, err: err}
				return
			}
			paths = append(paths, p)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"received": len(paths)})
		got <- result{paths: paths}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 20 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	var ask struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := gw.Do(ctx, "POST", "/v2/net/asks", map[string]any{
		"kind": kind, "reason": *reason, "max": *max, "from": host, "device": deviceID,
		"upload_url": fmt.Sprintf("http://%s:%d/%s", ip, port, token),
	}, &ask); err != nil {
		return err
	}
	if !*asJSON {
		fmt.Fprintf(os.Stderr, "Asked your phone for %s: %q\nApprove it in the Cerver app on the phone (Cerver must be on). Waiting...\n", kind, *reason)
	}

	finish := func(status string, paths []string, err error) error {
		if *asJSON {
			b, _ := json.Marshal(map[string]any{"ask": ask.ID, "status": status, "files": paths, "dir": dir})
			fmt.Println(string(b))
		} else if len(paths) > 0 {
			for _, p := range paths {
				fmt.Println(p)
			}
		}
		if err != nil {
			return err
		}
		if status != "received" {
			return fmt.Errorf("the phone %s the ask", status)
		}
		return nil
	}

	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	deadline := time.After(*wait)
	for {
		select {
		case r := <-got:
			return finish("received", r.paths, r.err)
		case <-deadline:
			return finish("expired", nil, fmt.Errorf("no answer from the phone in %s", *wait))
		case <-tick.C:
			var st struct{ Status string }
			if gw.Do(ctx, "GET", "/v2/net/asks/"+ask.ID, nil, &st) == nil && (st.Status == "denied" || st.Status == "expired") {
				// An approve lands after the upload; a deny or expiry ends it here.
				select {
				case r := <-got:
					return finish("received", r.paths, r.err)
				case <-time.After(2 * time.Second):
				}
				return finish(st.Status, nil, nil)
			}
		}
	}
}

// phoneDevice finds a phone by name, or "" for any of your phones.
func phoneDevice(ctx context.Context, gw interface {
	Do(context.Context, string, string, any, any) error
}, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	var st struct {
		Nodes []struct{ NodeID, Name, Label, OS string } `json:"nodes"`
	}
	if err := gw.Do(ctx, "GET", "/v2/net", nil, &st); err != nil {
		return "", err
	}
	for _, n := range st.Nodes {
		if strings.EqualFold(n.Name, name) || strings.EqualFold(n.Label, name) {
			return n.NodeID, nil
		}
	}
	return "", fmt.Errorf("no device named %q in your network (see: cerver net)", name)
}

// splitFlags lets files and flags come in any order: `send a.pdf --reason x b.jpg`.
func splitFlags(args []string, valued map[string]bool) (flags, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if valued[strings.TrimLeft(a, "-")] && !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		rest = append(rest, a)
	}
	return
}

func phoneSend(args []string) error {
	fs := flag.NewFlagSet("phone send", flag.ContinueOnError)
	reason := fs.String("reason", "", "Why: shown on the phone (required)")
	device := fs.String("device", "", "Which phone, by name (default: any of your phones)")
	wait := fs.Duration("wait", 10*time.Minute, "How long to wait for the phone")
	asJSON := fs.Bool("json", false, "Print the result as JSON")
	flags, paths := splitFlags(args, map[string]bool{"reason": true, "device": true, "wait": true})
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("usage: cerver phone send <file>... --reason \"why\"")
	}
	if len(paths) > 50 {
		return fmt.Errorf("at most 50 files at a time")
	}
	if strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("--reason is required: it's what you see on the phone before accepting")
	}
	type offer struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		path string
	}
	var offers []offer
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if st.IsDir() {
			return fmt.Errorf("%s is a folder; send files", p)
		}
		offers = append(offers, offer{Name: filepath.Base(p), Size: st.Size(), path: p})
	}
	ip, err := myNetAddress()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait+30*time.Second)
	defer cancel()
	gw, err := netGateway(ctx)
	if err != nil {
		return err
	}
	deviceID, err := phoneDevice(ctx, gw, *device)
	if err != nil {
		return err
	}

	tb := make([]byte, 16)
	rand.Read(tb)
	token := hex.EncodeToString(tb)
	ln, err := net.Listen("tcp", ip+":0")
	if err != nil {
		return fmt.Errorf("couldn't open the download address: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	var mu sync.Mutex
	done := map[int]bool{}
	all := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/"+token+"/", func(w http.ResponseWriter, r *http.Request) {
		var i int
		if _, err := fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/"+token+"/"), "%d", &i); err != nil || i < 0 || i >= len(offers) || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(offers[i].path)
		if err != nil {
			http.Error(w, "gone", 410)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(offers[i].Size))
		n, err := io.Copy(w, f)
		if err == nil && n == offers[i].Size {
			mu.Lock()
			done[i] = true
			if len(done) == len(offers) {
				select {
				case <-all:
				default:
					close(all)
				}
			}
			mu.Unlock()
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 20 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	var ask struct {
		ID string `json:"id"`
	}
	if err := gw.Do(ctx, "POST", "/v2/net/asks", map[string]any{
		"kind": "send", "reason": *reason, "from": host, "device": deviceID, "files": offers,
		"upload_url": fmt.Sprintf("http://%s:%d/%s", ip, port, token),
	}, &ask); err != nil {
		return err
	}
	if !*asJSON {
		fmt.Fprintf(os.Stderr, "Offered %d file(s) to your phone: %q\nAccept it in the Cerver app on the phone (Cerver must be on). Waiting...\n", len(offers), *reason)
	}
	finish := func(status string) error {
		mu.Lock()
		n := len(done)
		mu.Unlock()
		if *asJSON {
			b, _ := json.Marshal(map[string]any{"ask": ask.ID, "status": status, "sent": n, "offered": len(offers)})
			fmt.Println(string(b))
		} else if status == "sent" {
			fmt.Printf("Sent %d file(s) to your phone (Downloads/Cerver).\n", n)
		}
		if status != "sent" {
			return fmt.Errorf("the phone %s the files", status)
		}
		return nil
	}
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	deadline := time.After(*wait)
	for {
		select {
		case <-all:
			return finish("sent")
		case <-deadline:
			return finish("expired")
		case <-tick.C:
			var st struct{ Status string }
			if gw.Do(ctx, "GET", "/v2/net/asks/"+ask.ID, nil, &st) == nil && (st.Status == "denied" || st.Status == "expired") {
				return finish(map[string]string{"denied": "declined", "expired": "didn't take"}[st.Status])
			}
		}
	}
}
