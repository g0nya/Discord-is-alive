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

func main() {
	if err := run(); err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\nDiscordLink:", err)
		}
		if len(os.Args) == 1 {
			fmt.Println("Press Enter to close.")
			_, _ = fmt.Scanln()
		}
		os.Exit(1)
	}
}

func run() error {
	check := flag.Bool("check", false, "measure API, Gateway and CDN on the current connection")
	samples := flag.Int("samples", 3, "number of rounds with -check (1 to 9)")
	tune := flag.Bool("tune", false, "test every profile and select by median connection time")
	manual := flag.String("profile", "", "start a specific profile: split, overlap or disorder")
	flag.Parse()
	if flag.NArg() != 0 || *samples < 1 || *samples > 9 ||
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
		if _, ok := measure(ctx, "Current connection", *samples); !ok {
			return fmt.Errorf("at least one Discord check failed")
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
	engineDir, err := prepareEngine(ctx, dataDir, filepath.Dir(exe))
	if err != nil {
		return err
	}
	defer os.RemoveAll(engineDir)

	if selected.name != "" {
		e, err := launch(ctx, engineDir, selected)
		if err != nil {
			return err
		}
		defer e.stop()
		measure(ctx, selected.name, 1)
		return keepRunning(ctx, e)
	}

	id := networkID()
	cachePath := filepath.Join(dataDir, "profiles", id+".txt")
	var preferred string
	if id != "" {
		if data, err := os.ReadFile(cachePath); err == nil {
			preferred = strings.TrimSpace(string(data))
		}
	}
	var successful []profileScore
	for _, p := range orderedProfiles(preferred) {
		e, err := launch(ctx, engineDir, p)
		if err != nil {
			return err
		}

		rounds := 1
		if *tune {
			rounds = 3
		}
		delay, ok := measure(ctx, p.name, rounds)
		if ctx.Err() != nil {
			e.stop()
			return ctx.Err()
		}
		select {
		case <-e.done:
			return e.failure()
		default:
		}
		if ok && !*tune {
			defer e.stop()
			saveProfile(id, cachePath, p)
			return keepRunning(ctx, e)
		}
		e.stop()
		if ok {
			successful = append(successful, profileScore{p, delay})
		}
	}
	if len(successful) == 0 {
		return fmt.Errorf("no profile passed API, Gateway and CDN checks on this connection")
	}

	sortScores(successful)
	for _, candidate := range successful {
		e, err := launch(ctx, engineDir, candidate.profile)
		if err != nil {
			return err
		}
		_, ok := measure(ctx, candidate.profile.name, 1)
		if ok {
			defer e.stop()
			saveProfile(id, cachePath, candidate.profile)
			return keepRunning(ctx, e)
		}
		e.stop()
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return fmt.Errorf("successful profiles could not reconnect")
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
	fmt.Println("\nEngine running. Open Discord and check a voice channel. Ctrl+C to stop.")
	select {
	case <-ctx.Done():
		return nil
	case <-e.done:
		return e.failure()
	}
}

func saveProfile(id, path string, p profile) {
	if id != "" {
		if err := writeAtomic(path, []byte(p.name+"\n")); err != nil {
			fmt.Fprintln(os.Stderr, "Could not cache profile:", err)
		}
	}
}

func measure(ctx context.Context, label string, rounds int) (time.Duration, bool) {
	times := make([]time.Duration, 0, rounds)
	for range rounds {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		results := probe(ctx, discordTargets)
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
		elapsed, ok := score(results)
		if !ok {
			return 0, false
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
	return median, true
}
