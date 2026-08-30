//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// diskStats reports the root filesystem's total/free space. Linux-only
// (syscall.Statfs) - see hoststatus_other.go for the non-Linux stub.
func diskStats() (totalMB, freeMB uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return 0, 0, err
	}
	blockSize := uint64(stat.Bsize)
	return (stat.Blocks * blockSize) / (1024 * 1024), (stat.Bavail * blockSize) / (1024 * 1024), nil
}

// uptimeSeconds reads /proc/uptime - the first field is seconds since boot.
func uptimeSeconds() (float64, error) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("unexpected /proc/uptime format")
	}
	return strconv.ParseFloat(fields[0], 64)
}

// loadAvg reads the three load-average figures from /proc/loadavg.
func loadAvg() (avg1, avg5, avg15 float64, err error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("unexpected /proc/loadavg format")
	}
	avg1, err1 := strconv.ParseFloat(fields[0], 64)
	avg5, err5 := strconv.ParseFloat(fields[1], 64)
	avg15, err15 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil {
		return 0, 0, 0, err1
	}
	if err5 != nil {
		return 0, 0, 0, err5
	}
	if err15 != nil {
		return 0, 0, 0, err15
	}
	return avg1, avg5, avg15, nil
}
