package urf

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

const (
	urfFileHeaderSize = 12
	urfPageHeaderSize = 32
	pwgPageHeaderSize = 1796
	pwgSync           = uint32(0x52615332) // RaS2, PWG Raster
	urfHeaderPrefix   = "UNIR"
	urfHeaderReversed = "RINU"
	// The largest literal packet in the supported mappings is 128 pixels of
	// DEVRGB24, or 384 bytes.  Keep this scratch storage on the translator so
	// packet-sized copies do not allocate a large buffer for every packet.
	maxCopyScratch = 128 * 3
	maxU32         = uint64(^uint32(0))
)

// Translate validates one Apple Raster document and writes its equivalent
// PWG Raster stream to dst.  Source packets are copied as they are read.  The
// source page count and ignored Apple header fields follow CUPS semantics:
// they are advisory, and page parsing continues until EOF.  A short suffix
// after the final complete page is treated as CUPS treats an incomplete next
// header and ignored; an I/O error is never treated as EOF.
// The Apple file header accepts the UNIR sync word and its RINU byte-order
// spelling; CUPS interprets page-header integers as network-order for both.
// The remaining eight bytes are consumed without validation.
//
// dst must be a private staging destination.  Translate writes an invalid
// zero marker while work is in progress, patches the actual page count, then
// writes the PWG marker and rewinds dst only after complete success.  Any
// returned error has a zero Result; callers must discard dst.
func Translate(ctx context.Context, src io.Reader, dst io.WriteSeeker, opts Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := checkContext(ctx, "validate options", -1, 0, 0); err != nil {
		return Result{}, err
	}
	if src == nil {
		return Result{}, wrapError(KindInvalidOptions, "source", errors.New("nil source"), -1, 0, 0)
	}
	if dst == nil {
		return Result{}, wrapError(KindInvalidOptions, "destination", errors.New("nil destination"), -1, 0, 0)
	}
	if err := validateLimits(opts.Limits); err != nil {
		return Result{}, err
	}
	limits := effectiveLimits(opts.Limits)
	pagePolicy, err := validatePageSettings(opts.Page)
	if err != nil {
		return Result{}, err
	}
	if err := validateMapping(opts.Mapping); err != nil {
		return Result{}, err
	}

	w := &translator{
		ctx:     ctx,
		src:     &sourceReader{ctx: ctx, r: src, limit: limits.MaxInputBytes},
		dst:     dst,
		mapping: opts.Mapping,
		policy:  pagePolicy,
		limits:  limits,
	}

	// The destination is private staging and must be empty.  io.WriteSeeker
	// cannot truncate a stale suffix, so reject one before emitting anything.
	// Check every returned position: a nil seek error does not guarantee that
	// the requested position was honored.
	pos, err := dst.Seek(0, io.SeekStart)
	if err != nil {
		return Result{}, wrapError(KindDestinationIO, "seek destination", err, -1, 0, 0)
	}
	if pos != 0 {
		return Result{}, wrapError(KindDestinationIO, "seek destination", fmt.Errorf("seek returned offset %d, want 0", pos), -1, 0, 0)
	}
	end, err := dst.Seek(0, io.SeekEnd)
	if err != nil {
		return Result{}, wrapError(KindDestinationIO, "inspect destination", err, -1, 0, 0)
	}
	if end != 0 {
		return Result{}, wrapError(KindDestinationIO, "inspect destination", fmt.Errorf("destination is not empty (size %d)", end), -1, 0, 0)
	}
	pos, err = dst.Seek(0, io.SeekStart)
	if err != nil {
		return Result{}, wrapError(KindDestinationIO, "rewind destination", err, -1, 0, 0)
	}
	if pos != 0 {
		return Result{}, wrapError(KindDestinationIO, "rewind destination", fmt.Errorf("seek returned offset %d, want 0", pos), -1, 0, 0)
	}
	if err := w.write(make([]byte, 4), 0, 0); err != nil {
		return Result{}, err
	}

	if err := w.readFileHeader(); err != nil {
		return Result{}, err
	}
	if err := w.translatePages(); err != nil {
		return Result{}, err
	}
	if w.pages == 0 {
		return Result{}, w.malformed("document contains no complete pages", 0)
	}
	if err := w.finalize(); err != nil {
		return Result{}, err
	}

	return Result{InputBytes: w.src.count, OutputBytes: w.outputSize, Pages: w.pages}, nil
}

