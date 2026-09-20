package urf_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/grimir/golieipp/internal/urf"
)

const (
	urfHeaderSize = 12
	urfPageSize   = 32
	pwgHeaderSize = 1796
)

// The Apple Raster header is deliberately built here instead of through the
// production parser.  The fixture layout follows CUPS raster-stream.c:
// UNIR + AST/count, followed by a 32-byte page header and modified PackBits.
type urfFixturePage struct {
	bitsPerPixel  byte
	colorSpace    byte
	duplex        byte
	quality       byte
	mediaType     byte
	mediaPosition byte
	width         uint32
	height        uint32
	dpi           uint32
	groups        []rowGroup
	reserved      bool
}

type rowGroup struct {
	repeat  uint8 // actual repeat count; 0 means one row in this test model
	packets []byte
}

type pwgHeader struct {
	pageSizeName string
	mediaType    string
	mediaSource  uint32
	duplex       uint32
	tumble       uint32
	resolutionX  uint32
	resolutionY  uint32
	width        uint32
	height       uint32
	bitsColor    uint32
	bitsPixel    uint32
	bytesLine    uint32
	colorOrder   uint32
	colorSpace   uint32
	numColors    uint32
	totalPages   uint32
	cross        uint32
	feed         uint32
	imageLeft    uint32
	imageTop     uint32
	imageRight   uint32
	imageBottom  uint32
	quality      uint32
}

type pwgPage struct {
	header pwgHeader
	rows   [][]byte
}

func buildURF(count uint32, pages ...urfFixturePage) []byte {
	var out bytes.Buffer
	out.Write([]byte{'U', 'N', 'I', 'R'})
	out.Write([]byte{'A', 'S', 'T', 0})
	var countBytes [4]byte
	binary.BigEndian.PutUint32(countBytes[:], count)
	out.Write(countBytes[:])
	for _, page := range pages {
		var header [urfPageSize]byte
		header[0] = page.bitsPerPixel
		header[1] = page.colorSpace
		header[2] = page.duplex
		header[3] = page.quality
		header[4] = page.mediaType
		header[5] = page.mediaPosition
		binary.BigEndian.PutUint32(header[12:16], page.width)
		binary.BigEndian.PutUint32(header[16:20], page.height)
		binary.BigEndian.PutUint32(header[20:24], page.dpi)
		if page.reserved {
			// CUPS ignores these fields.  Keep the fixture visibly non-zero so
			// a strict, non-CUPS parser cannot accidentally become the oracle.
			for i := 6; i < 12; i++ {
				header[i] = byte(0xa0 + i)
			}
			for i := 24; i < len(header); i++ {
				header[i] = byte(0xc0 + i)
			}
		}
		out.Write(header[:])
		for _, group := range page.groups {
			repeat := group.repeat
			if repeat == 0 {
				repeat = 1
			}
			out.WriteByte(repeat - 1)
			out.Write(group.packets)
		}
	}
	return out.Bytes()
}

func fixturePage(mapping urf.Mapping, width, height, dpi uint32, groups []rowGroup) urfFixturePage {
	bpp, cspace := mappingDetails(mapping)
	return urfFixturePage{
		bitsPerPixel: bpp * 8,
		colorSpace:   cspace,
		duplex:       1,
		quality:      2,
		mediaType:    1,
		width:        width,
		height:       height,
		dpi:          dpi,
		groups:       groups,
	}
}

func mappingDetails(mapping urf.Mapping) (bytesPerPixel, colorSpace byte) {
	switch mapping {
	case urf.MappingW8ToSGray8:
		return 1, 0 // W8 in Apple Raster / CUPS raw color-space table.
	case urf.MappingSRGB24ToSRGB8:
		return 3, 1
	case urf.MappingDEVRGB24ToRGB8:
		return 3, 5
	default:
		return 1, 0
	}
}

func baseOptions(mapping urf.Mapping, width, height, dpi uint32) urf.Options {
	return urf.Options{
		Mapping: mapping,
		Page: urf.PageSettings{
			MediaName:    "test_120x80mm",
			MediaWidth:   width * 2540 / dpi,
			MediaHeight:  height * 2540 / dpi,
			MediaType:    "stationery",
			MediaSource:  7,
			ResolutionX:  dpi,
			ResolutionY:  dpi,
			PrintQuality: 5,
			Sides:        "one-sided",
			SheetBack:    "normal",
		},
		Limits: urf.DefaultLimits(),
	}
}

