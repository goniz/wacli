package main

import (
	"strings"
	"testing"
)

func TestSyncCommandExposesWebhookFlags(t *testing.T) {
	cmd := newSyncCmd(&rootFlags{})
	for _, name := range []string{"webhook", "webhook-secret", "webhook-allow-private", "webhook-events", "webhook-auth", "webhook-chat", "webhook-filter", "webhook-include-from-me"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s flag", name)
		}
	}
}

func TestSyncCommandWebhookEventsDefaultsToMessage(t *testing.T) {
	cmd := newSyncCmd(&rootFlags{})
	flag := cmd.Flags().Lookup("webhook-events")
	if flag == nil {
		t.Fatal("missing --webhook-events flag")
	}
	if flag.DefValue != "message" {
		t.Fatalf("--webhook-events default = %q, want \"message\"", flag.DefValue)
	}
}

func TestSyncCommandRejectsUnknownWebhookEvent(t *testing.T) {
	cmd := newSyncCmd(&rootFlags{})
	cmd.SetArgs([]string{"--webhook", "https://example.test/hook", "--webhook-events", "message,presence"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--webhook-events must be a comma-separated list of") {
		t.Fatalf("expected webhook-events validation error, got %v", err)
	}
}

func TestSyncCommandRequiresWebhookForEvents(t *testing.T) {
	cmd := newSyncCmd(&rootFlags{})
	cmd.SetArgs([]string{"--webhook-events", "receipt"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--webhook-events requires --webhook") {
		t.Fatalf("expected webhook-events validation error, got %v", err)
	}
}

func TestSyncCommandRequiresWebhookForSecret(t *testing.T) {
	cmd := newSyncCmd(&rootFlags{})
	cmd.SetArgs([]string{"--webhook-secret", "secret"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--webhook-secret requires --webhook") {
		t.Fatalf("expected webhook-secret validation error, got %v", err)
	}
}

func TestSyncCommandRejectsIneffectiveStaleThreshold(t *testing.T) {
	cmd := newSyncCmd(&rootFlags{})
	cmd.SetArgs([]string{"--stale-threshold", "2m20s"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--stale-threshold must be less than 2m20s") {
		t.Fatalf("expected stale-threshold validation error, got %v", err)
	}
}

func TestSyncCommandRejectsInvalidPresenceMode(t *testing.T) {
	cmd := newSyncCmd(&rootFlags{})
	cmd.SetArgs([]string{"--presence-mode", "loud"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--presence-mode must be one of: normal, quiet") {
		t.Fatalf("expected presence-mode validation error, got %v", err)
	}
}

func TestSyncCommandWebhookAuthDefaultsToHMAC(t *testing.T) {
	cmd := newSyncCmd(&rootFlags{})
	flag := cmd.Flags().Lookup("webhook-auth")
	if flag == nil {
		t.Fatal("missing --webhook-auth flag")
	}
	if flag.DefValue != "hmac" {
		t.Fatalf("--webhook-auth default = %q, want hmac", flag.DefValue)
	}
}

func TestSyncCommandGrokRequiresChatAndSecret(t *testing.T) {
	cmd := newSyncCmd(&rootFlags{})
	cmd.SetArgs([]string{"--webhook", "https://example.test/hook", "--webhook-auth", "grok", "--webhook-secret", "k"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--webhook-chat is required with --webhook-auth grok") {
		t.Fatalf("expected grok chat error, got %v", err)
	}

	cmd = newSyncCmd(&rootFlags{})
	cmd.SetArgs([]string{"--webhook", "https://example.test/hook", "--webhook-auth", "grok", "--webhook-chat", "15551234567@s.whatsapp.net"})
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--webhook-auth grok requires --webhook-secret") {
		t.Fatalf("expected grok secret error, got %v", err)
	}
}

func TestSyncCommandGrokRejectsNonMessageEvents(t *testing.T) {
	cmd := newSyncCmd(&rootFlags{})
	cmd.SetArgs([]string{
		"--webhook", "https://example.test/hook",
		"--webhook-auth", "grok",
		"--webhook-secret", "k",
		"--webhook-chat", "15551234567@s.whatsapp.net",
		"--webhook-events", "receipt",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--webhook-auth grok only posts message events") {
		t.Fatalf("expected grok events error, got %v", err)
	}
}

func TestSyncCommandRejectsInvalidWebhookFilter(t *testing.T) {
	cmd := newSyncCmd(&rootFlags{})
	cmd.SetArgs([]string{"--webhook", "https://example.test/hook", "--webhook-filter", "("})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "invalid --webhook-filter") {
		t.Fatalf("expected filter compile error, got %v", err)
	}
}

func TestSyncCommandGrokEnvRequiresChat(t *testing.T) {
	t.Setenv("WACLI_WEBHOOK_URL", "https://example.test/hook")
	t.Setenv("WACLI_WEBHOOK_AUTH", "grok")
	t.Setenv("WACLI_WEBHOOK_SECRET", "k")
	cmd := newSyncCmd(&rootFlags{})
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--webhook-chat is required with --webhook-auth grok") {
		t.Fatalf("expected grok env chat error, got %v", err)
	}
}
