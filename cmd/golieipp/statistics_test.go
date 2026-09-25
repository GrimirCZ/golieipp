package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grimir/golieipp/internal/stats"
)

func TestStatisticsCLIValidation(t *testing.T) {
	for _, args := range [][]string{
		{"bogus"}, {"summary", "--format", "xml"}, {"summary", "--since", "bad"},
		{"jobs", "--limit", "0"}, {"jobs", "--offset", "-1"}, {"summary", "--group", "year"},
		{"summary", "--since", "2026-09-22", "--until", "2026-09-21"},
	} {
		var out, errOut bytes.Buffer
		if err := runStats(args, &out, &errOut); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	var out, errOut bytes.Buffer
	if err := runStats([]string{"--help"}, &out, &errOut); err != nil || !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("help: %s / %v", out.String(), err)
	}
}

func TestStatisticsOutput(t *testing.T) {
	report := stats.Report{Columns: []string{"user", "pages"}, Rows: []map[string]any{{"user": "a,b\nnext", "pages": nil}}, Metadata: map[string]any{"incomplete": true}}
	var out, errOut bytes.Buffer
	if err := writeReport(&out, &errOut, "csv", report); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(out.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if rows[1][0] != "a,b\nnext" || rows[1][1] != "" || !strings.Contains(errOut.String(), "incomplete") {
		t.Fatalf("csv: %v / %s", rows, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if err := writeReport(&out, &errOut, "json", report); err != nil {
		t.Fatal(err)
	}
	var decoded stats.Report
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Rows[0]["pages"] != nil || errOut.Len() != 0 {
		t.Fatal("JSON lost unknown value or mixed diagnostics")
	}
	out.Reset()
	if err := writeReport(&out, &errOut, "table", report); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 2 || !strings.Contains(out.String(), "unknown") {
		t.Fatalf("unsafe table: %q", out.String())
	}
}

func TestStatisticsCLIReadOnlyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	var out, diagnostics bytes.Buffer
	if err := runStats([]string{"summary", "--db", path}, &out, &diagnostics); err == nil {
		t.Fatal("missing database was accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("read-only report created missing DB: %v", err)
	}

	c := stats.New(stats.Options{Enabled: true, Path: path, QueueSize: 32, BatchSize: 16, FlushInterval: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	c.RecordAction(stats.Action{ID: "cli-example", Kind: "client", Operation: "Print-Job", User: "alice", Queue: "office", Outcome: "accepted", StartedAt: now.Add(-time.Second), FinishedAt: now, DurationNS: int64(time.Second), ClientBytes: 123})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"summary", "users", "jobs", "actions", "resources"} {
		t.Run(command, func(t *testing.T) {
			out.Reset()
			diagnostics.Reset()
			if err := runStats([]string{command, "--db", path, "--format", "json"}, &out, &diagnostics); err != nil {
				t.Fatal(err)
			}
			var report stats.Report
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatalf("invalid report %s: %v", out.String(), err)
			}
			if report.Metadata == nil {
				t.Fatal("missing report coverage metadata")
			}
			if command == "actions" && len(report.Rows) == 0 {
				t.Fatal("action not persisted or listed")
			}
		})
	}
}
