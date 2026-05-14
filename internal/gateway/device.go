package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DeviceStartResp mirrors the gateway's POST /v2/auth/device return
// shape. user_code is what we display to the user; device_code is the
// secret we poll with.
type DeviceStartResp struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// DevicePollResp mirrors GET /v2/auth/device?device_code=... — three
// shapes: pending, approved (with access_token), or error.
type DevicePollResp struct {
	Status      string `json:"status"`
	Error       string `json:"error"`
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
}

// StartDeviceAuth begins a CLI device-code flow. The user opens the
// returned verification_uri in a browser, signs in with magic link if
// not already, and confirms the user_code. The CLI keeps polling
// PollDeviceAuth(device_code) until approval.
//
// Unauthenticated — first half of the flow runs with no bearer (the
// user doesn't have a key yet; this whole flow is HOW they get one).
func (c *Client) StartDeviceAuth(ctx context.Context, machineName string) (*DeviceStartResp, error) {
	body, _ := json.Marshal(map[string]string{"machine_name": machineName})
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v2/auth/device", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device start: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("device start: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var out DeviceStartResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("device start: decode: %w", err)
	}
	return &out, nil
}

// PollDeviceAuth checks the status of an in-flight device login.
// Returns Status="approved" with AccessToken set when the user has
// completed approval in the browser. Returns Error="authorization_
// pending" (or empty) when still waiting. Other Error values:
// "expired_token", "access_denied", "invalid_device_code".
func (c *Client) PollDeviceAuth(ctx context.Context, deviceCode string) (*DevicePollResp, error) {
	url := c.BaseURL + "/v2/auth/device?device_code=" + deviceCode
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device poll: %w", err)
	}
	defer resp.Body.Close()
	// 202/200 are both expected here. >=400 is a real failure.
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("device poll: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var out DevicePollResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("device poll: decode: %w", err)
	}
	return &out, nil
}
