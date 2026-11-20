package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const (
	wmDestroy   = 0x0002
	wmClose     = 0x0010
	wmNull      = 0x0000
	wmTimer     = 0x0113
	wmPower     = 0x0218
	wmTray      = 0x8001
	wmRoute     = 0x8002
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2
	nifFields = 1 | 2 | 4 // callback, icon and tooltip

)

const (
	actionStart = iota + 1
	actionStop
	actionCheck
	actionCopy
	actionReport
	actionGame
	actionStartup
	actionTelemetry
	actionToggleConsole
	actionExit
)

type winPoint struct{ x, y int32 }
type winMessage struct {
	window   uintptr
	msg      uint32
	w, l     uintptr
	time     uint32
	point    winPoint
	reserved uint32
}

type windowClass struct {
	style                              uint32
	proc                               uintptr
	extra, windowExtra                 int32
	instance, icon, cursor, background uintptr
	menu, name                         *uint16
}

type notifyIcon struct {
	size                uint32
	window              uintptr
	id, flags, callback uint32
	icon                uintptr
	tip                 [128]uint16
	state, mask         uint32
	info                [256]uint16
	version             uint32
	title               [64]uint16
	infoFlags           uint32
	guid                [16]byte
	balloonIcon         uintptr
}

type windowsTray struct {
	reporter                       *reporter
	reportConfigErr                bool
	app                            *desktopController
	user, shell                    *syscall.LazyDLL
	hwnd, icon, taskbar            uintptr
	console                        uintptr
	ownConsole, consoleShown       bool
	consoleStarted, autostart      bool
	consoleVT                      bool
	consoleMode                    uint32
	consoleScreen                  int
	consoleRendered, consoleNotice string
	input                          chan string
	tooltip                        string
	systemDir                      string
}

// The window and its callback are confined to the locked UI thread.
var currentTray *windowsTray

