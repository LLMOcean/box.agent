package main

import (
	"log"
	"os"
	"strings"
)

// debugEnabled mirrors router.agent's debug.Enabled convention (DEBUG=true/1/yes)
// so both halves of the pipeline can be turned up with the same env var.
var debugEnabled = func() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("DEBUG")))
	return v == "true" || v == "1" || v == "yes"
}()

// debugLog behaves like log.Printf but is a no-op when DEBUG isn't set.
func debugLog(format string, args ...interface{}) {
	if debugEnabled {
		log.Printf("[debug] "+format, args...)
	}
}
