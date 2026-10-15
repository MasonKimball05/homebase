//go:build !windows

package projects

import "os/exec"

func hideWindow(*exec.Cmd) {}
