package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebSocketProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, err := base64.StdEncoding.DecodeString(r.Header.Get("Sec-WebSocket-Key"))
		if err != nil || len(key) != 16 || r.Header.Get("Connection") != "Upgrade" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		stream, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer stream.Close()
		digest := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
		if err != nil {
			t.Error(err)
		}
		if err := rw.Flush(); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	gateway := connectionTargets[1]
	gateway.url = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results := probe(ctx, []probeTarget{gateway})
	if results[0].err != nil {
		t.Fatal(results[0].err)
	}
}

func TestProfilePriorityAndScore(t *testing.T) {
	if got := orderedProfiles("disorder"); got[0].name != "disorder" || got[1].name != "split" {
		t.Fatalf("unexpected order: %v", got)
	}
	results := []measurement{{elapsed: 10 * time.Millisecond}, {elapsed: 20 * time.Millisecond}}
	if delay, ok := score(results); !ok || delay != 20*time.Millisecond {
		t.Fatalf("score = %v, %v", delay, ok)
	}
	results[1].err = errors.New("blocked")
	if _, ok := score(results); ok {
		t.Fatal("failed probe must disqualify a profile")
	}
}

func TestEngineArchive(t *testing.T) {
	if _, err := readArchive(bytes.NewBufferString("altered archive")); err == nil {
		t.Fatal("accepted invalid archive digest")
	}
	path := os.Getenv("DISCORDLINK_TEST_ARCHIVE")
	if path == "" {
		t.Skip("set DISCORDLINK_TEST_ARCHIVE to verify the pinned release archive")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := readArchive(file)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := extractEngine(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(engineFiles) {
		t.Fatalf("unexpected extracted files: %v", entries)
	}
	for _, name := range engineFiles {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.Size() == 0 {
			t.Fatalf("missing or empty engine file %s: %v", name, err)
		}
	}
}

type probeTransport func(*http.Request) (*http.Response, error)

func (f probeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestWindowsUpdateProbe(t *testing.T) {
	for _, tc := range []struct {
		name, packageHost string
		status            int
		want              string
	}{
		{"available", "stable.dl2.discordapp.net", 206, ""},
		{"blocked package CDN", "stable.dl2.discordapp.net", 403, "update package CDN"},
		{"untrusted host", "discordapp.net.example.com", 206, "official Windows package URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: probeTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				resp := &http.Response{Header: make(http.Header)}
				if req.URL.String() == updateManifestURL {
					resp.StatusCode = 200
					resp.Body = io.NopCloser(strings.NewReader(
						fmt.Sprintf(`{"full":{"url":"https://%s/distro/app/stable/win/x64/1.0.0/full.distro"}}`, tc.packageHost)))
					return resp, nil
				}
				if req.URL.Hostname() != tc.packageHost || req.Header.Get("Range") != "bytes=0-0" {
					t.Fatalf("unexpected package request: %v", req)
				}
				resp.StatusCode = tc.status
				resp.Header.Set("Content-Range", "bytes 0-0/123456")
				resp.Body = io.NopCloser(strings.NewReader("x"))
				return resp, nil
			})}
			err := requestProbe(context.Background(), client, connectionTargets[3])
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("update probe error = %v, wanted %q", err, tc.want)
			}
			if tc.name == "untrusted host" && calls != 1 {
				t.Fatalf("sent %d requests, wanted manifest only", calls)
			}
		})
	}
}