type pagePolicy struct {
	mediaName   string
	mediaWidth  uint32
	mediaHeight uint32
	mediaType   string
	mediaSource uint32
	resX        uint32
	resY        uint32
	quality     uint32
	sides       string
	sheetBack   string
}

func validatePageSettings(p PageSettings) (pagePolicy, error) {
	if err := validatePWGString("media name", p.MediaName); err != nil {
		return pagePolicy{}, err
	}
	if err := validatePWGString("media type", p.MediaType); err != nil {
		return pagePolicy{}, err
	}
	if p.MediaWidth == 0 || p.MediaHeight == 0 {
		return pagePolicy{}, wrapError(KindInvalidOptions, "page settings", errors.New("media dimensions must be positive hundredths of a millimetre"), -1, 0, 0)
	}
	if p.ResolutionX == 0 || p.ResolutionY == 0 {
		return pagePolicy{}, wrapError(KindInvalidOptions, "page settings", errors.New("resolution must be positive"), -1, 0, 0)
	}
	if p.ResolutionX != p.ResolutionY {
		return pagePolicy{}, wrapError(KindInvalidOptions, "page settings", errors.New("URF carries one square resolution; ResolutionX and ResolutionY must match"), -1, 0, 0)
	}

	sides := p.Sides
	if sides == "" {
		sides = "one-sided"
	}
	switch sides {
	case "one-sided", "two-sided-long-edge", "two-sided-short-edge":
	default:
		return pagePolicy{}, wrapError(KindInvalidOptions, "page settings", fmt.Errorf("unsupported sides %q", sides), -1, 0, 0)
	}

	sheetBack := p.SheetBack
	if sheetBack == "" {
		sheetBack = "normal"
	}
	switch sheetBack {
	case "normal", "flipped", "manual-tumble", "rotated":
	default:
		return pagePolicy{}, wrapError(KindInvalidOptions, "page settings", fmt.Errorf("unsupported sheet-back %q", sheetBack), -1, 0, 0)
	}

	switch p.PrintQuality {
	case 0, 3, 4, 5:
		// Zero is the normalized/default value; 3, 4, and 5 are the IPP
		// draft, normal, and high values represented by PWG Raster.
	default:
		return pagePolicy{}, wrapError(KindInvalidOptions, "page settings", fmt.Errorf("unsupported print quality %d", p.PrintQuality), -1, 0, 0)
	}

	return pagePolicy{
		mediaName:   p.MediaName,
		mediaWidth:  p.MediaWidth,
		mediaHeight: p.MediaHeight,
		mediaType:   p.MediaType,
		mediaSource: p.MediaSource,
		resX:        p.ResolutionX,
		resY:        p.ResolutionY,
		quality:     p.PrintQuality,
		sides:       sides,
		sheetBack:   sheetBack,
	}, nil
}

func validatePWGString(field, value string) error {
	if strings.IndexByte(value, 0) >= 0 {
		return wrapError(KindInvalidOptions, "page settings", fmt.Errorf("%s contains NUL", field), -1, 0, 0)
	}
	if len(value) >= 64 {
		return wrapError(KindInvalidOptions, "page settings", fmt.Errorf("%s is too long for a PWG header", field), -1, 0, 0)
	}
	return nil
}

func validateMapping(m Mapping) error {
	switch m {
	case MappingW8ToSGray8, MappingSRGB24ToSRGB8, MappingDEVRGB24ToRGB8:
		return nil
	default:
		return wrapError(KindInvalidOptions, "mapping", fmt.Errorf("unsupported mapping %d", m), -1, 0, 0)
	}
}

type effectiveLimitValues struct {
	MaxInputBytes   int64
	MaxOutputBytes  int64
	MaxPages        uint32
	MaxWidth        uint32
	MaxHeight       uint32
	MaxRowBytes     uint64
	MaxDecodedBytes uint64
}

