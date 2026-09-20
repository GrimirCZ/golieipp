// Command qualify runs the translator over the checked-in URF fixtures and
// asks a local CUPS raster filter to decode the resulting PWG stream.  It is
// intentionally a development qualification command; production and normal
// Go tests do not need CUPS, ImageMagick, or cgo.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/grimir/golieipp/internal/urf"
)

type fixture struct {
	Name              string `json:"name"`
	Mapping           string `json:"mapping"`
	MediaName         string `json:"media_name"`
	MediaWidth        uint32 `json:"media_width_hundredth_mm"`
	MediaHeight       uint32 `json:"media_height_hundredth_mm"`
	MediaType         string `json:"media_type"`
	MediaSource       uint32 `json:"media_source"`
	ResolutionX       uint32 `json:"resolution_x_dpi"`
	ResolutionY       uint32 `json:"resolution_y_dpi"`
	PrintQuality      uint32 `json:"print_quality"`
	Sides             string `json:"sides"`
	SheetBack         string `json:"sheet_back"`
	Pages             uint32 `json:"pages"`
	Width             uint32 `json:"width"`
	Height            uint32 `json:"height"`
	BitsPerPixel      uint32 `json:"bits_per_pixel"`
	PWGColorSpace     uint32 `json:"pwg_color_space"`
	ExpectedPixelHash string `json:"expected_pixel_sha256"`
	ExpectedHeader    header `json:"expected_header"`
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
	Fixtures []fixture `json:"fixtures"`
}

type cupsTools struct {
	python string
	oracle string
}

type cupsDecoded struct {
	Pages       []header `json:"pages"`
	PixelSHA256 string   `json:"pixel_sha256"`
	PixelBytes  int64    `json:"pixel_bytes"`
}

func main() {
	manifestPath := flag.String("manifest", "internal/urf/testdata/manifest.json", "fixture manifest")
	requireCUPS := flag.Bool("require-cups", false, "fail when libcups/Python are unavailable")
	flag.Parse()

	m, err := readManifest(*manifestPath)
	if err != nil {
		fatal(err)
	}
	manifestPathAbs, err := filepath.Abs(*manifestPath)
	if err != nil {
		fatal(fmt.Errorf("resolve manifest path: %w", err))
	}
	root := filepath.Dir(manifestPathAbs)
	oraclePath := filepath.Join(root, "cmd", "qualify", "cups_oracle.py")
	tools, ok := findCUPSTools(oraclePath)
	if !ok {
		message := "libcups raster API or Python ctypes oracle is unavailable; qualification skipped"
		if *requireCUPS {
			fatal(errors.New(message))
		}
		fmt.Println(message)
		return
	}

	fixtureDir := filepath.Join(root, "fixtures")
	referenceDir := filepath.Join(root, "reference")
	for _, f := range m.Fixtures {
		if err := qualifyOne(f, fixtureDir, referenceDir, tools); err != nil {
			fatal(fmt.Errorf("%s: %w", f.Name, err))
		}
		fmt.Printf("PASS %s\n", f.Name)
	}
}

func readManifest(path string) (manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return manifest{}, err
	}
	if len(m.Fixtures) == 0 {
		return manifest{}, errors.New("manifest has no fixtures")
	}
	return m, nil
}

func findCUPSTools(oracle string) (cupsTools, bool) {
	// Decoding goes through libcups directly so CUPS_CSPACE_SW and all pages
	// are read from the original PWG stream.
	python, err := exec.LookPath("python3")
	if err != nil {
		return cupsTools{}, false
	}
	if _, err := os.Stat(oracle); err != nil {
		return cupsTools{}, false
	}
	probe := exec.Command(python, oracle, "--probe")
	if err := probe.Run(); err != nil {
		return cupsTools{}, false
	}
	return cupsTools{python: python, oracle: oracle}, true
}

