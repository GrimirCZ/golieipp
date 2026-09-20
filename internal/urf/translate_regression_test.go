package urf_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/grimir/golieipp/internal/urf"
)

func TestTranslateInputCapAndTrailingHeaderBoundaries(t *testing.T) {
	const width, height, dpi = 12, 8, 254
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	base := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
	complete := buildURF(1, page)

	tests := []struct {
		name   string
		src    []byte
		cap    int64
		wantIn int64
		wantOK bool
	}{
		// The cap ending exactly at the complete document is a valid EOF.
		{"exact-cap-clean-eof", complete, int64(len(complete)), int64(len(complete)), true},
		// A short next header is CUPS-compatible and remains valid when the
		// cap includes the complete suffix.
		{"near-cap-short-header", append(append([]byte(nil), complete...), 0xa5), int64(len(complete) + 1), int64(len(complete) + 1), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			opts.Limits.MaxInputBytes = tc.cap
			result, _, err := translate(t, tc.src, opts)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("Translate() error: %v", err)
				}
				if result.InputBytes != tc.wantIn || result.Pages != 1 {
					t.Fatalf("result = %+v, want input=%d one page", result, tc.wantIn)
				}
				return
			}
			if err == nil || urf.ErrorKindOf(err) != urf.KindResourceLimit {
				t.Fatalf("Translate() error = %v, want resource limit", err)
			}
		})
	}

	// Reaching the cap in the middle of a complete page is a resource
	// failure, rather than a CUPS short-header suffix.
	opts := base
	opts.Limits.MaxInputBytes = int64(len(complete) - 1)
	result, _, err := translate(t, complete, opts)
	if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindResourceLimit {
		t.Fatalf("short exact page cap result=%+v kind=%v err=%v, want resource limit", result, urf.ErrorKindOf(err), err)
	}
}

type finalErrorReader struct {
	r        *bytes.Reader
	err      error
	finalEOF bool
}

func (r *finalErrorReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 && r.r.Len() == 0 {
		if r.finalEOF {
			return n, io.EOF
		}
		return n, r.err
	}
	return n, err
}

type noProgressReader struct {
	r       *bytes.Reader
	started bool
}

func (r *noProgressReader) Read(p []byte) (int, error) {
	if !r.started {
		r.started = true
		return 0, nil
	}
	return r.r.Read(p)
}

func TestTranslateReaderTerminalAndNoProgressContracts(t *testing.T) {
	const width, height, dpi = 4, 3, 254
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	src := buildURF(1, page)
	opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)

	t.Run("full-n-plus-eof", func(t *testing.T) {
		reader := &finalErrorReader{r: bytes.NewReader(src), finalEOF: true}
		result, _, err := translateReader(t, reader, opts)
		if err != nil || result.Pages != 1 || result.InputBytes != int64(len(src)) {
			t.Fatalf("result=%+v err=%v, want one successful page", result, err)
		}
	})

	t.Run("full-n-plus-non-eof", func(t *testing.T) {
		sentinel := errors.New("terminal source failure")
		reader := &finalErrorReader{r: bytes.NewReader(src), err: sentinel}
		result, _, err := translateReader(t, reader, opts)
		if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindSourceIO || !errors.Is(err, sentinel) {
			t.Fatalf("result=%+v kind=%v err=%v, want preserved source failure", result, urf.ErrorKindOf(err), err)
		}
	})

	t.Run("zero-progress-is-source-error", func(t *testing.T) {
		reader := &noProgressReader{r: bytes.NewReader(src)}
		result, _, err := translateReader(t, reader, opts)
		if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindSourceIO || !errors.Is(err, io.ErrNoProgress) {
			t.Fatalf("result=%+v kind=%v err=%v, want io.ErrNoProgress source failure", result, urf.ErrorKindOf(err), err)
		}
	})
}

type wrongPositionSeeker struct {
	inner   *writeSeeker
	wrongAt int
}