func literalPackets(pixels []byte, bytesPerPixel int) []byte {
	if len(pixels) == 0 || bytesPerPixel <= 0 || len(pixels)%bytesPerPixel != 0 {
		panic("literalPackets requires complete pixels")
	}
	var out []byte
	for len(pixels) > 0 {
		count := len(pixels) / bytesPerPixel
		if count > 128 {
			count = 128
		}
		if count == 1 {
			out = append(out, 0)
			out = append(out, pixels[:bytesPerPixel]...)
		} else {
			out = append(out, byte(257-count))
			out = append(out, pixels[:count*bytesPerPixel]...)
		}
		pixels = pixels[count*bytesPerPixel:]
	}
	return out
}

func mixedPackets(pixels []byte, bytesPerPixel int) []byte {
	if len(pixels)/bytesPerPixel < 3 {
		return literalPackets(pixels, bytesPerPixel)
	}
	var out []byte
	// Two equal pixels exercise the repeat form, followed by a literal tail.
	out = append(out, 1)
	out = append(out, pixels[:bytesPerPixel]...)
	return append(out, literalPackets(pixels[2*bytesPerPixel:], bytesPerPixel)...)
}

func rowPixels(mapping urf.Mapping, row, width int) []byte {
	bpp, _ := mappingDetails(mapping)
	result := make([]byte, width*int(bpp))
	for x := 0; x < width; x++ {
		p := result[x*int(bpp) : (x+1)*int(bpp)]
		for c := range p {
			// Deliberately asymmetric values expose channel swaps and vertical
			// orientation mistakes in the translated stream.
			p[c] = byte((row*37 + x*19 + c*53 + 7) & 0xff)
		}
	}
	if width > 1 {
		copy(result[int(bpp):2*int(bpp)], result[:int(bpp)])
	}
	return result
}

func bytesPerPixel(mapping urf.Mapping) int {
	bpp, _ := mappingDetails(mapping)
	return int(bpp)
}

func compactPageFixed(mapping urf.Mapping, width, height, dpi uint32) (urfFixturePage, [][]byte) {
	rows := make([][]byte, 0, height)
	groups := make([]rowGroup, 0, height)
	bpp := bytesPerPixel(mapping)
	for y := uint32(0); y < height; y++ {
		pixels := rowPixels(mapping, int(y), int(width))
		rows = append(rows, pixels)
		groups = append(groups, rowGroup{repeat: 1, packets: mixedPackets(pixels, bpp)})
	}
	return fixturePage(mapping, width, height, dpi, groups), rows
}

func putU32(b []byte, off int, value uint32) {
	binary.BigEndian.PutUint32(b[off:off+4], value)
}

