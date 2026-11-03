package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// cpuTime is the user + system CPU time used so far by process pid, from
// /proc/<pid>/stat (fields 14 and 15, in clock ticks of 1/100 s).
func cpuTime(pid int) (time.Duration, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The command name (field 2) may hold spaces; fields restart after ")".
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("bad /proc stat")
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3 (state)
	if len(f) < 13 {
		return 0, fmt.Errorf("bad /proc stat")
	}
	utime, err1 := strconv.ParseInt(f[11], 10, 64)
	stime, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("bad /proc stat")
	}
	return time.Duration(utime+stime) * 10 * time.Millisecond, nil
}