func runDesktop(ctx context.Context, dataDir, exeDir string, showConsole bool) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	systemDir, err := systemDirectory()
	if err != nil {
		return err
	}
	game, err := readGameFilter(dataDir)
	if err != nil {
		return fmt.Errorf("cannot read settings: %w", err)
	}
	d := newDesktop(ctx, dataDir, exeDir)
	d.gameFilter = game
	t := &windowsTray{
		app: d, systemDir: systemDir,
		user:  syscall.NewLazyDLL(filepath.Join(systemDir, "user32.dll")),
		shell: syscall.NewLazyDLL(filepath.Join(systemDir, "shell32.dll")),
		input: make(chan string, 1),
	}
	t.reporter, err = loadReporter(exeDir, dataDir)
	t.reportConfigErr = err != nil
	defer func() {
		if t.reporter != nil && t.reporter.cancel != nil {
			t.reporter.cancel()
		}
	}()
	currentTray = t
	defer t.leaveConsoleScreen()
	defer func() { currentTray = nil }()

	instance, _, err := kernel.NewProc("GetModuleHandleW").Call(0)
	if instance == 0 {
		return err
	}
	name := utf16ptr("DiscordLink.Tray.Window")
	class := windowClass{proc: syscall.NewCallback(trayWindowProc), instance: instance, name: name}
	atom, _, err := t.user.NewProc("RegisterClassW").Call(uintptr(unsafe.Pointer(&class)))
	if atom == 0 {
		return fmt.Errorf("cannot register tray window: %w", err)
	}
	title := utf16ptr("DiscordLink")
	t.hwnd, _, err = t.user.NewProc("CreateWindowExW").Call(
		0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(title)), 0,
		0, 0, 0, 0, 0, 0, instance, 0,
	)
	if t.hwnd == 0 {
		return fmt.Errorf("cannot create tray window: %w", err)
	}
	defer t.user.NewProc("DestroyWindow").Call(t.hwnd)
	t.icon = t.createIcon()
	if t.icon != 0 {
		defer t.user.NewProc("DestroyIcon").Call(t.icon)
	} else {
		t.icon, _, _ = t.user.NewProc("LoadIconW").Call(0, 32512) // IDI_APPLICATION
	}
	taskbar := utf16ptr("TaskbarCreated")
	t.taskbar, _, _ = t.user.NewProc("RegisterWindowMessageW").Call(uintptr(unsafe.Pointer(taskbar)))
	// Explorer runs at medium integrity even when DiscordLink is elevated.
	for _, msg := range []uintptr{wmTray, t.taskbar} {
		if msg != 0 {
			t.user.NewProc("ChangeWindowMessageFilterEx").Call(t.hwnd, msg, 1, 0)
		}
	}
	if !t.updateIcon(nimAdd) && !showConsole {
		return fmt.Errorf("cannot add notification icon; launch with -console")
	}
	defer t.updateIcon(nimDelete)

	timer, _, err := t.user.NewProc("SetTimer").Call(t.hwnd, 1, 250, 0)
	if timer == 0 {
		return fmt.Errorf("cannot start tray timer: %w", err)
	}
	defer t.user.NewProc("KillTimer").Call(t.hwnd, 1)

	stopRouteNotifications := t.monitorRoutes()
	defer stopRouteNotifications()
	t.autostart = t.startupEnabled()
	t.console, _, _ = kernel.NewProc("GetConsoleWindow").Call()
	var consolePIDs [2]uint32
	processes, _, _ := kernel.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&consolePIDs[0])), 2)
	t.ownConsole = t.console != 0 && processes == 1
	if t.ownConsole {
		kernel.NewProc("SetConsoleTitleW").Call(uintptr(unsafe.Pointer(utf16ptr("DiscordLink · Панель подключения"))))
	}
	d.start()
	if showConsole {
		t.openConsole()
	} else if t.ownConsole {
		t.user.NewProc("ShowWindow").Call(t.console, 0)
	}
	defer func() {
		d.stop()
		if d.done != nil {
			<-d.done
		}
	}()

	for {
		var msg winMessage
		result, _, getErr := t.user.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		switch int32(result) {
		case -1:
			return getErr
		case 0:
			return nil
		}
		t.user.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg)))
		t.user.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func trayWindowProc(hwnd, msg, w, l uintptr) uintptr {
	t := currentTray
	if t == nil {
		return 0
	}
	switch msg {
	case wmTray:
		if l == wmRButtonUp || l == wmLButtonUp {
			t.menu()
		}
		return 0
	case wmRoute:
		if t.app.done != nil {
			notify(t.app.resume)
		}
		return 0
	case wmPower:
		if w == 0x12 && t.app.done != nil { // PBT_APMRESUMEAUTOMATIC
			notify(t.app.resume)
		}
		return 1
	case wmTimer:
		t.app.tick()
		if t.app.ctx.Err() != nil {
			t.user.NewProc("PostQuitMessage").Call(0)
			return 0
		}
		if t.reporter != nil {
			t.reporter.tick(t.app.ctx, t.app.snapshot())
		}
		if t.tooltip != t.tipText() {
			t.updateIcon(nimModify)
		}
		select {
		case choice := <-t.input:
			t.consoleAction(choice)
		default:
		}
		t.renderConsole()
		return 0
	case wmClose, wmDestroy:
		t.user.NewProc("PostQuitMessage").Call(0)
		return 0
	default:
		if msg == t.taskbar && msg != 0 {
			t.updateIcon(nimAdd)
			return 0
		}
		result, _, _ := t.user.NewProc("DefWindowProcW").Call(hwnd, msg, w, l)
		return result
	}
}

func (t *windowsTray) monitorRoutes() func() {
	ipHelper := syscall.NewLazyDLL(filepath.Join(t.systemDir, "iphlpapi.dll"))
	post := t.user.NewProc("PostMessageW")
	callback := syscall.NewCallback(func(window, row, notification uintptr) uintptr {
		// MIB_IPFORWARD_ROW2 is owned by Windows. The tray only needs the change event.
		post.Call(window, wmRoute, 0, 0)
		return 0
	})
	var handle uintptr
	status, _, _ := ipHelper.NewProc("NotifyRouteChange2").Call(
		0, callback, t.hwnd, 0, uintptr(unsafe.Pointer(&handle)),
	)
	if status != 0 {
		return func() {}
	}
	return func() { ipHelper.NewProc("CancelMibChangeNotify2").Call(handle) }
}

func (t *windowsTray) tipText() string {
	s := t.app.snapshot()
	if s.profile != "" {
		return "DiscordLink · " + s.label + " (" + s.profile + ")"
	}
	return "DiscordLink · " + s.label
}

