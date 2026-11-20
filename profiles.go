package main

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
)

type profile struct {
	name    string
	tls     []string
	updates bool
}

var overlapTLS = []string{
	"--dpi-desync=multisplit", "--dpi-desync-split-pos=1",
	"--dpi-desync-split-seqovl=568", "--dpi-desync-split-seqovl-pattern=tls_clienthello_4pda_to.bin",
}

var profiles = []profile{
	{name: "split", tls: []string{
		"--dpi-desync=multisplit", "--dpi-desync-split-pos=1,midsld",
	}},
	{name: "update", tls: overlapTLS, updates: true},
	{name: "overlap", tls: overlapTLS},
	{name: "disorder", tls: []string{
		"--dpi-desync=fake,multidisorder", "--dpi-desync-split-pos=1,midsld",
		"--dpi-desync-repeats=11", "--dpi-desync-fooling=badseq",
		"--dpi-desync-fake-tls=0x00000000", "--dpi-desync-fake-tls=!",
		"--dpi-desync-fake-tls-mod=rnd,dupsid,sni=www.google.com",
	}},
}

const discordHosts = "discord.com,discord.gg,discordapp.com,discordapp.net,discord.media,discordcdn.com,discordsays.com,discordactivities.com,discord-activities.com,discord-attachments-uploads-prd.storage.googleapis.com"
const voicePorts = "19294-19344,50000-50100"
const mediaTLSPorts = "2053,2083,2087,2096,8443"

func (p profile) args() []string {
	args := []string{
		"--wf-tcp=443," + mediaTLSPorts, "--wf-udp=443," + voicePorts,
	}
	if p.updates {
		args = append(args, "--filter-tcp=443",
			"--hostlist-domains=updates.discord.com,dl.discordapp.net,dl2.discordapp.net",
			"--dpi-desync=hostfakesplit", "--dpi-desync-hostfakesplit-mod=host=ozon.ru",
			"--dpi-desync-fooling=ts,md5sig", "--dpi-desync-repeats=4",
			"--dpi-desync-cutoff=d6", "--new")
	}
	args = append(args, "--filter-tcp=443", "--hostlist-domains="+discordHosts)
	args = append(args, p.tls...)
	args = append(args, "--dpi-desync-cutoff=n8")
	args = append(args, "--new", "--filter-tcp="+mediaTLSPorts, "--hostlist-domains=discord.media")
	args = append(args, p.tls...)
	args = append(args, "--dpi-desync-cutoff=n8")
	return append(args,
		"--new", "--filter-udp=443", "--filter-l7=quic", "--hostlist-domains="+discordHosts,
		"--dpi-desync=fake", "--dpi-desync-repeats=6", "--dpi-desync-cutoff=n7",
		"--dpi-desync-fake-quic=quic_initial_www_google_com.bin",
		"--new", "--filter-udp="+voicePorts, "--filter-l7=discord,stun",
		"--dpi-desync=fake", "--dpi-desync-repeats=6", "--dpi-desync-cutoff=n7",
		"--dpi-desync-fake-discord=ACTIVE_DISCORD_UDP.bin",
		"--dpi-desync-fake-stun=ACTIVE_DISCORD_UDP.bin")
}

func orderedProfiles(preferred string) []profile {
	ordered := slices.Clone(profiles)
	slices.SortStableFunc(ordered, func(a, b profile) int {
		if a.name == preferred && b.name != preferred {
			return -1
		}
		if b.name == preferred && a.name != preferred {
			return 1
		}
		return 0
	})
	return ordered
}

func score(results []measurement) (time.Duration, bool) {
	var slowest time.Duration
	for _, result := range results {
		if result.err != nil {
			return 0, false
		}
		slowest = max(slowest, result.elapsed)
	}
	return slowest, len(results) > 0
}

type profileScore struct {
	profile profile
	elapsed time.Duration
}

func sortScores(scores []profileScore) {
	slices.SortStableFunc(scores, func(a, b profileScore) int {
		return cmp.Compare(a.elapsed, b.elapsed)
	})
}

func networkID() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var addresses []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.To4() != nil && ip.IsGlobalUnicast() {
				addresses = append(addresses, fmt.Sprintf("%s/%s", iface.Name, ip))
			}
		}
	}
	if len(addresses) == 0 {
		return ""
	}
	slices.Sort(addresses)
	hash := sha256.Sum256([]byte(strings.Join(addresses, "\n")))
	return hex.EncodeToString(hash[:12])
}
