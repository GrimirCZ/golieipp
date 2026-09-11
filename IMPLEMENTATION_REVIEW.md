# IPP Policy Proxy Hardening — Implementation Review

## Status and claim boundary

This release hardens the proxy's IPP parsing, capability projection, policy
enforcement, job virtualization, transport, and observability. It also adds an
optional Avahi DNS-SD publisher.

The `ipp-everywhere` and `ipp-everywhere-server` values are deliberately a
**compatibility advertisement**, not a formal conformance assertion. A queue
advertises them only when its configured upstream printer advertises
`ipp-features-supported=ipp-everywhere` and the queue mode permits it. The
proxy trusts that upstream claim; it does not run the PWG self-certification
suite at runtime. `ipp_everywhere_mode: required` turns absence of the upstream
claim into an inactive queue. `auto` leaves such a queue active as ordinary IPP.

DNS-SD availability never controls the IPP feature attributes or queue
readiness. Losing Avahi marks discovery degraded, retains the IPP queue, and
starts bounded retries.

## Design seams

The implementation is organized around four state-owning modules:

1. **Protocol processor** validates the HTTP/IPP envelope, target, groups,
   cardinality, syntax, operation-specific document rules, and response shape.
2. **Capability model** owns coupled client-visible capabilities, including
   media instances, formats, raster/URF families, color, operations, identity,
   and the optional IPP Everywhere feature pair.
3. **Job registry** owns proxy IDs, upstream mappings, lifecycle uncertainty,
   document counters, observations, reconciliation, and retention.
4. **DNS-SD publisher** accepts a complete queue publication snapshot and
   isolates Avahi/D-Bus lifecycle and degradation from the IPP server.

## Changes, reasons, and implementation

### Protocol validation and response shaping

- Requires HTTP POST and `application/ipp`.
- Accepts IPP 1.0, 1.1, and 2.0; an unsupported version gets
  `server-error-version-not-supported` using the closest supported response
  version.
- Echoes all 32 request-id bits, including zero. RFC 8011 reserves zero for a
  server unable to recover an ID from an invalid request; it does not require
  rejecting a decoded client request solely because its ID is zero.
- Requires the charset and natural-language attributes first, validates UTF-8,
  rejects duplicate attributes and invalid group ordering, and checks known
  tags/cardinalities/ranges.
- Validates the exact proxy printer/job target and queue ownership before
  rewriting it for the upstream. A job request must use one unambiguous target
  form.
- Requires document data for Print-Job. Send-Document requires a boolean
  `last-document`, accepts a first document with either boolean value, and
  accepts a no-data `last-document=true` terminator for the supported
  single-document Create-Job flow.
- Rejects simultaneous `media` and `media-col` and structurally invalid
  override collections.
- Every IPP response has leading charset/language attributes, canonical
  Operation/Unsupported/object group ordering, HTTP 200 with
  `application/ipp`, and `Cache-Control: no-cache`. Repeated Job groups in a
  Get-Jobs response remain distinct.
- Get-Printer-Attributes honors `requested-attributes` and returns
  `media-col-database` only when it is named explicitly.

These rules align with RFC 8010's encoding model and RFC 8011's operation and
target requirements. The typed protocol error path prevents malformed client
requests from being mistaken for upstream or database failures.

### Fidelity and authoritative policy

`policy.fidelity_mode` is `warn` by default:

- `warn` accepts a well-formed conflict in the proxy-controlled media, source,
  type, color, output, or configured vendor-selector family, applies policy,
  and returns `successful-ok-ignored-or-substituted-attributes` with the
  original attributes in an Unsupported group.
- `reject` honors `ipp-attribute-fidelity=true` by rejecting those conflicts.
- Malformed or unsafe values are rejected in both modes.
- Unsupported attributes outside the narrow policy exception follow fidelity
  semantics.

Continuing after a policy conflict while fidelity is true is an intentional,
documented deviation from strict RFC 8011 fidelity behavior. It exists because
enforcement is the proxy's purpose; operators who require strict behavior use
`reject`.

`passthrough.preserve_job_attrs` is now the only non-policy job-template
allowlist. The legacy `allow_unknown_attributes` key is parsed for migration,
logs a warning, and has no effect. Nested override collections are recursively
cleaned so they cannot bypass top-level policy.

### Coupled capability synthesis

The client view no longer copies isolated upstream values that can contradict
one another:

- Configured `color` and `monochrome` modes drive `color-supported`,
  print/output mode defaults, and compatible raster types together.
- `image/urf` is retained only with a complete, parseable `urf-supported`
  family containing a resolution token.
- `image/pwg-raster` is retained only with compatible type, valid resolution,
  and valid sheet-back attributes.
- Operations are generated from what the proxy implements end-to-end.
  Create-Job/Send-Document are a pair; Close-Job and Identify-Printer are
  exposed only when upstream-supported.
- `overrides-supported` is omitted because the proxy does not implement the
  full nested override surface. If an operator explicitly preserves an
  `overrides` request attribute, policy-controlled nested members are removed
  before forwarding so an override cannot bypass the top-level policy.
- Upstream `ipp-features-supported` is filtered. Eligible queues synthesize
  exactly `ipp-everywhere` and `ipp-everywhere-server`; ineligible queues do
  not leak an upstream feature value.

This avoids the earlier condition where individual attributes looked plausible
but macOS/CUPS rejected the overall driverless capability set.

### Media-instance model and the safe margin choice

Media is modeled as a full instance: size/name, source, type, margins, and
ready state. Database and ready collections are correlated without flattening
distinct trays or borderless variants.

- A reported zero margin remains zero.
- A reported nonzero margin remains nonzero.
- An unknown margin is omitted; it is never converted to zero or inferred from
  a global minimum.
- When an ordinary default has several valid instances, a ready nonzero-margin
  instance is preferred over a borderless instance.
- `media-col-default`, database, ready, and size attributes are derived from
  the same correlated catalog.

This resolves the macOS `.Borderless` mismatch documented in the earlier
review. The safest default is the upstream-correlated nonzero-margin ordinary
instance; omission is safer than fabrication when the upstream gives no
instance margin.

When `media_source` is unset, the proxy chooses an upstream-proven `auto`, then
an upstream-supported default, otherwise omits the source.

### Durable job virtualization

SQLite now represents `reserved`, `mapped`, `uncertain`, and `terminal`
lifecycle states independently of the last observed printer state/error.
Upstream job ID/URI are nullable. Rows also carry queue/user ownership,
document count, last-document, payload/page/impression metadata, terminal time,
and reconciliation clocks.

Print-Job and Create-Job reserve the proxy job ID before calling upstream. If
the upstream reports success, the proxy returns success even if the follow-up
mapping or metadata write fails; it marks the row uncertain where possible and
logs a critical error instead of returning a misleading retryable 503 after an
irreversible print acceptance.

Get-Jobs is a live upstream query joined to the registry. Unmapped upstream jobs
are hidden, proxy identities replace upstream identities in repeated Job
groups, and `my-jobs` filters by recorded requesting user. Cancel-My-Jobs is
emulated as mapped Cancel-Job calls. Requesting-user-name is explicitly not an
authentication boundary.

The proxy enforces one document per created job. Startup/periodic maintenance
reconciles uncertain jobs only on a unique identity match; ambiguous matches
stay uncertain, and an empty identity is never matched automatically. Failed
reconciliation attempts use bounded exponential backoff so an old batch cannot
starve newer uncertain rows. Terminal rows expire after the configured
retention (30 days by default). Schema migration is transactional and preserves
prior mappings.

### Availability and resource limits

All operations are gated until a queue's first successful capability probe.
After activation, refresh failures retain the last-known-good snapshot. A
snapshot older than twice its refresh interval is marked stale but stays
active.

- `/healthz`: process liveness.
- `/readyz`: JSON overall/per-queue readiness, optional/active/stale fields,
  last success/error, and IPP Everywhere eligibility. Linux `avahi` builds add
  only a small aggregate `mdns.state` entry; detailed mDNS state is available
  through the signal diagnostic dump.
- A required inactive queue returns 503; an inactive optional queue does not.
- Defaults: 1 MiB envelope, 1 GiB document, 32 MiB upstream response, two
  concurrent payload submissions per queue, and 30-day terminal retention.
- Capability probes have a 30-second timeout; job transport keeps the existing
  10-minute timeout.
- Printer configuration-change clocks are queue-local. Volatile upstream
  status, clocks, counters, supplies, and ready inventory do not create a new
  configuration epoch or affect another queue.

### Transport and diagnostics

- `ipp`/`ipps` URIs without a port use port 631.
- The public listener currently supports plaintext `ipp://` only. Public IPPS
  and TLS advertisement are deferred until the proxy has an end-to-end TLS
  listener and certificate configuration; upstream printers may still use
  `ipps://`.
