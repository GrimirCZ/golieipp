package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/grimir/golieipp/internal/config"
	"github.com/grimir/golieipp/internal/stats"
)

func statisticsOptions(c config.StatisticsConfig) stats.Options {
	return stats.Options{Enabled: c.Enabled, Path: c.SQLitePath, QueueSize: c.QueueSize,
		BatchSize: c.BatchSize, FlushInterval: c.FlushInterval,
		DetailRetention: c.DetailRetention, CPURetention: c.CPURetention,
		RollupRetention: c.RollupRetention, Resources: c.Resources, CPU: c.CPU}
}

func runStats(args []string, out, diagnostics io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, "Usage: golieipp stats <summary|users|jobs|actions|resources> [--config config.yaml | --db stats.sqlite] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--queue name] [--user name] [--group day|month] [--format table|json|csv] [--limit 100] [--offset 0]\nDates use UTC and --until is exclusive. RFC3339 timestamps are also accepted.")
		return err
	}
	command := args[0]
	switch command {
	case "summary", "users", "jobs", "actions", "resources":
	default:
		return fmt.Errorf("unknown report %q", command)
	}
	fs := flag.NewFlagSet("stats "+command, flag.ContinueOnError)
	fs.SetOutput(diagnostics)
	configPath := fs.String("config", "config.yaml", "path to YAML configuration")
	dbPath := fs.String("db", "", "statistics database path; bypasses configuration")
	sinceText := fs.String("since", "", "inclusive UTC date or RFC3339 timestamp")
	untilText := fs.String("until", "", "exclusive UTC date or RFC3339 timestamp")
	queue := fs.String("queue", "", "exact queue name")
	user := fs.String("user", "", "exact username; an explicit empty value selects unknown")
	group := fs.String("group", "", "summary grouping: day or month")
	format := fs.String("format", "table", "table, json, or csv")
	limit := fs.Int("limit", 100, "maximum detailed rows (1-1000)")
	offset := fs.Int("offset", 0, "detailed rows to skip")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *format != "table" && *format != "json" && *format != "csv" {
		return errors.New("format must be table, json, or csv")
	}
	if *group != "" && *group != "day" && *group != "month" {
		return errors.New("group must be day or month")
	}
	if *group != "" && command != "summary" && command != "users" {
		return errors.New("group applies only to summary and users")
	}
	if *limit < 1 || *limit > 1000 || *offset < 0 {
		return errors.New("limit must be 1-1000 and offset must be nonnegative")
	}
	now := time.Now().UTC()
	options := stats.ReportOptions{Command: command, Since: now.Truncate(24*time.Hour).AddDate(0, 0, -30), Until: now,
		Queue: *queue, User: *user, Group: *group, Limit: *limit, Offset: *offset}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "user" {
			options.UserSet = true
		}
	})
	var err error
	if *sinceText != "" {
		options.Since, err = reportTime(*sinceText)
		if err != nil {
			return fmt.Errorf("since: %w", err)
		}
	}
	if *untilText != "" {
		options.Until, err = reportTime(*untilText)
		if err != nil {
			return fmt.Errorf("until: %w", err)
		}
	}
	if !options.Since.Before(options.Until) {
		return errors.New("since must be earlier than until")
	}
	if *dbPath == "" {
		cfg, loadErr := config.LoadWithLogger(*configPath, slog.New(slog.NewTextHandler(diagnostics, nil)))
		if loadErr != nil {
			return loadErr
		}
		*dbPath = cfg.Statistics.SQLitePath
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := stats.Query(ctx, *dbPath, options)
	if err != nil {
		return err
	}
	return writeReport(out, diagnostics, *format, report)
}

func reportTime(value string) (time.Time, error) {
	if t, err := time.Parse(time.DateOnly, value); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New("expected YYYY-MM-DD or RFC3339 timestamp")
	}
	return t.UTC(), nil
}

func writeReport(out, diagnostics io.Writer, format string, report stats.Report) error {
	if format == "json" {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	// CSV remains a plain table; report coverage and health are emitted separately.
	keys := make([]string, 0, len(report.Metadata))
	for k := range report.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b, err := json.Marshal(report.Metadata[k])
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(diagnostics, "%s: %s\n", k, b); err != nil {
			return err
		}
	}
	if format == "csv" {
		w := csv.NewWriter(out)
		if err := w.Write(report.Columns); err != nil {
			return err
		}
		for _, row := range report.Rows {
			values := make([]string, len(report.Columns))
			for i, col := range report.Columns {
				values[i] = reportValue(row[col])
			}
			if err := w.Write(values); err != nil {
				return err
			}
		}
		w.Flush()
		return w.Error()
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, strings.Join(report.Columns, "\t")); err != nil {
		return err
	}
	for _, row := range report.Rows {
		values := make([]string, len(report.Columns))
		for i, col := range report.Columns {
			v := row[col]
			values[i] = reportValue(v)
			if v == nil {
				values[i] = "unknown"
			}
			if (col == "user" || col == "username" || col == "user_name") && values[i] == "" {
				values[i] = "(unknown)"
			}
			if strings.IndexFunc(values[i], unicode.IsControl) >= 0 {
				values[i] = strconv.Quote(values[i])
			}
		}
		if _, err := fmt.Fprintln(w, strings.Join(values, "\t")); err != nil {
			return err
		}
	}
	return w.Flush()
}

func reportValue(value any) string {
	if value == nil {
		return ""
	}
	if b, ok := value.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(value)
}