func getU32(b []byte, off int) uint32 {
	return binary.BigEndian.Uint32(b[off : off+4])
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func parsePWG(data []byte) ([]pwgPage, error) {
	if len(data) < 4 || string(data[:4]) != "RaS2" {
		return nil, fmt.Errorf("PWG marker %q", data[:min(len(data), 4)])
	}
	pos := 4
	var pages []pwgPage
	for pos < len(data) {
		if len(data)-pos < pwgHeaderSize {
			return nil, fmt.Errorf("short PWG page header at %d", pos)
		}
		h := data[pos : pos+pwgHeaderSize]
		page := pwgPage{header: pwgHeader{
			pageSizeName: cString(h[1732:1796]),
			mediaType:    cString(h[128:192]),
			mediaSource:  getU32(h, 324),
			duplex:       getU32(h, 272),
			tumble:       getU32(h, 368),
			resolutionX:  getU32(h, 276),
			resolutionY:  getU32(h, 280),
			width:        getU32(h, 372),
			height:       getU32(h, 376),
			bitsColor:    getU32(h, 384),
			bitsPixel:    getU32(h, 388),
			bytesLine:    getU32(h, 392),
			colorOrder:   getU32(h, 396),
			colorSpace:   getU32(h, 400),
			numColors:    getU32(h, 420),
			totalPages:   getU32(h, 452),
			cross:        getU32(h, 456),
			feed:         getU32(h, 460),
			imageLeft:    getU32(h, 464),
			imageTop:     getU32(h, 468),
			imageRight:   getU32(h, 472),
			imageBottom:  getU32(h, 476),
			quality:      getU32(h, 484),
		}}
		pos += pwgHeaderSize
		bpp := int((page.header.bitsPixel + 7) / 8)
		if page.header.width == 0 || page.header.height == 0 || bpp <= 0 {
			return nil, fmt.Errorf("invalid PWG geometry")
		}
		rows, next, err := decodeRows(data, pos, int(page.header.width), int(page.header.height), bpp, page.header.colorSpace)
		if err != nil {
			return nil, err
		}
		page.rows = rows
		pages = append(pages, page)
		pos = next
	}
	return pages, nil
}

// decodeRows is an independent CUPS-compatible decoder.  It deliberately
// clips overlong repeat/literal packets to the current row and treats 0x80 as
// clear-to-white, matching cupsRasterReadPixels for the supported mappings.
func decodeRows(data []byte, pos, width, height, bpp int, colorSpace uint32) ([][]byte, int, error) {
	if width <= 0 || height <= 0 || bpp <= 0 {
		return nil, pos, fmt.Errorf("invalid dimensions")
	}
	rowBytes := width * bpp
	rows := make([][]byte, 0, height)
	for len(rows) < height {
		if pos >= len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		repeat := int(data[pos]) + 1
		pos++
		row, next, err := decodeRow(data, pos, rowBytes, bpp, colorSpace)
		if err != nil {
			return nil, pos, err
		}
		pos = next
		for i := 0; i < repeat && len(rows) < height; i++ {
			rows = append(rows, append([]byte(nil), row...))
		}
	}
	return rows, pos, nil
}

func decodeRow(data []byte, pos, rowBytes, bpp int, colorSpace uint32) ([]byte, int, error) {
	row := make([]byte, rowBytes)
	filled := 0
	for filled < rowBytes {
		if pos >= len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		control := data[pos]
		pos++
		switch {
		case control == 0x80:
			fill := byte(0)
			if colorSpace == 0 || colorSpace == 18 || colorSpace == 19 || colorSpace == 1 {
				fill = 0xff
			}
			for i := filled; i < rowBytes; i++ {
				row[i] = fill
			}
			filled = rowBytes
		case control&0x80 != 0:
			count := 257 - int(control)
			want := count * bpp
			remain := rowBytes - filled
			if want > remain {
				want = remain
			}
			if want > len(data)-pos {
				return nil, pos, io.ErrUnexpectedEOF
			}
			copy(row[filled:filled+want], data[pos:pos+want])
			pos += want
			filled += want
		default:
			count := int(control) + 1
			remain := rowBytes - filled
			if bpp > len(data)-pos {
				return nil, pos, io.ErrUnexpectedEOF
			}
			pixel := data[pos : pos+bpp]
			pos += bpp
			want := count * bpp
			if want > remain {
				want = remain
			}
			for i := 0; i < want; i += bpp {
				copy(row[filled+i:filled+i+bpp], pixel)
			}
			filled += want
		}
	}
	return row, pos, nil
}

func translate(t *testing.T, src []byte, opts urf.Options) (urf.Result, *writeSeeker, error) {
	t.Helper()
	dst := &writeSeeker{}
	result, err := urf.Translate(context.Background(), bytes.NewReader(src), dst, opts)
	return result, dst, err
}

func expectSuccess(t *testing.T, src []byte, opts urf.Options) (urf.Result, []byte, []pwgPage) {
	t.Helper()
	result, dst, err := translate(t, src, opts)
	if err != nil {
		t.Fatalf("Translate() error: %v", err)
	}
	if dst.off != 0 {
		t.Fatalf("destination offset after success = %d, want 0", dst.off)
	}
	data := dst.bytes()
	if result.InputBytes != int64(len(src)) {
		t.Fatalf("InputBytes = %d, want %d", result.InputBytes, len(src))
	}
	if result.OutputBytes != int64(len(data)) {
		t.Fatalf("OutputBytes = %d, want %d", result.OutputBytes, len(data))
	}
	pages, err := parsePWG(data)
	if err != nil {
		t.Fatalf("independent PWG decode: %v", err)
	}
	if result.Pages != uint32(len(pages)) {
		t.Fatalf("Pages = %d, decoded pages = %d", result.Pages, len(pages))
	}
	return result, data, pages
}

func expectKind(t *testing.T, src []byte, opts urf.Options, want urf.ErrorKind) {
	t.Helper()
	result, _, err := translate(t, src, opts)
	if err == nil {
		t.Fatalf("Translate() unexpectedly succeeded with result %+v", result)
	}
	if result != (urf.Result{}) {
		t.Fatalf("failed Translate() returned non-zero result %+v", result)
	}
	if got := urf.ErrorKindOf(err); got != want {
		t.Fatalf("error kind = %v, want %v (%v)", got, want, err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// writeSeeker is an in-memory destination with controllable short writes and
// seek failures.  It intentionally does not expose production writer state.
type writeSeeker struct {
	data        []byte
	off         int64
	writeCalls  int
	seekCalls   int
	shortAfter  int // return this many bytes on every write after this call
	zeroWriteAt int
	writeErrAt  int
	writeErr    error
	seekErrAt   int
	seekErr     error
}

func (w *writeSeeker) Write(p []byte) (int, error) {
	w.writeCalls++
	if w.writeErrAt > 0 && w.writeCalls >= w.writeErrAt {
		return 0, w.writeErr
	}
	if w.zeroWriteAt > 0 && w.writeCalls >= w.zeroWriteAt {
		return 0, nil
	}
	n := len(p)
	if w.shortAfter > 0 && n > w.shortAfter {
		n = w.shortAfter
	}
	end := w.off + int64(n)
	if end > int64(len(w.data)) {
		w.data = append(w.data, make([]byte, int(end)-len(w.data))...)
	}
	copy(w.data[w.off:end], p[:n])
	w.off = end
	return n, nil
}

func (w *writeSeeker) Seek(offset int64, whence int) (int64, error) {
	w.seekCalls++
	if w.seekErrAt > 0 && w.seekCalls >= w.seekErrAt {
		return w.off, w.seekErr
	}
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = w.off + offset
	case io.SeekEnd:
		next = int64(len(w.data)) + offset
	default:
		return w.off, errors.New("invalid whence")
	}
	if next < 0 {
		return w.off, errors.New("negative seek")
	}
	w.off = next
	return w.off, nil
}

func (w *writeSeeker) bytes() []byte {
	return append([]byte(nil), w.data...)
}

type chunkReader struct {
	data       []byte
	pos        int
	chunk      int
	errAt      int
	err        error
	afterRead  func()
	calledOnce bool
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.errAt > 0 && r.pos >= r.errAt {
		return 0, r.err
	}
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := len(p)
	if r.chunk > 0 && n > r.chunk {
		n = r.chunk
	}
	if r.errAt > 0 && r.pos+n > r.errAt {
		n = r.errAt - r.pos
	}
	copy(p[:n], r.data[r.pos:r.pos+n])
	r.pos += n
	if r.afterRead != nil && !r.calledOnce {
		r.calledOnce = true
		r.afterRead()
	}
	if r.errAt > 0 && r.pos >= r.errAt {
		return n, r.err
	}
	return n, nil
}

func TestTranslateExactMappingsHeaderAndPixels(t *testing.T) {
	const width, height, dpi = 12, 8, 254
	for _, tc := range []struct {
		name       string
		mapping    urf.Mapping
		colorSpace uint32
		bpp        int
		numColors  uint32
	}{
		{"W8-to-sgray_8", urf.MappingW8ToSGray8, 18, 1, 1},
		{"SRGB24-to-srgb_8", urf.MappingSRGB24ToSRGB8, 19, 3, 3},
		{"DEVRGB24-to-rgb_8", urf.MappingDEVRGB24ToRGB8, 1, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, wantRows := compactPageFixed(tc.mapping, width, height, dpi)
			src := buildURF(1, page)
			_, _, gotPages := expectSuccess(t, src, baseOptions(tc.mapping, width, height, dpi))
			if len(gotPages) != 1 {
				t.Fatalf("got %d pages, want one", len(gotPages))
			}
			got := gotPages[0]
			if got.header.pageSizeName != "test_120x80mm" {
				t.Errorf("page size name = %q", got.header.pageSizeName)
			}
			if got.header.mediaType != "stationery" || got.header.mediaSource != 7 {
				t.Errorf("media metadata = %q/%d, want stationery/7", got.header.mediaType, got.header.mediaSource)
			}
			if got.header.width != width || got.header.height != height {
				t.Errorf("geometry = %dx%d, want %dx%d", got.header.width, got.header.height, width, height)
			}
			if got.header.resolutionX != dpi || got.header.resolutionY != dpi {
				t.Errorf("resolution = %dx%d, want %d", got.header.resolutionX, got.header.resolutionY, dpi)
			}
			if got.header.bitsColor != 8 || got.header.bitsPixel != uint32(tc.bpp*8) {
				t.Errorf("bit depths = %d/%d", got.header.bitsColor, got.header.bitsPixel)
			}
			if got.header.bytesLine != width*uint32(tc.bpp) || got.header.colorOrder != 0 {
				t.Errorf("line/order = %d/%d", got.header.bytesLine, got.header.colorOrder)
			}
			if got.header.colorSpace != tc.colorSpace || got.header.numColors != tc.numColors {
				t.Errorf("color metadata = space %d/colors %d, want %d/%d", got.header.colorSpace, got.header.numColors, tc.colorSpace, tc.numColors)
			}
			if got.header.totalPages != 1 || got.header.quality != 5 {
				t.Errorf("count/quality = %d/%d, want 1/5", got.header.totalPages, got.header.quality)
			}
			if !equalRows(got.rows, wantRows) {
				t.Fatalf("decoded pixels differ from independent source expectation")
			}
			// Exact mappings are allowed to preserve compressed packets.  This
			// independent byte check catches accidental full-page decode/re-encode.
			_, output, _ := translate(t, src, baseOptions(tc.mapping, width, height, dpi))
			if !bytes.Equal(output.bytes()[4+pwgHeaderSize:], src[urfHeaderSize+urfPageSize:]) {
				t.Fatalf("compressed packet bytes were rewritten for exact mapping")
			}
		})
	}
}

func TestTranslateLandscapeGeometryAndOrientation(t *testing.T) {
	const width, height, dpi = 12, 8, 254
	// The selected media is 12x8 pixels at 254 dpi.  This page arrives in the
	// permitted landscape orientation and retains its row order/pixel bytes.
	landscape, wantRows := compactPageFixed(urf.MappingW8ToSGray8, height, width, dpi)
	opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
	_, _, pages := expectSuccess(t, buildURF(1, landscape), opts)
	if len(pages) != 1 {
		t.Fatalf("got %d pages", len(pages))
	}
	if pages[0].header.width != height || pages[0].header.height != width {
		t.Fatalf("landscape geometry = %dx%d, want %dx%d", pages[0].header.width, pages[0].header.height, height, width)
	}
	if !equalRows(pages[0].rows, wantRows) {
		t.Fatal("landscape rows were rotated or reordered")
	}
}

func equalRows(got, want [][]byte) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !bytes.Equal(got[i], want[i]) {
			return false
		}
	}
	return true
}

