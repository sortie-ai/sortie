package main

import (
	"bytes"
	"os"
	"testing"
)

func TestPrintHelp(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printHelp(&buf)

	golden, err := os.ReadFile("testdata/help.txt")
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", "testdata/help.txt", err)
	}

	got := buf.String()
	want := string(golden)
	if got != want {
		t.Errorf("printHelp() output mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestPrintValidateHelp(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printValidateHelp(&buf)

	golden, err := os.ReadFile("testdata/help-validate.txt")
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", "testdata/help-validate.txt", err)
	}

	got := buf.String()
	want := string(golden)
	if got != want {
		t.Errorf("printValidateHelp() output mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestPrintMCPServerHelp(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printMCPServerHelp(&buf)

	golden, err := os.ReadFile("testdata/help-mcp-server.txt")
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", "testdata/help-mcp-server.txt", err)
	}

	got := buf.String()
	want := string(golden)
	if got != want {
		t.Errorf("printMCPServerHelp() output mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestInterceptShortFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantAction string
	}{
		{name: "-h", args: []string{"-h"}, wantAction: "help"},
		{name: "-V", args: []string{"-V"}, wantAction: "version"},
		{name: "-help", args: []string{"-help"}, wantAction: "help"},
		{name: "-h after other flags", args: []string{"--dry-run", "-h"}, wantAction: "help"},
		{name: "-V after other flags", args: []string{"--dry-run", "-V"}, wantAction: "version"},
		{name: "validate before -h", args: []string{"validate", "-h"}, wantAction: ""},
		{name: "mcp-server before -h", args: []string{"mcp-server", "-h"}, wantAction: ""},
		{name: "-- before -h", args: []string{"--", "-h"}, wantAction: ""},
		{name: "positional arg only", args: []string{"WORKFLOW.md"}, wantAction: ""},
		{name: "empty args", args: []string{}, wantAction: ""},
		{name: "-h before validate", args: []string{"-h", "validate"}, wantAction: "help"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			action := interceptShortFlags(tt.args)

			if action != tt.wantAction {
				t.Errorf("interceptShortFlags(%v) = %q, want %q", tt.args, action, tt.wantAction)
			}
		})
	}
}

func TestContainsHelpFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "-h", args: []string{"-h"}, want: true},
		{name: "--help", args: []string{"--help"}, want: true},
		{name: "-help", args: []string{"-help"}, want: true},
		{name: "-h after flags", args: []string{"--format", "json", "-h"}, want: true},
		{name: "--help after flags", args: []string{"--format", "json", "--help"}, want: true},
		{name: "-- then -h", args: []string{"--", "-h"}, want: false},
		{name: "-- then --help", args: []string{"--", "--help"}, want: false},
		{name: "no help flag", args: []string{"--format", "json"}, want: false},
		{name: "empty", args: []string{}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := containsHelpFlag(tt.args)
			if got != tt.want {
				t.Errorf("containsHelpFlag(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
