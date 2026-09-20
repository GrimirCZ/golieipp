package urf

import "errors"

var errNegativeLimit = errors.New("limits must not be negative")

// Mapping is an exact source-to-PWG pixel-layout mapping.  No color
// conversion, channel reordering, or resampling is performed.
type Mapping uint8

const (
	// MappingW8ToSGray8 maps URF W8 (color-space code 0, 8 bits/pixel) to
	// PWG sgray_8.
	MappingW8ToSGray8 Mapping = iota + 1
	// MappingSRGB24ToSRGB8 maps URF SRGB24 (color-space code 1, 24 bits/pixel)
	// to PWG srgb_8.
	MappingSRGB24ToSRGB8
	// MappingDEVRGB24ToRGB8 maps URF DEVRGB24 (color-space code 5, 24 bits/pixel)
	// to PWG rgb_8.
	MappingDEVRGB24ToRGB8
)

func (m Mapping) String() string {
	switch m {
	case MappingW8ToSGray8:
		return "W8->sgray_8"
	case MappingSRGB24ToSRGB8:
		return "SRGB24->srgb_8"
	case MappingDEVRGB24ToRGB8:
		return "DEVRGB24->rgb_8"
	default:
		return "unknown mapping"
	}
}

// PageSettings contains normalized queue policy used for every translated
// page.  MediaWidth and MediaHeight are hundredths of a millimetre.  A page
// may arrive in either orientation; its width/height pair must match the
// selected media pair after applying the same orientation.
// PrintQuality is the PWG/IPP value 3, 4, or 5, or zero for the normalized
// printer default.  MediaName and MediaType are copied into 64-byte PWG
// fields and therefore may contain at most 63 bytes and no NUL byte.
type PageSettings struct {
	MediaName   string
	MediaWidth  uint32
	MediaHeight uint32
	MediaType   string
	MediaSource uint32

	ResolutionX  uint32
	ResolutionY  uint32
	PrintQuality uint32
	Sides        string
	SheetBack    string
}

// Result describes a successful translation.  InputBytes includes the full
// URF source, OutputBytes is the final PWG size after any header patching, and
// Pages is the number of complete pages emitted.
type Result struct {
	InputBytes  int64
	OutputBytes int64
	Pages       uint32
}

// Options controls one translation.
type Options struct {
	Mapping Mapping
	Page    PageSettings
	Limits  Limits
}