func TestTranslateKnownAndUnspecifiedPageCounts(t *testing.T) {
	const width, height, dpi = 12, 8, 254
	page1, rows1 := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	page2, rows2 := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	// Make page two distinguishable from page one without using translator
	// internals as an oracle.
	for _, row := range rows2 {
		for i := range row {
			row[i] ^= 0x5a
		}
	}
	page2.groups = make([]rowGroup, 0, height)
	for _, row := range rows2 {
		page2.groups = append(page2.groups, rowGroup{repeat: 1, packets: literalPackets(row, 1)})
	}
	for _, count := range []uint32{2, 0xffffffff, 1, 0} {
		t.Run(fmt.Sprintf("count-%08x", count), func(t *testing.T) {
			src := buildURF(count, page1, page2)
			result, _, pages := expectSuccess(t, src, baseOptions(urf.MappingW8ToSGray8, width, height, dpi))
			if result.Pages != 2 || len(pages) != 2 {
				t.Fatalf("pages = %d/%d, want 2", result.Pages, len(pages))
			}
			if !equalRows(pages[0].rows, rows1) || !equalRows(pages[1].rows, rows2) {
				t.Fatal("multipage pixel data changed")
			}
			if pages[0].header.totalPages != 2 || pages[1].header.totalPages != 2 {
				t.Fatalf("patched page count = %d/%d, want 2", pages[0].header.totalPages, pages[1].header.totalPages)
			}
		})
	}
}