func (t *windowsTray) updateIcon(operation uintptr) bool {
	data := notifyIcon{window: t.hwnd, id: 1, flags: nifFields, callback: wmTray, icon: t.icon}
	data.size = uint32(unsafe.Sizeof(data))
	t.tooltip = t.tipText()
	copy(data.tip[:127], syscall.StringToUTF16(t.tooltip))
	result, _, _ := t.shell.NewProc("Shell_NotifyIconW").Call(operation, uintptr(unsafe.Pointer(&data)))
	return result != 0
}

func (t *windowsTray) menu() {
	t.app.refresh()
	t.autostart = t.startupEnabled()
	menu, _, _ := t.user.NewProc("CreatePopupMenu").Call()
	if menu == 0 {
		return
	}
	defer t.user.NewProc("DestroyMenu").Call(menu)
	add := func(id, flags uintptr, text string) {
		t.user.NewProc("AppendMenuW").Call(menu, flags, id, uintptr(unsafe.Pointer(utf16ptr(text))))
	}
	const disabled = 1
	add(0, disabled, t.tipText())
	add(0, 0x800, "")
	if t.app.done == nil {
		add(actionStart, 0, "Запустить")
	} else {
		add(actionStop, 0, "Остановить")
		add(actionCheck, 0, "Проверить соединение")
	}
	add(actionReport, 0, "Диагностика…")
	add(actionCopy, 0, "Скопировать диагностику")
	add(0, 0x800, "")
	add(actionStartup, checked(t.autostart), "Автозапуск при входе")
	add(actionGame, checked(t.app.gameFilter), "Game Filter")
	add(actionTelemetry, checked(t.reporter != nil && t.reporter.enabled), "Отчёты в Telegram…")
	add(actionToggleConsole, 0, "Главное меню")
	add(actionExit, 0, "Выход")
	var p winPoint
	t.user.NewProc("GetCursorPos").Call(uintptr(unsafe.Pointer(&p)))
	t.user.NewProc("SetForegroundWindow").Call(t.hwnd)
	action, _, _ := t.user.NewProc("TrackPopupMenu").Call(menu, 0x102, uintptr(p.x), uintptr(p.y), 0, t.hwnd, 0)
	t.user.NewProc("PostMessageW").Call(t.hwnd, wmNull, 0, 0)
	t.action(action)
}

func checked(enabled bool) uintptr {
	if enabled {
		return 8
	}
	return 0
}

func (t *windowsTray) action(id uintptr) {
	switch id {
	case actionStart:
		t.app.start()
	case actionStop:
		t.app.stop()
	case actionCheck:
		if t.app.done != nil {
			notify(t.app.recheck)
		}
	case actionCopy:
		if err := t.copyReport(t.diagnosticText()); err != nil {
			t.popup("Не удалось скопировать диагностику. "+err.Error(), 0x30)
		}
	case actionReport:
		t.popup(t.diagnosticText(), 0x40)
	case actionGame:
		if err := t.app.toggleGameFilter(); err != nil {
			t.popup("Не удалось сохранить Game Filter. "+err.Error(), 0x30)
		}
	case actionStartup:
		if err := t.setStartup(!t.startupEnabled()); err != nil {
			t.popup("Не удалось изменить автозапуск. "+err.Error(), 0x30)
		}
		t.autostart = t.startupEnabled()
	case actionTelemetry:
		t.toggleTelemetry()
	case actionToggleConsole:
		t.openConsole()
	case actionExit:
		t.user.NewProc("PostQuitMessage").Call(0)
	}
}

func (t *windowsTray) toggleTelemetry() {
	r := t.reporter
	if r == nil {
		message := "Сервер для отчётов ещё не настроен. Рядом с DiscordLink.exe нужен telemetry.json."
		if t.reportConfigErr {
			message = "Проверьте формат URL и ключа в telemetry.json."
		}
		t.popup(message, 0x40)
		return
	}
	next := !r.enabled
	if next {
		question := "Разрешить отчёты владельцу DiscordLink в Telegram?\n\n" +
			"Отправляются случайный ID установки, версия, профиль, результаты проверок Discord и YouTube, время их ответа и тип сбоя. " +
			"При повторных запусках отправляется только первый успешный отчёт за день. " +
			"Отчёт о сбое отправляется не чаще раза в час. Пока программа запущена, статус обновляется для сводки примерно каждые 15 минут. " +
			"Сервер хранит последний отчёт по ID примерно 24 часа.\n\nСервер: " + r.cfg.URL
		if backup := backupRelayURL(r.cfg.URL); backup != "" {
			question += "\nЗапасной адрес того же сервера: " + backup
		}
		if t.popup(question, 0x124) != 6 { // Yes/No, with No selected by default.
			return
		}
	}
	if err := r.setEnabled(next); err != nil {
		t.popup("Не удалось сохранить настройку отправки отчётов.", 0x30)
	}
}

