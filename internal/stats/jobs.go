package stats

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

func cloneInt(p *int) *int {
	if p == nil {
		return nil
	}
	n := *p
	return &n
}
func maxIntPtr(a, b *int) *int {
	if b == nil || *b < 0 {
		return a
	}
	if a == nil || *b > *a {
		return cloneInt(b)
	}
	return a
}
func terminalOutcome(s string) bool {
	switch s {
	case "completed", "canceled", "aborted", "failed", "rejected":
		return true
	}
	return false
}

func jobOutcome(j Job) string {
	if terminalOutcome(j.ObservedState) {
		return j.ObservedState
	}
	if j.State == "failed" || j.State == "rejected" {
		if !j.Accepted {
			return "rejected"
		}
		return "failed"
	}
	return "unresolved"
}

func mergeJob(old, next Job) Job {
	// Identity, ownership and the submission cohort are immutable. Unknown
	// measurements in a later response never erase earlier known measurements.
	next.RegistryID, next.JobID, next.Queue, next.User, next.CreatedAt = old.RegistryID, old.JobID, old.Queue, old.User, old.CreatedAt
	next.Register = false
	newer := !next.UpdatedAt.Before(old.UpdatedAt)
	if !newer {
		next.UpdatedAt = old.UpdatedAt
		next.State = old.State
		next.StateReasons = old.StateReasons
	}
	if next.State == "" {
		next.State = old.State
	}
	if next.ObservedState == "" || terminalOutcome(old.ObservedState) || !newer {
		next.ObservedState = old.ObservedState
	}
	if next.StateReasons == "" {
		next.StateReasons = old.StateReasons
	}
	next.Accepted = old.Accepted || next.Accepted
	next.CancelAccepted = old.CancelAccepted || next.CancelAccepted
	if old.ProcessingAt != nil {
		next.ProcessingAt = old.ProcessingAt
	}
	if old.TerminalAt != nil {
		next.TerminalAt = old.TerminalAt
	}
	next.DocumentCount = max(old.DocumentCount, next.DocumentCount)
	next.Copies = max(old.Copies, next.Copies)
	next.PayloadBytes = max(old.PayloadBytes, next.PayloadBytes)
	next.PageCount = maxIntPtr(old.PageCount, next.PageCount)
	next.EstimatedImpressions = maxIntPtr(old.EstimatedImpressions, next.EstimatedImpressions)
	next.CompletedImpressions = maxIntPtr(old.CompletedImpressions, next.CompletedImpressions)
	next.CompletedSheets = maxIntPtr(old.CompletedSheets, next.CompletedSheets)
	// These fields are the submission's pinned route/settings, not the printer's
	// current defaults after a capability refresh.
	for _, pair := range [][2]*string{{&next.DocumentFormat, &old.DocumentFormat}, {&next.UpstreamFormat, &old.UpstreamFormat}, {&next.Route, &old.Route}, {&next.RequestedColor, &old.RequestedColor}, {&next.EffectiveColor, &old.EffectiveColor}, {&next.RequestedSides, &old.RequestedSides}, {&next.EffectiveSides, &old.EffectiveSides}, {&next.RequestedMedia, &old.RequestedMedia}, {&next.EffectiveMedia, &old.EffectiveMedia}} {
		if *pair[1] != "" {
			*pair[0] = *pair[1]
		}
	}
	return next
}

func contribution(j Job) map[string]int64 {
	v := map[string]int64{"job_count": 1, "accepted_count": int64(boolInt(j.Accepted)), "cancel_accepted_count": int64(boolInt(j.CancelAccepted)), "document_count": int64(j.DocumentCount), "copies": int64(j.Copies), "payload_bytes": j.PayloadBytes,
		"unresolved_count": 0, "completed_count": 0, "canceled_count": 0, "aborted_count": 0, "failed_count": 0, "rejected_count": 0, "stopped_count": 0}
	v[jobOutcome(j)+"_count"] = 1
	for _, p := range []struct {
		name  string
		value *int
	}{{"page_count", j.PageCount}, {"estimated_impressions", j.EstimatedImpressions}, {"completed_impressions", j.CompletedImpressions}, {"completed_sheets", j.CompletedSheets}} {
		v[p.name], v[p.name+"_known"] = 0, 0
		if p.value != nil && *p.value >= 0 {
			v[p.name] = int64(*p.value)
			v[p.name+"_known"] = 1
		}
	}
	for _, p := range []struct {
		name string
		at   *time.Time
	}{{"processing", j.ProcessingAt}, {"completion", j.TerminalAt}} {
		v[p.name+"_observations"], v[p.name+"_total_ns"], v[p.name+"_max_ns"] = 0, 0, 0
		if p.at != nil && !p.at.Before(j.CreatedAt) {
			d := p.at.Sub(j.CreatedAt).Nanoseconds()
			v[p.name+"_observations"], v[p.name+"_total_ns"], v[p.name+"_max_ns"] = 1, d, d
		}
	}
	return v
}