func TestTranslateDuplexAndSheetBackMatrix(t *testing.T) {
	const width, height, dpi = 12, 8, 254
	for _, tc := range []struct {
		sides, sheetBack string
		duplex, tumble   uint32
		cross, feed      uint32
	}{
		{"one-sided", "normal", 0, 0, 1, 1},
		{"one-sided", "flipped", 0, 0, 1, 1},
		{"one-sided", "manual-tumble", 0, 0, 1, 1},
		{"one-sided", "rotated", 0, 0, 1, 1},
		{"two-sided-long-edge", "normal", 1, 0, 1, 1},
		{"two-sided-long-edge", "flipped", 1, 0, 1, 1},
		{"two-sided-long-edge", "manual-tumble", 1, 0, 1, 1},
		{"two-sided-long-edge", "rotated", 1, 0, 1, 1},
		{"two-sided-short-edge", "normal", 1, 1, 1, 1},
		{"two-sided-short-edge", "flipped", 1, 1, 1, 1},
		{"two-sided-short-edge", "manual-tumble", 1, 1, 1, 1},
		{"two-sided-short-edge", "rotated", 1, 1, 1, 1},
	} {
		t.Run(tc.sides+"/"+tc.sheetBack, func(t *testing.T) {
			page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
			switch tc.sides {
			case "two-sided-long-edge":
				page.duplex = 3
			case "two-sided-short-edge":
				page.duplex = 2
			}
			opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
			opts.Page.Sides = tc.sides
			opts.Page.SheetBack = tc.sheetBack
			_, _, pages := expectSuccess(t, buildURF(1, page), opts)
			h := pages[0].header
			if h.duplex != tc.duplex || h.tumble != tc.tumble || h.cross != tc.cross || h.feed != tc.feed {
				t.Fatalf("duplex=%d tumble=%d cross=%08x feed=%08x, want %d %d %08x %08x", h.duplex, h.tumble, h.cross, h.feed, tc.duplex, tc.tumble, tc.cross, tc.feed)
			}
		})
	}
}

