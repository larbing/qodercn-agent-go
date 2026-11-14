//go:build windows

package qoder

import "syscall"

// spawnAttr hides the console window of the spawned CLI process. The CLI
// executable is a console-subsystem binary; when the parent is a windowsgui
// app (e.g. the Wails desktop build of qodercn-server) there is no console
// to inherit, so Windows allocates a new one and a black window flashes on
// every daemon spawn. CREATE_NO_WINDOW gives the child a hidden console and
// HideWindow is set as a belt-and-braces measure; stdio pipes are unaffected.
const createNoWindow = 0x08000000

func spawnAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