- Client printer and job targets are matched by absolute URI scheme and
  resource path. Host and port are intentionally ignored after HTTP routing,
  because DNS-SD supplies the client-facing authority and may use an alias or
  an explicit/default port; query strings and fragments remain invalid.
- Redirects are not followed.
- The upstream must return HTTP 200 and `application/ipp`.
- Response version is negotiated against the request; request ID, response
  group structure, and leading operation attributes are checked.
- Known-length document payloads remain streamed through `io.MultiReader`.
  Unknown-length payloads are size-checked into a temporary file before any
  upstream dispatch, preventing a truncated oversized job from being accepted.
  Temporary files are removed on success, failure, and cancellation.
- Logs redact credentials and sensitive attribute values.
- Capability dumps run four probes concurrently, remain queue-ordered, retain
  a tag for every value (including out-of-band values), recursively redact
  collections, and return partial reports with a failing exit status.

### Optional Avahi DNS-SD

Configuration:

```yaml
dns_sd:
  mode: auto        # auto | off
  hostname: ""      # optional override
  interface: ""     # optional interface name

printers:
  office:
    ipp_everywhere_mode: auto  # disabled | auto | required
    dns_sd: true
    geo_location: ""
```

Linux builds with the `avahi` tag use the system D-Bus Avahi API. Other builds
use the stub. Eligible queues publish plaintext `_ipp._tcp`, the `_print` and
`_universal` subtypes, and legacy `_printer._tcp` on port zero. Public IPPS
publication is deferred until the proxy has a TLS listener and certificate
configuration.
TXT is synthesized only from filtered proxy capabilities and excludes
`application/octet-stream` from `pdl` (the IPP format list may still retain it
for ordinary IPP clients). Entries use the PWG priority order, keep `rp` in the
first 400 bytes, and enforce the per-entry and multicast aggregate limits.
Publication uses bounded collision names and retries, updates entry groups,
records the actual DNS-SD name, and withdraws on shutdown or lost eligibility.
A valid upstream `printer-geo-location` wins;
the per-queue override is the fallback for a LOC record. TXT capability updates
are applied to the existing Avahi entry group; endpoint/interface/LOC changes
require a proxy restart so a failed refresh cannot destroy the last good
structural publication.

The official Linux Make target and Docker build enable `avahi`. The default
Compose file intentionally does not mount the host system bus; deployments must
make an explicit D-Bus policy and multicast-networking decision.

`SIGUSR1` requests a structured application diagnostic dump. Diagnostic output
is assembled through compile-time diagnostic plugins: the common application
plugin emits effective redacted configuration, queue/capability state, and job
registry aggregates, while the `linux && avahi` build adds the complete
proxy-owned publisher snapshot and every registration/TXT record. Non-Avahi
builds omit the mDNS diagnostic section and the `/readyz` mDNS entry. The dump
does not include document bytes, does not enumerate registrations owned by
other Avahi clients, and does not verify multicast reception by a client. The
command binds this dump to `SIGUSR1` on Unix; platforms without that signal
still retain the in-process diagnostic API and simply do not install a signal
trigger.

## Configuration validation

Configuration rejects missing/invalid IPP URIs, unsupported modes, malformed
keywords, impossible ranges/sizes, nonpositive durations/limits, and invalid
geo URIs. Unknown YAML keys are logged rather than fatal so forward-compatible
rollouts remain possible.

## Verification and remaining external work

Required automated gate:

```sh
go test ./...
go test -race ./...
go test -tags avahi ./...
go test -race -tags avahi ./...
go vet ./...
go vet -tags avahi ./...
git diff --check
```

The Go suite covers configuration/migration, protocol validation, normalization
and fidelity, media instances, raster/URF coupling, identity clocks, registry
transitions/migration/retention, strict upstream transport, DNS record/LOC
encoding, readiness, dumps, streaming, and mapped job operations.

Still recommended before labeling a release formally conformant:

- run the PWG IPP Everywhere self-certification suite against every advertised
  queue;
- exercise `ipptool` negative and override tests;
- create fresh queues on current macOS, Windows, and CUPS clients;
- validate Avahi publication/loss/collision behavior on the target Linux image;
- confirm authentication and authorization at the deployment boundary.

Until those pass and the policy-fidelity deviation is either disabled or
accepted by the conformance program, describe the proxy as IPP Everywhere
compatible—not certified.
