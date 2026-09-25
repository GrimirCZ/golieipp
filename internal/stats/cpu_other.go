//go:build !linux && !darwin

package stats

import "time"

// ReadCPU reports unavailable CPU counters on platforms without the supported
// process resource-usage implementation.
func ReadCPU() CPUReading {
	return CPUReading{At: time.Now().UTC()}
}
