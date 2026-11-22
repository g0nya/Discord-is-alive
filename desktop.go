package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	networkPoll  = 3 * time.Second
	networkGrace = 4 * time.Second
	healthPeriod = 60 * time.Second
	retryDelay   = 4 * time.Second
)

type desktopStatus struct {
	label, profile, reason string
	event, cause           string
	checks                 []measurement
	checkedAt              time.Time
}

func (s desktopStatus) diagnostic() string {
	var text strings.Builder
	fmt.Fprintf(&text, "DiscordLink v%s\r\nСостояние: %s\r\n", version, s.label)
	if s.profile != "" {
		fmt.Fprintf(&text, "Профиль: %s\r\n", s.profile)
	}
	if s.reason != "" {
		fmt.Fprintf(&text, "Причина: %s\r\n", s.reason)
	}
	if !s.checkedAt.IsZero() {
		fmt.Fprintf(&text, "Проверено: %s\r\n", s.checkedAt.Format("2006-01-02 15:04:05 MST"))
	}
	for _, check := range s.checks {
		fmt.Fprintf(&text, "%s: %s\r\n", check.name, describeCheck(check))
	}
	return text.String()
}

func describeCheck(m measurement) string {
	if m.err == nil {
		return m.elapsed.String()
	}
	stage := ""
	if m.name == updatesProbe {
		stage = "манифест обновления: "
		if strings.HasPrefix(m.err.Error(), "update package CDN") {
			stage = "пакет обновления: "
		}
	}
	var dns *net.DNSError
	switch {
	case errors.As(m.err, &dns):
		return stage + "ошибка DNS (" + dns.Name + ")"
	case errors.Is(m.err, context.DeadlineExceeded):
		return stage + "тайм-аут"
	default:
		// url.Error includes the URL, and a dial error can include a raw IP.
		// Keep clipboard diagnostics focused on the stage and failure class.
		for _, prefix := range []string{"update manifest returned HTTP", "update package CDN did not honor byte range", "unexpected", "WebSocket upgrade failed"} {
			if strings.HasPrefix(m.err.Error(), prefix) {
				return m.err.Error()
			}
		}
		switch {
		case strings.Contains(m.err.Error(), "update package CDN"):
			return "сбой соединения с CDN обновлений"
		case strings.Contains(m.err.Error(), "TLS") || strings.Contains(m.err.Error(), "tls:"):
			return stage + "ошибка TLS"
		default:
			return stage + "ошибка соединения или ответа"
		}
	}
}

type desktopController struct {
	gameFilter     gameFilterMode
	restartPending bool
	// The tray thread alone owns the lifecycle fields. The worker owns the
	// engine and publishes immutable status snapshots under the mutex.
	ctx             context.Context
	dataDir, exeDir string
	cancel          context.CancelFunc
	done            chan struct{}
	recheck, resume chan struct{}
	mu              sync.Mutex
	status          desktopStatus
}

func newDesktop(ctx context.Context, dataDir, exeDir string) *desktopController {
	return &desktopController{
		ctx: ctx, dataDir: dataDir, exeDir: exeDir,
		status: desktopStatus{label: "Остановлен"},
	}
}

func (d *desktopController) setStatus(s desktopStatus) {
	d.mu.Lock()
	d.status = s
	d.mu.Unlock()
}

func (d *desktopController) snapshot() desktopStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

func (d *desktopController) refresh() {
	select {
	case <-d.done:
		d.cancel, d.done = nil, nil

	default:
	}
}

func (d *desktopController) tick() {
	d.refresh()
	if d.done == nil && d.restartPending {
		d.restartPending = false
		d.start()
	}
}

func (d *desktopController) start() {
	d.refresh()
	if d.done != nil {
		return
	}
	d.restartPending = false
	ctx, cancel := context.WithCancel(d.ctx)
	done, recheck, resume := make(chan struct{}), make(chan struct{}, 1), make(chan struct{}, 1)
	d.cancel, d.done, d.recheck, d.resume = cancel, done, recheck, resume
	d.setStatus(desktopStatus{label: "Запускается"})
	gameFilter := d.gameFilter
	go func() {
		defer close(done)
		defer cancel()
		err := d.runSession(ctx, recheck, resume, gameFilter)
		if ctx.Err() != nil {
			d.setStatus(desktopStatus{label: "Остановлен"})
		} else if err != nil {
			fmt.Fprintln(os.Stderr, "DiscordLink:", err)
			d.setStatus(desktopStatus{label: "Ошибка запуска", event: eventFailure, cause: "startup", reason: "Проверьте движок, права или активный winws.exe. Подробности в консоли."})
		}
	}()
}

func (d *desktopController) stop() {
	d.restartPending = false
	d.refresh()
	if d.cancel != nil {
		d.cancel()
	}
}

func (d *desktopController) cycleGameFilter() error {
	d.refresh()
	next := d.gameFilter + 1
	if next > gameFilterSiege {
		next = gameFilterOff
	}
	if err := writeGameFilter(d.dataDir, next); err != nil {
		return err
	}
	d.gameFilter = next
	if d.cancel != nil {
		d.restartPending = true
		d.cancel()
	}
	return nil
}

func activeLabel(checks []measurement) string {
	_, _, youtube := scoreServices(checks)
	if !youtube {
		return "Discord работает · YouTube недоступен"
	}
	return "Discord и YouTube доступны"
}