func persistJob(tx *sql.Tx, next Job, retention time.Duration) error {
	if next.RegistryID == "" || next.JobID < 1 {
		return nil
	}
	var old Job
	var data string
	err := tx.QueryRow(`SELECT snapshot_json FROM job_checkpoints WHERE registry_id=? AND job_id=?`, next.RegistryID, next.JobID).Scan(&data)
	exists := err == nil
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if !exists && !next.Register {
		return nil
	}
	if exists {
		if err := json.Unmarshal([]byte(data), &old); err != nil {
			return err
		}
		next = mergeJob(old, next)
	}
	if next.PageCount != nil && *next.PageCount < 0 {
		next.PageCount = nil
	}
	if next.EstimatedImpressions != nil && *next.EstimatedImpressions < 0 {
		next.EstimatedImpressions = nil
	}
	if next.CompletedImpressions != nil && *next.CompletedImpressions < 0 {
		next.CompletedImpressions = nil
	}
	if next.CompletedSheets != nil && *next.CompletedSheets < 0 {
		next.CompletedSheets = nil
	}
	day := dayString(next.CreatedAt)
	retainRollup := retention == 0 || day >= dayString(time.Now().UTC().Add(-retention))
	if retainRollup {
		values := contribution(next)
		if exists {
			for k, v := range contribution(old) {
				if !strings.HasSuffix(k, "_max_ns") {
					values[k] -= v
				}
			}
		}
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		columns := []string{"submission_day", "queue", "user_name"}
		columns = append(columns, keys...)
		args := []any{day, next.Queue, next.User}
		updates := make([]string, 0, len(keys))
		for _, key := range keys {
			args = append(args, values[key])
			if strings.HasSuffix(key, "_max_ns") {
				updates = append(updates, key+"=MAX(job_rollups."+key+",excluded."+key+")")
			} else {
				updates = append(updates, key+"=job_rollups."+key+"+excluded."+key)
			}
		}
		_, err = tx.Exec(`INSERT INTO job_rollups(`+strings.Join(columns, ",")+`) VALUES(`+placeholders(len(columns))+`) ON CONFLICT(submission_day,queue,user_name) DO UPDATE SET `+strings.Join(updates, ","), args...)
		if err != nil {
			return fmt.Errorf("update job rollup: %w", err)
		}
		if exists {
			if err := settingContribution(tx, old, -1); err != nil {
				return err
			}
		}
		if err := settingContribution(tx, next, 1); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO job_checkpoints(registry_id,job_id,submission_day,queue,user_name,unresolved,updated_at,snapshot_json) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(registry_id,job_id) DO UPDATE SET unresolved=excluded.unresolved,updated_at=excluded.updated_at,snapshot_json=excluded.snapshot_json`, next.RegistryID, next.JobID, day, next.Queue, next.User, boolInt(jobOutcome(next) == "unresolved"), formatTime(next.UpdatedAt), string(encoded))
	if err != nil {
		return err
	}
	// Reflection enumerates our fixed struct tags, never names supplied by a
	// client. Values remain bound SQL parameters, including exact usernames.
	columns, args := jobColumns(next)
	columns = append(columns, "submission_day")
	args = append(args, day)
	updates := make([]string, 0, len(columns))
	for _, key := range columns {
		if key != "registry_id" && key != "job_id" {
			updates = append(updates, key+"=excluded."+key)
		}
	}
	_, err = tx.Exec(`INSERT INTO jobs(`+strings.Join(columns, ",")+`) VALUES(`+placeholders(len(columns))+`) ON CONFLICT(registry_id,job_id) DO UPDATE SET `+strings.Join(updates, ","), args...)
	return err
}

func jobColumns(j Job) ([]string, []any) {
	v := reflect.ValueOf(j)
	t := v.Type()
	var columns []string
	var args []any
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name == "register" {
			continue
		}
		if name == "user" {
			name = "user_name"
		}
		value := v.Field(i).Interface()
		switch p := value.(type) {
		case time.Time:
			value = formatTime(p)
		case *time.Time:
			if p == nil {
				value = nil
			} else {
				value = formatTime(*p)
			}
		case *int:
			if p == nil {
				value = nil
			} else {
				value = *p
			}
		case bool:
			value = boolInt(p)
		}
		columns = append(columns, name)
		args = append(args, value)
	}
	return columns, args
}

func settingContribution(tx *sql.Tx, j Job, n int) error {
	keys := []string{"submission_day", "queue", "user_name", "document_format", "upstream_format", "route", "requested_color", "effective_color", "requested_sides", "effective_sides", "requested_media", "effective_media"}
	args := []any{dayString(j.CreatedAt), j.Queue, j.User, j.DocumentFormat, j.UpstreamFormat, j.Route, j.RequestedColor, j.EffectiveColor, j.RequestedSides, j.EffectiveSides, j.RequestedMedia, j.EffectiveMedia, n}
	_, err := tx.Exec(`INSERT INTO job_setting_rollups(`+strings.Join(keys, ",")+`,job_count) VALUES(`+placeholders(len(args))+`) ON CONFLICT(`+strings.Join(keys, ",")+`) DO UPDATE SET job_count=job_setting_rollups.job_count+excluded.job_count`, args...)
	return err
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
