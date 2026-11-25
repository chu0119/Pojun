package agent

// The four constructors below are the single source of truth for the
// TailOptions each runtime builder hands to the shared assembly
// (RuntimeSystem). The runtime builders and their anti-drift tests both call
// them, so changing which fields an agent's tails interpolate is a one-line
// edit that the projection/runtime comparison test sees immediately, instead
// of two hand-written literals that could silently diverge.

// PlannerTailOptions builds the planner's tail options.
func PlannerTailOptions(workDir string) TailOptions {
	return TailOptions{WorkDir: workDir}
}

// WorkerTailOptions builds the worker's tail options. CAEnabled is true exactly
// when a CA cert is present, which is when the recording MITM (and thus the
// traffic_* tools) is on.
func WorkerTailOptions(proxyAddr, caCert, runDir string) TailOptions {
	return TailOptions{RunDir: runDir, ProxyAddr: proxyAddr, CAEnabled: caCert != "", IncludeHostTools: true}
}

// MainAgentTailOptions builds the main agent's tail options. It includes the
// host-tools list because mainagent also has a Bash tool and may inspect hosts.
func MainAgentTailOptions(workDir string) TailOptions {
	return TailOptions{WorkDir: workDir, IncludeHostTools: true}
}

// ChatTailOptions builds a conversational agent's tail options.
func ChatTailOptions(workDir string) TailOptions {
	return TailOptions{WorkDir: workDir}
}

// GoalsTailOptions builds the one-shot goals decomposer's tail options. goalScope
// is true only when a task plus an asset library are in context.
func GoalsTailOptions(goalScope bool) TailOptions {
	return TailOptions{GoalScope: goalScope}
}
