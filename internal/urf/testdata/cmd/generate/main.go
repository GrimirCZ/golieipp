// Command generate writes deterministic, compact URF fixtures.  It does not
// import the production parser: the bytes are deliberately an independent
// source for qualification.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

type row struct {
	pixels  []byte
	repeat  int
	mode    string
	control int
	data    []byte
}

type page struct {
	width, height int
	bpp, space    byte
	duplex        byte
	quality       byte
	media         byte
	position      byte
	dpi           uint32
	rows          []row
}

type fixture struct {
	Name                string `json:"name"`
	Mapping             string `json:"mapping"`
	MediaName           string `json:"media_name"`
	MediaWidth          uint32 `json:"media_width_hundredth_mm"`
	MediaHeight         uint32 `json:"media_height_hundredth_mm"`
	MediaType           string `json:"media_type"`
	MediaSource         uint32 `json:"media_source"`
	ResolutionX         uint32 `json:"resolution_x_dpi"`
	ResolutionY         uint32 `json:"resolution_y_dpi"`
	PrintQuality        uint32 `json:"print_quality"`
	Sides               string `json:"sides"`
	SheetBack           string `json:"sheet_back"`
	DeclaredPageCount   uint32 `json:"declared_page_count"`
	URFSignature        string `json:"urf_signature"`
	URFHeaderTagHex     string `json:"urf_header_tag_hex"`
	Width               uint32 `json:"width"`
	Height              uint32 `json:"height"`
	BitsPerPixel        uint32 `json:"bits_per_pixel"`
	URFColorSpace       uint32 `json:"urf_color_space"`
	PWGColorSpace       uint32 `json:"pwg_color_space"`
	Pages               uint32 `json:"pages"`
	ExpectedPixelSHA256 string `json:"expected_pixel_sha256"`
	SourceSHA256        string `json:"source_sha256"`
	ExpectedHeader      header `json:"expected_header"`
	CUPSPermissive      bool   `json:"cups_permissive"`
}

type header struct {
	MediaClass     string `json:"media_class"`
	MediaName      string `json:"media_name"`
	MediaType      string `json:"media_type"`
	Width          uint32 `json:"width"`
	Height         uint32 `json:"height"`
	BitsPerColor   uint32 `json:"bits_per_color"`
	BitsPerPixel   uint32 `json:"bits_per_pixel"`
	BytesPerLine   uint32 `json:"bytes_per_line"`
	ColorOrder     uint32 `json:"color_order"`
	ColorSpace     uint32 `json:"color_space"`
	NumColors      uint32 `json:"num_colors"`
	ResolutionX    uint32 `json:"resolution_x_dpi"`
	ResolutionY    uint32 `json:"resolution_y_dpi"`
	PageSizeX      uint32 `json:"page_size_x_points"`
	PageSizeY      uint32 `json:"page_size_y_points"`
	Duplex         uint32 `json:"duplex"`
	Tumble         uint32 `json:"tumble"`
	TotalPageCount uint32 `json:"total_page_count"`
	CrossTransform uint32 `json:"cross_feed_transform"`
	FeedTransform  uint32 `json:"feed_transform"`
	ImageBoxRight  uint32 `json:"image_box_right"`
	ImageBoxBottom uint32 `json:"image_box_bottom"`
	PrintQuality   uint32 `json:"print_quality"`
}

type manifest struct {
	Generator         string    `json:"generator"`
	CUPSSource        string    `json:"cups_source"`
	SystemCUPSVersion string    `json:"system_cups_version"`
	Fixtures          []fixture `json:"fixtures"`
}

