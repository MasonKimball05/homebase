//go:build !windows

package resources

import "os/exec"

func hideWindow(*exec.Cmd) {}
