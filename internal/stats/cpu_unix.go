//go:build linux || darwin

package stats

import (
	"time"

	"golang.org/x/sys/unix"
)

// ReadCPU returns cumulative process CPU time. The reading is deliberately
// process-wide: assigning it to overlapping actions would double count CPU.
func ReadCPU() CPUReading {
	reading := CPUReading{At: time.Now().UTC()}
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return reading
	}
	reading.Available = true
	reading.UserSeconds = timevalSeconds(usage.Utime)
	reading.SystemSeconds = timevalSeconds(usage.Stime)
	return reading
}

func timevalSeconds(value unix.Timeval) float64 {
	return float64(value.Sec) + float64(value.Usec)/1_000_000
}
