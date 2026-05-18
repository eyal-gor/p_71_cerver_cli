// Package output formats the headers, tables, and cost lines printed
// by every cerver CLI command. Keeping it in one place means
// `cerver run` and `cerver compare` share a consistent visual.
package output

import (
	"fmt"
	"strings"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
)

// Pricing — current public list prices, per million tokens. Update
// when vendors change them. Used to compute the rate-card cost shown
// in every reply header (informational under subscription billing,
// actual charge under api billing).
type Rate struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

var Rates = map[string]Rate{
	"claude": {InputPerMTok: 3.00, OutputPerMTok: 15.00},  // Sonnet 4.6
	"codex":  {InputPerMTok: 1.25, OutputPerMTok: 10.00},  // gpt-5-codex
	"grok":   {InputPerMTok: 5.00, OutputPerMTok: 15.00},  // grok-4
}

// Cost returns the USD cost of a usage based on the CLI's current
// rate-card. For subscription billing this is informational only.
func Cost(cli string, u *gateway.Usage) float64 {
	if u == nil {
		return 0
	}
	r, ok := Rates[cli]
	if !ok {
		return 0
	}
	return (float64(u.InputTokens)*r.InputPerMTok + float64(u.OutputTokens)*r.OutputPerMTok) / 1_000_000
}

// Header builds the "==== claude (3s · billing=subscription · ... ) ===="
// line the user sees above each reply. billingMode is "subscription" or
// "api"; if "" the billing section is omitted (run with no --bill resolved
// to a default still passes the resolved value).
//
// The billing tag is rendered as `billing=subscription` / `billing=apikey`
// so it's unambiguous in a side-by-side compare — a glance tells you
// which CLI is on the user's OAuth subscription vs. which is per-token
// billed to a vendor api key.
func Header(cli string, elapsedSec int, billingMode string, u *gateway.Usage) string {
	parts := []string{fmt.Sprintf("%ds", elapsedSec)}
	switch billingMode {
	case "subscription":
		parts = append(parts, "billing=subscription · local OAuth")
	case "api":
		parts = append(parts, "billing=apikey · "+apiKeyEnvFor(cli))
	}
	if u != nil && (u.InputTokens > 0 || u.OutputTokens > 0) {
		parts = append(parts, fmt.Sprintf("%d in / %d out", u.InputTokens, u.OutputTokens))
		cost := Cost(cli, u)
		if cost > 0 {
			suffix := "billed"
			if billingMode == "subscription" {
				suffix = "rate-card, not billed"
			}
			parts = append(parts, fmt.Sprintf("$%.4f %s", cost, suffix))
		}
	}
	return fmt.Sprintf("==== %s (%s) ====", cli, strings.Join(parts, " · "))
}

func apiKeyEnvFor(cli string) string {
	switch cli {
	case "claude":
		return "ANTHROPIC_API_KEY"
	case "codex":
		return "OPENAI_API_KEY"
	case "grok":
		return "XAI_API_KEY"
	}
	return "API_KEY"
}