func (t *windowsTray) popup(text string, flags uintptr) uintptr {
	result, _, _ := t.user.NewProc("MessageBoxW").Call(t.hwnd, uintptr(unsafe.Pointer(utf16ptr(text))),
		uintptr(unsafe.Pointer(utf16ptr("DiscordLink"))), flags)
	return result
}

func reportLaunchError(err error) {
	path, dirErr := systemDirectory()
	if dirErr != nil {
		return
	}
	user := syscall.NewLazyDLL(filepath.Join(path, "user32.dll"))
	user.NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(utf16ptr(err.Error()))),
		uintptr(unsafe.Pointer(utf16ptr("DiscordLink"))), 0x10)
}

func utf16ptr(value string) *uint16 {
	ptr, _ := syscall.UTF16PtrFromString(strings.ReplaceAll(value, "\x00", ""))
	return ptr
}

func (t *windowsTray) diagnosticText() string {
	text := t.app.snapshot().diagnostic() + t.reporter.statusText() + "\r\n"
	if t.reporter != nil && t.reporter.enabled && t.reporter.installation != "" {
		text += "ID установки: " + t.reporter.installation + "\r\n"
	}
	return text
}

func (t *windowsTray) copyReport(text string) error {
	w := syscall.StringToUTF16(text)
	mem, _, err := kernel.NewProc("GlobalAlloc").Call(2, uintptr(2*len(w))) // GMEM_MOVEABLE
	if mem == 0 {
		return err
	}
	defer func() {
		if mem != 0 {
			kernel.NewProc("GlobalFree").Call(mem)
		}
	}()
	ptr, _, err := kernel.NewProc("GlobalLock").Call(mem)
	if ptr == 0 {
		return err
	}
	kernel.NewProc("lstrcpyW").Call(ptr, uintptr(unsafe.Pointer(&w[0])))
	kernel.NewProc("GlobalUnlock").Call(mem)

	ok, _, err := t.user.NewProc("OpenClipboard").Call(t.hwnd)
	if ok == 0 {
		return err
	}
	defer t.user.NewProc("CloseClipboard").Call()
	ok, _, err = t.user.NewProc("EmptyClipboard").Call()
	if ok == 0 {
		return err
	}
	ok, _, err = t.user.NewProc("SetClipboardData").Call(13, mem) // CF_UNICODETEXT
	if ok == 0 {
		return err
	}
	mem = 0 // Windows owns the global memory after SetClipboardData succeeds.
	return nil
}

const startupKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func (t *windowsTray) reg(args ...string) error {
	cmd := exec.Command(filepath.Join(t.systemDir, "reg.exe"), args...)
	configureChild(cmd)
	return cmd.Run()
}

func (t *windowsTray) startupEnabled() bool {
	return t.reg("query", startupKey, "/v", "DiscordLink") == nil
}

func (t *windowsTray) setStartup(enabled bool) error {
	if !enabled {
		return t.reg("delete", startupKey, "/v", "DiscordLink", "/f")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	command := `"` + exe + `" -tray`
	return t.reg("add", startupKey, "/v", "DiscordLink", "/t", "REG_SZ", "/d", command, "/f")
}

func (t *windowsTray) createIcon() uintptr {
	const size = 32
	var mask [size * 4]byte
	var pixels [size * size]uint32
	for y := range size {
		for x := range size {
			// An indigo disc and two staggered mint connection bars.
			dx, dy := 2*x-size+1, 2*y-size+1
			if dx*dx+dy*dy > 28*28 {
				mask[y*4+x/8] |= 0x80 >> (x % 8)
				continue
			}
			color := uint32(0xff383474)
			if (x >= 7 && x <= 19 && y >= 11 && y <= 14) ||
				(x >= 12 && x <= 24 && y >= 17 && y <= 20) ||
				(x >= 14 && x <= 17 && y >= 14 && y <= 17) {
				color = 0xff82f8c8
			}
			pixels[y*size+x] = color
		}
	}
	icon, _, _ := t.user.NewProc("CreateIcon").Call(0, size, size, 1, 32,
		uintptr(unsafe.Pointer(&mask[0])), uintptr(unsafe.Pointer(&pixels[0])))
	return icon
}
