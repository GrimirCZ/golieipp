# Project Notes

- This repository implements a standalone Go IPP policy proxy: Windows clients -> proxy -> upstream IPP printer.
- Use `github.com/OpenPrinting/goipp` for IPP encoding/decoding and `net/http` for transport.
- Preserve document payload bytes; enforce policy by rewriting IPP job-template attributes.
- SQLite is used from the start for proxy-to-upstream job ID mapping.
- Prefer streaming with `io.MultiReader` when forwarding payload-carrying IPP operations.
- Printers configured with `optional: true` skip the blocking startup probe, retry in the background, and return `printer-is-deactivated` until capabilities are fetched successfully.
- `SIGUSR1` emits the compile-time diagnostic sections, including effective redacted configuration and queue/job state; only Linux `avahi` builds add the detailed proxy-owned mDNS registrations.
- The client-facing listener is plaintext `ipp://` only for now; upstream printer URIs may still use `ipps://`. AirPrint support is intentionally practical and Avahi-based, not a runtime IPP Everywhere certification suite; ordinary IPP service must remain available when AirPrint discovery is ineligible.