func (w *wrongPositionSeeker) Write(p []byte) (int, error) { return w.inner.Write(p) }

func (w *wrongPositionSeeker) Seek(offset int64, whence int) (int64, error) {
	position, err := w.inner.Seek(offset, whence)
	if err == nil && w.inner.seekCalls == w.wrongAt {
		return position + 1, nil
	}
	return position, err
}

type offsetFailureSeeker struct {
	inner       *writeSeeker
	failOffset  int64
	failAfter   int
	sentinel    error
	failureSeen bool
}

func (w *offsetFailureSeeker) Write(p []byte) (int, error) {
	if w.inner.off == w.failOffset && w.inner.writeCalls > w.failAfter && !w.failureSeen {
		w.failureSeen = true
		return 0, w.sentinel
	}
	return w.inner.Write(p)
}

func (w *offsetFailureSeeker) Seek(offset int64, whence int) (int64, error) {
	return w.inner.Seek(offset, whence)
}

func TestTranslateDestinationSeekAndPatchFailures(t *testing.T) {
	const width, height, dpi = 4, 3, 254
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	src := buildURF(1, page)
	opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)

	for _, wrongAt := range []struct {
		name string
		call int
	}{
		{"initial-seek", 1},
		{"inspect-seek", 2},
		// Translate performs three empty-destination checks before writing:
		// SeekStart, SeekEnd, and SeekStart. Finalization then seeks for the
		// page-count patch, marker patch, size check, and final rewind.
		{"page-count-patch-seek", 4},
		{"marker-patch-seek", 5},
		{"measure-seek", 6},
		{"rewind-seek", 7},
	} {
		t.Run("wrong-position-"+wrongAt.name, func(t *testing.T) {
			dst := &wrongPositionSeeker{inner: &writeSeeker{}, wrongAt: wrongAt.call}
			result, err := urf.Translate(context.Background(), bytes.NewReader(src), dst, opts)
			if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindDestinationIO {
				t.Fatalf("result=%+v kind=%v err=%v, want destination error", result, urf.ErrorKindOf(err), err)
			}
		})
	}

	t.Run("nonempty-staging-destination", func(t *testing.T) {
		dst := &writeSeeker{data: []byte{0xa5}}
		result, err := urf.Translate(context.Background(), bytes.NewReader(src), dst, opts)
		if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindDestinationIO {
			t.Fatalf("result=%+v kind=%v err=%v, want nonempty-destination error", result, urf.ErrorKindOf(err), err)
		}
	})

	sentinel := errors.New("patch write failure")
	for _, tc := range []struct {
		name      string
		offset    int64
		afterCall int
	}{
		// The first page's TotalPageCount field is at marker (4) + 452.
		{"page-count-patch", 4 + 452, 0},
		// Offset zero is used once for the placeholder and again for RaS2.
		{"marker-patch", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := &offsetFailureSeeker{
				inner:      &writeSeeker{},
				failOffset: tc.offset,
				failAfter:  tc.afterCall,
				sentinel:   sentinel,
			}
			result, err := urf.Translate(context.Background(), bytes.NewReader(src), dst, opts)
			if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindDestinationIO || !errors.Is(err, sentinel) {
				t.Fatalf("result=%+v kind=%v err=%v, want preserved patch failure", result, urf.ErrorKindOf(err), err)
			}
		})
	}
}

