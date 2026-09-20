# URF to PWG Raster

`internal/urf` translates the supported Apple Raster (`image/urf`) subset to
PWG Raster without rendering a page. `Translate` validates each page and
copies the supported compressed packets into a private staging
`io.WriteSeeker`; it returns only after the source has reached EOF, page-count
patches have succeeded, and the destination has been rewound.

The supported input mappings are exact byte-layout mappings:

| URF input | URF color-space/depth | PWG output |
| --- | --- | --- |
| W8 | code 0, 8 bits/pixel | `sgray_8` (code 18) |
| SRGB24 | code 1, 24 bits/pixel | `srgb_8` (code 19) |
| DEVRGB24 | code 5, 24 bits/pixel | `rgb_8` (code 1) |

There are no color filters, channel conversions, rotations, resampling, or
full-page buffers. The compressed stream follows CUPS packet behavior,
including clear-to-end-of-line (`0x80`), clipped literal/repeat packets, and
row-repeat clipping. `UNIR` and reverse-byte-order `RINU` file signatures are
accepted; CUPS parses page-header integers as network-order for either
spelling. The other eight file-header bytes and declared page count are
advisory and ignored.

Normalized `PageSettings` control output media, resolution, quality, sides,
and sheet-back metadata. `PrintQuality` accepts zero (the normalized/default
value) or IPP values 3, 4, and 5. Media name and type are limited to 63 bytes
and may not contain NUL. The selected resolution must be square, and page
geometry must match the selected media in portrait or landscape orientation.
For duplex documents CUPS sheet-back transforms apply to even physical pages;
the raster pixels remain in their original order.

Zero fields in `Limits` select finite values from `DefaultLimits`. Input,
output, page count, dimensions, row bytes, and logical decoded bytes are
bounded independently. A failed translation returns a zero `Result`; callers
must discard the staging destination. Cancellation is checked between bounded
read/write, packet, patch, and finalization operations, but cannot interrupt a
reader or writer that blocks inside its own method.

The implementation follows the CUPS 2.4.14 raster implementation and its
documented Apple Raster/PWG behavior:

- [`raster-stream.c`](https://github.com/OpenPrinting/cups/blob/v2.4.14/cups/raster-stream.c)
- [`rastertopwg.c`](https://github.com/OpenPrinting/cups/blob/v2.4.14/filter/rastertopwg.c)
- [CUPS raster format](https://apple.github.io/cups/doc/spec-raster.html)
- [PWG Raster Format 5102.4](https://ftp.pwg.org/pub/pwg/candidates/cs-ippraster10-20120420-5102.4.pdf)

Independent fixtures and the optional CUPS qualification command are under
[`testdata`](testdata/README.md).
