package agent

import (
	"regexp"
	"strings"
	"testing"
)

// TestRealBuildersUseSharedAssembly drives the SAME entry points the runtime
// uses (plannerSystemWith / workerSystemWith / mainAgentSystemWith /
// chatSystemWith) and compares them, byte for byte, with the shared assembly
// given the options those builders are documented to use. If a builder starts
// passing a different TailOptions set, this fails — that is exactly the drift
// the read-only chain projection cannot see.
func TestRealBuildersUseSharedAssembly(t *testing.T) {
	nowRe := regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [A-Z]+`)
	norm := func(s string) string { return nowRe.ReplaceAllString(s, "<now>") }

	const (
		goal    = "probe goal"
		dataDir = "/data"
		workDir = "/work/run"
		proxy   = "127.0.0.1:8788"
		caCert  = "/data/ca.pem"
	)
	pp := ProfilePrompt{Text: "PROFILE-OVERLAY", Mode: "append"}

	cases := []struct {
		name     string
		agentKey string
		def      string
		vars     any
		opts     TailOptions
		build    func() string
	}{
		{
			name: "planner", agentKey: "planner", def: plannerDefaultTmpl,
			vars:  PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()},
			opts:  PlannerTailOptions(workDir),
			build: func() string { return plannerSystemWith(goal, dataDir, workDir, pp) },
		},
		{
			name: "worker", agentKey: "worker", def: workerDefaultTmpl,
			vars:  WorkerVars{ProxyAddr: proxy, DataDir: dataDir, Now: nowStr()},
			opts:  WorkerTailOptions(proxy, caCert, workDir),
			build: func() string { return workerSystemWith(proxy, caCert, dataDir, workDir, pp) },
		},
		{
			name: "mainagent", agentKey: "mainagent", def: mainAgentDefaultTmpl,
			vars:  MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()},
			opts:  MainAgentTailOptions(workDir),
			build: func() string { return mainAgentSystemWith(goal, dataDir, workDir, pp) },
		},
		{
			name: "pentest chat", agentKey: "pentest", def: DefaultPromptFor("pentest"),
			vars:  chatVars{DataDir: dataDir, Now: nowStr()},
			opts:  ChatTailOptions(workDir),
			build: func() string { return chatSystemWith("pentest", dataDir, workDir, pp) },
		},
	}
	for _, c := range cases {
		want, _ := RuntimeSystem(c.agentKey, c.def, c.vars, pp, c.opts)
		got := c.build()
		if norm(got) != norm(want) {
			t.Fatalf("%s: real builder drifted from shared assembly\n--- got ---\n%s\n--- want ---\n%s",
				c.name, norm(got), norm(want))
		}
		if !strings.Contains(got, "PROFILE-OVERLAY") {
			t.Fatalf("%s: profile prompt must be present in the real builder output", c.name)
		}
	}
}
