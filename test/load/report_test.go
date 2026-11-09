package main

import (
	"strings"
	"testing"
)

func TestMarkdownPartialRun(t *testing.T) {
	r := &report{
		Recipients: 1_000_000, Done: 384_848, InFlight: 615_152,
		Sent: 380_657, Failed: 4_191, RunSeconds: 2400, ThroughputPerSecond: 160,
		ByStatus: map[string]int64{"sent": 380_657, "failed": 4_191, "queued": 615_152},
		Failures: []string{"run aborted: budget 40m0s exhausted"},
	}
	md := r.markdown()
	for _, want := range []string{
		"FAIL (aborted at 38.5% - partial results)",
		"| done / recipients | 384,848 / 1,000,000 (38.5%), 615,152 in flight |",
		"| duplicates from recovery | n/a",
		"Last observed delivery statuses",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	t.Log(md)

	dup := int64(3)
	r = &report{Recipients: 10, Done: 10, Completed: true, DuplicatesFromRecovery: &dup}
	if md := r.markdown(); !strings.Contains(md, "PASS") || strings.Contains(md, "partial") {
		t.Errorf("completed run rendered as partial:\n%s", md)
	}
}
