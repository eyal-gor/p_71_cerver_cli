//go:build linux

// Package cervernet is Cerver's network engine for the Android app (built
// with gomobile). It's the same engine the Mac and Linux service runs
// (internal/wgnet): direct connections, Cerver's backup route when direct
// fails, and following devices that move. Plus a name server inside the app
// (dns.go), so the phone reaches computers by name.
package cervernet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/wgnet"
	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

var (
	mu      sync.Mutex
	running *engine
)

type engine struct {
	cancel context.CancelFunc
	dev    *device.Device
	node   *wgnet.Node
	relay  *wgnet.RelayClient
}

// NewPrivateKey makes a device key (base64).
func NewPrivateKey() (string, error) {
	k, err := wgnet.NewKey()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k.Private[:]), nil
}

func key(privB64 string) (wgnet.Key, error) {
	var k wgnet.Key
	raw, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil || len(raw) != 32 {
		return k, errors.New("bad device key")
	}
	copy(k.Private[:], raw)
	pub, err := curve25519.X25519(k.Private[:], curve25519.Basepoint)
	if err != nil {
		return k, err
	}
	copy(k.Public[:], pub)
	return k, nil
}

// PublicKey is the device's public key (base64), for its first check-in.
func PublicKey(privB64 string) (string, error) {
	k, err := key(privB64)
	if err != nil {
		return "", err
	}
	return k.PublicB64(), nil
}

// Start runs the engine on the VPN's tunnel (fd, owned from now on) until Stop.
// The app must already be checked in (its address is on the tunnel).
func Start(fd int32, privB64, gatewayURL, token, name string) error {
	Stop()
	k, err := key(privB64)
	if err != nil {
		return err
	}
	tdev, _, err := tun.CreateUnmonitoredTUNFromFD(int(fd))
	if err != nil {
		return err
	}
	nm := &names{}
	t := &dnsTUN{Device: tdev, names: nm}
	bind := wgnet.NewBind()
	dev := device.NewDevice(t, bind, device.NewLogger(device.LogLevelError, "cervernet: "))
	gw := gateway.New(token)
	gw.BaseURL = gatewayURL
	relay := &wgnet.RelayClient{URL: gatewayURL, Token: token, PublicB64: k.PublicB64(), Bind: bind, Log: func(string, ...any) {}}
	bind.SetRelay(relay)
	node := &wgnet.Node{
		Port: k.StablePort(), Dev: dev, Bind: bind, Key: k, Name: name, OS: "android", Relay: true,
		Log: func(string, ...any) {},
		CheckIn: func(ctx context.Context, r wgnet.CheckInRequest) (*wgnet.CheckInAnswer, error) {
			var ans wgnet.CheckInAnswer
			return &ans, gw.Do(ctx, "POST", "/v2/net/nodes", r, &ans)
		},
	}
	if err := node.Start(); err != nil {
		dev.Close()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &engine{cancel: cancel, dev: dev, node: node, relay: relay}
	mu.Lock()
	running = e
	mu.Unlock()
	go e.run(ctx, nm)
	return nil
}

func (e *engine) run(ctx context.Context, nm *names) {
	relayOn := false
	tick := time.NewTicker(5 * time.Second)
	retry := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	defer retry.Stop()
	for {
		if err := e.node.Tick(ctx); err == nil {
			if !relayOn {
				relayOn = true
				go e.relay.Run(ctx) // after the first check-in: the relay admits registered devices
			}
			nm.set(nameMap(e.node.Answer))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-retry.C:
			e.node.Retry()
			continue
		}
	}
}

// nameMap: each device by its network name and by the name you gave it.
func nameMap(a *wgnet.CheckInAnswer) map[string]netip.Addr {
	m := map[string]netip.Addr{}
	if a == nil {
		return m
	}
	for _, p := range a.Peers {
		ip, err := netip.ParseAddr(p.IP)
		if err != nil {
			continue
		}
		for _, n := range []string{p.Label, p.Name} {
			if h := wgnet.HostName(n); h != "" {
				if _, taken := m[h]; !taken {
					m[h] = ip
				}
			}
		}
	}
	return m
}

// Stop ends the engine and closes the tunnel.
func Stop() {
	mu.Lock()
	e := running
	running = nil
	mu.Unlock()
	if e != nil {
		e.cancel()
		e.dev.Close()
	}
}

// Status is JSON: {"ip": "...", "peers": [{"name","ip","online","connected","via","label","os"}]}.
func Status() string {
	mu.Lock()
	e := running
	mu.Unlock()
	type peer struct {
		Name      string `json:"name"`
		Label     string `json:"label"`
		IP        string `json:"ip"`
		OS        string `json:"os"`
		Online    bool   `json:"online"`
		Connected bool   `json:"connected"`
		Via       string `json:"via"`
		Host      string `json:"host"`
	}
	out := struct {
		Running bool   `json:"running"`
		IP      string `json:"ip"`
		Peers   []peer `json:"peers"`
	}{}
	if e != nil && e.node.Answer != nil {
		out.Running = true
		out.IP = e.node.Answer.IP
		st := map[string]wgnet.PeerStatus{}
		for _, s := range e.node.Status() {
			st[s.IP] = s
		}
		for _, p := range e.node.Answer.Peers {
			s := st[p.IP]
			via := ""
			if s.Connected {
				via = "direct"
				if s.Endpoint == "Cerver relay" {
					via = "backup route"
				}
			}
			out.Peers = append(out.Peers, peer{p.Name, p.Label, p.IP, p.OS, p.Online, s.Connected, via, wgnet.HostName(p.Name)})
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}
