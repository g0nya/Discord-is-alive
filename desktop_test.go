package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRecoverySchedule(t *testing.T) {
	now := time.Unix(0, 0)
	s := recoverySchedule{network: "wifi-a"}
	s.observeNetwork(now, "wifi-b")
	if s.network != "wifi-b" || !s.nextCheck.Equal(now.Add(networkGrace)) {
		t.Fatal("changed network did not schedule a check")
	}
	now = s.nextCheck
	s.observeNetwork(now, "wifi-b")
	if !s.nextCheck.Equal(now) {
		t.Fatal("unchanged network postponed a due check")
	}
	if s.recoverAfterCheck(now, false) {
		t.Fatal("one failed check must leave the engine running")
	}
	now = s.nextCheck
	if s.recoverAfterCheck(now, true) || s.failed {
		t.Fatal("a good confirmation must leave the engine running and clear the failure")
	}
	now = s.nextCheck
	if s.recoverAfterCheck(now, false) || !s.recoverAfterCheck(s.nextCheck, false) {
		t.Fatal("two consecutive failures must initiate recovery")
	}
}

func TestYouTubeFailurePreservesDiscord(t *testing.T) {
	checks := []measurement{
		{name: "API", elapsed: 30 * time.Millisecond},
		{name: youtubeProbe, err: context.DeadlineExceeded},
		{name: youtubeMediaProbe, elapsed: 50 * time.Millisecond},
	}
	elapsed, discord, youtube := scoreServices(checks)
	if elapsed != 30*time.Millisecond || !discord || youtube {
		t.Fatalf("unexpected service score: %s %v %v", elapsed, discord, youtube)
	}
	ranked := []profileScore{
		{profile: profiles[0], elapsed: elapsed},
		{profile: profiles[1], elapsed: time.Second, youtube: true},
	}
	sortScores(ranked)
	if !ranked[0].youtube {
		t.Fatal("profile with both services should outrank a Discord-only profile")
	}
}

func TestGameFilterSettingsAndArguments(t *testing.T) {
	dir := t.TempDir()
	if enabled, err := readGameFilter(dir); enabled || err != nil {
		t.Fatalf("Game Filter default = %v, %v", enabled, err)
	}
	for _, enabled := range []bool{true, false} {
		if err := writeGameFilter(dir, enabled); err != nil {
			t.Fatal(err)
		}
		p := profiles[0]
		var err error
		p.gameFilter, err = readGameFilter(dir)
		if err != nil || p.gameFilter != enabled {
			t.Fatalf("Game Filter = %v, %v; wanted %v", p.gameFilter, err, enabled)
		}
		args := p.args()
		if slices.Contains(args, "--filter-udp="+gamePorts) != enabled {
			t.Fatalf("unexpected Game Filter arguments: %v", args)
		}
		if !slices.Contains(args, "--hostlist-domains="+youtubeHosts) {
			t.Fatal("missing YouTube TLS rule")
		}
	}
}

func TestDiagnosticsDistinguishUpdatePackage(t *testing.T) {
	result := describeCheck(measurement{name: updatesProbe, err: fmt.Errorf("update package CDN: %w", context.DeadlineExceeded)})
	if !strings.Contains(result, "пакет обновления") || !strings.Contains(result, "тайм-аут") {
		t.Fatalf("unexpected diagnostics: %s", result)
	}
}