func TestTranslateRejectsInvalidPageOptions(t *testing.T) {
	const width, height, dpi = 4, 3, 254
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	base := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
	long := strings.Repeat("x", 64)
	for _, tc := range []struct {
		name string
		mut  func(*urf.Options)
	}{
		{"media-name-nul", func(o *urf.Options) { o.Page.MediaName = "x\x00y" }},
		{"media-name-too-long", func(o *urf.Options) { o.Page.MediaName = long }},
		{"media-type-nul", func(o *urf.Options) { o.Page.MediaType = "x\x00y" }},
		{"media-type-too-long", func(o *urf.Options) { o.Page.MediaType = long }},
		{"zero-media-width", func(o *urf.Options) { o.Page.MediaWidth = 0 }},
		{"zero-media-height", func(o *urf.Options) { o.Page.MediaHeight = 0 }},
		{"zero-resolution-x", func(o *urf.Options) { o.Page.ResolutionX = 0 }},
		{"zero-resolution-y", func(o *urf.Options) { o.Page.ResolutionY = 0 }},
		{"non-square-resolution", func(o *urf.Options) { o.Page.ResolutionY++ }},
		{"invalid-sides", func(o *urf.Options) { o.Page.Sides = "three-sided" }},
		{"invalid-sheet-back", func(o *urf.Options) { o.Page.SheetBack = "mirrored" }},
		{"invalid-quality", func(o *urf.Options) { o.Page.PrintQuality = 6 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			tc.mut(&opts)
			expectKind(t, buildURF(1, page), opts, urf.KindInvalidOptions)
		})
	}

	t.Run("nil-source", func(t *testing.T) {
		result, err := urf.Translate(context.Background(), nil, &writeSeeker{}, base)
		if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindInvalidOptions {
			t.Fatalf("result=%+v kind=%v err=%v, want invalid source", result, urf.ErrorKindOf(err), err)
		}
	})
	t.Run("nil-destination", func(t *testing.T) {
		result, err := urf.Translate(context.Background(), bytes.NewReader(buildURF(1, page)), nil, base)
		if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindInvalidOptions {
			t.Fatalf("result=%+v kind=%v err=%v, want invalid destination", result, urf.ErrorKindOf(err), err)
		}
	})
}

type cancelOnSeekSeeker struct {
	inner    *writeSeeker
	cancel   context.CancelFunc
	cancelAt int
}

func (w *cancelOnSeekSeeker) Write(p []byte) (int, error) { return w.inner.Write(p) }

func (w *cancelOnSeekSeeker) Seek(offset int64, whence int) (int64, error) {
	position, err := w.inner.Seek(offset, whence)
	if err == nil && w.inner.seekCalls == w.cancelAt {
		w.cancel()
	}
	return position, err
}

func TestTranslateCancellationDuringFinalization(t *testing.T) {
	const width, height, dpi = 4, 3, 254
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	src := buildURF(1, page)
	opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
	ctx, cancel := context.WithCancel(context.Background())
	// Seek calls 1..3 validate the empty staging destination. Call 4 is the
	// first finalization patch, so cancellation here exercises finalization
	// after all source data has already been staged.
	dst := &cancelOnSeekSeeker{inner: &writeSeeker{}, cancel: cancel, cancelAt: 4}
	result, err := urf.Translate(ctx, bytes.NewReader(src), dst, opts)
	if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("result=%+v kind=%v err=%v, want finalization cancellation", result, urf.ErrorKindOf(err), err)
	}
}