func TestTranslateCUPSPermissivePacketForms(t *testing.T) {
	const width, height, dpi = 4, 2, 254
	base := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
	base.Page.MediaWidth = width * 10
	base.Page.MediaHeight = height * 10
	tests := []struct {
		name  string
		page  urfFixturePage
		want  [][]byte
		extra []byte
	}{
		{
			name: "clear-to-white-extension",
			page: fixturePage(urf.MappingW8ToSGray8, width, height, dpi, []rowGroup{
				{repeat: 1, packets: []byte{0, 0x11, 0x80}},
				{repeat: 1, packets: []byte{0x80}},
			}),
			want: [][]byte{{0x11, 0xff, 0xff, 0xff}, {0xff, 0xff, 0xff, 0xff}},
		},
		{
			name: "overlong-literal-clips",
			page: fixturePage(urf.MappingW8ToSGray8, width, height, dpi, []rowGroup{
				{repeat: 1, packets: []byte{0xfc, 1, 2, 3, 4}}, // five literals, clipped to four
				{repeat: 1, packets: []byte{0xfc, 5, 6, 7, 8}},
			}),
			want: [][]byte{{1, 2, 3, 4}, {5, 6, 7, 8}},
		},
		{
			name: "overlong-repeat-clips",
			page: fixturePage(urf.MappingW8ToSGray8, width, height, dpi, []rowGroup{
				{repeat: 1, packets: []byte{0x7f, 0x2a}},
				{repeat: 1, packets: []byte{0x7f, 0x55}},
			}),
			want: [][]byte{{0x2a, 0x2a, 0x2a, 0x2a}, {0x55, 0x55, 0x55, 0x55}},
		},
		{
			name: "row-repeat-overflow-clips",
			page: fixturePage(urf.MappingW8ToSGray8, width, height, dpi, []rowGroup{
				{repeat: 9, packets: []byte{0x7f, 0x3c}},
			}),
			want: [][]byte{{0x3c, 0x3c, 0x3c, 0x3c}, {0x3c, 0x3c, 0x3c, 0x3c}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, pages := expectSuccess(t, buildURF(1, tc.page), base)
			if !equalRows(pages[0].rows, tc.want) {
				t.Fatalf("decoded rows = %#v, want %#v", pages[0].rows, tc.want)
			}
		})
	}

	// CUPS treats the file count as advisory, ignores reserved bytes, and
	// stops successfully at a page boundary when the suffix is not a complete
	// page. Keep these behaviors separate from malformed packet truncation.
	page, want := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	page.reserved = true
	for _, tc := range []struct {
		name   string
		count  uint32
		suffix []byte
	}{
		{"reserved-fields", 1, nil},
		{"trailing-junk", 1, []byte("JUNK")},
		{"trailing-partial-header", 1, make([]byte, 7)},
		{"advisory-count-too-large", 9, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := append(buildURF(tc.count, page), tc.suffix...)
			_, _, pages := expectSuccess(t, src, base)
			if !equalRows(pages[0].rows, want) {
				t.Fatal("permissive fixture pixels changed")
			}
		})
	}
}

func TestTranslateRejectsUnsupportedAndMismatchedInput(t *testing.T) {
	const width, height, dpi = 12, 8, 254
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
	for _, tc := range []struct {
		name string
		mut  func(*urf.Options, *urfFixturePage)
		kind urf.ErrorKind
	}{
		{"invalid-mapping", func(o *urf.Options, p *urfFixturePage) { o.Mapping = urf.Mapping(99) }, urf.KindInvalidOptions},
		{"color-space-mismatch", func(o *urf.Options, p *urfFixturePage) { p.colorSpace = 1 }, urf.KindUnsupportedInput},
		{"depth-mismatch", func(o *urf.Options, p *urfFixturePage) { p.bitsPerPixel = 16 }, urf.KindUnsupportedInput},
		{"media-width-mismatch", func(o *urf.Options, p *urfFixturePage) { o.Page.MediaWidth += 10 }, urf.KindSettingsMismatch},
		{"resolution-mismatch", func(o *urf.Options, p *urfFixturePage) { o.Page.ResolutionX++; o.Page.ResolutionY++ }, urf.KindSettingsMismatch},
		{"sides-mismatch", func(o *urf.Options, p *urfFixturePage) { o.Page.Sides = "two-sided-long-edge" }, urf.KindSettingsMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copyPage := page
			copyPage.groups = append([]rowGroup(nil), page.groups...)
			copyOpts := opts
			tc.mut(&copyOpts, &copyPage)
			expectKind(t, buildURF(1, copyPage), copyOpts, tc.kind)
		})
	}
}

