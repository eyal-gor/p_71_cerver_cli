// Package gateway is a thin HTTP client for the cerver gateway at
// gateway.cerver.ai. The API shape is documented in the cerver skill —
// this file just gives a typed wrapper around the JSON calls.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const DefaultURL = "https://gateway.cerver.ai"

type Client struct {
	BaseURL string
	Token   string
	http    *http.Client
}

func New(token string) *Client {
	return &Client{
		BaseURL: DefaultURL,
		Token:   token,
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
}

// Do is the low-level call. Decodes JSON into out if non-nil.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		// 402 = the gateway wants money before it does work (free tier
		// exhausted / no subscription, or a spending cap). Turn the
		// JSON blob into a human sentence and, when there's a checkout
		// URL, open it — the user shouldn't have to think about how
		// to go pay.
		if resp.StatusCode == http.StatusPaymentRequired {
			if err := paymentRequiredError(body); err != nil {
				return err
			}
		}
		// 403 app_key_required: sessions now need an app-scoped key. The CLI's
		// account-wide token can't create them — guide the user to wire one.
		if resp.StatusCode == http.StatusForbidden {
			if err := appKeyRequiredError(body); err != nil {
				return err
			}
		}
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, string(body))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// appKeyRequiredError turns the gateway's strict "sessions need an app-scoped
// key" 403 (code app_key_required) into a clear, actionable message. This
// machine's cerver token is account-wide and can't create sessions; the fix is
// to wire an app-scoped key as CERVER_CLI_APP_KEY, which run/compare use
// automatically. Returns nil for any other 403 (let the raw error through).
func appKeyRequiredError(body []byte) error {
	var p struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &p) != nil || p.Code != "app_key_required" {
		return nil
	}
	return fmt.Errorf("%s\n\nThis machine's cerver key is account-wide; sessions now require an app-scoped key. Fix it once:\n"+
		"  1. cerver apps                           # list your app slugs\n"+
		"  2. cerver keys create --app <app-slug>   # mint a ck_ app key\n"+
		"  3. echo 'CERVER_CLI_APP_KEY=<that ck_ key>' >> ~/.cerver/cerver.env\n"+
		"Then re-run — cerver run/compare pick up CERVER_CLI_APP_KEY automatically.", p.Error)
}
