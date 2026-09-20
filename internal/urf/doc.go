// Package urf translates the supported Apple Raster (URF) subset to PWG
// Raster.
//
// The package deliberately keeps the compressed raster packets in their
// original form.  URF and PWG use the same modified PackBits stream for the
// supported pixel layouts, and CUPS accepts the URF clear-to-end-of-line
// opcode (0x80) while reading PWG too.  Translate validates the stream while
// copying it; it does not render, resample, reorder, or buffer a complete
// page.
//
// A source passed to Translate starts with the 12-byte URF file header.  CUPS
// recognizes the four-byte UNIR (or reverse-byte-order spelling RINU) sync
// word and ignores the remaining eight file-header bytes, including the
// advisory page count.  The destination is a private, empty io.WriteSeeker.  A
// successful call rewinds the destination to offset zero.  On any error the
// destination contains unusable staging data and the caller must discard it.
//
// The accepted mappings are MappingW8ToSGray8, MappingSRGB24ToSRGB8, and
// MappingDEVRGB24ToRGB8.  The source color-space and bit depth are checked on
// every page; an arbitrary eight-bit device color is not accepted as W8.
// PrintQuality accepts the normalized/default value 0 and IPP values 3, 4, or
// 5.  MediaName and MediaType are copied to fixed 64-byte PWG fields and must
// contain no NUL byte or more than 63 bytes.
// Zero Limits fields select the finite DefaultLimits value for that resource.
package urf
