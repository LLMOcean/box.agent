//go:build !linux

package main

import "fmt"

// Non-Linux stand-ins for hoststatus_linux.go - box-agent ships Windows and
// Darwin binaries too (deploy/install.ps1, deploy/install.sh), where there's
// no /proc and no syscall.Statfs_t in this shape. Returning an error just
// leaves these HostStatus fields at their zero value (collectHostStatus's
// callers all check err before assigning), omitted from the report via
// omitempty rather than sent as misleading zeros.

func diskStats() (totalMB, freeMB uint64, err error) {
	return 0, 0, fmt.Errorf("disk stats not implemented on this platform")
}

func uptimeSeconds() (float64, error) {
	return 0, fmt.Errorf("uptime not implemented on this platform")
}

func loadAvg() (avg1, avg5, avg15 float64, err error) {
	return 0, 0, 0, fmt.Errorf("load average not implemented on this platform")
}
