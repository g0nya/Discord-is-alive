package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const (
	primaryRelayHost = "discordlink-telegram.g0nya.workers.dev"
	backupRelayHost  = "discordlink-reports-g0nya.pages.dev"
	publicRelayURL   = "https://" + primaryRelayHost + "/v1/public-report"
	eventReady       = "ready"
	eventFailure     = "failure"
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
	Digest       bool          `json:"include_in_digest,omitempty"`
	Snapshot     bool          `json:"snapshot,omitempty"`
	Installation string        `json:"installation_id,omitempty"`
	Event        string        `json:"event"`
	Version      string        `json:"version"`
	Profile      string        `json:"profile,omitempty"`
	Cause        string        `json:"cause,omitempty"`
	Checks       []reportCheck `json:"checks"`
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
	installation     string
	installationPath string
	cfg              relayConfig
	path             string
	history          deliveryHistory
	enabled          bool
	consentPath      string
	lastAttempt      map[string]time.Time
	readyDay         string
	lastSnapshot     time.Time
	lastError        string
	forceSnapshot    bool
	pending          chan delivery
	cancel           context.CancelFunc
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
	r := &reporter{cfg: cfg, path: filepath.Join(dir, "history.json"), consentPath: filepath.Join(dir, "consent-v3"), installationPath: filepath.Join(dir, "installation-id"), lastAttempt: make(map[string]time.Time)}
	if data, err := os.ReadFile(r.consentPath); err == nil {
		r.enabled = string(data) == "yes\n"
		if r.enabled {
			r.installation, err = installationID(r.installationPath)
			if err != nil {
				return nil, err
			}
		}
	}
	if data, err := os.ReadFile(r.path); err == nil {
		_ = json.Unmarshal(data, &r.history)
	}
	return r, nil
}

func installationID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if decoded, decodeErr := hex.DecodeString(id); decodeErr == nil && len(decoded) == 8 {
			return strings.ToLower(id), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var random [8]byte
	rand.Read(random[:])
	id := hex.EncodeToString(random[:])
	return id, writeAtomic(path, []byte(id+"\n"))
}

func (r *reporter) setEnabled(enabled bool) error {
	value := []byte("no\n")
	if enabled {
		id, err := installationID(r.installationPath)
		if err != nil {
			return err
		}
		r.installation = id
		value = []byte("yes\n")
	}
	if err := writeAtomic(r.consentPath, value); err != nil {
		return err
	}
	r.enabled = enabled
	r.forceSnapshot = enabled
	r.lastError = ""
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
				r.lastSnapshot = result.when
				r.lastError = ""
			} else if r.enabled {
				r.lastError = reportErrorText(result.err)
			}
			if result.err == nil && result.event != "snapshot" {
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
	if s.event != eventReady && s.event != eventFailure {
		return
	}
	event := s.event
	if event == eventReady && r.readyDay == "" {
		r.readyDay = now.Format(time.DateOnly)
	}
	if r.forceSnapshot {
		event = "snapshot"
	} else if !r.history.allows(event, now) || (event == eventReady && r.readyDay != now.Format(time.DateOnly)) {
		if now.Sub(r.lastSnapshot) < 15*time.Minute || (s.checkedAt.IsZero() && s.cause == "") {
			return
		}
		event = "snapshot"
	}
	gap := 2 * time.Minute
	if r.forceSnapshot {
		gap = 10 * time.Second
	}
	if now.Sub(r.lastAttempt[event]) < gap {
		return
	}
	r.forceSnapshot = false
	r.lastAttempt[event] = now
	requestCtx, cancel := context.WithTimeout(ctx, 14*time.Second)
	ch := make(chan delivery, 1)
	r.pending, r.cancel = ch, cancel
	payload := s.report()
	payload.Installation = r.installation
	payload.Digest = true
	payload.Snapshot = event == "snapshot"
	go func() {
		err := sendTelemetry(requestCtx, r.cfg, payload)
		ch <- delivery{event, now, err}
	}()
}

func sendTelemetry(ctx context.Context, cfg relayConfig, payload telemetryReport) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	backup := backupRelayURL(cfg.URL)
	if backup == "" {
		err, _ := sendRelay(ctx, cfg, data)
		return err
	}

	mainErr, beforeConnection := sendRelay(ctx, cfg, data)
	// Retry only before HTTP can have reached the relay. Individual
	// Telegram alerts must not be duplicated after a lost response.
	if mainErr == nil || !beforeConnection || ctx.Err() != nil {
		return mainErr
	}
	cfg.URL = backup
	backupErr, _ := sendRelay(ctx, cfg, data)
	if backupErr == nil {
		return nil
	}
	return relayFallbackError{mainErr, backupErr}
}

func backupRelayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != primaryRelayHost ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "/v1/report" && u.Path != "/v1/public-report") {
		return ""
	}
	return "https://" + backupRelayHost + u.Path
}

func sendRelay(ctx context.Context, cfg relayConfig, data []byte) (err error, beforeConnection bool) {
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(data))
	if err != nil {
		return err, false
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Key != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Key)
	}
	req.Header.Set("User-Agent", "DiscordLink/"+version)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 3 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 4 * time.Second
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err, !connected.Load()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return relayHTTPError(resp.StatusCode), false
	}
	return nil, false
}

type relayFallbackError struct {
	primary, backup error
}

func (e relayFallbackError) Error() string {
	return "workers.dev: " + e.primary.Error() + " | pages.dev: " + e.backup.Error()
}

type relayHTTPError int

func (status relayHTTPError) Error() string {
	return fmt.Sprintf("relay HTTP %d", status)
}

func reportErrorText(err error) string {
	var fallback relayFallbackError
	if errors.As(err, &fallback) {
		return "workers.dev: " + reportErrorText(fallback.primary) + " · pages.dev: " + reportErrorText(fallback.backup)
	}
	var invalid x509.CertificateInvalidError
	var untrusted x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var httpStatus relayHTTPError
	var dns *net.DNSError
	switch {
	case errors.As(err, &httpStatus):
		return fmt.Sprintf("ошибка HTTP %d", httpStatus)
	case errors.As(err, &dns):
		return "ошибка DNS"
	case errors.Is(err, context.DeadlineExceeded):
		return "сервер не ответил вовремя"
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return "сертификат вне срока действия (проверь дату и время Windows)"
	case errors.As(err, &untrusted):
		return "Windows не доверяет сертификату"
	case errors.As(err, &hostname):
		return "сертификат выдан для другого адреса"
	case strings.Contains(strings.ToLower(err.Error()), "tls"):
		return "сбой TLS-рукопожатия"
	default:
		return "не удалось соединиться с сервером"
	}
}

func (r *reporter) statusText() string {
	switch {
	case r == nil:
		return "Отчёты Telegram: проверь конфигурацию"
	case !r.enabled:
		return "Отчёты Telegram: выключены"
	case r.pending != nil:
		return "Отчёты Telegram: отправляется"
	case r.lastError != "":
		return "Отчёты Telegram: " + r.lastError
	case r.forceSnapshot || r.lastSnapshot.IsZero():
		return "Отчёты Telegram: ожидается первая отправка"
	default:
		return "Отчёт принят сервером в " + r.lastSnapshot.Format("15:04:05")
	}
}