func TestTranslateTruncatedInputsReturnZeroResult(t *testing.T) {
	const width, height, dpi = 12, 8, 254
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	src := buildURF(1, page)
	opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
	for n := 0; n < len(src); n++ {
		t.Run(fmt.Sprintf("prefix-%d", n), func(t *testing.T) {
			result, _, err := translate(t, src[:n], opts)
			if err == nil {
				t.Fatalf("prefix length %d unexpectedly succeeded with %+v", n, result)
			}
			if result != (urf.Result{}) {
				t.Fatalf("prefix length %d returned non-zero result %+v", n, result)
			}
			kind := urf.ErrorKindOf(err)
			if kind != urf.KindMalformedInput && kind != urf.KindSourceIO {
				t.Fatalf("prefix length %d kind = %v, error %v", n, kind, err)
			}
		})
	}
}

func TestTranslateIOShortReadsAndTypedErrors(t *testing.T) {
	const width, height, dpi = 12, 8, 254
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	src := buildURF(1, page)
	opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)

	t.Run("short-source-reads", func(t *testing.T) {
		reader := &chunkReader{data: src, chunk: 1}
		dst := &writeSeeker{}
		result, err := urf.Translate(context.Background(), reader, dst, opts)
		if err != nil {
			t.Fatalf("Translate() short reads: %v", err)
		}
		if result.InputBytes != int64(len(src)) || result.Pages != 1 {
			t.Fatalf("result = %+v", result)
		}
	})

	sentinel := errors.New("source exploded")
	t.Run("source-error-wraps", func(t *testing.T) {
		reader := &chunkReader{data: src, chunk: 3, errAt: len(src) / 2, err: sentinel}
		result, _, err := translateReader(t, reader, opts)
		if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindSourceIO || !errors.Is(err, sentinel) {
			t.Fatalf("result=%+v kind=%v err=%v", result, urf.ErrorKindOf(err), err)
		}
	})

	t.Run("short-destination-write", func(t *testing.T) {
		dst := &writeSeeker{shortAfter: 1}
		result, err := urf.Translate(context.Background(), bytes.NewReader(src), dst, opts)
		if err != nil || result.Pages != 1 {
			t.Fatalf("result=%+v kind=%v err=%v", result, urf.ErrorKindOf(err), err)
		}
	})

	t.Run("zero-destination-write", func(t *testing.T) {
		dst := &writeSeeker{zeroWriteAt: 1}
		result, err := urf.Translate(context.Background(), bytes.NewReader(src), dst, opts)
		if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindDestinationIO {
			t.Fatalf("result=%+v kind=%v err=%v", result, urf.ErrorKindOf(err), err)
		}
	})

	t.Run("destination-write-error-wraps", func(t *testing.T) {
		sentinel := errors.New("destination exploded")
		dst := &writeSeeker{writeErrAt: 1, writeErr: sentinel}
		result, err := urf.Translate(context.Background(), bytes.NewReader(src), dst, opts)
		if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindDestinationIO || !errors.Is(err, sentinel) {
			t.Fatalf("result=%+v kind=%v err=%v", result, urf.ErrorKindOf(err), err)
		}
	})

	t.Run("destination-seek-error", func(t *testing.T) {
		sentinel := errors.New("seek exploded")
		dst := &writeSeeker{seekErrAt: 1, seekErr: sentinel}
		result, err := urf.Translate(context.Background(), bytes.NewReader(src), dst, opts)
		if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindDestinationIO || !errors.Is(err, sentinel) {
			t.Fatalf("result=%+v kind=%v err=%v", result, urf.ErrorKindOf(err), err)
		}
	})
}

func translateReader(t *testing.T, reader io.Reader, opts urf.Options) (urf.Result, *writeSeeker, error) {
	t.Helper()
	dst := &writeSeeker{}
	result, err := urf.Translate(context.Background(), reader, dst, opts)
	return result, dst, err
}

