//go:build !unix

package fakemodel

// livenessChecked reports that remainingRuntime probes nothing here.
const livenessChecked = false

func remainingRuntime(int) string { return "" }