func effectiveLimits(l Limits) effectiveLimitValues {
	d := DefaultLimits()
	if l.MaxInputBytes != 0 {
		d.MaxInputBytes = l.MaxInputBytes
	}
	if l.MaxOutputBytes != 0 {
		d.MaxOutputBytes = l.MaxOutputBytes
	}
	if l.MaxPages != 0 {
		d.MaxPages = l.MaxPages
	}
	if l.MaxWidth != 0 {
		d.MaxWidth = l.MaxWidth
	}
	if l.MaxHeight != 0 {
		d.MaxHeight = l.MaxHeight
	}
	if l.MaxRowBytes != 0 {
		d.MaxRowBytes = l.MaxRowBytes
	}
	if l.MaxDecodedBytes != 0 {
		d.MaxDecodedBytes = l.MaxDecodedBytes
	}
	return effectiveLimitValues{
		MaxInputBytes: d.MaxInputBytes, MaxOutputBytes: d.MaxOutputBytes,
		MaxPages: d.MaxPages, MaxWidth: d.MaxWidth, MaxHeight: d.MaxHeight,
		MaxRowBytes: d.MaxRowBytes, MaxDecodedBytes: d.MaxDecodedBytes,
	}
}

type translator struct {
	ctx    context.Context
	src    *sourceReader
	dst    io.WriteSeeker
	limits effectiveLimitValues

	mapping Mapping
	policy  pagePolicy

	pages       uint32
	pageCounts  []int64
	outputBytes int64
	outputSize  int64
	pageLogical uint64
	scratch     [maxCopyScratch]byte
}

func (t *translator) readFileHeader() error {
	var h [urfFileHeaderSize]byte
	if err := t.readFull(h[:], 0, 0); err != nil {
		return err
	}
	// CUPS treats the first four bytes as the Apple raster sync word and reads
	// the remaining eight file-header bytes without validating their AST/count
	// contents.  Accept both byte-order spellings for compatibility.
	if string(h[:4]) != urfHeaderPrefix && string(h[:4]) != urfHeaderReversed {
		return t.malformed("invalid Apple Raster file signature", 0)
	}
	return nil
}

func (t *translator) translatePages() error {
	for {
		if err := checkContext(t.ctx, "read page header", t.src.count, t.pages+1, 0); err != nil {
			return err
		}
		var raw [urfPageHeaderSize]byte
		n, err := t.readHeader(raw[:])
		if err != nil {
			return err
		}
		if n == 0 {
			// Clean EOF after a complete page.
			return nil
		}
		if n < len(raw) {
			// CUPS stops at an incomplete next page header and treats it as
			// the end of the raster stream.  The suffix has already been
			// consumed and is included in InputBytes.
			return nil
		}

		if t.limits.MaxPages > 0 && t.pages >= t.limits.MaxPages {
			return t.resource("maximum page count exceeded", t.pages+1, 0)
		}
		page, err := t.parsePage(raw[:])
		if err != nil {
			return err
		}
		if err := t.emitPageHeader(page); err != nil {
			return err
		}
		if err := t.copyPageData(page); err != nil {
			return err
		}
		t.pages++
	}
}

// readHeader returns 0 for clean EOF, 1..31 for a CUPS-compatible incomplete
// suffix, and 32 for a full header.  A source error is always surfaced.
func (t *translator) readHeader(buf []byte) (int, error) {
	if len(buf) != urfPageHeaderSize {
		panic("internal: invalid page header buffer")
	}
	total := 0
	for total < len(buf) {
		n, err := t.src.read(buf[total:])
		total += n
		if err == nil {
			continue
		}
		var limitErr *inputLimitError
		if errors.As(err, &limitErr) {
			return 0, t.resource("maximum input bytes exceeded", t.pages+1, 0)
		}
		if t.ctx.Err() != nil {
			return 0, canceledError(t.ctx, "read page header", t.src.count, t.pages+1, 0)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return total, nil
		}
		return 0, wrapError(KindSourceIO, "read page header", err, t.src.count, t.pages+1, 0)
	}
	return total, nil
}

type pageDescription struct {
	bitsPerPixel uint8
	colorSpace   uint8
	duplexMode   uint8
	width        uint32
	height       uint32
	resolution   uint32
	rowBytes     uint64
	landscape    bool

	pwgColorSpace   uint32
	pwgBitsPerPixel uint32
	pwgBitsPerColor uint32
	pwgNumColors    uint32
}

