package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")

func systemDirectory() (string, error) {
	var systemDir [260]uint16
	length, _, err := kernel.NewProc("GetSystemDirectoryW").Call(
		uintptr(unsafe.Pointer(&systemDir[0])), uintptr(len(systemDir)),
	)
	if length == 0 || length >= uintptr(len(systemDir)) {
		return "", fmt.Errorf("cannot locate Windows system directory: %w", err)
	}
	return syscall.UTF16ToString(systemDir[:length]), nil
}

func requestElevation() (bool, error) {
	dir, err := systemDirectory()
	if err != nil {
		return false, err
	}
	shell := syscall.NewLazyDLL(filepath.Join(dir, "shell32.dll"))
	admin, _, _ := shell.NewProc("IsUserAnAdmin").Call()
	if admin != 0 {
		return false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	encoded := make([]string, 0, len(os.Args)-1)
	for _, arg := range os.Args[1:] {
		encoded = append(encoded, syscall.EscapeArg(arg))
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	args, _ := syscall.UTF16PtrFromString(strings.Join(encoded, " "))
	result, _, err := shell.NewProc("ShellExecuteW").Call(
		0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(args)), 0, 1,
	)
	if result <= 32 {
		return false, fmt.Errorf("administrator launch failed (%d): %w", result, err)
	}
	return true, nil
}

func acquireSession() (func(), error) {
	name, _ := syscall.UTF16PtrFromString(`Global\DiscordLink-Session-v1`)
	handle, _, err := kernel.NewProc("CreateMutexW").Call(0, 0, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		return nil, fmt.Errorf("cannot lock session: %w", err)
	}
	release := func() { syscall.CloseHandle(syscall.Handle(handle)) }
	if err == syscall.ERROR_ALREADY_EXISTS {
		release()
		return nil, fmt.Errorf("DiscordLink is already running")
	}
	if err := checkOtherEngine(); err != nil {
		release()
		return nil, err
	}
	if err := bindJob(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func checkOtherEngine() error {
	snapshot, err := syscall.CreateToolhelp32Snapshot(2, 0) // TH32CS_SNAPPROCESS
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(snapshot)
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err := syscall.Process32First(snapshot, &entry); ; err = syscall.Process32Next(snapshot, &entry) {
		if err == syscall.ERROR_NO_MORE_FILES {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.EqualFold(syscall.UTF16ToString(entry.ExeFile[:]), "winws.exe") {
			return fmt.Errorf("another winws.exe is running. Stop it before starting DiscordLink")
		}
	}
}

type basicLimitInformation struct {
	perProcessUserTime, perJobUserTime int64
	flags                              uint32
	minWorkingSet, maxWorkingSet       uintptr
	activeProcessLimit                 uint32
	affinity                           uintptr
	priorityClass, schedulingClass     uint32
}

type extendedLimitInformation struct {
	basic                                                      basicLimitInformation
	ioCounters                                                 [6]uint64
	processMemory, jobMemory, peakProcessMemory, peakJobMemory uintptr
}

func bindJob() error {
	// Descendants inherit job membership. When the controller exits, Windows
	// closes its only job handle and terminates the packet engine as well.
	job, _, err := kernel.NewProc("CreateJobObjectW").Call(0, 0)
	if job == 0 {
		return fmt.Errorf("cannot create job: %w", err)
	}
	info := extendedLimitInformation{basic: basicLimitInformation{flags: 0x2000}} // KILL_ON_JOB_CLOSE
	ok, _, err := kernel.NewProc("SetInformationJobObject").Call(
		job, 9, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info),
	)
	if ok != 0 {
		const currentProcess = ^uintptr(0)
		ok, _, err = kernel.NewProc("AssignProcessToJobObject").Call(job, currentProcess)
	}
	if ok == 0 {
		syscall.CloseHandle(syscall.Handle(job))
		return fmt.Errorf("cannot set automatic engine cleanup: %w", err)
	}
	return nil
}

func configureChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
