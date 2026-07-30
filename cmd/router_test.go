package cmd

import (
	"errors"
	"testing"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
)

func sessionWith(reply, completed string) *gateway.Session {
	s := &gateway.Session{}
	if reply != "" {
		s.Transcript = append(s.Transcript, gateway.TranscriptEntry{Role: "assistant", Kind: "text", Content: reply})
	}
	if completed != "" {
		s.Transcript = append(s.Transcript, gateway.TranscriptEntry{Role: "system", Kind: "session_completed", Content: completed})
	}
	return s
}

func TestClassifyReply(t *testing.T) {
	if reason, ok := classifyReply(nil, errors.New("no reply within 2m")); ok || reason != "no reply in time" {
		t.Errorf("timeout: %q %v", reason, ok)
	}
	if _, ok := classifyReply(sessionWith("", ""), nil); ok {
		t.Error("empty reply must be busy")
	}
	if reason, ok := classifyReply(sessionWith("API Error: You have reached your specified API usage limits.", `{"exit_code":1}`), nil); ok || reason == "" {
		t.Errorf("rate limit must be busy, got %q %v", reason, ok)
	}
	if _, ok := classifyReply(sessionWith("all good, here is your answer", `{"exit_code":1}`), nil); ok {
		t.Error("non-zero exit must be busy")
	}
	if reason, ok := classifyReply(sessionWith("the answer is 42", `{"exit_code":0}`), nil); !ok {
		t.Errorf("good reply misclassified as busy: %q", reason)
	}
	if _, ok := classifyReply(sessionWith("the answer is 42", ""), nil); !ok {
		t.Error("good reply without completion event must be ok")
	}
}

func TestRouterHealthCooldown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := loadRouterHealth()
	if _, cooling := h.cooling("claude"); cooling {
		t.Error("fresh health must not cool")
	}
	h.markBusy("claude", "rate limit")
	h2 := loadRouterHealth()
	if reason, cooling := h2.cooling("claude"); !cooling || reason == "" {
		t.Error("busy harness must be cooling after reload")
	}
	h2.markOK("claude")
	if _, cooling := loadRouterHealth().cooling("claude"); cooling {
		t.Error("markOK must clear the cooldown")
	}
}
