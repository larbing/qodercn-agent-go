//go:build !windows

package qoder

import "syscall"

// spawnAttr returns nil on non-Windows: the spawned CLI inherits the
// parent's terminal (or has none in service contexts) and can never flash
// a console window, so no special attributes are needed.
func spawnAttr() *syscall.SysProcAttr { return nil }