func (t *translator) parsePage(raw []byte) (pageDescription, error) {
	p := pageDescription{
		bitsPerPixel: raw[0],
		colorSpace:   raw[1],
		duplexMode:   raw[2],
		width:        binary.BigEndian.Uint32(raw[12:16]),
		height:       binary.BigEndian.Uint32(raw[16:20]),
		resolution:   binary.BigEndian.Uint32(raw[20:24]),
	}

	wantSpace, wantBPP, outSpace := sourceMapping(t.mapping)
	if p.colorSpace != wantSpace || p.bitsPerPixel != wantBPP {
		return pageDescription{}, t.unsupported(fmt.Sprintf("page color-space %d/bits-per-pixel %d do not match %s", p.colorSpace, p.bitsPerPixel, t.mapping), t.pages+1)
	}
	if p.width == 0 || p.height == 0 {
		return pageDescription{}, t.malformed("page dimensions must be positive", t.pages+1)
	}
	if p.resolution == 0 {
		return pageDescription{}, t.malformed("page resolution must be positive", t.pages+1)
	}
	if p.resolution != t.policy.resX || p.resolution != t.policy.resY {
		return pageDescription{}, t.settingsMismatch("page resolution conflicts with normalized settings", t.pages+1)
	}
	if t.limits.MaxWidth > 0 && p.width > t.limits.MaxWidth {
		return pageDescription{}, t.resource("maximum page width exceeded", t.pages+1, 0)
	}
	if t.limits.MaxHeight > 0 && p.height > t.limits.MaxHeight {
		return pageDescription{}, t.resource("maximum page height exceeded", t.pages+1, 0)
	}

	bpp := uint64(wantBPP / 8)
	rowBytes, ok := checkedMul(uint64(p.width), bpp)
	if !ok || rowBytes == 0 {
		return pageDescription{}, t.resource("page row size overflows", t.pages+1, 0)
	}
	if t.limits.MaxRowBytes > 0 && rowBytes > t.limits.MaxRowBytes {
		return pageDescription{}, t.resource("maximum row size exceeded", t.pages+1, 0)
	}
	if rowBytes > uint64(^uint32(0)) {
		return pageDescription{}, t.resource("page row size does not fit PWG header", t.pages+1, 0)
	}
	pageBytes, ok := checkedMul(rowBytes, uint64(p.height))
	if !ok {
		return pageDescription{}, t.resource("page decoded size overflows", t.pages+1, 0)
	}
	if !checkedAddWithin(&t.pageLogical, pageBytes, t.limits.MaxDecodedBytes) {
		return pageDescription{}, t.resource("maximum decoded bytes exceeded", t.pages+1, 0)
	}

	if t.policy.mediaWidth != 0 && t.policy.mediaHeight != 0 {
		directW, directH, ok := mediaPixels(t.policy.mediaWidth, t.policy.mediaHeight, p.resolution)
		if !ok {
			return pageDescription{}, t.resource("media geometry overflows", t.pages+1, 0)
		}
		switch {
		case p.width == directW && p.height == directH:
			p.landscape = false
		case p.width == directH && p.height == directW:
			p.landscape = true
		default:
			return pageDescription{}, t.settingsMismatch("page geometry conflicts with normalized media", t.pages+1)
		}
	}

	if err := t.validateDuplex(p.duplexMode, t.pages+1); err != nil {
		return pageDescription{}, err
	}
	p.rowBytes = rowBytes
	p.pwgColorSpace = outSpace
	p.pwgBitsPerPixel = uint32(wantBPP)
	p.pwgBitsPerColor = 8
	if wantBPP == 8 {
		p.pwgNumColors = 1
	} else {
		p.pwgNumColors = 3
	}
	return p, nil
}

func sourceMapping(m Mapping) (space, bits uint8, outSpace uint32) {
	switch m {
	case MappingW8ToSGray8:
		return 0, 8, 18 // CUPS_CSPACE_SW
	case MappingSRGB24ToSRGB8:
		return 1, 24, 19 // CUPS_CSPACE_SRGB
	case MappingDEVRGB24ToRGB8:
		return 5, 24, 1 // CUPS_CSPACE_RGB
	default:
		return 0, 0, 0
	}
}

