package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportAllowlist(t *testing.T) {
	s := desktopStatus{
		event: eventFailure, cause: "no_profile", reason: "private detail",
		profile: "split",
		checks: []measurement{
			{name: "API", elapsed: 23 * time.Millisecond},
			{name: "Updates", err: fmt.Errorf("raw endpoint: %w", &net.DNSError{Name: "private.example"})},
		},
	}
	raw, err := json.Marshal(s.report())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "raw endpoint") {
		t.Fatalf("report exposed raw diagnostics: %s", raw)
	}
	if got := s.report(); got.Checks[0].MS != 23 || got.Checks[1].Code != "dns" || got.Cause != "no_profile" {
		t.Fatalf("unexpected sanitized report: %+v", got)
	}
}

func TestReportConsentAndDailyLimit(t *testing.T) {
	exeDir, dataDir := t.TempDir(), t.TempDir()
	cfg := relayConfig{URL: "https://relay.example/v1/report", Key: strings.Repeat("x", 64)}
	save := func() {
		raw, _ := json.Marshal(cfg)
		if err := os.WriteFile(filepath.Join(exeDir, "telemetry.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save()
	r, err := loadReporter(exeDir, dataDir)
	if err != nil || r == nil || r.enabled {
		t.Fatalf("unexpected initial reporter: %v, %v", r, err)
	}
	r.tick(context.Background(), desktopStatus{event: eventReady})
	if r.pending != nil {
		t.Fatal("report attempted without consent")
	}
	if err := r.setEnabled(true); err != nil {
		t.Fatal(err)
	}
	id := r.installation
	r, err = loadReporter(exeDir, dataDir)
	if err != nil || r == nil || !r.enabled || len(id) != 16 || r.installation != id {
		t.Fatalf("explicit consent was not saved: %v", err)
	}
	now := time.Now()
	r.history.ReadyDay = now.Format(time.DateOnly)
	r.history.Failure = now
	for _, event := range []string{eventReady, eventFailure} {
		r.tick(context.Background(), desktopStatus{event: event})
		if r.pending != nil {
			t.Fatalf("duplicate %s report attempted", event)
		}
	}
	cfg.URL = "https://another.example/v1/report"
	save()
	r, err = loadReporter(exeDir, dataDir)
	if err != nil || r == nil || r.enabled {
		t.Fatalf("changed destination inherited consent: %v", err)
	}
}

func TestBundledRelayIsOptIn(t *testing.T) {
	r, err := loadReporter(t.TempDir(), t.TempDir())
	if err != nil || r == nil || r.enabled || r.cfg.Key != "" || r.cfg.URL != publicRelayURL {
		t.Fatalf("unexpected bundled configuration: %v", err)
	}
	r.tick(context.Background(), desktopStatus{event: eventReady})
	if r.pending != nil {
		t.Fatal("bundled report attempted without consent")
	}
}
