package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
)

// Forward makes an app on one of your machines show up here, peer to peer.
//
//	cerver forward <machine> <port>              # localhost:<port> here → <port> there
//	cerver forward <machine> <port> --local 8080 # a different port here
//
// <machine> is a name from `cerver pool` or a compute id. Each connection
// to the local port opens one encrypted WebRTC data channel straight to the
// machine, which connects to its own 127.0.0.1:<port>. Cerver's gateway only
// introduces the two; the app's traffic never passes through it. Your own
// machines only. If the link drops, it reconnects.
func Forward(args []string) error {
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	fs := flag.NewFlagSet("forward", flag.ContinueOnError)
	local := fs.Int("local", 0, "Port to open here (default: the same port)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos = append(pos, fs.Args()...)
	if len(pos) != 2 {
		return fmt.Errorf("usage: cerver forward <machine> <port> [--local <port>]")
	}
	port, err := strconv.Atoi(pos[1])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port must be a number from 1 to 65535")
	}
	if *local == 0 {
		*local = port
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	tok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return err
	}
	if tok == "" {
		return fmt.Errorf("no cerver credentials found — run cerver.ai/install.sh first")
	}
	gw := gateway.New(tok)
	machine, err := findOwnMachine(ctx, gw, pos[0])
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *local))
	if err != nil {
		return fmt.Errorf("can't open localhost:%d here: %w", *local, err)
	}
	defer ln.Close()

	f := &forwarder{gw: gw, computeID: machine.ComputeID, port: port}
	if err := f.connect(ctx); err != nil {
		return err
	}
	fmt.Printf("%s:%d is now at http://localhost:%d (direct, peer to peer). Ctrl-C to stop.\n", machine.Name, port, *local)
	go f.keepAlive(ctx, machine.Name)
	go func() { <-ctx.Done(); ln.Close() }()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				fmt.Println("\nStopped.")
				f.close()
				return nil
			}
			return err
		}
		go f.serve(conn)
	}
}

func findOwnMachine(ctx context.Context, gw *gateway.Client, want string) (*gateway.PoolMachine, error) {
	pool, _, err := gw.GetPool(ctx, "", "")
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range pool.Machines {
		m := &pool.Machines[i]
		if !m.Mine {
			continue
		}
		names = append(names, m.Name)
		if m.ComputeID == want || strings.EqualFold(m.Name, want) {
			if m.State == "offline" {
				return nil, fmt.Errorf("%s is offline. Start Cerver on it (`cerver connect`), then try again", m.Name)
			}
			return m, nil
		}
	}
	// Cloud computers (E2B, Vercel) aren't pool machines; find them by id or label.
	if computes, err := gw.ListComputes(ctx); err == nil {
		for _, c := range computes {
			if !strings.HasPrefix(c.ID, "comp_") {
				continue // provider entries, not machines
			}
			if c.ID == want || strings.EqualFold(c.Label, want) {
				return &gateway.PoolMachine{ComputeID: c.ID, Name: c.Label, Mine: true, State: "free"}, nil
			}
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("you have no machines in your pool yet. Add one with `cerver connect` on it")
	}
	return nil, fmt.Errorf("no machine of yours called %q. Yours: %s", want, strings.Join(names, ", "))
}

// forwarder holds one peer connection to the machine and reopens it when it drops.
type forwarder struct {
	gw        *gateway.Client
	computeID string
	port      int

	mu sync.Mutex
	pc *webrtc.PeerConnection
	up chan struct{} // closed when the current connection fails
}

func (f *forwarder) connect(ctx context.Context) error {
	se := webrtc.SettingEngine{}
	se.DetachDataChannels()
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.cloudflare.com:3478", "stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		return err
	}
	// An offer needs one data channel up front; the real ones open per connection.
	if _, err := pc.CreateDataChannel("hello", nil); err != nil {
		pc.Close()
		return err
	}
	connected, down := make(chan struct{}), make(chan struct{})
	var once, onceDown sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			once.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			onceDown.Do(func() { close(down) })
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return err
	}
	select {
	case <-gathered:
	case <-time.After(10 * time.Second):
	}

	var answer struct {
		SDP  string `json:"sdp"`
		Type string `json:"type"`
	}
	body := map[string]string{"sdp": pc.LocalDescription().SDP, "type": "offer"}
	if err := f.gw.Do(ctx, "POST", "/v2/computes/"+f.computeID+"/forward", body, &answer); err != nil {
		pc.Close()
		return err
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		pc.Close()
		return err
	}
	select {
	case <-connected:
	case <-down:
		pc.Close()
		return errors.New("couldn't reach the machine directly. Its network, or this one, blocks direct connections")
	case <-time.After(20 * time.Second):
		pc.Close()
		return errors.New("couldn't reach the machine directly within 20 s. Its network, or this one, may block direct connections")
	case <-ctx.Done():
		pc.Close()
		return ctx.Err()
	}

	f.mu.Lock()
	old := f.pc
	f.pc, f.up = pc, down
	f.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

// keepAlive reopens the link whenever it drops (machine restarted, network changed).
func (f *forwarder) keepAlive(ctx context.Context, name string) {
	for {
		f.mu.Lock()
		down := f.up
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-down:
		}
		fmt.Printf("Link to %s dropped; reconnecting…\n", name)
		for wait := time.Second; ; wait = backoff(wait) {
			if err := f.connect(ctx); err == nil {
				fmt.Printf("Reconnected to %s.\n", name)
				break
			} else if ctx.Err() != nil {
				return
			} else {
				fmt.Printf("  still down (%v); retrying in %s\n", err, wait)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}

func (f *forwarder) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pc != nil {
		f.pc.Close()
	}
}

// serve carries one local TCP connection over its own data channel.
func (f *forwarder) serve(conn net.Conn) {
	f.mu.Lock()
	pc := f.pc
	f.mu.Unlock()
	dc, err := pc.CreateDataChannel(fmt.Sprintf("tcp:%d", f.port), nil)
	if err != nil {
		conn.Close()
		return
	}
	dc.OnOpen(func() {
		raw, err := dc.Detach()
		if err != nil {
			conn.Close()
			return
		}
		go func() { // machine → here
			buf := make([]byte, 64*1024)
			for {
				n, err := raw.Read(buf)
				if n > 0 {
					if _, werr := conn.Write(buf[:n]); werr != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
			conn.Close()
		}()
		// here → machine, in messages small enough for every WebRTC stack
		io.CopyBuffer(raw, conn, make([]byte, 16*1024))
		raw.Close()
	})
}

func backoff(d time.Duration) time.Duration {
	if d*2 > 30*time.Second {
		return 30 * time.Second
	}
	return d * 2
}
