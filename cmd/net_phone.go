package cmd

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/eyal-gor/p_71_cerver_cli/internal/wgnet"
)

// `cerver net phone` puts a phone (or any device with the WireGuard app) on
// your network: it makes the phone a key, registers it, and shows a QR code
// to scan in the WireGuard app. The phone then reaches your computers by
// their addresses (100.96.0.x).
//
// What it can't do (the stock WireGuard app doesn't): use Cerver's network
// backup, or follow a computer whose address changes. So it connects best on
// the same Wi-Fi as your computers. Scan a new code if a computer moved.
//
// The phone's private key exists only in the QR code; nothing is saved.
func netPhone(args []string) error {
	fs := flag.NewFlagSet("net phone", flag.ContinueOnError)
	name := fs.String("name", "phone", "The phone's name on your network")
	config := fs.Bool("config", false, "Print the WireGuard config instead of a QR code (for the Mac app)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gw, err := netGateway(ctx)
	if err != nil {
		return err
	}
	key, err := wgnet.NewKey()
	if err != nil {
		return err
	}
	var ans struct {
		IP    string `json:"ip"`
		Peers []struct {
			Name      string   `json:"name"`
			IP        string   `json:"ip"`
			PublicKey string   `json:"public_key"`
			Endpoints []string `json:"endpoints"`
			OS        string   `json:"os"`
		} `json:"peers"`
	}
	body := map[string]any{"public_key": key.PublicB64(), "name": cleanDeviceName(*name), "endpoints": []string{}, "os": "phone"}
	if err := gw.Do(ctx, "POST", "/v2/net/nodes", body, &ans); err != nil {
		return err
	}

	var c strings.Builder
	fmt.Fprintf(&c, "[Interface]\nPrivateKey = %s\nAddress = %s/32\n", base64.StdEncoding.EncodeToString(key.Private[:]), ans.IP)
	n := 0
	for _, p := range ans.Peers {
		if p.OS == "phone" {
			continue // phones can't reach each other without an address to call
		}
		ep := pickPhoneEndpoint(p.Endpoints)
		if ep == "" {
			continue
		}
		fmt.Fprintf(&c, "\n# %s\n[Peer]\nPublicKey = %s\nAllowedIPs = %s/32\nEndpoint = %s\nPersistentKeepalive = 25\n", p.Name, p.PublicKey, p.IP, ep)
		n++
	}
	if n == 0 {
		return fmt.Errorf("none of your computers is on the network right now; turn one on and try again")
	}
	if *config {
		fmt.Print(c.String())
		return nil
	}
	fmt.Printf("Scan this in the WireGuard app on your phone (＋ → Create from QR code):\n\n")
	qrterminal.GenerateWithConfig(c.String(), qrterminal.Config{Level: qrterminal.L, Writer: os.Stdout, HalfBlocks: true, BlackChar: qrterminal.BLACK_BLACK, WhiteChar: qrterminal.WHITE_WHITE, BlackWhiteChar: qrterminal.BLACK_WHITE, WhiteBlackChar: qrterminal.WHITE_BLACK, QuietZone: 2})
	fmt.Printf("\n%s gets address %s. It reaches %d computer(s) by their 100.96.0.x addresses, best on the same Wi-Fi.\nRemove it later: cerver net rm %s\n", cleanDeviceName(*name), ans.IP, n, cleanDeviceName(*name))
	return nil
}

// pickPhoneEndpoint prefers a home-network address (a phone on the same Wi-Fi
// reaches it directly), then a public one.
func pickPhoneEndpoint(eps []string) string {
	var public string
	for _, e := range eps {
		ap, err := netip.ParseAddrPort(e)
		if err != nil {
			continue
		}
		if ap.Addr().Is4() && ap.Addr().IsPrivate() && !strings.HasPrefix(e, "100.") {
			return e
		}
		if public == "" && ap.Addr().Is4() && !ap.Addr().IsPrivate() {
			public = e
		}
	}
	return public
}

func cleanDeviceName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "phone"
	}
	return out
}