func qualifyOne(f fixture, fixtureDir, referenceDir string, tools cupsTools) error {
	srcPath := filepath.Join(fixtureDir, f.Name+".urf")
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open URF: %w", err)
	}
	defer src.Close()

	work, err := os.MkdirTemp("", "golieipp-urf-qualify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	outPath := filepath.Join(work, f.Name+".pwg")
	dst, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	result, translateErr := urf.Translate(context.Background(), src, dst, urf.Options{
		Mapping: mappingFor(f.Mapping),
		Page: urf.PageSettings{
			MediaName: f.MediaName, MediaWidth: f.MediaWidth, MediaHeight: f.MediaHeight,
			MediaType: f.MediaType, MediaSource: f.MediaSource,
			ResolutionX: f.ResolutionX, ResolutionY: f.ResolutionY,
			PrintQuality: f.PrintQuality, Sides: f.Sides, SheetBack: f.SheetBack,
		},
		Limits: urf.DefaultLimits(),
	})
	if translateErr != nil {
		dst.Close()
		return fmt.Errorf("Translate: %w", translateErr)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	if result.Pages != f.Pages {
		return fmt.Errorf("Translate pages=%d, want %d", result.Pages, f.Pages)
	}
	if result.InputBytes != fileSize(srcPath) {
		return fmt.Errorf("Translate input bytes=%d, want %d", result.InputBytes, fileSize(srcPath))
	}
	if result.OutputBytes != fileSize(outPath) {
		return fmt.Errorf("Translate output bytes=%d, file size %d", result.OutputBytes, fileSize(outPath))
	}

	actual, err := decodeWithCUPS(tools, outPath)
	if err != nil {
		return fmt.Errorf("CUPS decode of Translate output: %w", err)
	}
	referencePath := filepath.Join(referenceDir, f.Name+".pwg")
	reference, err := decodeWithCUPS(tools, referencePath)
	if err != nil {
		return fmt.Errorf("CUPS decode of independent reference: %w", err)
	}
	if uint32(len(actual.Pages)) != f.Pages {
		return fmt.Errorf("CUPS decoded pages=%d, want %d", len(actual.Pages), f.Pages)
	}
	if uint32(len(reference.Pages)) != f.Pages {
		return fmt.Errorf("CUPS reference pages=%d, want %d", len(reference.Pages), f.Pages)
	}
	if actual.PixelSHA256 != reference.PixelSHA256 {
		return fmt.Errorf("decoded pixel hash %s, want independent CUPS reference %s", actual.PixelSHA256, reference.PixelSHA256)
	}
	if actual.PixelSHA256 != f.ExpectedPixelHash {
		return fmt.Errorf("decoded pixel hash %s, want manifest source hash %s", actual.PixelSHA256, f.ExpectedPixelHash)
	}
	for page, got := range actual.Pages {
		want := expectedHeaderForPage(f, page)
		if err := compareHeader(got, want); err != nil {
			return fmt.Errorf("page %d: %w", page+1, err)
		}
	}
	return nil
}

func mappingFor(s string) urf.Mapping {
	switch s {
	case "W8->sgray_8":
		return urf.MappingW8ToSGray8
	case "SRGB24->srgb_8":
		return urf.MappingSRGB24ToSRGB8
	case "DEVRGB24->rgb_8":
		return urf.MappingDEVRGB24ToRGB8
	default:
		return 0
	}
}

func compareHeader(got, want header) error {
	checks := []struct {
		name      string
		got, want any
	}{
		{"media class", got.MediaClass, want.MediaClass}, {"media name", got.MediaName, want.MediaName},
		{"media type", got.MediaType, want.MediaType}, {"width", got.Width, want.Width}, {"height", got.Height, want.Height},
		{"bits/color", got.BitsPerColor, want.BitsPerColor}, {"bits/pixel", got.BitsPerPixel, want.BitsPerPixel},
		{"bytes/line", got.BytesPerLine, want.BytesPerLine}, {"color order", got.ColorOrder, want.ColorOrder},
		{"color space", got.ColorSpace, want.ColorSpace}, {"num colors", got.NumColors, want.NumColors},
		{"resolution x", got.ResolutionX, want.ResolutionX}, {"resolution y", got.ResolutionY, want.ResolutionY},
		{"page size x", got.PageSizeX, want.PageSizeX}, {"page size y", got.PageSizeY, want.PageSizeY},
		{"duplex", got.Duplex, want.Duplex}, {"tumble", got.Tumble, want.Tumble},
		{"total page count", got.TotalPageCount, want.TotalPageCount}, {"cross transform", got.CrossTransform, want.CrossTransform},
		{"feed transform", got.FeedTransform, want.FeedTransform}, {"image box right", got.ImageBoxRight, want.ImageBoxRight},
		{"image box bottom", got.ImageBoxBottom, want.ImageBoxBottom}, {"print quality", got.PrintQuality, want.PrintQuality},
	}
	for _, c := range checks {
		if c.got != c.want {
			return fmt.Errorf("header %s=%v, want %v", c.name, c.got, c.want)
		}
	}
	return nil
}

func decodeWithCUPS(tools cupsTools, input string) (cupsDecoded, error) {
	cmd := exec.Command(tools.python, tools.oracle, input)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return cupsDecoded{}, fmt.Errorf("%w (%s)", err, strings.TrimSpace(stderr.String()))
		}
		return cupsDecoded{}, err
	}
	var decoded cupsDecoded
	if err := json.Unmarshal([]byte(stdout.String()), &decoded); err != nil {
		return cupsDecoded{}, fmt.Errorf("parse CUPS oracle JSON: %w", err)
	}
	return decoded, nil
}

func expectedHeaderForPage(f fixture, page int) header {
	h := f.ExpectedHeader
	// ExpectedHeader stores page-one/front-side values.  A duplex sheet-back
	// transform is applied to each back page, matching CUPS' per-page header
	// initialization semantics.
	h.CrossTransform, h.FeedTransform = 1, 1
	if f.Sides == "one-sided" || page%2 == 0 {
		return h
	}
	tumble := f.Sides == "two-sided-short-edge"
	switch f.SheetBack {
	case "flipped":
		if tumble {
			h.CrossTransform = ^uint32(0)
		} else {
			h.FeedTransform = ^uint32(0)
		}
	case "manual-tumble":
		if tumble {
			h.CrossTransform, h.FeedTransform = ^uint32(0), ^uint32(0)
		}
	case "rotated":
		if !tumble {
			h.CrossTransform, h.FeedTransform = ^uint32(0), ^uint32(0)
		}
	}
	return h
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return st.Size()
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
