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

Start from `config.example.yaml`.

## Install with systemd

Build the Linux binary:

```sh
make linux-x86
```

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
- `printers.<queue>.location`: optional override for the advertised `printer-location`; use `""` or omit it to advertise an empty location.
- `policy.media`: choose a value advertised in `media-supported`, for example `iso_a4_210x297mm`.
- `policy.print_color_mode`: choose a value advertised in `print-color-mode-supported`, usually `monochrome` for this proxy's default policy.
- `policy.media_type`: choose a value from `media-type-supported`, if the printer advertises it; otherwise the default `stationery` is used.
- `policy.media_source`: choose a value from `media-source-supported` when you need to force a tray, otherwise leave it `null`.

## Implemented IPP operations

- `Get-Printer-Attributes`
- `Validate-Job`
- `Print-Job`
- `Create-Job`
- `Send-Document`
- `Close-Job`
- `Get-Job-Attributes`
- `Get-Jobs`
- `Cancel-Job`