func TestTranslateCancellationAndInvalidLimits(t *testing.T) {
	const width, height, dpi = 12, 8, 254
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	src := buildURF(1, page)
	opts := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, _, err := translateContext(t, ctx, bytes.NewReader(src), opts)
	if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled result=%+v kind=%v err=%v", result, urf.ErrorKindOf(err), err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	reader := &chunkReader{data: src, chunk: 1, afterRead: cancel}
	result, _, err = translateContext(t, ctx, reader, opts)
	if result != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-stream result=%+v kind=%v err=%v", result, urf.ErrorKindOf(err), err)
	}

	for _, limit := range []urf.Limits{
		{MaxInputBytes: -1},
		{MaxOutputBytes: -1},
	} {
		expectKind(t, src, urf.Options{Mapping: opts.Mapping, Page: opts.Page, Limits: limit}, urf.KindInvalidOptions)
	}
}

func translateContext(t *testing.T, ctx context.Context, src io.Reader, opts urf.Options) (urf.Result, *writeSeeker, error) {
	t.Helper()
	dst := &writeSeeker{}
	result, err := urf.Translate(ctx, src, dst, opts)
	return result, dst, err
}

func TestTranslateResourceLimits(t *testing.T) {
	const width, height, dpi = 12, 8, 254
	page, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	src := buildURF(1, page)
	base := baseOptions(urf.MappingW8ToSGray8, width, height, dpi)
	result, data, _ := expectSuccess(t, src, base)
	logical := uint64(width * height)
	rowBytes := uint64(width)

	tests := []struct {
		name string
		good func(*urf.Limits)
		bad  func(*urf.Limits)
	}{
		{"input", func(l *urf.Limits) { l.MaxInputBytes = int64(len(src)) }, func(l *urf.Limits) { l.MaxInputBytes = int64(len(src) - 1) }},
		{"output", func(l *urf.Limits) { l.MaxOutputBytes = int64(len(data)) }, func(l *urf.Limits) { l.MaxOutputBytes = int64(len(data) - 1) }},
		{"width", func(l *urf.Limits) { l.MaxWidth = width }, func(l *urf.Limits) { l.MaxWidth = width - 1 }},
		{"height", func(l *urf.Limits) { l.MaxHeight = height }, func(l *urf.Limits) { l.MaxHeight = height - 1 }},
		{"row-bytes", func(l *urf.Limits) { l.MaxRowBytes = rowBytes }, func(l *urf.Limits) { l.MaxRowBytes = rowBytes - 1 }},
		{"decoded-bytes", func(l *urf.Limits) { l.MaxDecodedBytes = logical }, func(l *urf.Limits) { l.MaxDecodedBytes = logical - 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name+"-at-boundary", func(t *testing.T) {
			limits := base.Limits
			tc.good(&limits)
			got, _, err := translate(t, src, urf.Options{Mapping: base.Mapping, Page: base.Page, Limits: limits})
			if err != nil || got != result {
				t.Fatalf("boundary result=%+v err=%v, want %+v", got, err, result)
			}
		})
		t.Run(tc.name+"-one-beyond", func(t *testing.T) {
			limits := base.Limits
			tc.bad(&limits)
			got, _, err := translate(t, src, urf.Options{Mapping: base.Mapping, Page: base.Page, Limits: limits})
			if got != (urf.Result{}) || urf.ErrorKindOf(err) != urf.KindResourceLimit {
				t.Fatalf("result=%+v kind=%v err=%v, want resource limit", got, urf.ErrorKindOf(err), err)
			}
		})
	}

	// MaxPages=0 is the documented unlimited value; use a two-page input to
	// distinguish it from a one-page bound.
	page2, _ := compactPageFixed(urf.MappingW8ToSGray8, width, height, dpi)
	result2, _, _ := expectSuccess(t, buildURF(2, page, page2), base)
	if result2.Pages != 2 {
		t.Fatalf("unlimited MaxPages result = %+v", result2)
	}
	limits := base.Limits
	limits.MaxPages = 2
	if got, _, err := translate(t, buildURF(2, page, page2), urf.Options{Mapping: base.Mapping, Page: base.Page, Limits: limits}); err != nil || got.Pages != 2 {
		t.Fatalf("MaxPages boundary result=%+v err=%v", got, err)
	}
	limits.MaxPages = 1
	expectKind(t, buildURF(2, page, page2), urf.Options{Mapping: base.Mapping, Page: base.Page, Limits: limits}, urf.KindResourceLimit)
}
