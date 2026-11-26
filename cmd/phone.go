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
func Phone(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "pull":
		return phonePull(args)
	default:
		return fmt.Errorf("usage: cerver phone pull --photos|--files --reason \"why\" [--max 10] [--out dir] [--device name] [--json]")
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
