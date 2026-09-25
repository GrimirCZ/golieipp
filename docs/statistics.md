# Statistics reference

Statistics provide usage visibility rather than billing-grade accounting. The
subsystem runs independently of the job registry, using a separate SQLite file.
The default path is the registry path with `.stats.sqlite` appended.

## Configuration and lifetime

See the `statistics` section of `config.example.yaml`. Collection is enabled by
default. Set `enabled: false` to avoid opening the statistics database, starting
its writer, or collecting resource counters. Existing history remains available
to CLI reports. Configuration is read at startup.

`resources` controls explicit buffer and temporary-file accounting. `cpu` controls
process CPU readings at action boundaries. Neither starts a periodic sampler.
The flush interval is a persistence batching timer, not a resource sampler.

Detailed actions and finalized jobs default to 90 days of retention, CPU
observations to 30 days, and daily rollups to indefinite retention. A retention
value of `0` means indefinite. Cleanup preserves aggregate contributions before
removing detail and retains compact state needed to update unresolved jobs.
Job detail ages from submission; action detail ages from completion. Component
buffer peaks and observed job-latency summaries also survive detail cleanup.
Changing the primary registry's job retention does not remove statistics.

The writer uses a bounded queue. Printing continues on storage failures or queue
overflow. Known recording errors and dropped records appear in logs, reports,
and `SIGUSR1` diagnostics. An abrupt process exit can lose unflushed records even
when no error was reported; a graceful shutdown attempts to flush pending work.
Statistics are not reconstructed from older registry jobs when first enabled.

Keep the statistics database and its WAL on persistent storage. With Docker,
use a path under the mounted `/data` directory for both database paths. Reports
can run while the service is active. Copying a live SQLite file alone is not a
consistent backup: use SQLite's backup facilities.

## Measurement semantics

- **User:** Exact `requesting-user-name` at job submission. Case and domain
  prefixes remain distinct. Empty/missing usernames form the unknown group.
  Request actors and job owners are distinct fields. Background actions are
  not attributed to a user. These client-supplied identities are unauthenticated.
- **Logical job:** One `Print-Job`, or one `Create-Job` followed by its
  `Send-Document`. Request retries are separate attempts; an accepted empty
  closing request is not another document.
- **Output:** Estimated pages and impressions are separate from printer-reported
  completed impressions (printed sides) and completed sheets. Missing counts are
  SQL `NULL`; zero means a measured/reported zero. A canceled job can have
  partially printed output. PDF page inspection is best effort and copies/page
  selection can affect estimates. Color mode records the effective setting,
  not ink usage or a count of colored pixels.
- **State:** Request acceptance does not imply physical completion. Cancellation
  acceptance and observed cancellation are distinct. Printer history can expire
  before reconciliation observes completion, leaving an unresolved outcome.
- **Timing:** Wall durations measure elapsed action time, including I/O waits.
  Processing/completion observations reflect polling, not precise printer
  timestamps. Parent and child durations overlap and must not be added together.
- **Bytes:** Client document bytes are counted once as consumed from the request;
  staging replays do not count as another upload. Upstream bytes describe bytes
  consumed by the transport, not confirmation of physical printing. Translation
  output size and temporary-file writes are separate measurements. Failed actions
  retain partial measurements where available. The IPP envelope is not document
  payload.
- **Buffers:** Track capacities of explicitly instrumented buffers and their
  overlapping lifetimes. Peaks describe tracked simultaneously held capacity,
  not total process memory or heap allocation. Internal Go, TLS, HTTP, SQLite,
  and operating-system allocations are outside this accounting. A raster row
  limit or logical decoded document size is not an allocated buffer. Large
  documents still stream or stage to disk.
- **Temporary files:** Logical file size is distinct from total bytes written,
  because seeks and header rewrites can write the same bytes more than once.
  Peak usage measures simultaneously retained tracked files, not filesystem
  block allocation.
- **CPU:** User-mode and kernel-mode CPU seconds are process-wide cumulative
  counters. Readings at action boundaries are not per-action CPU attribution.
  Non-overlapping observation deltas may be converted to interval averages;
  100% represents one fully used CPU core, so multi-core usage can exceed 100%.
  No readings are collected merely because the process is idle. Unsupported
  platforms expose CPU as unavailable, not zero.