func TestTranslateFiniteZeroDefaultsAndNaturalNumericBounds(t *testing.T) {
	t.Run("zero-limits-use-finite-width-default", func(t *testing.T) {
		width := urf.DefaultLimits().MaxWidth + 1
		page := fixturePage(urf.MappingW8ToSGray8, width, 1, 2540, nil)
		opts := urf.Options{
			Mapping: urf.MappingW8ToSGray8,
			Page:    urf.PageSettings{MediaName: "wide", MediaWidth: width, MediaHeight: 1, ResolutionX: 2540, ResolutionY: 2540},
			Limits:  urf.Limits{},
		}
		expectKind(t, buildURF(1, page), opts, urf.KindResourceLimit)
	})

	t.Run("zero-limits-use-finite-page-default", func(t *testing.T) {
		height := urf.DefaultLimits().MaxHeight + 1
		page := fixturePage(urf.MappingW8ToSGray8, 1, height, 2540, nil)
		opts := urf.Options{
			Mapping: urf.MappingW8ToSGray8,
			Page:    urf.PageSettings{MediaName: "tall", MediaWidth: 1, MediaHeight: height, ResolutionX: 2540, ResolutionY: 2540},
			Limits:  urf.Limits{},
		}
		expectKind(t, buildURF(1, page), opts, urf.KindResourceLimit)
	})

	t.Run("zero-limits-use-finite-row-default", func(t *testing.T) {
		width := uint32(urf.DefaultLimits().MaxRowBytes + 1)
		page := fixturePage(urf.MappingW8ToSGray8, width, 1, 2540, nil)
		opts := urf.Options{
			Mapping: urf.MappingW8ToSGray8,
			Page:    urf.PageSettings{MediaName: "row", MediaWidth: width, MediaHeight: 1, ResolutionX: 2540, ResolutionY: 2540},
			Limits:  urf.Limits{MaxWidth: width},
		}
		expectKind(t, buildURF(1, page), opts, urf.KindResourceLimit)
	})

	t.Run("zero-limits-use-finite-decoded-default", func(t *testing.T) {
		width := uint32(1 << 20)
		height := uint32((1 << 20) + 1)
		page := fixturePage(urf.MappingW8ToSGray8, width, height, 2540, nil)
		opts := urf.Options{
			Mapping: urf.MappingW8ToSGray8,
			Page:    urf.PageSettings{MediaName: "decoded", MediaWidth: width, MediaHeight: height, ResolutionX: 2540, ResolutionY: 2540},
			Limits:  urf.Limits{MaxWidth: width, MaxHeight: height, MaxRowBytes: uint64(width)},
		}
		expectKind(t, buildURF(1, page), opts, urf.KindResourceLimit)
	})

	t.Run("zero-limits-use-finite-page-count-default", func(t *testing.T) {
		const width, height, dpi = 1, 1, 254
		page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
		pages := make([]urfFixturePage, urf.DefaultLimits().MaxPages+1)
		for i := range pages {
			pages[i] = page
		}
		opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
		opts.Limits = urf.Limits{}
		expectKind(t, buildURF(0, pages...), opts, urf.KindResourceLimit)
	})

	t.Run("24-bit-row-does-not-wrap-u32", func(t *testing.T) {
		width := uint32(math.MaxUint32/3 + 1)
		page := fixturePage(urf.MappingSRGB24ToSRGB8, width, 1, 2540, nil)
		opts := urf.Options{
			Mapping: urf.MappingSRGB24ToSRGB8,
			Page:    urf.PageSettings{MediaName: "overflow", MediaWidth: width, MediaHeight: 1, ResolutionX: 2540, ResolutionY: 2540},
			Limits:  urf.Limits{MaxWidth: math.MaxUint32, MaxHeight: 1, MaxRowBytes: math.MaxUint64, MaxDecodedBytes: math.MaxUint64},
		}
		expectKind(t, buildURF(1, page), opts, urf.KindResourceLimit)
	})

	t.Run("media-pixel-value-outside-u32", func(t *testing.T) {
		const width, height, dpi = 1, 1, math.MaxUint32
		page := fixturePage(urf.MappingW8ToSGray8, width, height, dpi, nil)
		opts := urf.Options{
			Mapping: urf.MappingW8ToSGray8,
			Page:    urf.PageSettings{MediaName: "geometry", MediaWidth: math.MaxUint32, MediaHeight: math.MaxUint32, ResolutionX: dpi, ResolutionY: dpi},
			Limits:  urf.Limits{MaxWidth: math.MaxUint32, MaxHeight: math.MaxUint32, MaxRowBytes: math.MaxUint64, MaxDecodedBytes: math.MaxUint64},
		}
		expectKind(t, buildURF(1, page), opts, urf.KindResourceLimit)
	})
}

