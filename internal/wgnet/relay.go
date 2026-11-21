package wgnet

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.zx2c4.com/wireguard/conn"
)

// The backup relay: when two devices can't reach each other directly, their
// WireGuard packets go through Cerver's relay (gateway v2/net/relay.ts). They
// are encrypted end to end already; the relay sees only which device a
// packet is for.
//
// To WireGuard the relay is just another address for a peer: "relay:<hex key>".
// Sending to it goes up our one WebSocket; packets from the relay arrive on a
// second receive path of the same Bind, so WireGuard also answers that way.

const relayPrefix = "relay:"

// RelayAddr is the address that sends to this peer through the relay.
func RelayAddr(publicKey [32]byte) string { return relayPrefix + hex.EncodeToString(publicKey[:]) }

type relayEndpoint struct{ key [32]byte }

func (e *relayEndpoint) ClearSrc()           {}
func (e *relayEndpoint) SrcToString() string { return "" }
func (e *relayEndpoint) DstToString() string { return RelayAddr(e.key) }
func (e *relayEndpoint) DstToBytes() []byte  { return e.key[:] }
func (e *relayEndpoint) DstIP() netip.Addr   { return netip.Addr{} }
func (e *relayEndpoint) SrcIP() netip.Addr   { return netip.Addr{} }

func parseRelayEndpoint(s string) (conn.Endpoint, bool) {
	if !strings.HasPrefix(s, relayPrefix) {
		return nil, false
	}
	raw, err := hex.DecodeString(s[len(relayPrefix):])
	if err != nil || len(raw) != 32 {
		return nil, false
	}
	e := &relayEndpoint{}
	copy(e.key[:], raw)
	return e, true
}

// RelaySender is the way out to the relay (the WebSocket, or a fake in tests).
type RelaySender interface {
	SendTo(to [32]byte, packet []byte) error
}

type relayPacket struct {
	from [32]byte
	data []byte
}

// FromRelay hands the Bind a packet that came through the relay.
func (b *Bind) FromRelay(from [32]byte, packet []byte) {
	select {
	case b.relayIn <- relayPacket{from, packet}:
	default: // full: drop, like a busy network would
	}
}

// SetRelay connects the Bind to a relay (nil: no relay).
func (b *Bind) SetRelay(r RelaySender) {
	b.mu.Lock()
	b.relay = r
	b.mu.Unlock()
}

func (b *Bind) receiveRelay(done chan struct{}) conn.ReceiveFunc {
	return func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		select {
		case p := <-b.relayIn:
			sizes[0] = copy(packets[0], p.data)
			eps[0] = &relayEndpoint{key: p.from}
			return 1, nil
		case <-done:
			return 0, net.ErrClosed
		}
	}
}

// RelayClient keeps one WebSocket to the account's relay open, reconnecting
// when it drops.
type RelayClient struct {
	URL       string // https://gateway.cerver.ai
	Token     string
	PublicB64 string
	Bind      *Bind
	Log       func(format string, a ...any)

	mu sync.Mutex
	ws *websocket.Conn
}

func (r *RelayClient) SendTo(to [32]byte, packet []byte) error {
	r.mu.Lock()
	ws := r.ws
	r.mu.Unlock()
	if ws == nil {
		return errors.New("relay not connected")
	}
	frame := make([]byte, 32+len(packet))
	copy(frame, to[:])
	copy(frame[32:], packet)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return ws.Write(ctx, websocket.MessageBinary, frame)
}

// Connected reports whether the relay socket is up.
func (r *RelayClient) Connected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ws != nil
}

// Run connects and keeps reconnecting until ctx ends.
func (r *RelayClient) Run(ctx context.Context) {
	wait := time.Second
	said := false
	for ctx.Err() == nil {
		err := r.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if !said && r.Log != nil {
			r.Log("Backup relay unavailable (%v); retrying.", err)
			said = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < 30*time.Second {
			wait *= 2
		}
	}
}

func (r *RelayClient) session(ctx context.Context) error {
	u := strings.Replace(strings.TrimRight(r.URL, "/"), "http", "ws", 1) + "/v2/net/relay?key=" + url.QueryEscape(r.PublicB64)
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ws, _, err := websocket.Dial(dctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + r.Token}}})
	cancel()
	if err != nil {
		return err
	}
	ws.SetReadLimit(64 << 10)
	r.mu.Lock()
	r.ws = ws
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.ws = nil
		r.mu.Unlock()
		ws.CloseNow()
	}()

	sctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { // keep NATs and proxies from closing an idle socket
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				wctx, c := context.WithTimeout(sctx, 5*time.Second)
				ws.Write(wctx, websocket.MessageText, []byte("ping"))
				c()
			}
		}
	}()
	for {
		typ, data, err := ws.Read(sctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageBinary || len(data) <= 32 {
			continue
		}
		var from [32]byte
		copy(from[:], data[:32])
		r.Bind.FromRelay(from, data[32:])
	}
}