func (t *translator) validateDuplex(mode uint8, page uint32) error {
	inputDuplex := mode >= 2
	inputTumble := mode == 2
	switch t.policy.sides {
	case "one-sided":
		if inputDuplex {
			return t.settingsMismatch("URF duplex mode conflicts with one-sided policy", page)
		}
	case "two-sided-long-edge":
		if !inputDuplex || inputTumble {
			return t.settingsMismatch("URF duplex mode conflicts with long-edge policy", page)
		}
	case "two-sided-short-edge":
		if !inputDuplex || !inputTumble {
			return t.settingsMismatch("URF duplex mode conflicts with short-edge policy", page)
		}
	}
	return nil
}

func mediaPixels(width, height, dpi uint32) (uint32, uint32, bool) {
	w, ok := checkedMul(uint64(width), uint64(dpi))
	if !ok {
		return 0, 0, false
	}
	h, ok := checkedMul(uint64(height), uint64(dpi))
	if !ok {
		return 0, 0, false
	}
	w /= 2540
	h /= 2540
	if w == 0 || h == 0 || w > maxU32 || h > maxU32 {
		return 0, 0, false
	}
	return uint32(w), uint32(h), true
}

func (t *translator) emitPageHeader(p pageDescription) error {
	h := make([]byte, pwgPageHeaderSize)
	copyString(h[0:64], "PwgRaster")
	copyString(h[128:192], t.policy.mediaType)
	copyString(h[1732:1796], t.policy.mediaName)

	put32(h, 272, bool32(t.policy.sides != "one-sided")) // Duplex
	put32(h, 276, t.policy.resX)
	put32(h, 280, t.policy.resY)

	pageWidth, pageHeight := t.policy.mediaWidth, t.policy.mediaHeight
	if p.landscape {
		pageWidth, pageHeight = pageHeight, pageWidth
	}
	pageWPoints, pageHPoints := uint32(uint64(pageWidth)*72/2540), uint32(uint64(pageHeight)*72/2540)
	put32(h, 284, 0)
	put32(h, 288, 0)
	put32(h, 292, pageWPoints)
	put32(h, 296, pageHPoints)
	put32(h, 324, t.policy.mediaSource)
	put32(h, 352, pageWPoints)
	put32(h, 356, pageHPoints)
	put32(h, 368, bool32(t.policy.sides == "two-sided-short-edge")) // Tumble
	put32(h, 372, p.width)
	put32(h, 376, p.height)
	put32(h, 384, p.pwgBitsPerColor)
	put32(h, 388, p.pwgBitsPerPixel)
	put32(h, 392, uint32(p.rowBytes))
	put32(h, 396, 0) // CUPS_ORDER_CHUNKED
	put32(h, 400, p.pwgColorSpace)
	put32(h, 420, p.pwgNumColors)

	// CUPS initializes the floating-point page size from the selected media and
	// leaves the floating-point imaging bounding box zeroed; the integer image
	// box below carries the raster geometry.
	putFloat32(h, 428, float32(float64(pageWidth)*72/2540.0))
	putFloat32(h, 432, float32(float64(pageHeight)*72/2540.0))
	putFloat32(h, 436, 0)
	putFloat32(h, 440, 0)
	putFloat32(h, 444, 0)
	putFloat32(h, 448, 0)

	// TotalPageCount is patched after the complete stream is validated.
	countOffset := t.outputBytes + 452
	put32(h, 452, 0)

	// Sheet transforms follow CUPS's _cupsRasterInitPWGHeader mapping.  CUPS
	// applies the configured sheet-back value only when initializing a back
	// side; the first page of each duplex pair is always the front side.
	put32(h, 456, 1)
	put32(h, 460, 1)
	t.setSheetBack(h, t.policy.sides != "one-sided" && t.pages%2 == 1)
	put32(h, 464, 0)
	put32(h, 468, 0)
	put32(h, 472, p.width)
	put32(h, 476, p.height)
	put32(h, 480, 0xffffff)
	put32(h, 484, t.policy.quality)

	if t.limits.MaxPages > 0 && uint32(len(t.pageCounts)+1) > t.limits.MaxPages {
		return t.resource("maximum page count exceeded", t.pages+1, 0)
	}
	if err := t.write(h, t.pages+1, 0); err != nil {
		return err
	}
	t.pageCounts = append(t.pageCounts, countOffset)
	return nil
}