// discardWriteSeeker models a bounded staging file without retaining output
// bytes. It makes allocation tests measure translator work rather than the
// destination's backing buffer.
type discardWriteSeeker struct {
	off, size int64
}

func (w *discardWriteSeeker) Write(p []byte) (int, error) {
	w.off += int64(len(p))
	if w.off > w.size {
		w.size = w.off
	}
	return len(p), nil
}

func (w *discardWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = w.off + offset
	case io.SeekEnd:
		next = w.size + offset
	default:
		return w.off, errors.New("invalid seek whence")
	}
	if next < 0 {
		return w.off, errors.New("negative seek")
	}
	w.off = next
	return next, nil
}

func repeatedRowsFixture(height uint32) ([]byte, urf.Options) {
	const width, dpi = 1, 254
	groups := make([]rowGroup, 0, int(height/255)+1)
	remaining := height
	for remaining > 0 {
		repeat := uint32(255)
		if remaining < repeat {
			repeat = remaining
		}
		groups = append(groups, rowGroup{repeat: uint8(repeat), packets: []byte{0, 0x66}})
		remaining -= repeat
	}
	page := fixturePage(urf.MappingW8ToSGray8, width, height, dpi, groups)
	return buildURF(1, page), baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
}

func TestTranslateRepeatedRowsDoNotAllocateLogicalRaster(t *testing.T) {
	const height = uint32(1_000_000)
	src, opts := repeatedRowsFixture(height)
	if len(src) > 100_000 {
		t.Fatalf("compressed repeated-row fixture grew to %d bytes", len(src))
	}

	run := func() {
		dst := &discardWriteSeeker{}
		result, err := urf.Translate(context.Background(), bytes.NewReader(src), dst, opts)
		if err != nil || result.Pages != 1 || result.OutputBytes != dst.size {
			t.Fatalf("result=%+v size=%d err=%v", result, dst.size, err)
		}
		if result.OutputBytes > int64(len(src)+pwgHeaderSize+4) {
			t.Fatalf("output grew with logical row count: %d bytes", result.OutputBytes)
		}
	}
	for i := 0; i < 2; i++ {
		run()
	}
	allocs := testing.AllocsPerRun(3, run)
	if allocs > 10000 {
		t.Fatalf("allocations per translation = %.0f, want bounded compressed-stream work", allocs)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 3; i++ {
		run()
	}
	runtime.ReadMemStats(&after)
	perRun := (after.TotalAlloc - before.TotalAlloc) / 3
	if perRun > 512*1024 {
		t.Fatalf("allocated %d bytes per repeated-row translation, want no logical raster allocation", perRun)
	}
}

type boundedWriteSeeker struct {
	limit, off, size int64
}

func (w *boundedWriteSeeker) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.off {
		return 0, io.ErrShortWrite
	}
	w.off += int64(len(p))
	if w.off > w.size {
		w.size = w.off
	}
	return len(p), nil
}

func (w *boundedWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = w.off + offset
	case io.SeekEnd:
		next = w.size + offset
	default:
		return w.off, errors.New("invalid seek whence")
	}
	if next < 0 || next > w.limit {
		return w.off, io.ErrShortBuffer
	}
	w.off = next
	return next, nil
}

