package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SessionCreate builds and POSTs a /v2/sessions request. Returns the
// new session_id.
type SessionCreate struct {
	SessionType string                 `json:"session_type"`
	Compute     map[string]any         `json:"compute"`
	Task        string                 `json:"task"`
	Workload    string                 `json:"workload"`
	SessionName string                 `json:"session_name,omitempty"`
	Metadata    map[string]any         `json:"metadata,omitempty"`
}

type SessionCreateResp struct {
	SessionID string `json:"session_id"`
}

// isAgentCapError returns true when the gateway/relay rejected a session
// create because the per-relay agent pool is full. The relay caps active
// agents at MAX_AGENTS and a burst of parallel `cerver compare` calls
// can briefly exceed it before previous one-shot agents finish cleaning
// up. The right user experience is to wait a beat and retry, not to
// surface a raw HTTP 500.
func isAgentCapError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Maximum number of agents")
}

func (c *Client) CreateSession(ctx context.Context, req SessionCreate) (string, error) {
	var resp SessionCreateResp
	// Retry with backoff if we hit the relay's MAX_AGENTS cap. 4 total
	// attempts, 1s/2s/4s sleeps — by the third retry the previous burst
	// of one-shot agents has almost always finished and freed slots. If
	// we still 500 after the last try, surface the error (the cap is
	// genuinely saturated or something else is wrong — let the user see).
	backoffs := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	var err error
	for attempt := 0; ; attempt++ {
		err = c.Do(ctx, "POST", "/v2/sessions", req, &resp)
		if err == nil || !isAgentCapError(err) || attempt >= len(backoffs) {
			break
		}
		select {
		case <-time.After(backoffs[attempt]):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if err != nil {
		return "", err
	}
	if resp.SessionID == "" {
		return "", fmt.Errorf("cerver returned no session_id")
	}
	return resp.SessionID, nil
}

// SendInput pushes a user message to a session. Without this the agent
// stays in `prepared` and never spawns the CLI.
func (c *Client) SendInput(ctx context.Context, sessionID, content string) error {
	return c.Do(ctx, "POST", fmt.Sprintf("/v2/sessions/%s/input", sessionID),
		map[string]string{"content": content, "role": "user"}, nil)
}

// SwitchTool continues the same session with a different CLI. The
// previous agent (if any) is paused; a new agent under `cliTool` is
// started with `content` as its next user message. Used by tests that
// exercise cross-CLI continuity — does claude → codex → grok preserve
// the conversation context, or does each switch reset?
//
// `content` is optional; the gateway treats an empty body as "just
// swap the tool, wait for the next /input". We always pass content
// here because the test framework wants the new agent to actually run
// in response to a prompt, not sit idle.
func (c *Client) SwitchTool(ctx context.Context, sessionID, cliTool, content string) error {
	body := map[string]any{"cli_tool": cliTool}
	if content != "" {
		body["content"] = content
	}
	return c.Do(ctx, "POST", fmt.Sprintf("/v2/sessions/%s/switch-tool", sessionID), body, nil)
}

// Session is the GET /v2/sessions/:id response, slimmed to fields the
// CLI actually reads. There's more in the wire JSON; we ignore the rest.
//
// When fetched with `?since=N`, Transcript only contains entries after
// index N and TranscriptTotal reflects the server's full count. With no
// cursor, TranscriptTotal == len(Transcript) (both = full count).
type Session struct {
	SessionID       string            `json:"sessionId"`
	Status          string            `json:"status"`
	ComputeID       string            `json:"computeId"`
	Metadata        map[string]any    `json:"metadata"`
	Transcript      []TranscriptEntry `json:"transcript"`
	Metrics         map[string]any    `json:"metrics"`
	TranscriptTotal int               `json:"transcriptTotal,omitempty"`
}

type TranscriptEntry struct {
	Role    string `json:"role"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
	At      string `json:"at"`
}

func (c *Client) GetSession(ctx context.Context, sessionID string) (*Session, error) {
	var s Session
	if err := c.Do(ctx, "GET", "/v2/sessions/"+sessionID, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) GetSessionFull(ctx context.Context, sessionID string) (*Session, error) {
	var s Session
	if err := c.Do(ctx, "GET", "/v2/sessions/"+sessionID+"?full=1", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) GetSessionTail(ctx context.Context, sessionID string, tail int) (*Session, error) {
	if tail < 0 {
		tail = 0
	}
	var s Session
	path := fmt.Sprintf("/v2/sessions/%s?tail=%d", sessionID, tail)
	if err := c.Do(ctx, "GET", path, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// GetSessionSince fetches only the transcript entries after `sinceIdx`.
// Server still returns the full session header (status, metadata,
// metrics). Used by WaitForReply during polling — slashes Neon data
// egress from ~5MB/poll on long sessions to a few KB.
//
// On the first poll, pass sinceIdx=0 to get the full transcript. Then
// use the returned `TranscriptTotal` as the cursor for subsequent calls.
func (c *Client) GetSessionSince(ctx context.Context, sessionID string, sinceIdx int) (*Session, error) {
	var s Session
	path := fmt.Sprintf("/v2/sessions/%s?since=%d", sessionID, sinceIdx)
	if err := c.Do(ctx, "GET", path, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SessionSummary is the slim shape returned in the list view — we don't
// pull full transcripts here, just enough to render `cerver sessions`.
type SessionSummary struct {
	SessionID    string         `json:"sessionId"`
	SessionName  string         `json:"sessionName"`
	Status       string         `json:"status"`
	ComputeID    string         `json:"computeId"`
	CreatedAt    string         `json:"createdAt"`
	UpdatedAt    string         `json:"updatedAt"`
	Workload     string         `json:"workload"`
	Metadata     map[string]any `json:"metadata"`
	// MessageCount is a denormalized count of transcript entries,
	// computed server-side via jsonb_array_length. Avoids shipping the
	// full transcript on list responses. Zero for older gateway builds
	// that don't yet emit this field.
	MessageCount int `json:"messageCount"`
}

type sessionsListResp struct {
	Sessions []SessionSummary `json:"sessions"`
}

// ListSessions returns the most recent sessions for the authenticated
// account, newest first. `limit` caps the page size (gateway typically
// honors up to 50).
func (c *Client) ListSessions(ctx context.Context, limit int) ([]SessionSummary, error) {
	if limit <= 0 {
		limit = 20
	}
	var resp sessionsListResp
	path := fmt.Sprintf("/v2/sessions?limit=%d", limit)
	if err := c.Do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// CliTool reads .metadata.cli_tool — used by the sessions list view so
// users can tell at a glance which CLI was running.
func (m SessionSummary) CliTool() string {
	if v, ok := m.Metadata["cli_tool"].(string); ok {
		return v
	}
	return ""
}

// CliModel reads .metadata.cli_model. The relay PATCHes this at run
// time with the actual model the CLI used (observed from its result
// event). A user-supplied --model override at session-create lives in
// the same key, but gets overwritten by the observed value once the
// CLI emits its first result.
func (m SessionSummary) CliModel() string {
	if v, ok := m.Metadata["cli_model"].(string); ok {
		return v
	}
	return ""
}

// ChangeCompute moves an existing session to a new compute. The new
// agent inherits the transcript and the same session_id is preserved;
// the old agent on the source compute is terminated.
func (c *Client) ChangeCompute(ctx context.Context, sessionID, computeID string) error {
	body := map[string]any{"compute_priority": []string{computeID}}
	return c.Do(ctx, "POST", "/v2/sessions/"+sessionID+"/change-compute", body, nil)
}

// LoginResp is the shape of /v2/auth/login. `api_key` is the bearer
// token to stash in ~/.cerver/cerver.env; `is_new` tells the caller
// whether this email created a fresh account.
type LoginResp struct {
	APIKey string `json:"api_key"`
	IsNew  bool   `json:"is_new"`
}

// Login (unauthenticated) — POST email, get back an API key.
// Note: this is the only Client method that doesn't require c.Token.
func (c *Client) Login(ctx context.Context, email string) (*LoginResp, error) {
	// We can't reuse Do() because it would inject an Authorization
	// header from c.Token (empty here). Hand-roll a small POST.
	body, _ := json.Marshal(map[string]string{"email": email})
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v2/auth/login",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("login: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var out LoginResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("login decode: %w", err)
	}
	if out.APIKey == "" {
		return nil, fmt.Errorf("login returned no api_key")
	}
	return &out, nil
}

// LastAssistantText pulls the most recent assistant text entry (skips
// tool_use / tool_result).
func (s *Session) LastAssistantText() string {
	for i := len(s.Transcript) - 1; i >= 0; i-- {
		e := s.Transcript[i]
		if e.Role == "assistant" && (e.Kind == "text" || e.Kind == "") {
			return e.Content
		}
	}
	return ""
}

// Usage pulls metadata.usage_total. Returns nil if the relay hasn't
// pushed usage yet (older relay build, or CLI crashed pre-result).
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	Turns        int `json:"turns"`
}

// WaitForReply polls the session until the agent is done emitting and
// returns the final Session snapshot. Two stop conditions:
//
//  1. The transcript has an assistant text entry, and no new transcript
//     entries have landed for `stable` seconds. This is the happy path
//     for tool-using agents: interim assistant text, tool calls, tool
//     results, and the final answer all count as activity.
//  2. Timeout: no stable reply arrived within `timeout`.
//
// Uses a transcript cursor (?since=N) so each poll only downloads the
// transcript entries added since the previous poll, not the entire
// session. On long-running sessions this drops the per-poll payload
// from megabytes to a few KB — the difference between a $87 Neon
// egress bill and a $3 one. Maintains a local merged transcript so
// LastAssistantText / IsDone see the full picture.
//
// Do not trust status alone for readiness. Some relay/provider paths can
// flip a session back to "ready" while a CLI is still emitting tool-use
// progress, so returning on "status != running" truncates Codex runs at
// the preamble. Transcript quiescence is the safer completion signal.
func (c *Client) WaitForReply(ctx context.Context, sessionID string, timeout time.Duration, stable time.Duration) (*Session, error) {
	return c.WaitForReplyFromCursor(ctx, sessionID, 0, timeout, stable)
}

// WaitForReplyFromCursor is the same as WaitForReply but starts the
// transcript cursor at `startCursor`. Used by chat to resume an
// existing session — without this the loop would return immediately
// after seeing the *previous* turn's assistant reply, because that's
// the most recent text in the transcript at the moment status flips
// back to "ready" between turns.
func (c *Client) WaitForReplyFromCursor(ctx context.Context, sessionID string, startCursor int, timeout time.Duration, stable time.Duration) (*Session, error) {
	start := time.Now()
	stableSince := time.Time{}

	merged := &Session{SessionID: sessionID}
	cursor := startCursor

	for {
		if time.Since(start) > timeout {
			return nil, fmt.Errorf("no reply within %s", timeout)
		}
		// 3s is the cost/responsiveness sweet spot. Cursor reads make the
		// per-poll payload tiny, so we're no longer paying the data-egress
		// penalty that pushed earlier versions to want shorter intervals;
		// at the same time, sub-second polling buys nothing on a
		// network-bound agent that takes seconds per turn.
		time.Sleep(3 * time.Second)

		s, err := c.GetSessionSince(ctx, sessionID, cursor)
		if err != nil {
			continue // transient — keep trying
		}
		// Merge: header fields always come from the latest response;
		// transcript entries are appended (gateway returns only new ones).
		merged.Status = s.Status
		merged.ComputeID = s.ComputeID
		merged.Metadata = s.Metadata
		merged.Metrics = s.Metrics
		merged.Transcript = append(merged.Transcript, s.Transcript...)
		// Advance the cursor. If the gateway echoed transcriptTotal,
		// trust it; otherwise fall back to our local count.
		if s.TranscriptTotal > 0 {
			cursor = s.TranscriptTotal
		} else {
			cursor = len(merged.Transcript)
		}
		merged.TranscriptTotal = cursor

		reply := merged.LastAssistantText()
		if reply == "" {
			continue
		}

		// Nothing new for `stable` seconds after at least one assistant
		// text entry — treat as done. We intentionally do not return just
		// because status is no longer "running": Codex can append several
		// progress/tool-use entries while the session header already says
		// "ready".
		// "Nothing new" = no transcript entries appended on this poll.
		// Previously this checked text-changes only, so a tool-using
		// agent (codex doing several rg/grep calls between text turns)
		// triggered the timeout in the middle of its investigation.
		// Any transcript activity (tool_use included) counts as alive.
		if len(s.Transcript) > 0 {
			stableSince = time.Now()
			continue
		}
		quietWindow := stable
		if looksLikeProgressReply(reply) {
			// Research/tool runs often begin with "I'll check..." and then
			// spend 20-40s in CLI-internal work before the next transcript
			// append. Treat those preambles as not-final unless they stay
			// quiet for a much longer window. If that longer window expires,
			// fail loudly instead of presenting the preamble as the answer.
			quietWindow = 90 * time.Second
		}
		if !stableSince.IsZero() && time.Since(stableSince) > quietWindow {
			if looksLikeProgressReply(reply) {
				return nil, fmt.Errorf("only progress text was captured; no final assistant answer after %s", quietWindow)
			}
			return merged, nil
		}
	}
}

func looksLikeProgressReply(reply string) bool {
	text := strings.ToLower(strings.TrimSpace(reply))
	if text == "" {
		return false
	}
	progressStarts := []string{
		"i'll ",
		"i’ll ",
		"i'm ",
		"i’m ",
		"i am ",
		"let me ",
		"checking ",
	}
	for _, prefix := range progressStarts {
		if strings.HasPrefix(text, prefix) {
			return strings.Contains(text, "check") ||
				strings.Contains(text, "look") ||
				strings.Contains(text, "search") ||
				strings.Contains(text, "run ") ||
				strings.Contains(text, "fetch") ||
				strings.Contains(text, "pull") ||
				strings.Contains(text, "rerun") ||
				strings.Contains(text, "extract") ||
				strings.Contains(text, "ground")
		}
	}
	return strings.Contains(text, "i have enough to answer, but")
}

func (s *Session) Usage() *Usage {
	if s.Metadata == nil {
		return nil
	}
	raw, ok := s.Metadata["usage_total"].(map[string]any)
	if !ok {
		return nil
	}
	intOr := func(v any) int {
		switch x := v.(type) {
		case float64:
			return int(x)
		case int:
			return x
		}
		return 0
	}
	return &Usage{
		InputTokens:  intOr(raw["input_tokens"]),
		OutputTokens: intOr(raw["output_tokens"]),
		Turns:        intOr(raw["turns"]),
	}
}
