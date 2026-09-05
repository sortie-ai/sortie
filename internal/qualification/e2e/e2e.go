// Package e2e provides a deterministic, isolated end-to-end harness for
// driving a real orchestrator run against a file tracker and an
// injected agent adapter. Callers assemble a [Harness] with [NewHarness]
// or [NewHarnessWithAgent], start its workflow with [StartWorkflow], and
// observe its terminal condition with [ObserveTerminalCondition].
package e2e
