//go:build !windows && !linux

package main

import (
	"errors"
	"time"
)

func cpuTime(pid int) (time.Duration, error) {
	return 0, errors.New("process CPU time is measured on Windows and Linux only")
}