func (t *translator) setSheetBack(h []byte, back bool) {
	if !back {
		return
	}
	flipCross := uint32(0xffffffff)
	flipFeed := uint32(0xffffffff)
	tumble := t.policy.sides == "two-sided-short-edge"
	switch t.policy.sheetBack {
	case "flipped":
		if tumble {
			put32(h, 456, flipCross)
		} else {
			put32(h, 460, flipFeed)
		}
	case "manual-tumble":
		if tumble {
			put32(h, 456, flipCross)
			put32(h, 460, flipFeed)
		}
	case "rotated":
		if !tumble {
			put32(h, 456, flipCross)
			put32(h, 460, flipFeed)
		}
	}
}

func (t *translator) copyPageData(p pageDescription) error {
	rowsDone := uint64(0)
	rowBytes := p.rowBytes
	bpp := uint64(p.pwgBitsPerPixel / 8)
	if bpp == 0 {
		return t.malformed("zero bytes per pixel", t.pages+1)
	}
	for rowsDone < uint64(p.height) {
		if err := checkContext(t.ctx, "copy raster row", t.src.count, t.pages+1, uint32(rowsDone+1)); err != nil {
			return err
		}
		var repeat [1]byte
		if err := t.readFull(repeat[:], t.pages+1, uint32(rowsDone+1)); err != nil {
			return err
		}
		if err := t.write(repeat[:], t.pages+1, uint32(rowsDone+1)); err != nil {
			return err
		}
		repeatRows := uint64(repeat[0]) + 1
		remaining := rowBytes
		for remaining > 0 {
			if err := checkContext(t.ctx, "copy raster packet", t.src.count, t.pages+1, uint32(rowsDone+1)); err != nil {
				return err
			}
			var control [1]byte
			if err := t.readFull(control[:], t.pages+1, uint32(rowsDone+1)); err != nil {
				return err
			}
			if err := t.write(control[:], t.pages+1, uint32(rowsDone+1)); err != nil {
				return err
			}

			switch {
			case control[0] == 0x80:
				// CUPS fills the rest of a row with 0xff for all three
				// supported source spaces.  The opcode has no payload.
				remaining = 0
			case control[0]&0x80 != 0:
				pixels := uint64(257 - uint16(control[0]))
				want, ok := checkedMul(pixels, bpp)
				if !ok {
					return t.resource("literal packet size overflows", t.pages+1, uint32(rowsDone+1))
				}
				if want > remaining {
					want = remaining
				}
				if err := t.copyBytes(want, t.pages+1, uint32(rowsDone+1)); err != nil {
					return err
				}
				remaining -= want
			default:
				pixels := uint64(control[0]) + 1
				want, ok := checkedMul(pixels, bpp)
				if !ok {
					return t.resource("repeat packet size overflows", t.pages+1, uint32(rowsDone+1))
				}
				if want > remaining {
					want = remaining
				}
				// CUPS consumes one pixel for a repeat packet, even when
				// the represented run is clipped at the row boundary.
				if want < bpp {
					remaining = 0
					continue
				}
				if err := t.copyBytes(bpp, t.pages+1, uint32(rowsDone+1)); err != nil {
					return err
				}
				remaining -= want
			}
		}
		if repeatRows > uint64(p.height)-rowsDone {
			rowsDone = uint64(p.height)
		} else {
			rowsDone += repeatRows
		}
	}
	return nil
}