## CLI

```sh
golieipp stats summary --db /data/jobs.db.stats.sqlite --group day
golieipp stats users --db /data/jobs.db.stats.sqlite --since 2026-09-01 --until 2026-10-01
golieipp stats jobs --db /data/jobs.db.stats.sqlite --user 'DOMAIN\alice' --limit 100 --offset 0
golieipp stats actions --db /data/jobs.db.stats.sqlite --queue office-a4-bw --format csv
golieipp stats resources --db /data/jobs.db.stats.sqlite --format json
```

`--db` bypasses loading printer configuration; otherwise `--config` defaults to
`config.yaml`. Reporting never starts the proxy, probes printers, or migrates a
database. A missing database produces an error rather than an empty new file.

Reports default to the last 30 UTC days. Date-only boundaries use UTC; RFC3339
timestamps accept explicit offsets and are converted to UTC. `--since` is
inclusive and `--until` is exclusive. Daily job summaries use submission date;
daily action summaries use completion date. Summary reports disclose the daily
granularity and effective date range in metadata. Job and action detail queries
use exact timestamps. `--group day|month` applies to summaries and user reports.
Use `--user ''` to explicitly select unknown users; omitting the flag includes
all users. Detailed listing pagination is bounded to 1,000 rows per request.

The default table output quotes control characters in names. JSON includes
columns, rows, and coverage/health metadata. CSV emits the header and rows to
stdout and metadata to stderr, so exports remain valid CSV. NULL values are
empty in CSV and `null` in JSON. Tables show them as `unknown`.

CPU cannot be filtered by user or queue. Resource reports with those filters
still report attributable action resources and disclose that process-wide CPU
has been omitted. Read coverage metadata before interpreting absent rows as
zero usage: detail may have expired, collection may have been disabled, and
known recording gaps may exist.

## Manual SQLite queries

Use the statistics file, not the primary job registry. The versioned schema
provides `stats_jobs`, `stats_actions`, `stats_job_rollups`,
`stats_action_rollups`, `stats_buffer_rollups`, `stats_cpu_daily`, and `stats_runs` views. Timestamps
use UTC. `user_name = ''` represents unknown users; background action kinds
must be kept separate from client activity.

```sh
sqlite3 -readonly /data/jobs.db.stats.sqlite
```

Daily usage by user, surviving detailed-job retention:

```sql
SELECT submission_day, user_name,
       SUM(job_count) AS jobs,
       SUM(accepted_count) AS accepted,
       SUM(payload_bytes) AS document_bytes,
       CASE WHEN SUM(completed_impressions_known) > 0
            THEN SUM(completed_impressions) END AS reported_impressions,
       SUM(completed_impressions_known) AS jobs_with_reported_impressions
FROM stats_job_rollups
WHERE submission_day >= '2026-09-01' AND submission_day < '2026-10-01'
GROUP BY submission_day, user_name
ORDER BY submission_day, user_name;
```

Action latency and resource requirements, with nested stages kept separate:

```sql
SELECT kind, operation, outcome,
       SUM(action_count) AS actions,
       SUM(duration_ns) / 1000000.0 / SUM(action_count) AS average_ms,
       MAX(buffer_peak_bytes) AS largest_tracked_buffer_peak,
       MAX(temp_peak_bytes) AS largest_tracked_temporary_file_peak,
       SUM(temp_written_bytes) AS temporary_bytes_written
FROM stats_action_rollups
WHERE completion_day >= '2026-09-01' AND completion_day < '2026-10-01'
GROUP BY kind, operation, outcome;
```

Inspect a retained job's known output separately from its estimate:

```sql
SELECT registry_id, job_id, user_name, queue, state, observed_state,
       cancel_accepted, page_count, estimated_impressions,
       completed_impressions, completed_sheets
FROM stats_jobs
WHERE user_name = 'alice'
ORDER BY created_at DESC
LIMIT 100;
```

CPU daily aggregates assign observed process-counter deltas to the observation
day; they do not imply continuous sampling or exact attribution across midnight.
Check `stats_runs` for observed process starts and graceful stops, and use CLI
report metadata for recording health and retention coverage. Do not join raw
action rows directly to aggregate job totals and then sum the job totals: one
job can have many actions.
