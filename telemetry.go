package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	publicRelayURL = "https://discordlink-telegram.g0nya.workers.dev/v1/public-report"
	eventReady     = "ready"
	eventFailure   = "failure"
)

// The deployment file carries only a relay URL and its admission key.
type relayConfig struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

type reportCheck struct {
	Name string `json:"name"`
	Code string `json:"code"`
	MS   int64  `json:"ms,omitempty"`
}

type telemetryReport struct {
	Event   string        `json:"event"`
	Version string        `json:"version"`
	Profile string        `json:"profile,omitempty"`
	Cause   string        `json:"cause,omitempty"`
	Checks  []reportCheck `json:"checks"`
}

func (s desktopStatus) report() telemetryReport {
	result := telemetryReport{
		Event: s.event, Version: version, Profile: s.profile, Cause: s.cause,
		Checks: make([]reportCheck, 0, len(s.checks)),
	}
	for _, check := range s.checks {
		code := "ok"
		var dns *net.DNSError
		switch {
		case check.err == nil:
		case errors.As(check.err, &dns):
			code = "dns"
		case errors.Is(check.err, context.DeadlineExceeded):
			code = "timeout"
		case strings.Contains(strings.ToLower(check.err.Error()), "tls"):
			code = "tls"
		case strings.Contains(check.err.Error(), "HTTP "):
			code = "http"
		default:
			code = "connection"
		}
		item := reportCheck{Name: check.name, Code: code}
		if code == "ok" {
			item.MS = min(10000, check.elapsed.Milliseconds())
		}
		result.Checks = append(result.Checks, item)
	}
	return result
}

type deliveryHistory struct {
	ReadyDay string    `json:"ready_day"`
	Failure  time.Time `json:"failure"`
}

func (h deliveryHistory) allows(event string, now time.Time) bool {
	switch event {
	case eventReady:
		return h.ReadyDay != now.Format(time.DateOnly)
	case eventFailure:
		return h.Failure.IsZero() || now.Sub(h.Failure) >= time.Hour
	default:
		return false
	}
}

type delivery struct {
	event string
	when  time.Time
	err   error
}

type reporter struct {
	cfg         relayConfig
	path        string
	history     deliveryHistory
	enabled     bool
	consentPath string
	lastAttempt map[string]time.Time
	readyDay    string
	pending     chan delivery
	cancel      context.CancelFunc
}

func loadReporter(exeDir, dataDir string) (*reporter, error) {
	cfg := relayConfig{URL: publicRelayURL}
	raw, err := os.ReadFile(filepath.Join(exeDir, "telemetry.json"))
	if err == nil {
		cfg = relayConfig{}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		endpoint, err := url.Parse(cfg.URL)
		if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" ||
			endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
			endpoint.Path != "/v1/report" || len(cfg.Key) < 32 || len(cfg.Key) > 128 ||
			strings.IndexFunc(cfg.Key, func(r rune) bool {
				return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
			}) >= 0 {
			return nil, fmt.Errorf("invalid Telegram relay URL or admission key")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// Consent is scoped to the destination and enrollment key.
	identity := sha256.Sum256([]byte(cfg.URL + "\n" + cfg.Key))
	dir := filepath.Join(dataDir, "telemetry", fmt.Sprintf("%x", identity))
	r := &reporter{cfg: cfg, path: filepath.Join(dir, "history.json"), consentPath: filepath.Join(dir, "consent"), lastAttempt: make(map[string]time.Time)}
	if data, err := os.ReadFile(r.consentPath); err == nil {
		r.enabled = string(data) == "yes\n"
	}
	if data, err := os.ReadFile(r.path); err == nil {
		_ = json.Unmarshal(data, &r.history)
	}
	return r, nil
}

func (r *reporter) setEnabled(enabled bool) error {
	value := []byte("no\n")
	if enabled {
		value = []byte("yes\n")
	}
	if err := writeAtomic(r.consentPath, value); err != nil {
		return err
	}
	r.enabled = enabled
	if !enabled && r.cancel != nil {
		r.cancel()
	}
	return nil
}

func (r *reporter) tick(ctx context.Context, s desktopStatus) {
	if r.pending != nil {
		select {
		case result := <-r.pending:
			r.pending = nil
			r.cancel()
			r.cancel = nil
			if result.err == nil {
				if result.event == eventReady {
					r.history.ReadyDay = result.when.Format(time.DateOnly)
				} else {
					r.history.Failure = result.when
				}
				data, err := json.Marshal(r.history)
				if err == nil {
					if err := writeAtomic(r.path, data); err != nil {
						fmt.Fprintln(os.Stderr, "DiscordLink: could not save report delivery history")
					}
				}
			}
		default:
			return
		}
	}
	now := time.Now()
	if !r.enabled || ctx.Err() != nil {
		return
	}
	if s.event == eventReady {
		today := now.Format(time.DateOnly)
		if r.readyDay == "" {
			r.readyDay = today
		}
		if r.readyDay != today {
			return
		}
	}
	if !r.history.allows(s.event, now) || now.Sub(r.lastAttempt[s.event]) < 2*time.Minute {
		return
	}
	r.lastAttempt[s.event] = now
	requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	ch := make(chan delivery, 1)
	r.pending, r.cancel = ch, cancel
	payload := s.report()
	go func() {
		err := sendTelemetry(requestCtx, r.cfg, payload)
		ch <- delivery{s.event, now, err}
	}()
}

func sendTelemetry(ctx context.Context, cfg relayConfig, payload telemetryReport) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Key != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Key)
	}
	req.Header.Set("User-Agent", "DiscordLink/"+version)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("relay HTTP %d", resp.StatusCode)
	}
	return nil
}