func (t *translator) copyBytes(n uint64, page, row uint32) error {
	for n > 0 {
		if err := checkContext(t.ctx, "copy raster payload", t.src.count, page, row); err != nil {
			return err
		}
		chunk := uint64(len(t.scratch))
		if chunk > n {
			chunk = n
		}
		if err := t.readFull(t.scratch[:chunk], page, row); err != nil {
			return err
		}
		if err := t.write(t.scratch[:chunk], page, row); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

func (t *translator) readFull(buf []byte, page, row uint32) error {
	if len(buf) == 0 {
		return nil
	}
	total := 0
	for total < len(buf) {
		n, err := t.src.read(buf[total:])
		total += n
		if err == nil {
			continue
		}
		var limitErr *inputLimitError
		if errors.As(err, &limitErr) {
			return t.resource("maximum input bytes exceeded", page, row)
		}
		if t.ctx.Err() != nil {
			return canceledError(t.ctx, "read source", t.src.count, page, row)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return t.malformed("truncated raster data", page, row)
		}
		return wrapError(KindSourceIO, "read source", err, t.src.count, page, row)
	}
	return nil
}

func (t *translator) write(buf []byte, page, row uint32) error {
	if err := checkContext(t.ctx, "write destination", t.src.count, page, row); err != nil {
		return err
	}
	if t.limits.MaxOutputBytes > 0 && uint64(t.outputBytes)+uint64(len(buf)) > uint64(t.limits.MaxOutputBytes) {
		return t.resource("maximum output bytes exceeded", page, row)
	}
	for len(buf) > 0 {
		if err := checkContext(t.ctx, "write destination", t.src.count, page, row); err != nil {
			return err
		}
		n, err := t.dst.Write(buf)
		if n < 0 || n > len(buf) {
			return wrapError(KindDestinationIO, "write destination", errors.New("invalid write count"), t.src.count, page, row)
		}
		t.outputBytes += int64(n)
		if err != nil {
			return wrapError(KindDestinationIO, "write destination", err, t.src.count, page, row)
		}
		if n == 0 {
			return wrapError(KindDestinationIO, "write destination", io.ErrShortWrite, t.src.count, page, row)
		}
		buf = buf[n:]
	}
	return nil
}

func (t *translator) finalize() error {
	if err := checkContext(t.ctx, "finalize", t.src.count, t.pages, 0); err != nil {
		return err
	}
	if uint64(t.pages) > maxU32 {
		return t.resource("page count overflows PWG metadata", t.pages, 0)
	}
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], t.pages)
	for _, off := range t.pageCounts {
		if err := t.seekWrite(off, count[:], "patch page count"); err != nil {
			return err
		}
	}
	var marker [4]byte
	binary.BigEndian.PutUint32(marker[:], pwgSync)
	if err := t.seekWrite(0, marker[:], "write PWG marker"); err != nil {
		return err
	}
	if err := checkContext(t.ctx, "measure destination", t.src.count, t.pages, 0); err != nil {
		return err
	}
	end, err := t.dst.Seek(0, io.SeekEnd)
	if err != nil {
		return wrapError(KindDestinationIO, "measure destination", err, t.src.count, t.pages, 0)
	}
	if end != t.outputBytes {
		return wrapError(KindDestinationIO, "measure destination", fmt.Errorf("destination size %d, want emitted size %d", end, t.outputBytes), t.src.count, t.pages, 0)
	}
	if t.limits.MaxOutputBytes > 0 && t.outputBytes > t.limits.MaxOutputBytes {
		return t.resource("maximum output bytes exceeded", t.pages, 0)
	}
	if err := checkContext(t.ctx, "rewind destination", t.src.count, t.pages, 0); err != nil {
		return err
	}
	pos, err := t.dst.Seek(0, io.SeekStart)
	if err != nil {
		return wrapError(KindDestinationIO, "rewind destination", err, t.src.count, t.pages, 0)
	}
	if pos != 0 {
		return wrapError(KindDestinationIO, "rewind destination", fmt.Errorf("seek returned offset %d, want 0", pos), t.src.count, t.pages, 0)
	}
	if err := checkContext(t.ctx, "complete translation", t.src.count, t.pages, 0); err != nil {
		return err
	}
	// Header patches seek backwards and therefore do not change the emitted
	// byte count.  Report the logical stream size rather than relying on a
	// destination's end position or any stale suffix it might expose.
	t.outputSize = t.outputBytes
	return nil
}

func (t *translator) seekWrite(off int64, buf []byte, op string) error {
	if err := checkContext(t.ctx, op, t.src.count, t.pages, 0); err != nil {
		return err
	}
	if off < 0 {
		return wrapError(KindDestinationIO, op, errors.New("negative seek offset"), t.src.count, t.pages, 0)
	}
	pos, err := t.dst.Seek(off, io.SeekStart)
	if err != nil {
		return wrapError(KindDestinationIO, op, err, t.src.count, t.pages, 0)
	}
	if pos != off {
		return wrapError(KindDestinationIO, op, fmt.Errorf("seek returned offset %d, want %d", pos, off), t.src.count, t.pages, 0)
	}
	for len(buf) > 0 {
		if err := checkContext(t.ctx, op, t.src.count, t.pages, 0); err != nil {
			return err
		}
		n, err := t.dst.Write(buf)
		if n < 0 || n > len(buf) {
			return wrapError(KindDestinationIO, op, errors.New("invalid write count"), t.src.count, t.pages, 0)
		}
		if err != nil {
			return wrapError(KindDestinationIO, op, err, t.src.count, t.pages, 0)
		}
		if n == 0 {
			return wrapError(KindDestinationIO, op, io.ErrShortWrite, t.src.count, t.pages, 0)
		}
		buf = buf[n:]
	}
	return nil
}

