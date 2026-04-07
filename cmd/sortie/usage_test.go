package main

import (
	"bytes"
	"os"
	"reflect"
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
		name           string
		args           []string
		wantAction     string
		wantReturnArgs bool // true: remaining should equal args; false: remaining should be nil
	}{
		{name: "-h", args: []string{"-h"}, wantAction: "help", wantReturnArgs: false},
		{name: "-V", args: []string{"-V"}, wantAction: "version", wantReturnArgs: false},
		{name: "-help", args: []string{"-help"}, wantAction: "help", wantReturnArgs: false},
		{name: "-h after other flags", args: []string{"--dry-run", "-h"}, wantAction: "help", wantReturnArgs: false},
		{name: "-V after other flags", args: []string{"--dry-run", "-V"}, wantAction: "version", wantReturnArgs: false},
		{name: "validate before -h", args: []string{"validate", "-h"}, wantAction: "", wantReturnArgs: true},
		{name: "mcp-server before -h", args: []string{"mcp-server", "-h"}, wantAction: "", wantReturnArgs: true},
		{name: "-- before -h", args: []string{"--", "-h"}, wantAction: "", wantReturnArgs: true},
		{name: "positional arg only", args: []string{"WORKFLOW.md"}, wantAction: "", wantReturnArgs: true},
		{name: "empty args", args: []string{}, wantAction: "", wantReturnArgs: true},
		{name: "-h before validate", args: []string{"-h", "validate"}, wantAction: "help", wantReturnArgs: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			action, remaining := interceptShortFlags(tt.args)

			if action != tt.wantAction {
				t.Errorf("interceptShortFlags(%v) action = %q, want %q", tt.args, action, tt.wantAction)
			}

			if tt.wantReturnArgs {
				if !reflect.DeepEqual(remaining, tt.args) {
					t.Errorf("interceptShortFlags(%v) remaining = %v, want original args %v", tt.args, remaining, tt.args)
				}
			} else {
				if remaining != nil {
					t.Errorf("interceptShortFlags(%v) remaining = %v, want nil", tt.args, remaining)
				}
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