func FuzzTranslateBoundedDestination(f *testing.F) {
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, 2, 2, 254)
	validW8 := buildURF(1, page)
	pageRGB, _ := compactPageFixed(urf.MappingSRGB24ToSRGB8, 2, 2, 254)
	validRGB := buildURF(1, pageRGB)
	pageDEVRGB, _ := compactPageFixed(urf.MappingDEVRGB24ToRGB8, 2, 2, 254)
	validDEVRGB := buildURF(1, pageDEVRGB)
	permissive := buildURF(1, fixturePage(urf.MappingW8ToSGray8, 2, 2, 254, []rowGroup{
		{repeat: 1, packets: []byte{0, 0x11, 0x80}},
		{repeat: 1, packets: []byte{0x80}},
	}))
	pageTwo, _ := compactPageFixed(urf.MappingW8ToSGray8, 2, 2, 254)
	multipageTrailing := append(buildURF(7, page, pageTwo), 0xa5, 0x5a)
	f.Add(validW8, uint8(0))
	f.Add(validRGB, uint8(1))
	f.Add(validDEVRGB, uint8(2))
	f.Add(permissive, uint8(0))
	f.Add(multipageTrailing, uint8(0))
	f.Add([]byte("UNIRAST\x00"), uint8(2))

	f.Fuzz(func(t *testing.T, data []byte, mappingByte uint8) {
		if len(data) > 8192 {
			data = data[:8192]
		}
		mapping := []urf.Mapping{urf.MappingW8ToSGray8, urf.MappingSRGB24ToSRGB8, urf.MappingDEVRGB24ToRGB8}[mappingByte%3]
		opts := baseOptions(mapping, 2, 2, 254)
		opts.Limits = urf.Limits{
			MaxInputBytes:   8192,
			MaxOutputBytes:  4096,
			MaxPages:        4,
			MaxWidth:        64,
			MaxHeight:       64,
			MaxRowBytes:     192,
			MaxDecodedBytes: 4096,
		}
		run := func() (urf.Result, error, int64) {
			dst := &boundedWriteSeeker{limit: opts.Limits.MaxOutputBytes}
			result, err := urf.Translate(context.Background(), bytes.NewReader(data), dst, opts)
			return result, err, dst.size
		}
		result, err, outputSize := run()
		resultAgain, errAgain, outputSizeAgain := run()
		if result != resultAgain || urf.ErrorKindOf(err) != urf.ErrorKindOf(errAgain) || outputSize != outputSizeAgain {
			t.Fatalf("non-deterministic translation: first result=%+v kind=%v size=%d; second result=%+v kind=%v size=%d", result, urf.ErrorKindOf(err), outputSize, resultAgain, urf.ErrorKindOf(errAgain), outputSizeAgain)
		}
		if err != nil {
			if urf.ErrorKindOf(err) == 0 {
				t.Fatalf("untyped translation failure: %v", err)
			}
			if result != (urf.Result{}) {
				t.Fatalf("failed translation returned result %+v: %v", result, err)
			}
			return
		}
		if result.OutputBytes > opts.Limits.MaxOutputBytes || outputSize > opts.Limits.MaxOutputBytes || result.Pages == 0 {
			t.Fatalf("successful translation exceeded bounds: result=%+v size=%d", result, outputSize)
		}
	})
}

type goldenFixtureSpec struct {
	name                                   string
	mapping                                urf.Mapping
	mediaName                              string
	mediaWidth, mediaHeight, width, height uint32
	sides, sheetBack                       string
}

