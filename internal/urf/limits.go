package urf

// Limits bounds work performed by Translate.  A zero field selects the finite
// value from DefaultLimits; there is no unlimited mode because the translator
// is intended for network-facing input.  Negative signed limits are invalid
// options.
//
// MaxDecodedBytes counts logical raster bytes, including row repetitions.  It
// is a work bound and does not cause those bytes to be allocated.  The
// compressed stream is copied with fixed-size scratch storage.
type Limits struct {
	MaxInputBytes   int64
	MaxOutputBytes  int64
	MaxPages        uint32
	MaxWidth        uint32
	MaxHeight       uint32
	MaxRowBytes     uint64
	MaxDecodedBytes uint64
}

// DefaultLimits returns a conservative set of finite limits suitable for
// network-facing use.  Callers that need a different policy can copy it and
// adjust individual fields.  A zero field in Options.Limits still selects
// the corresponding default.
func DefaultLimits() Limits {
	return Limits{
		MaxInputBytes:   1 << 30,
		MaxOutputBytes:  1 << 30,
		MaxPages:        1024,
		MaxWidth:        1 << 20,
		MaxHeight:       1 << 20,
		MaxRowBytes:     1 << 24,
		MaxDecodedBytes: 1 << 40,
	}
}

func validateLimits(l Limits) error {
	if l.MaxInputBytes < 0 || l.MaxOutputBytes < 0 {
		return wrapError(KindInvalidOptions, "limits", errNegativeLimit, -1, 0, 0)
	}
	return nil
}