func (t *translator) malformed(message string, page ...uint32) error {
	var p, row uint32
	if len(page) > 0 {
		p = page[0]
	}
	if len(page) > 1 {
		row = page[1]
	}
	return wrapError(KindMalformedInput, message, errors.New(message), t.src.count, p, row)
}

func (t *translator) unsupported(message string, page uint32) error {
	return wrapError(KindUnsupportedInput, message, errors.New(message), t.src.count, page, 0)
}

func (t *translator) settingsMismatch(message string, page uint32) error {
	return wrapError(KindSettingsMismatch, message, errors.New(message), t.src.count, page, 0)
}

func (t *translator) resource(message string, page, row uint32) error {
	return wrapError(KindResourceLimit, message, errors.New(message), t.src.count, page, row)
}

func checkContext(ctx context.Context, op string, offset int64, page, row uint32) error {
	if err := ctx.Err(); err != nil {
		return &Error{Kind: KindCanceled, Op: op, Offset: offset, Page: page, Row: row, Err: err}
	}
	return nil
}

type sourceReader struct {
	ctx        context.Context
	r          io.Reader
	count      int64
	limit      int64
	pendingErr error
}

func (r *sourceReader) read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.pendingErr != nil {
		err := r.pendingErr
		r.pendingErr = nil
		return 0, err
	}
	readBuf := p
	if r.limit > 0 {
		remaining := r.limit - r.count
		if remaining < 0 {
			return 0, &inputLimitError{}
		}
		if remaining == 0 {
			// An exact-size source is valid.  Probe one byte so an extra source
			// byte is reported as a limit violation while a real EOF succeeds.
			var probe [1]byte
			n, err := r.r.Read(probe[:])
			if n < 0 || n > len(probe) {
				return 0, errors.New("invalid source read count")
			}
			if n > 0 {
				if err != nil && !errors.Is(err, io.EOF) {
					// Preserve a non-EOF source failure even though the probe
					// also discovered data beyond the configured limit.
					return 0, err
				}
				return 0, &inputLimitError{}
			}
			if err == nil {
				return 0, io.ErrNoProgress
			}
			return 0, err
		}
		if int64(len(readBuf)) > remaining {
			readBuf = readBuf[:remaining]
		}
	}
	n, err := r.r.Read(readBuf)
	if n < 0 || n > len(readBuf) {
		return 0, errors.New("invalid source read count")
	}
	r.count += int64(n)
	if err != nil {
		// Reader implementations may legally return all requested bytes and
		// io.EOF together.  Make the bytes available now and report EOF on the
		// following read; non-EOF errors retain their original n+err result.
		if n > 0 && errors.Is(err, io.EOF) {
			r.pendingErr = io.EOF
			return n, nil
		}
		return n, err
	}
	if n == 0 {
		return 0, io.ErrNoProgress
	}
	return n, nil
}

type inputLimitError struct{}

func (*inputLimitError) Error() string { return "maximum input bytes exceeded" }

func checkedMul(a, b uint64) (uint64, bool) {
	if a != 0 && b > math.MaxUint64/a {
		return 0, false
	}
	return a * b, true
}

func checkedAddWithin(sum *uint64, add, limit uint64) bool {
	if add > math.MaxUint64-*sum {
		return false
	}
	next := *sum + add
	if limit > 0 && next > limit {
		return false
	}
	*sum = next
	return true
}

func copyString(dst []byte, s string) {
	if len(s) > len(dst)-1 {
		s = s[:len(dst)-1]
	}
	copy(dst, s)
}

func put32(dst []byte, off int, v uint32) {
	binary.BigEndian.PutUint32(dst[off:off+4], v)
}

func putFloat32(dst []byte, off int, v float32) {
	if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		v = 0
	}
	binary.BigEndian.PutUint32(dst[off:off+4], math.Float32bits(v))
}

func bool32(v bool) uint32 {
	if v {
		return 1
	}
	return 0
}
