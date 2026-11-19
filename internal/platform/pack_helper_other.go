//go:build !linux

package platform

import "syscall"

// Pack builds require the Linux subreaper to establish quiescence.
const PackHelperInvocation = "__internal-pack-helper"

type packHelperResult struct {
	ExitCode int `json:"exit_code"`
}

func RunPackHelper() int { return 1 }

func packHelperSysProcAttr() *syscall.SysProcAttr { return nil }
