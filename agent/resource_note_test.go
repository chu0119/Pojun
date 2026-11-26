package agent

import (
	"strings"
	"testing"

	"github.com/chu0119/pojun/resource"
)

func TestResourceCapabilityNoteDisabledIsEmpty(t *testing.T) {
	resource.SetPlan(resource.NeutralPlan())
	defer resource.SetPlan(resource.NeutralPlan())
	if got := ResourceCapabilityNote(); got != "" {
		t.Fatalf("neutral plan must not change Bash description, got %q", got)
	}
}

func TestResourceCapabilityNoteRendersPlan(t *testing.T) {
	resource.SetPlan(resource.Plan{
		Enabled: true, MaxJobs: 2, JobMaxPerTask: 1, JobRuntimeS: 3600,
		ScannerScale: 0.5, CrackerWorkload: 1, CrackerRuntimeS: 1800,
	})
	defer resource.SetPlan(resource.NeutralPlan())
	got := ResourceCapabilityNote()
	for _, want := range []string{"全局 2 个", "单任务 1 个", "60 分钟", "-rate-limit 75", "-w 1 --runtime 1800", "--max-run-time=1800"} {
		if !strings.Contains(got, want) {
			t.Fatalf("note missing %q:\n%s", want, got)
		}
	}
}

func TestResourceCapabilityNoteNoScanLineAtFullScale(t *testing.T) {
	resource.SetPlan(resource.Plan{Enabled: true, MaxJobs: 4, JobMaxPerTask: 2, ScannerScale: 1})
	defer resource.SetPlan(resource.NeutralPlan())
	got := ResourceCapabilityNote()
	if strings.Contains(got, "-rate-limit") {
		t.Fatalf("full-scale plan must not suggest rate limiting:\n%s", got)
	}
}
