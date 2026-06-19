# Project Notes

- This repository implements a standalone Go IPP policy proxy: Windows clients -> proxy -> upstream IPP printer.
- Use `github.com/OpenPrinting/goipp` for IPP encoding/decoding and `net/http` for transport.
- Preserve document payload bytes; enforce policy by rewriting IPP job-template attributes.
- SQLite is used from the start for proxy-to-upstream job ID mapping.
- Prefer streaming with `io.MultiReader` when forwarding payload-carrying IPP operations.