func main() {
	out := flag.String("out", ".", "fixture directory")
	flag.Parse()
	if err := generate(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(out string) error {
	if err := os.MkdirAll(filepath.Join(out, "fixtures"), 0o755); err != nil {
		return err
	}
	fixtures := ordinaryFixtures()
	fixtures = append(fixtures, permissiveFixtures()...)
	m := manifest{
		Generator:         "internal/urf/testdata/cmd/generate (stdlib Go)",
		CUPSSource:        "OpenPrinting/cups v2.4.14 tag 2190813d (cups/raster-stream.c; tag object short ID)",
		SystemCUPSVersion: "macOS Apple CUPS 2.3.4 (recorded at generation time)",
	}
	for _, f := range fixtures {
		data, pixels := encodeFixture(f)
		name := filepath.Join(out, "fixtures", f.Name+".urf")
		if err := os.WriteFile(name, data, 0o644); err != nil {
			return err
		}
		f.SourceSHA256 = sha256Hex(data)
		f.ExpectedPixelSHA256 = sha256Hex(pixels)
		m.Fixtures = append(m.Fixtures, f)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(filepath.Join(out, "manifest.json"), b, 0o644)
}

func ordinaryFixtures() []fixture {
	return []fixture{
		newFixture("w8-portrait-simplex", "W8->sgray_8", "fixture-portrait", 106, 142, 3, 4, 8, 0, 1, "one-sided", "normal", 1, false),
		newFixture("w8-landscape-duplex", "W8->sgray_8", "fixture-landscape", 177, 106, 5, 3, 8, 0, 2, "two-sided-short-edge", "flipped", 1, false),
		newFixture("srgb24-portrait-simplex", "SRGB24->srgb_8", "fixture-rgb-portrait", 106, 142, 3, 4, 24, 1, 1, "one-sided", "normal", 1, false),
		newFixture("devrgb24-landscape-duplex", "DEVRGB24->rgb_8", "fixture-devrgb-landscape", 177, 106, 5, 3, 24, 5, 3, "two-sided-long-edge", "flipped", 1, false),
		newFixture("w8-multipage-known", "W8->sgray_8", "fixture-portrait", 106, 142, 3, 4, 8, 0, 1, "one-sided", "normal", 2, false),
		newFixture("w8-multipage-unspecified", "W8->sgray_8", "fixture-portrait", 106, 142, 3, 4, 8, 0, 1, "one-sided", "normal", 0xffffffff, false),
	}
}

func permissiveFixtures() []fixture {
	fixtures := []fixture{
		newFixture("cups-permissive-clear-eol", "W8->sgray_8", "fixture-edge", 142, 106, 4, 3, 8, 0, 1, "one-sided", "normal", 1, true),
		newFixture("cups-permissive-repeat-height", "W8->sgray_8", "fixture-edge", 142, 106, 4, 3, 8, 0, 1, "one-sided", "normal", 1, true),
		newFixture("cups-permissive-literal-width", "W8->sgray_8", "fixture-edge", 142, 36, 4, 1, 8, 0, 1, "one-sided", "normal", 1, true),
	}
	// CUPS recognizes both Apple sync spellings and ignores the eight bytes
	// following the four-byte signature. Keep these cases in the generated
	// corpus so the native writer and the translator see the same behavior.
	fixtures[0].URFSignature = "RINU"
	fixtures[1].URFHeaderTagHex = "deadbeef"
	return fixtures
}

func newFixture(name, mapping, media string, mw, mh uint32, w, h int, bpp, space, duplex byte, sides, sheet string, declared uint32, permissive bool) fixture {
	pwgSpace := uint32(18)
	if bpp == 24 && space == 1 {
		pwgSpace = 19
	} else if bpp == 24 && space == 5 {
		pwgSpace = 1
	}
	f := fixture{
		Name: name, Mapping: mapping, MediaName: media, MediaWidth: mw, MediaHeight: mh,
		MediaType: "stationery", MediaSource: 0, ResolutionX: 72, ResolutionY: 72,
		PrintQuality: 4, Sides: sides, SheetBack: sheet, DeclaredPageCount: declared,
		URFSignature: "UNIR", URFHeaderTagHex: "41535400",
		Width: uint32(w), Height: uint32(h), BitsPerPixel: uint32(bpp), URFColorSpace: uint32(space),
		PWGColorSpace: pwgSpace, Pages: 1, CUPSPermissive: permissive,
	}
	if name == "w8-multipage-known" || name == "w8-multipage-unspecified" {
		f.Pages = 2
	}
	f.ExpectedHeader = expectedHeader(f)
	return f
}

func expectedHeader(f fixture) header {
	pageW := f.MediaWidth * 72 / 2540
	pageH := f.MediaHeight * 72 / 2540
	return header{
		MediaClass: "PwgRaster", MediaName: f.MediaName, MediaType: f.MediaType,
		Width: f.Width, Height: f.Height, BitsPerColor: 8, BitsPerPixel: f.BitsPerPixel,
		BytesPerLine: f.Width * f.BitsPerPixel / 8, ColorOrder: 0, ColorSpace: f.PWGColorSpace,
		NumColors: f.BitsPerPixel / 8, ResolutionX: f.ResolutionX, ResolutionY: f.ResolutionY,
		PageSizeX: pageW, PageSizeY: pageH, Duplex: duplexForSides(f.Sides),
		Tumble: tumbleForSides(f.Sides), TotalPageCount: f.Pages,
		// The manifest describes the first emitted page.  CUPS applies
		// sheet-back transforms to duplex back sides, so page one retains the
		// front-side identity transforms.
		CrossTransform: 1, FeedTransform: 1,
		ImageBoxRight: f.Width, ImageBoxBottom: f.Height, PrintQuality: f.PrintQuality,
	}
}

func duplexForSides(s string) uint32 {
	if s == "two-sided-long-edge" || s == "two-sided-short-edge" {
		return 1
	}
	return 0
}
func tumbleForSides(s string) uint32 {
	if s == "two-sided-short-edge" {
		return 1
	}
	return 0
}
func encodeFixture(f fixture) ([]byte, []byte) {
	pageCount := f.DeclaredPageCount
	if pageCount == 0 {
		pageCount = 0
	}
	var out, pixels []byte
	if len(f.URFSignature) != 4 {
		panic("fixture URF signature must be four bytes")
	}
	tag, err := hex.DecodeString(f.URFHeaderTagHex)
	if err != nil || len(tag) != 4 {
		panic("fixture URF header tag must be four hex bytes")
	}
	out = append(out, []byte(f.URFSignature)...)
	out = append(out, tag...)
	var count [4]byte
	putBE32(count[:], pageCount)
	out = append(out, count[:]...)
	for pageIndex := 0; pageIndex < int(f.Pages); pageIndex++ {
		p := makePage(f, pageIndex)
		out = append(out, encodePageHeader(p)...)
		stream, pagePixels := encodeRows(p, f.CUPSPermissive, f.Name)
		out = append(out, stream...)
		pixels = append(pixels, pagePixels...)
	}
	return out, pixels
}

func makePage(f fixture, pageIndex int) page {
	p := page{width: int(f.Width), height: int(f.Height), bpp: byte(f.BitsPerPixel), space: byte(f.URFColorSpace), duplex: 1, quality: 0, media: 0, position: 0, dpi: f.ResolutionX}
	if f.Sides == "two-sided-short-edge" {
		p.duplex = 2
	} else if f.Sides == "two-sided-long-edge" {
		p.duplex = 3
	}
	bpp := int(p.bpp / 8)
	rows := make([]row, 0, p.height)
	for y := 0; y < p.height; y++ {
		pixels := makePixels(p.width, bpp, y, pageIndex)
		if y == 0 {
			rows = append(rows, row{pixels: pixels, repeat: 1, mode: "solid"})
		} else if y == 1 {
			rows = append(rows, row{pixels: pixels, repeat: 1, mode: "literal"})
		} else if y == 2 {
			rows = append(rows, row{pixels: pixels, repeat: 1, mode: "runs"})
		} else {
			rows = append(rows, row{pixels: pixels, repeat: 1, mode: "literal"})
		}
	}
	if p.height >= 4 && pageIndex == 0 {
		// Keep a repeated-looking row in the tiny fixture while retaining all
		// of its literal pixel values.
		rows[p.height-1] = row{pixels: rows[p.height-2].pixels, repeat: 1, mode: "literal"}
	}
	p.rows = rows
	return p
}

func makePixels(width, bpp, y, page int) []byte {
	row := make([]byte, width*bpp)
	if y == 0 {
		for x := 0; x < width; x++ {
			for c := 0; c < bpp; c++ {
				row[x*bpp+c] = byte(0x20 + page*0x10 + c*0x11)
			}
		}
		return row
	}
	if y == 2 {
		for x := 0; x < width; x++ {
			for c := 0; c < bpp; c++ {
				row[x*bpp+c] = byte(0xa0 + ((x/2)%2)*0x20 + c*0x07 + page)
			}
		}
		return row
	}
	for i := range row {
		row[i] = byte((i*29 + y*17 + page*13 + 3) & 0xff)
	}
	return row
}

func encodePageHeader(p page) []byte {
	h := make([]byte, 32)
	h[0], h[1], h[2], h[3], h[4], h[5] = p.bpp, p.space, p.duplex, p.quality, p.media, p.position
	putBE32(h[12:], uint32(p.width))
	putBE32(h[16:], uint32(p.height))
	putBE32(h[20:], p.dpi)
	return h
}

func encodeRows(p page, permissive bool, name string) ([]byte, []byte) {
	if permissive {
		switch name {
		case "cups-permissive-clear-eol":
			return encodePermissiveClear(p)
		case "cups-permissive-repeat-height":
			return encodePermissiveRepeat(p)
		case "cups-permissive-literal-width":
			return encodePermissiveLiteral(p)
		}
	}
	var stream, pixels []byte
	for _, r := range p.rows {
		stream = append(stream, byte(r.repeat-1))
		stream = append(stream, encodePixels(r.pixels, int(p.bpp/8), r.mode)...)
		for i := 0; i < r.repeat; i++ {
			pixels = append(pixels, r.pixels...)
		}
	}
	return stream, pixels
}

func encodePixels(pixels []byte, bpp int, mode string) []byte {
	width := len(pixels) / bpp
	if mode == "solid" {
		var out []byte
		for left := width; left > 0; {
			n := left
			if n > 128 {
				n = 128
			}
			out = append(out, byte(n-1))
			out = append(out, pixels[:bpp]...)
			left -= n
		}
		return out
	}
	if mode == "runs" {
		var out []byte
		for x := 0; x < width; {
			n := 1
			for x+n < width && n < 128 && equalPixel(pixels[x*bpp:(x+1)*bpp], pixels[(x+n)*bpp:(x+n+1)*bpp]) {
				n++
			}
			if n >= 2 {
				out = append(out, byte(n-1))
				out = append(out, pixels[x*bpp:(x+1)*bpp]...)
				x += n
				continue
			}
			start := x
			x++
			for x < width && x-start < 128 && (x+1 == width || !equalPixel(pixels[x*bpp:(x+1)*bpp], pixels[(x+1)*bpp:(x+2)*bpp])) {
				x++
			}
			n = x - start
			out = append(out, byte(257-n))
			out = append(out, pixels[start*bpp:x*bpp]...)
		}
		return out
	}
	return append([]byte{byte(257 - width)}, pixels...)
}

func equalPixel(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func encodePermissiveClear(p page) ([]byte, []byte) {
	stream := []byte{0, 0x80, 0, 249, 1, 2, 3, 4, 0, 249, 5, 6, 7, 8}
	pixels := make([]byte, p.width*p.height)
	for i := range pixels {
		pixels[i] = 0xff
	}
	copy(pixels[p.width:], []byte{1, 2, 3, 4})
	copy(pixels[2*p.width:], []byte{5, 6, 7, 8})
	return stream, pixels
}

func encodePermissiveRepeat(p page) ([]byte, []byte) {
	stream := []byte{byte(4 - 1), byte(p.width - 1), 0x7f}
	pixels := make([]byte, p.width*p.height)
	for i := range pixels {
		pixels[i] = 0x7f
	}
	return stream, pixels
}

func encodePermissiveLiteral(p page) ([]byte, []byte) {
	stream := []byte{0, 249, 1, 2, 3, 4}
	return stream, []byte{1, 2, 3, 4}
}

func putBE32(dst []byte, v uint32) {
	dst[0], dst[1], dst[2], dst[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