func notify(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (d *desktopController) runSession(ctx context.Context, recheck, resume <-chan struct{}, gameFilter gameFilterMode) error {
	if err := checkOtherEngine(); err != nil {
		return err
	}
	var dir string
	defer func() {
		if dir != "" {
			os.RemoveAll(dir)
		}
	}()

	var active *engine
	var current profile
	var last desktopStatus
	defer func() {
		if active != nil {
			active.stop()
		}
	}()

	check := func(ctx context.Context, name string, _ int) (time.Duration, bool, bool) {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		last = desktopStatus{
			label: "Проверка соединений", profile: name,
			checks: probe(probeCtx, connectionTargets), checkedAt: time.Now(),
		}
		d.setStatus(last)
		return scoreServices(last.checks)
	}

	activate := func(id, skip string) error {
		d.setStatus(desktopStatus{label: "Подбор профиля", profile: skip})
		var err error
		if dir == "" {
			dir, err = prepareEngine(ctx, d.dataDir, d.exeDir)
			if err != nil {
				return err
			}
		}
		active, current, err = chooseProfile(ctx, dir, d.dataDir, id, skip, false, gameFilter, check)
		if err == nil {
			last.label, last.event = activeLabel(last.checks), eventReady
			last.profile = current.name
			d.setStatus(last)
		}
		return err
	}

	state := recoverySchedule{network: networkID()}
	poll := time.NewTicker(networkPoll)
	defer poll.Stop()
	for ctx.Err() == nil {
		if !time.Now().Before(state.nextCheck) {
			if state.network == "" {
				last.label, last.reason, last.event = "Ожидает сеть", "Нет активного сетевого адреса", ""
				d.setStatus(last)
				state.nextCheck = time.Now().Add(healthPeriod)
			} else if active == nil {
				if err := activate(state.network, ""); err != nil && ctx.Err() == nil {
					fmt.Fprintln(os.Stderr, err)
					if dir == "" {
						last.label, last.reason, last.event, last.cause = "Движок недоступен", "Проверьте загрузку Flowseal и наличие ZIP рядом с DiscordLink", eventFailure, "engine_download"
					} else {
						last.label, last.reason, last.event, last.cause = "Discord недоступен", "Подходящий профиль не найден", eventFailure, "no_profile"
					}
					d.setStatus(last)
				}
				state.nextCheck = time.Now().Add(healthPeriod)
			} else {
				_, ok, _ := check(ctx, current.name, 1)
				if !state.recoverAfterCheck(time.Now(), ok) {
					if ok {
						last.label, last.event = activeLabel(last.checks), eventReady
						saveProfile(state.network, profileCachePath(d.dataDir, state.network), current)
					} else {
						last.label = "Связь нестабильна"
					}
					d.setStatus(last)
				} else if ctx.Err() == nil {
					previous := current
					active.stop()
					active = nil
					if err := activate(state.network, previous.name); err != nil && ctx.Err() == nil {
						fmt.Fprintln(os.Stderr, err)
						var restartErr error
						active, restartErr = launch(ctx, dir, previous)
						current = previous
						// Restore the last profile so partial access is preserved.
						last.profile = previous.name
						if restartErr != nil {
							fmt.Fprintln(os.Stderr, restartErr)
						} else if _, ok, _ := check(ctx, previous.name, 1); ok {
							last.label, last.event = activeLabel(last.checks), eventReady
							d.setStatus(last)
							state.nextCheck = time.Now().Add(healthPeriod)
							continue
						}
						last.label, last.reason, last.event, last.cause = "Discord недоступен", "Запасные профили не прошли проверку", eventFailure, "no_profile"
						d.setStatus(last)
					}
					state.nextCheck = time.Now().Add(healthPeriod)
				}
			}
		}
		var engineDone <-chan struct{}
		if active != nil {
			engineDone = active.done
		}
		select {
		case <-ctx.Done():
			return nil
		case <-engineDone:
			fmt.Fprintln(os.Stderr, active.failure())
			active = nil
			d.setStatus(desktopStatus{label: "Движок остановился", event: eventFailure, cause: "engine_exit", reason: "Будет повторная попытка"})
			state.nextCheck = time.Now().Add(retryDelay)
		case <-poll.C:
			state.observeNetwork(time.Now(), networkID())
		case <-resume:
			state.network = networkID()
			state.deferCheck(time.Now(), networkGrace)
		case <-recheck:
			state.network = networkID()
			state.deferCheck(time.Now(), 0)
		}
	}
	return nil
}

type recoverySchedule struct {
	network   string
	nextCheck time.Time
	failed    bool
}

func (s *recoverySchedule) observeNetwork(now time.Time, id string) {
	if s.network != id {
		s.network = id
		s.deferCheck(now, networkGrace)
	}
}

func (s *recoverySchedule) deferCheck(now time.Time, delay time.Duration) {
	s.failed = false
	s.nextCheck = now.Add(delay)
}

func (s *recoverySchedule) recoverAfterCheck(now time.Time, ok bool) bool {
	if ok || s.failed {
		s.deferCheck(now, healthPeriod)
		return !ok
	}
	s.failed = true
	s.nextCheck = now.Add(retryDelay)
	return false
}
