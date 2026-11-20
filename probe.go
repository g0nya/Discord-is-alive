package main

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type probeTarget struct {
	name, url string
	validate  func(*http.Response) error
}

var discordTargets = []probeTarget{
	{"API", "https://discord.com/api/v10/gateway", func(r *http.Response) error {
		var data struct{ URL string }
		if r.StatusCode != 200 || json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&data) != nil ||
			strings.TrimSuffix(data.URL, "/") != "wss://gateway.discord.gg" {
			return fmt.Errorf("unexpected gateway discovery response (HTTP %d)", r.StatusCode)
		}
		return nil
	}},
	{"Gateway", "https://gateway.discord.gg/?v=10&encoding=json", func(r *http.Response) error {
		// SHA-1 is mandated by the WebSocket Upgrade protocol.
		digest := sha1.Sum([]byte(r.Request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		if r.StatusCode != 101 || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
			r.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(digest[:]) {
			return fmt.Errorf("WebSocket upgrade failed (HTTP %d)", r.StatusCode)
		}
		return nil
	}},
	{"CDN", "https://cdn.discordapp.com/embed/avatars/0.png", func(r *http.Response) error {
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "image/") {
			return fmt.Errorf("unexpected CDN response (HTTP %d)", r.StatusCode)
		}
		return nil
	}},
}

type measurement struct {
	name    string
	elapsed time.Duration
	err     error
}

func probe(ctx context.Context, targets []probeTarget) []measurement {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport := &http.Transport{
		// Direct checks exercise the local packet path, without inheriting a proxy.
		DialContext:       (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		Protocols:         protocols,
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	results := make([]measurement, len(targets))
	done := make(chan struct{}, len(targets))
	for i, target := range targets {
		go func() {
			started := time.Now()
			err := requestProbe(ctx, client, target)
			results[i] = measurement{target.name, time.Since(started).Round(time.Millisecond), err}
			done <- struct{}{}
		}()
	}
	for range results {
		<-done
	}
	return results
}

func requestProbe(ctx context.Context, client *http.Client, target probeTarget) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.url, nil)
	if err != nil {
		return err
	}
	if target.name == "Gateway" {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		var nonce [16]byte
		rand.Read(nonce[:])
		req.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(nonce[:]))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return target.validate(resp)
}