func TestTranslateCUPSGoldenFixtures(t *testing.T) {
	tests := []goldenFixtureSpec{
		{name: "w8-portrait-simplex", mapping: urf.MappingW8ToSGray8, mediaName: "fixture-portrait", mediaWidth: 106, mediaHeight: 142, width: 3, height: 4, sides: "one-sided", sheetBack: "normal"},
		{name: "w8-landscape-duplex", mapping: urf.MappingW8ToSGray8, mediaName: "fixture-landscape", mediaWidth: 177, mediaHeight: 106, width: 5, height: 3, sides: "two-sided-short-edge", sheetBack: "flipped"},
		{name: "srgb24-portrait-simplex", mapping: urf.MappingSRGB24ToSRGB8, mediaName: "fixture-rgb-portrait", mediaWidth: 106, mediaHeight: 142, width: 3, height: 4, sides: "one-sided", sheetBack: "normal"},
		{name: "devrgb24-landscape-duplex", mapping: urf.MappingDEVRGB24ToRGB8, mediaName: "fixture-devrgb-landscape", mediaWidth: 177, mediaHeight: 106, width: 5, height: 3, sides: "two-sided-long-edge", sheetBack: "flipped"},
		{name: "w8-multipage-known", mapping: urf.MappingW8ToSGray8, mediaName: "fixture-portrait", mediaWidth: 106, mediaHeight: 142, width: 3, height: 4, sides: "one-sided", sheetBack: "normal"},
		{name: "w8-multipage-unspecified", mapping: urf.MappingW8ToSGray8, mediaName: "fixture-portrait", mediaWidth: 106, mediaHeight: 142, width: 3, height: 4, sides: "one-sided", sheetBack: "normal"},
		{name: "cups-permissive-clear-eol", mapping: urf.MappingW8ToSGray8, mediaName: "fixture-edge", mediaWidth: 142, mediaHeight: 106, width: 4, height: 3, sides: "one-sided", sheetBack: "normal"},
		{name: "cups-permissive-repeat-height", mapping: urf.MappingW8ToSGray8, mediaName: "fixture-edge", mediaWidth: 142, mediaHeight: 106, width: 4, height: 3, sides: "one-sided", sheetBack: "normal"},
		{name: "cups-permissive-literal-width", mapping: urf.MappingW8ToSGray8, mediaName: "fixture-edge", mediaWidth: 142, mediaHeight: 36, width: 4, height: 1, sides: "one-sided", sheetBack: "normal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", "fixtures", tc.name+".urf"))
			if err != nil {
				t.Fatal(err)
			}
			refBytes, err := os.ReadFile(filepath.Join("testdata", "reference", tc.name+".pwg"))
			if err != nil {
				t.Fatal(err)
			}
			refPages, err := parsePWG(refBytes)
			if err != nil {
				t.Fatalf("parse CUPS reference: %v", err)
			}
			opts := urf.Options{Mapping: tc.mapping, Page: urf.PageSettings{
				MediaName: tc.mediaName, MediaWidth: tc.mediaWidth, MediaHeight: tc.mediaHeight,
				MediaType: "stationery", ResolutionX: 72, ResolutionY: 72,
				PrintQuality: 4, Sides: tc.sides, SheetBack: tc.sheetBack,
			}, Limits: urf.DefaultLimits()}
			result, dst, err := translate(t, src, opts)
			if err != nil {
				t.Fatalf("Translate() error: %v", err)
			}
			gotPages, err := parsePWG(dst.bytes())
			if err != nil {
				t.Fatalf("parse translated PWG: %v", err)
			}
			if result.Pages != uint32(len(refPages)) || len(gotPages) != len(refPages) {
				t.Fatalf("pages result=%d translated=%d CUPS=%d", result.Pages, len(gotPages), len(refPages))
			}
			for i := range refPages {
				got, ref := gotPages[i], refPages[i]
				if got.header.width != ref.header.width || got.header.height != ref.header.height ||
					got.header.bitsColor != ref.header.bitsColor || got.header.bitsPixel != ref.header.bitsPixel ||
					got.header.bytesLine != ref.header.bytesLine || got.header.colorSpace != ref.header.colorSpace ||
					got.header.numColors != ref.header.numColors || got.header.resolutionX != ref.header.resolutionX ||
					got.header.resolutionY != ref.header.resolutionY || got.header.duplex != ref.header.duplex ||
					got.header.tumble != ref.header.tumble {
					t.Fatalf("page %d translated header=%+v, CUPS=%+v", i+1, got.header, ref.header)
				}
				if !equalRows(got.rows, ref.rows) {
					t.Fatalf("page %d pixels differ from CUPS reference", i+1)
				}
			}
		})
	}
}

func TestTranslateRINUSyncKeepsAppleBigEndianPageFields(t *testing.T) {
	const width, height, dpi = 4, 3, 254
	page, wantRows := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	src := buildURF(1, page)
	copy(src[:4], []byte("RINU"))
	_, _, gotPages := expectSuccess(t, src, baseOptions(urf.MappingW8ToSGray8, width, height, dpi))
	if len(gotPages) != 1 || !equalRows(gotPages[0].rows, wantRows) {
		t.Fatalf("RINU page decode changed: got %d pages", len(gotPages))
	}
}

