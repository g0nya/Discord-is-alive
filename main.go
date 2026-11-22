package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const version = "0.4.3"

func main() {
	if err := run(); err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\nDiscordLink:", err)
		}
		if len(os.Args) == 1 {
			reportLaunchError(err)
		}
		os.Exit(1)
	}
}

func run() error {
	trayOnly := flag.Bool("tray", false, "start minimized to the Windows tray")
	console := flag.Bool("console", false, "open the console main menu (default)")
	check := flag.Bool("check", false, "measure Discord and YouTube on the current connection")
	samples := flag.Int("samples", 3, "number of rounds with -check (1 to 9)")
	tune := flag.Bool("tune", false, "test every profile and select by median connection time")
	manual := flag.String("profile", "", "start a specific profile: split, update, overlap or disorder")
	flag.Parse()
	if flag.NArg() != 0 || (*console && *trayOnly) || *samples < 1 || *samples > 9 ||
		(*check && (*tune || *manual != "")) || (*tune && *manual != "") {
		return fmt.Errorf("invalid options; run with -help")
	}

	var selected profile
	if *manual != "" {
		for _, p := range profiles {
			if p.name == *manual {
				selected = p
			}
		}
		if selected.name == "" {
			return fmt.Errorf("unknown profile %q; run with -help", *manual)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *check {
		if _, discord, youtube := measure(ctx, "Current connection", *samples); !discord || !youtube {
			return fmt.Errorf("at least one service check failed")
		}
		return nil
	}

	relaunched, err := requestElevation()
	if relaunched || err != nil {
		return err
	}
	release, err := acquireSession()
	if err != nil {
		return err
	}
	defer release()

	dataDir, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	dataDir = filepath.Join(dataDir, "DiscordLink")
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if !*tune && selected.name == "" {
		return runDesktop(ctx, dataDir, filepath.Dir(exe), !*trayOnly)
	}
	engineDir, err := prepareEngine(ctx, dataDir, filepath.Dir(exe))
	if err != nil {
		return err
	}
	defer os.RemoveAll(engineDir)

	gameFilter, err := readGameFilter(dataDir)
	if err != nil {
		return err
	}
	if selected.name != "" {
		selected.gameFilter = gameFilter
		e, err := launch(ctx, engineDir, selected)
		if err != nil {
			return err
		}
		defer e.stop()
		measure(ctx, selected.name, 1)
		return keepRunning(ctx, e)
	}

	e, _, err := chooseProfile(ctx, engineDir, dataDir, networkID(), "", *tune, gameFilter, measure)
	if err != nil {
		return err
	}
	defer e.stop()
	return keepRunning(ctx, e)
}

type measureProfile func(context.Context, string, int) (time.Duration, bool, bool)

func chooseProfile(ctx context.Context, engineDir, dataDir, id, skip string, tune bool, gameFilter gameFilterMode, check measureProfile) (*engine, profile, error) {
	cachePath := profileCachePath(dataDir, id)
	var preferred string
	if id != "" {
		if data, err := os.ReadFile(cachePath); err == nil {
			preferred = strings.TrimSpace(string(data))
		}
	}
	rounds := 1
	if tune {
		rounds = 3
	}
	var successful []profileScore
	for _, p := range orderedProfiles(preferred) {
		if p.name == skip {
			continue
		}
		p.gameFilter = gameFilter
		e, err := launch(ctx, engineDir, p)
		if err != nil {
			return nil, profile{}, err
		}
		delay, ok, youtube := check(ctx, p.name, rounds)
		if ctx.Err() != nil {
			e.stop()
			return nil, profile{}, ctx.Err()
		}
		select {
		case <-e.done:
			return nil, profile{}, e.failure()
		default:
		}
		if ok && youtube && !tune {
			saveProfile(id, cachePath, p)
			return e, p, nil
		}
		e.stop()
		if ok {
			successful = append(successful, profileScore{p, delay, youtube})
		}
	}
	sortScores(successful)
	for _, candidate := range successful {
		e, err := launch(ctx, engineDir, candidate.profile)
		if err != nil {
			return nil, profile{}, err
		}
		_, ok, _ := check(ctx, candidate.profile.name, 1)
		if ok && ctx.Err() == nil {
			saveProfile(id, cachePath, candidate.profile)
			return e, candidate.profile, nil
		}
		e.stop()
		if ctx.Err() != nil {
			return nil, profile{}, ctx.Err()
		}
	}
	return nil, profile{}, fmt.Errorf("no profile passed Discord connection checks")
}

func launch(ctx context.Context, dir string, p profile) (*engine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e, err := startEngine(dir, p)
	if err != nil {
		return nil, err
	}
	// WinDivert can take longer to load its driver on the first start.
	timer := time.NewTimer(450 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		e.stop()
		return nil, ctx.Err()
	case <-e.done:
		return nil, e.failure()
	case <-timer.C:
		return e, nil
	}
}

func keepRunning(ctx context.Context, e *engine) error {
	fmt.Println("\nEngine running. Fully quit and reopen Discord, then check a voice channel. Ctrl+C to stop.")
	select {
	case <-ctx.Done():
		return nil
	case <-e.done:
		return e.failure()
	}
}

func profileCachePath(dataDir, id string) string {
	return filepath.Join(dataDir, "profiles", id+".txt")
}

func saveProfile(id, path string, p profile) {
	if id != "" {
		if err := writeAtomic(path, []byte(p.name+"\n")); err != nil {
			fmt.Fprintln(os.Stderr, "Could not cache profile:", err)
		}
	}
}

func measure(ctx context.Context, label string, rounds int) (time.Duration, bool, bool) {
	allYouTube := true
	times := make([]time.Duration, 0, rounds)
	for range rounds {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		results := probe(ctx, connectionTargets)
		cancel()
		fmt.Printf("%s", label)
		for _, result := range results {
			if result.err != nil {
				fmt.Printf(" | %s: %v", result.name, result.err)
			} else {
				fmt.Printf(" | %s: %s", result.name, result.elapsed)
			}
		}
		fmt.Println()
		elapsed, ok, youtube := scoreServices(results)
		allYouTube = allYouTube && youtube
		if !ok {
			return 0, false, allYouTube
		}
		times = append(times, elapsed)
	}
	slices.Sort(times)
	median := times[len(times)/2]
	if len(times)%2 == 0 {
		median = (times[len(times)/2-1] + median) / 2
	}
	if rounds > 1 {
		fmt.Printf("%s median of slowest check: %s\n", label, median)
	}
	return median, true, allYouTube
}
