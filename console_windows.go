package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	mainScreen = iota
	settingsScreen
	diagnosticsScreen
)

func (t *windowsTray) openConsole() {
	if t.console == 0 {
		t.popup("Откройте DiscordLink из Windows Terminal с параметром -console.", 0x40)
		return
	}
	if t.ownConsole {
		t.user.NewProc("ShowWindow").Call(t.console, 5)
		t.user.NewProc("SetForegroundWindow").Call(t.console)
	}
	if !t.consoleShown {
		t.consoleScreen = mainScreen
		t.consoleRendered = ""
		if syscall.GetConsoleMode(syscall.Handle(os.Stdout.Fd()), &t.consoleMode) == nil {
			ok, _, _ := kernel.NewProc("SetConsoleMode").Call(os.Stdout.Fd(), uintptr(t.consoleMode|0x0005))
			t.consoleVT = ok != 0
		}
		if t.consoleVT {
			fmt.Print("\x1b[?1049h\x1b[?25l")
		}
	}
	t.consoleShown = true
	t.renderConsole()
	if !t.consoleStarted {
		t.consoleStarted = true
		go t.readConsole(t.consoleVT)
	}
}

func (t *windowsTray) leaveConsoleScreen() {
	if t.consoleVT {
		fmt.Print("\x1b[?25h\x1b[?1049l")
		kernel.NewProc("SetConsoleMode").Call(os.Stdout.Fd(), uintptr(t.consoleMode))
	}
	t.consoleVT = false
	t.consoleShown = false
}

func (t *windowsTray) readConsole(singleKey bool) {
	send := func(choice string) bool {
		select {
		case t.input <- choice:
			return true
		case <-t.app.ctx.Done():
			return false
		}
	}
	if !singleKey {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if !send(strings.TrimSpace(scanner.Text())) {
				return
			}
		}
		return
	}
	getKey := syscall.NewLazyDLL(filepath.Join(t.systemDir, "msvcrt.dll")).NewProc("_getwch")
	for {
		key, _, _ := getKey.Call()
		switch {
		case key >= '0' && key <= '9':
			if !send(string(rune(key))) {
				return
			}
		case key == 0 || key == 224: // Extended keyboard sequence.
			getKey.Call()
		case key == 0xffff:
			return
		}
	}
}

func toggleLabel(enabled bool) string {
	if enabled {
		return "вкл."
	}
	return "выкл."
}

func (t *windowsTray) renderConsole() {
	if !t.consoleShown {
		return
	}
	s := t.app.snapshot()
	var text strings.Builder
	fmt.Fprintf(&text, "  DiscordLink %s\n  %s", version, s.label)
	if s.profile != "" {
		fmt.Fprintf(&text, "  ·  Профиль %s", s.profile)
	}
	if !s.checkedAt.IsZero() {
		fmt.Fprintf(&text, "  ·  Проверено в %s", s.checkedAt.Format("15:04:05"))
	}
	text.WriteString("\n  " + t.reporter.statusText() + "\n\n")
	switch t.consoleScreen {
	case settingsScreen:
		fmt.Fprintf(&text, "  Настройки\n\n  1  Автозапуск при входе (%s)\n  2  Сменить режим Game Filter (%s)\n  3  Отчёты в Telegram (%s)\n  4  Отправить статус сейчас\n  0  Назад\n",
			toggleLabel(t.autostart), t.app.gameFilter.label(), toggleLabel(t.reporter != nil && t.reporter.enabled))
	case diagnosticsScreen:
		text.WriteString(t.diagnosticText())
		text.WriteString("\n  0  Назад\n")
	default:
		text.WriteString("  Главное меню\n\n  1  Запустить обход\n  2  Остановить обход\n  3  Проверить соединение\n  4  Показать диагностику\n  5  Скопировать диагностику\n  6  Настройки\n  8  Свернуть в трей\n  0  Выход\n")
	}
	if t.consoleNotice != "" {
		fmt.Fprintf(&text, "\n  %s\n", t.consoleNotice)
	}
	if t.consoleVT {
		text.WriteString("\n  Нажми клавишу действия.\n")
		if rendered := text.String(); rendered != t.consoleRendered {
			fmt.Print("\x1b[2J\x1b[H" + rendered)
			t.consoleRendered = rendered
		}
	} else if t.consoleRendered == "" {
		text.WriteString("\n  Введи номер и нажми Enter.\n")
		fmt.Print(text.String())
		t.consoleRendered = text.String()
	}
}

func (t *windowsTray) consoleAction(choice string) {
	if !t.consoleShown {
		return
	}
	t.consoleNotice = ""
	t.consoleRendered = ""
	switch t.consoleScreen {
	case settingsScreen:
		switch choice {
		case "1":
			t.action(actionStartup)
		case "2":
			t.action(actionGame)
		case "3":
			t.action(actionTelemetry)
		case "4":
			if t.reporter == nil || !t.reporter.enabled {
				t.consoleNotice = "Сначала включи отчёты Telegram."
			} else {
				t.reporter.forceSnapshot = true
				t.consoleNotice = "Будет отправлен последний результат проверки."
			}
		case "0":
			t.consoleScreen = mainScreen
		}
	case diagnosticsScreen:
		if choice == "0" {
			t.consoleScreen = mainScreen
		}
	default:
		switch choice {
		case "1":
			t.action(actionStart)
		case "2":
			t.action(actionStop)
		case "3":
			t.action(actionCheck)
			if t.app.done == nil {
				t.consoleNotice = "Сначала запусти обход."
			}
		case "4":
			t.consoleScreen = diagnosticsScreen
		case "5":
			if err := t.copyReport(t.diagnosticText()); err != nil {
				t.consoleNotice = "Не удалось скопировать диагностику."
			} else {
				t.consoleNotice = "Диагностика скопирована."
			}
		case "6":
			t.autostart = t.startupEnabled()
			t.consoleScreen = settingsScreen
		case "8":
			if t.ownConsole {
				t.leaveConsoleScreen()
				t.user.NewProc("ShowWindow").Call(t.console, 0)
				return
			}
			t.consoleNotice = "Это общее окно терминала. Меню также доступно в трее."
		case "0":
			t.action(actionExit)
		}
	}
}
