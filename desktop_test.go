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
	if mode, err := readGameFilter(dir); mode != gameFilterOff || err != nil {
		t.Fatalf("Game Filter default = %v, %v", mode, err)
	}
	tests := []struct {
		mode             gameFilterMode
		tcp, udp         string
		gameTCP, gameUDP string
	}{
		{gameFilterOff, "443," + mediaTLSPorts, "443," + voicePorts, "", ""},
		{gameFilterAll, "443," + gamePorts, "443," + gamePorts, gamePorts, gamePorts},
		{gameFilterSiege, "443," + mediaTLSPorts, "443," + voicePorts + "," + siegeUDPPorts, "", siegeUDPPorts},
	}
	for _, test := range tests {
		t.Run(test.mode.label(), func(t *testing.T) {
			if err := writeGameFilter(dir, test.mode); err != nil {
				t.Fatal(err)
			}
			mode, err := readGameFilter(dir)
			if err != nil || mode != test.mode {
				t.Fatalf("Game Filter = %v, %v; wanted %v", mode, err, test.mode)
			}
			for _, p := range profiles {
				p.gameFilter = mode
				args := p.args()
				if args[0] != "--wf-tcp="+test.tcp || args[1] != "--wf-udp="+test.udp {
					t.Fatalf("%s: unexpected capture ranges %v", p.name, args[:2])
				}
				for _, ports := range []string{gamePorts, siegeUDPPorts} {
					if slices.Contains(args, "--filter-tcp="+ports) != (test.gameTCP == ports) ||
						slices.Contains(args, "--filter-udp="+ports) != (test.gameUDP == ports) {
						t.Fatalf("%s: unexpected game rules for ports %s", p.name, ports)
					}
				}
				if !slices.Contains(args, "--hostlist-domains="+youtubeHosts) ||
					!slices.Contains(args, "--filter-udp="+voicePorts) {
					t.Fatalf("%s: missing YouTube TLS or Discord voice rule", p.name)
				}
			}
		})
	}
}

func TestDiagnosticsDistinguishUpdatePackage(t *testing.T) {
	result := describeCheck(measurement{name: updatesProbe, err: fmt.Errorf("update package CDN: %w", context.DeadlineExceeded)})
	if !strings.Contains(result, "пакет обновления") || !strings.Contains(result, "тайм-аут") {
		t.Fatalf("unexpected diagnostics: %s", result)
	}
}
