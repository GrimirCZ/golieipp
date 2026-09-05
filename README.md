# golieipp

`golieipp` is a standalone enforcing IPP proxy written in Go. It mirrors an upstream IPP printer while advertising and enforcing a constrained job ticket policy, such as A4 monochrome stationery.

Document payloads are not rasterized or rewritten. The proxy parses the IPP envelope, normalizes policy-controlled job attributes, and forwards the original document stream to the upstream printer.

## Run

```sh
go run ./cmd/golieipp -config config.yaml
```

Enable debug tracing while diagnosing setup or print failures:

```sh
go run ./cmd/golieipp -config config.yaml -debug
```

Debug logs are structured JSON and include request correlation IDs, IPP operation/request IDs, upstream HTTP status, upstream IPP status, and timing. Document payload bytes are not logged.

`GET /healthz` is process liveness. `GET /readyz` returns JSON for every queue,
including active/stale state, the last refresh error, IPP Everywhere eligibility,
and DNS-SD degradation. A required inactive queue makes readiness return 503;
an inactive optional queue or a stale last-known-good snapshot does not.

Dump the raw capabilities of every configured upstream printer without starting
the proxy or opening its SQLite job store:

```sh
go run ./cmd/golieipp -config config.yaml -dump-printer-capabilities > capabilities.json
```

The command probes optional printers too and emits one JSON object per queue.
The `attributes` array contains the decoded upstream printer attributes before
policy filtering, including media sizes that are not in the configured policy.
Probe errors are kept on their queue entry and cause a non-zero exit status.

Each proxy queue has its own stable printer identity derived from its public
printer URI, so clients do not reuse the upstream printer's capability cache.
Configuration is loaded at startup; after changing a queue's policy, restart
the proxy so clients receive new proxy-owned configuration-change metadata.

Start from `config.example.yaml`.

## Install with systemd

Build the Linux binary:

```sh
make linux-x86
```

The official Linux target and Docker image include the `avahi` build tag. A
minimal or non-Linux build can omit it (`make build TAGS=`); IPP remains usable
but multicast discovery is unavailable.

Create the service user and installation directory:

```sh
sudo useradd --system --home-dir /opt/golieipp --shell /usr/sbin/nologin golieipp
sudo install -d -o golieipp -g golieipp /opt/golieipp
```

Install the binary and configuration:

```sh
sudo install -o golieipp -g golieipp -m 0755 dist/golieipp-linux-amd64 /opt/golieipp/golieipp
sudo install -o golieipp -g golieipp -m 0640 config.example.yaml /opt/golieipp/config.yaml
```

Edit `/opt/golieipp/config.yaml` for the target printer, especially `listen.public_base_url`,
`storage.sqlite_path`, and `printers.<queue>.upstream_uri`. If `storage.sqlite_path` is left as
`jobs.db`, the database is created in `/opt/golieipp` because the unit uses that as its working
directory.

Install and start the systemd unit:

```sh
sudo install -m 0644 systemd/golieipp.service /etc/systemd/system/golieipp.service
sudo systemctl daemon-reload
sudo systemctl enable --now golieipp.service
```

Check service status and logs:

```sh
systemctl status golieipp.service
journalctl -u golieipp.service -f
```

## Run with Docker Compose

Create a local config file:

```sh
cp config.example.yaml config.yaml
```

Edit `config.yaml` for the target printer. For the compose setup, keep `listen.addr` on `:8631`
and set SQLite storage to the mounted data volume:

```yaml
storage:
  sqlite_path: "/data/jobs.db"
```

Build and start the container:

```sh
docker compose up --build -d
```

DNS-SD from a container is intentionally not enabled by the supplied Compose
file. To publish through the host Avahi daemon, the deployment must deliberately
expose the system D-Bus socket, grant an Avahi/D-Bus policy to the container
user, and allow mDNS multicast (UDP 5353) on the selected network. Those are
host-specific security decisions; do not mount the system bus broadly without
a restrictive policy. Direct `ipp://`/`ipps://` queue URLs work without them.

Check service status and logs:

```sh
docker compose ps
docker compose logs -f golieipp
```

Stop the service:

```sh
docker compose down
```

## Probe upstream printer

Use `ipptool` against the real printer before writing `config.yaml`. Replace the URI with the printer's IPP endpoint:

```sh
UPSTREAM_URI="ipp://192.168.10.50/ipp/print"
ipptool -tv "$UPSTREAM_URI" /usr/share/cups/ipptool/get-printer-attributes.test
```

If that standard CUPS test file is not installed, create a small probe file:

```sh
cat > /tmp/golieipp-probe.test <<'EOF'
{
  NAME "Get printer attributes for golieipp config"
  OPERATION Get-Printer-Attributes
  GROUP operation-attributes-tag
  ATTR charset attributes-charset utf-8
  ATTR naturalLanguage attributes-natural-language en
  ATTR uri printer-uri $uri
  ATTR keyword requested-attributes all
}
EOF

ipptool -tv "$UPSTREAM_URI" /tmp/golieipp-probe.test
```

Use the output to fill:

- `printers.<queue>.upstream_uri`: the `UPSTREAM_URI` used for the probe.
- `printers.<queue>.optional`: set to `true` to let the proxy start while this printer is offline. Optional printers are probed in the background immediately after startup and then at `refresh_interval`; the queue returns `printer-is-deactivated` until the first successful probe.
- `printers.<queue>.ipp_everywhere_mode`: `auto` trusts an upstream
  `ipp-features-supported=ipp-everywhere` claim, `required` leaves the queue
  inactive without that claim, and `disabled` suppresses the proxy feature and
  discovery advertisement. This is a compatibility advertisement, not proof
  that the proxy passed the PWG self-certification suite.
- `printers.<queue>.dns_sd`: opt a queue out of DNS-SD while retaining IPP
  Everywhere feature attributes. Avahi loss is reported as degraded and
  retried; it never deactivates an otherwise usable queue.
- `printers.<queue>.geo_location`: optional `geo:` URI used for a DNS LOC
  record when the upstream does not provide `printer-geo-location`.
- `printers.<queue>.location`: optional override for the advertised `printer-location`; use `""` or omit it to advertise an empty location.
- `policy.media_supported`: choose one or more values advertised in `media-supported`, for example `iso_a4_210x297mm` and `na_letter_8.5x11in`.
- `policy.media_default`: choose the value used when a client omits media or requests an unsupported/malformed media value. It must be one of `media_supported`.
- `policy.media`: legacy single-size configuration; it is promoted to a one-item `media_supported` list and its default. Do not combine it with the new fields.
- `policy.print_color_mode`: choose a value advertised in `print-color-mode-supported`, usually `monochrome` for this proxy's default policy.
- `policy.media_type`: choose a value from `media-type-supported`, if the printer advertises it; otherwise the default `stationery` is used.
- `policy.media_source`: choose a value from `media-source-supported` when you need to force a tray. When unset, the proxy uses an upstream-proven `auto` source, otherwise the upstream default, and otherwise omits the source rather than inventing one.
- `policy.fidelity_mode`: `warn` (default) enforces configured media/color
  policy and returns `successful-ok-ignored-or-substituted-attributes` plus an
  Unsupported group for well-formed conflicts, even when the client asks for
  fidelity. `reject` rejects those conflicts when fidelity is true. Malformed
  and unsafe values are always rejected.
- `passthrough.preserve_job_attrs`: the authoritative allowlist for
  non-policy job-template attributes. `allow_unknown_attributes` is accepted
  only for migration, logs a deprecation warning, and has no effect.

Global `defaults` bound IPP envelopes (1 MiB), documents (1 GiB), upstream
responses (32 MiB), concurrent payload jobs per queue (2), and terminal-job
retention (30 days). All are configurable as shown in `config.example.yaml`.

Avahi capability refreshes replace DNS-SD TXT records on the existing entry
group. Structural records (service endpoint, interface, and LOC) are fixed for
that publisher lifetime and therefore require a proxy restart to change.

## Implemented IPP operations

- `Get-Printer-Attributes`
- `Validate-Job`
- `Print-Job`
- `Create-Job`
- `Send-Document`
- `Close-Job` (only when upstream-supported)
- `Get-Job-Attributes`
- `Get-Jobs`
- `Cancel-My-Jobs` (emulated over proxy-owned mapped jobs)
- `Identify-Printer` (only when upstream-supported)
- `Cancel-Job`

`Get-Jobs` is a live upstream query joined against the SQLite registry; jobs
that did not enter through this proxy are not exposed. `my-jobs` filters by the
requesting user name recorded at submission time. This is namespace isolation,
not authentication—deploy transport authentication separately when users are
not mutually trusted.

The Create-Job/Send-Document path supports one document. That document can be
sent with either value of `last-document`; when it is false, finish the job with
a no-data Send-Document carrying `last-document=true`. A second document is
rejected with `client-error-multiple-jobs-not-supported`.

## Verification

The release gate is:

```sh
go test ./...
go test -race ./...
go test -tags avahi ./...
go test -race -tags avahi ./...
go vet ./...
go vet -tags avahi ./...
git diff --check
```

`ipptool`, PWG self-certification, macOS queue creation, and live Avahi browsing
remain recommended external interoperability checks; they are not performed by
the Go test suite.
