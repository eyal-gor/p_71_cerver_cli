package gateway

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// paymentRequiredError turns the gateway's 402 body into a friendly,
// actionable error. The body shape is { "error": "...", plus detail
// fields like checkout_url, kind, spent_usd, free_tier_usd } — see
// the gateway's CerverHttpError serialization.
//
// When a checkout_url is present we open it in the default browser so
// "I need to pay" requires zero thought: the page is already in front
// of the user when the error prints. Returns nil when the body wasn't
// parseable (caller falls back to the raw error).
func paymentRequiredError(body []byte) error {
	var payload struct {
		Error       string  `json:"error"`
		Kind        string  `json:"kind"`
		CheckoutURL string  `json:"checkout_url"`
		SpentUSD    float64 `json:"spent_usd"`
		FreeTierUSD float64 `json:"free_tier_usd"`
		LimitUSD    float64 `json:"limit_usd"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Error == "" {
		return nil
	}

	var b strings.Builder
	b.WriteString("payment required: ")
	b.WriteString(payload.Error)

	switch payload.Kind {
	case "subscription_required":
		if payload.CheckoutURL != "" {
			if openBrowser(payload.CheckoutURL) {
				b.WriteString("\n\n  → opened the checkout page in your browser")
			} else {
				b.WriteString("\n\n  → subscribe here: " + payload.CheckoutURL)
			}
			b.WriteString("\n  (metered: $2 per 1M tokens, billed monthly for what you use)")
		}
	case "spend_per_month", "harness_spend_per_month":
		b.WriteString("\n\n  → raise it: cerver.ai/dashboard/spending")
	}

	return fmt.Errorf("%s", b.String())
}

// openBrowser best-effort opens a URL in the user's default browser.
// Returns false when we couldn't (headless box, unknown platform) —
// the caller prints the URL instead.
func openBrowser(url string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return false
	}
	return cmd.Start() == nil
}
