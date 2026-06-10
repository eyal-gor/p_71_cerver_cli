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
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, string(body))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
