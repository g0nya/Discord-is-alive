//go:build !windows

package main

import (
	"fmt"
	"os/exec"
)

func requestElevation() (bool, error) {
	return false, fmt.Errorf("the DPI engine requires Windows 10/11 x64; -check works on this system")
}

func acquireSession() (func(), error) { return func() {}, nil }
func configureChild(*exec.Cmd)        {}
