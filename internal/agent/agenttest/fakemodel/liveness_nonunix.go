//go:build !unix

package fakemodel

// livenessChecked reports that remainingRuntime probes nothing here.
const livenessChecked = false

func remainingRuntime(int) string { return "" }

func heldLaunches([]int) []func() (released bool, cleanupErr error) { return nil }

func killHeldGroups([]int) error { return nil }