func TestTranslateCUPSSheetBackAppliesToEvenPages(t *testing.T) {
	const width, height, dpi = 4, 3, 254
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	for _, tc := range []struct {
		sides, sheetBack string
		cross, feed      uint32
	}{
		{"one-sided", "normal", 1, 1},
		{"one-sided", "flipped", 1, 1},
		{"one-sided", "manual-tumble", 1, 1},
		{"one-sided", "rotated", 1, 1},
		{"two-sided-long-edge", "normal", 1, 1},
		{"two-sided-long-edge", "flipped", 1, math.MaxUint32},
		{"two-sided-long-edge", "manual-tumble", 1, 1},
		{"two-sided-long-edge", "rotated", math.MaxUint32, math.MaxUint32},
		{"two-sided-short-edge", "normal", 1, 1},
		{"two-sided-short-edge", "flipped", math.MaxUint32, 1},
		{"two-sided-short-edge", "manual-tumble", math.MaxUint32, math.MaxUint32},
		{"two-sided-short-edge", "rotated", 1, 1},
	} {
		t.Run(tc.sides+"/"+tc.sheetBack, func(t *testing.T) {
			inputPage := page
			switch tc.sides {
			case "two-sided-long-edge":
				inputPage.duplex = 3
			case "two-sided-short-edge":
				inputPage.duplex = 2
			}
			src := buildURF(3, inputPage, inputPage, inputPage)
			opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
			opts.Page.Sides = tc.sides
			opts.Page.SheetBack = tc.sheetBack
			_, _, pages := expectSuccess(t, src, opts)
			if len(pages) != 3 {
				t.Fatalf("got %d pages, want three", len(pages))
			}
			for i, page := range pages {
				wantCross, wantFeed := uint32(1), uint32(1)
				if i == 1 {
					wantCross, wantFeed = tc.cross, tc.feed
				}
				if page.header.cross != wantCross || page.header.feed != wantFeed {
					t.Fatalf("page %d cross=%08x feed=%08x, want %08x/%08x", i+1, page.header.cross, page.header.feed, wantCross, wantFeed)
				}
			}
		})
	}
}

func BenchmarkTranslateRaster(b *testing.B) {
	const width, height, dpi = 256, 64, 254
	compressibleGroups := make([]rowGroup, 0, height)
	for i := 0; i < height; i++ {
		compressibleGroups = append(compressibleGroups, rowGroup{repeat: 1, packets: []byte{0x7f, 0x66, 0x7f, 0x66}})
	}
	compressible := buildURF(1, fixturePage(urf.MappingW8ToSGray8, width, height, dpi, compressibleGroups))
	incompressibleGroups := make([]rowGroup, 0, height)
	for i := 0; i < height; i++ {
		pixels := rowPixels(urf.MappingW8ToSGray8, i, width)
		incompressibleGroups = append(incompressibleGroups, rowGroup{repeat: 1, packets: literalPackets(pixels, 1)})
	}
	incompressible := buildURF(1, fixturePage(urf.MappingW8ToSGray8, width, height, dpi, incompressibleGroups))

	for _, tc := range []struct {
		name string
		src  []byte
	}{
		{"compressible", compressible},
		{"incompressible", incompressible},
	} {
		b.Run(tc.name, func(b *testing.B) {
			opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
			b.SetBytes(int64(len(tc.src)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst := &discardWriteSeeker{}
				result, err := urf.Translate(context.Background(), bytes.NewReader(tc.src), dst, opts)
				if err != nil || result.Pages != 1 {
					b.Fatalf("result=%+v err=%v", result, err)
				}
			}
		})
	}
}
