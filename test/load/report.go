package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type stepTiming struct {
	Name    string  `json:"name"`
	Seconds float64 `json:"seconds"`
}

// report is the artifact of architecture 15.1. The absolute numbers are noisy
// on a shared runner, so they are recorded for the trend while the pass/fail
// judgement rests on the budget and on the consistency assertions.
type report struct {
	StartedAt   time.Time `json:"started_at"`
	Recipients  int       `json:"recipients"`
	ChunkSize   int       `json:"chunk_size"`
	CampaignID  string    `json:"campaign_id"`
	Seed        uint64    `json:"seed"`
	TempFail    float64   `json:"tempfail_rate"`
	PermFail    float64   `json:"permfail_rate"`
	Drop        float64   `json:"drop_rate"`
	MaxAttempts int       `json:"max_attempts"`

	IngestSeconds       float64 `json:"ingest_seconds"`
	IngestRowsPerSecond float64 `json:"ingest_rows_per_second"`
	IngestBudgetSeconds float64 `json:"ingest_budget_seconds"`
	ReplaySeconds       float64 `json:"chunk_replay_seconds"`
	RunSeconds          float64 `json:"run_seconds"`
	BudgetSeconds       float64 `json:"budget_seconds"`
	ThroughputPerSecond float64 `json:"throughput_msgs_per_second"`
	TotalSeconds        float64 `json:"total_seconds"`

	Sent       int64            `json:"sent"`
	Failed     int64            `json:"failed"`
	Suppressed int64            `json:"suppressed"`
	ByStatus   map[string]int64 `json:"by_status"`
	Expected   expectation      `json:"expected"`

	Chaos                  chaosStats `json:"chaos_smtp"`
	DuplicatesFromRecovery int64      `json:"duplicates_from_recovery"`
	SenderKilled           bool       `json:"sender_killed"`
	SenderRestarted        bool       `json:"sender_restarted"`

	FailedSampleSize   int     `json:"failed_sample_size"`
	LatencySampleSize  int     `json:"latency_sample_size"`
	P50CreatedToSentMs float64 `json:"p50_created_to_sent_ms"`
	P95CreatedToSentMs float64 `json:"p95_created_to_sent_ms"`
	TrackedLinks       int     `json:"tracked_links"`
	AttemptRows        int64   `json:"delivery_attempt_rows"`
	DBSizeBytes        int64   `json:"db_size_bytes"`

	Steps    []stepTiming `json:"steps"`
	Failures []string     `json:"failures"`
}

func newReport(o options) *report {
	return &report{
		StartedAt:           time.Now().UTC(),
		Recipients:          o.recipients,
		ChunkSize:           o.chunk,
		Seed:                o.seed,
		TempFail:            o.tempFail,
		PermFail:            o.permFail,
		Drop:                o.drop,
		MaxAttempts:         o.maxAttempts,
		IngestBudgetSeconds: o.ingestBudget.Seconds(),
		BudgetSeconds:       o.budget.Seconds(),
		Failures:            []string{},
		Steps:               []stepTiming{},
	}
}

func (r *report) write(jsonPath, summaryPath string) error {
	r.TotalSeconds = time.Since(r.StartedAt).Seconds()
	if jsonPath != "" {
		raw, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		// G703: jsonPath is this generator's own --json flag, not input.
		if err := os.WriteFile(jsonPath, append(raw, '\n'), 0o600); err != nil { //nolint:gosec
			return err
		}
	}
	if summaryPath == "" {
		return nil
	}
	// Appended, because $GITHUB_STEP_SUMMARY is a file several steps write to.
	f, err := os.OpenFile(summaryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: the path is this generator's own --summary flag
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(r.markdown())
	return err
}

func (r *report) markdown() string {
	var b strings.Builder
	verdict := "PASS"
	if len(r.Failures) > 0 {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "\n## sendplane load test - %s (%s recipients)\n\n", verdict, humanInt(int64(r.Recipients)))

	fmt.Fprintf(&b, "| metric | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| recipients | %s |\n", humanInt(int64(r.Recipients)))
	fmt.Fprintf(&b, "| ingest | %.1fs (%s rows/s, budget %.0fs) |\n",
		r.IngestSeconds, humanInt(int64(r.IngestRowsPerSecond)), r.IngestBudgetSeconds)
	fmt.Fprintf(&b, "| send run | %.1fs (budget %.0fs) |\n", r.RunSeconds, r.BudgetSeconds)
	fmt.Fprintf(&b, "| throughput | %s msg/s |\n", humanInt(int64(r.ThroughputPerSecond)))
	fmt.Fprintf(&b, "| sent / failed / suppressed | %s / %s / %s |\n",
		humanInt(r.Sent), humanInt(r.Failed), humanInt(r.Suppressed))
	fmt.Fprintf(&b, "| expected sent / failed | %s / %s |\n",
		humanInt(r.Expected.Sent), humanInt(r.Expected.Failed))
	fmt.Fprintf(&b, "| chaos-smtp accepted / tempfail / permfail / dropped | %s / %s / %s / %s |\n",
		humanInt(r.Chaos.Accepted), humanInt(r.Chaos.TempFailed),
		humanInt(r.Chaos.PermFailed), humanInt(r.Chaos.Dropped))
	fmt.Fprintf(&b, "| duplicates from recovery | %d (sender killed: %v) |\n",
		r.DuplicatesFromRecovery, r.SenderKilled)
	fmt.Fprintf(&b, "| delivery attempts (rows / derived) | %s / %s |\n",
		humanInt(r.AttemptRows), humanInt(r.Expected.Attempts))
	fmt.Fprintf(&b, "| created->sent p50 / p95 (n=%d) | %.0f ms / %.0f ms |\n",
		r.LatencySampleSize, r.P50CreatedToSentMs, r.P95CreatedToSentMs)
	fmt.Fprintf(&b, "| database size | %.1f MiB |\n", float64(r.DBSizeBytes)/(1<<20))
	fmt.Fprintf(&b, "| total wall clock | %.1fs |\n", r.TotalSeconds)

	if len(r.ByStatus) > 0 {
		fmt.Fprintf(&b, "\nFinal delivery statuses: ")
		first := true
		for _, s := range []string{
			"pending", "queued", "leased", "deferred", "sent", "failed",
			"bounced", "complained", "suppressed", "cancelled",
		} {
			n, ok := r.ByStatus[s]
			if !ok || n == 0 {
				continue
			}
			if !first {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "`%s`=%s", s, humanInt(n))
			first = false
		}
		b.WriteString("\n")
	}

	if len(r.Steps) > 0 {
		b.WriteString("\n<details><summary>step timings</summary>\n\n")
		for _, s := range r.Steps {
			fmt.Fprintf(&b, "- %s: %.1fs\n", s.Name, s.Seconds)
		}
		b.WriteString("\n</details>\n")
	}

	if len(r.Failures) > 0 {
		fmt.Fprintf(&b, "\n### %d failed assertion(s)\n\n", len(r.Failures))
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// humanInt groups digits so a seven-digit count is readable at a glance.
func humanInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
