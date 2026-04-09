package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
)

// --- Test helpers ---

// stubTrackerRegistry implements the TrackerRegistry interface in
// PreflightParams with configurable Get and Meta behavior.
type stubTrackerRegistry struct {
	getFunc  func(string) (registry.TrackerConstructor, error)
	metaFunc func(string) registry.AdapterMeta
}

func (s *stubTrackerRegistry) Get(kind string) (registry.TrackerConstructor, error) {
	return s.getFunc(kind)
}

func (s *stubTrackerRegistry) Meta(kind string) registry.AdapterMeta {
	return s.metaFunc(kind)
}

// stubAgentRegistry implements the AgentRegistry interface in
// PreflightParams with configurable Get and Meta behavior.
type stubAgentRegistry struct {
	getFunc  func(string) (registry.AgentConstructor, error)
	metaFunc func(string) registry.AdapterMeta
}

func (s *stubAgentRegistry) Get(kind string) (registry.AgentConstructor, error) {
	return s.getFunc(kind)
}

func (s *stubAgentRegistry) Meta(kind string) registry.AdapterMeta {
	return s.metaFunc(kind)
}

// validPreflightParams returns a PreflightParams where all checks
// pass. Tests override individual fields to inject failures.
func validPreflightParams() PreflightParams {
	return PreflightParams{
		ReloadWorkflow: func() error { return nil },
		ConfigFunc: func() config.ServiceConfig {
			return config.ServiceConfig{
				Tracker: config.TrackerConfig{
					Kind:   "test-tracker",
					APIKey: "secret",
				},
				Agent: config.AgentConfig{
					Kind:    "test-agent",
					Command: "/usr/bin/agent",
				},
			}
		},
		TrackerRegistry: &stubTrackerRegistry{
			getFunc:  func(string) (registry.TrackerConstructor, error) { return nil, nil },
			metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
		},
		AgentRegistry: &stubAgentRegistry{
			getFunc:  func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
		},
	}
}

// hasCheck reports whether the result contains an error with the
// given check name.
func hasCheck(t *testing.T, result PreflightResult, check string) bool {
	t.Helper()
	for _, e := range result.Errors {
		if e.Check == check {
			return true
		}
	}
	return false
}

// requireCheck fails the test if the result does not contain an
// error with the given check name.
func requireCheck(t *testing.T, result PreflightResult, check string) {
	t.Helper()
	if !hasCheck(t, result, check) {
		t.Errorf("ValidateDispatchConfig() missing error check %q; got errors: %v", check, result.Errors)
	}
}

// requireNoCheck fails the test if the result contains an error
// with the given check name.
func requireNoCheck(t *testing.T, result PreflightResult, check string) {
	t.Helper()
	if hasCheck(t, result, check) {
		t.Errorf("ValidateDispatchConfig() has unexpected error check %q; got errors: %v", check, result.Errors)
	}
}

// hasWarnCheck reports whether the result contains a warning with the
// given check name.
func hasWarnCheck(t *testing.T, result PreflightResult, check string) bool {
	t.Helper()
	for _, w := range result.Warnings {
		if w.Check == check {
			return true
		}
	}
	return false
}

// requireWarnCheck fails the test if the result does not contain a
// warning with the given check name.
func requireWarnCheck(t *testing.T, result PreflightResult, check string) {
	t.Helper()
	if !hasWarnCheck(t, result, check) {
		t.Errorf("ValidateDispatchConfig() missing warning check %q; got warnings: %v", check, result.Warnings)
	}
}

// requireNoWarnCheck fails the test if the result contains a warning
// with the given check name.
func requireNoWarnCheck(t *testing.T, result PreflightResult, check string) {
	t.Helper()
	if hasWarnCheck(t, result, check) {
		t.Errorf("ValidateDispatchConfig() has unexpected warning check %q; got warnings: %v", check, result.Warnings)
	}
}

// --- Tests ---

func TestValidateDispatchConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		modify         func(*PreflightParams)
		wantOK         bool
		wantChecks     []string // error checks that MUST be present
		noChecks       []string // error checks that MUST NOT be present
		wantWarnChecks []string // warning checks that MUST be present
		noWarnChecks   []string // warning checks that MUST NOT be present
	}{
		{
			name:   "all valid",
			modify: func(_ *PreflightParams) {},
			wantOK: true,
		},
		{
			name: "workflow reload failure",
			modify: func(p *PreflightParams) {
				p.ReloadWorkflow = func() error { return errors.New("parse error") }
			},
			wantChecks: []string{"workflow_load"},
		},
		{
			name: "missing tracker.kind",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							APIKey: "secret",
						},
						Agent: config.AgentConfig{
							Kind:    "test-agent",
							Command: "/usr/bin/agent",
						},
					}
				}
			},
			wantChecks: []string{"tracker.kind"},
		},
		{
			name: "empty tracker.api_key",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind: "test-tracker",
						},
						Agent: config.AgentConfig{
							Kind:    "test-agent",
							Command: "/usr/bin/agent",
						},
					}
				}
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresAPIKey: true}
					},
				}
			},
			wantChecks: []string{"tracker.api_key"},
		},
		{
			name: "missing tracker.project when meta requires it",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind:   "test-tracker",
							APIKey: "secret",
						},
						Agent: config.AgentConfig{
							Kind:    "test-agent",
							Command: "/usr/bin/agent",
						},
					}
				}
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresProject: true}
					},
				}
			},
			wantChecks: []string{"tracker.project"},
		},
		{
			name: "tracker.project not required when meta says so",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind:   "test-tracker",
							APIKey: "secret",
						},
						Agent: config.AgentConfig{
							Kind:    "test-agent",
							Command: "/usr/bin/agent",
						},
					}
				}
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresProject: false}
					},
				}
			},
			wantOK:   true,
			noChecks: []string{"tracker.project"},
		},
		{
			name: "unregistered tracker kind",
			modify: func(p *PreflightParams) {
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(kind string) (registry.TrackerConstructor, error) {
						return nil, &registry.RegistryError{
							Dimension: "tracker",
							Kind:      kind,
							Available: []string{},
						}
					},
					metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
				}
			},
			wantChecks: []string{"tracker_adapter"},
		},
		{
			name: "missing agent.command when meta requires it",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind:   "test-tracker",
							APIKey: "secret",
						},
						Agent: config.AgentConfig{
							Kind: "test-agent",
						},
					}
				}
				p.AgentRegistry = &stubAgentRegistry{
					getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresCommand: true}
					},
				}
			},
			wantChecks: []string{"agent.command"},
		},
		{
			name: "agent.command not required when meta says so",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind:   "test-tracker",
							APIKey: "secret",
						},
						Agent: config.AgentConfig{
							Kind: "test-agent",
						},
					}
				}
				p.AgentRegistry = &stubAgentRegistry{
					getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresCommand: false}
					},
				}
			},
			wantOK:   true,
			noChecks: []string{"agent.command"},
		},
		{
			name: "missing agent.kind skips command and adapter checks",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind:   "test-tracker",
							APIKey: "secret",
						},
						Agent: config.AgentConfig{},
					}
				}
				p.AgentRegistry = &stubAgentRegistry{
					getFunc: func(kind string) (registry.AgentConstructor, error) {
						return nil, &registry.RegistryError{
							Dimension: "agent",
							Kind:      kind,
							Available: []string{},
						}
					},
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresCommand: true}
					},
				}
			},
			wantChecks: []string{"agent.kind"},
			noChecks:   []string{"agent.command", "agent_adapter"},
		},
		{
			name: "unregistered agent kind",
			modify: func(p *PreflightParams) {
				p.AgentRegistry = &stubAgentRegistry{
					getFunc: func(kind string) (registry.AgentConstructor, error) {
						return nil, &registry.RegistryError{
							Dimension: "agent",
							Kind:      kind,
							Available: []string{},
						}
					},
					metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
				}
			},
			wantChecks: []string{"agent_adapter"},
		},
		{
			name: "multiple config errors collected",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{}
				}
				p.AgentRegistry = &stubAgentRegistry{
					getFunc: func(kind string) (registry.AgentConstructor, error) {
						return nil, &registry.RegistryError{
							Dimension: "agent",
							Kind:      kind,
							Available: []string{},
						}
					},
					metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
				}
			},
			wantChecks: []string{"tracker.kind", "agent.kind"},
			noChecks:   []string{"workflow_load", "tracker.api_key"},
		},
		{
			name: "workflow reload fails skips config checks",
			modify: func(p *PreflightParams) {
				p.ReloadWorkflow = func() error { return errors.New("file missing") }
			},
			wantChecks: []string{"workflow_load"},
			noChecks:   []string{"tracker.kind", "tracker.api_key", "tracker_adapter", "agent_adapter", "agent.kind", "agent.command", "tracker.project"},
		},
		{
			name: "tracker.api_key not required when meta says so",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind: "test-tracker",
						},
						Agent: config.AgentConfig{
							Kind:    "test-agent",
							Command: "/usr/bin/agent",
						},
					}
				}
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresAPIKey: false}
					},
				}
			},
			wantOK:   true,
			noChecks: []string{"tracker.api_key"},
		},
		{
			name: "tracker.api_key required when meta says so",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind: "test-tracker",
						},
						Agent: config.AgentConfig{
							Kind:    "test-agent",
							Command: "/usr/bin/agent",
						},
					}
				}
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{RequiresAPIKey: true}
					},
				}
			},
			wantChecks: []string{"tracker.api_key"},
		},
		{
			name: "zero-value meta from plain Register means no project or command or api_key error",
			modify: func(p *PreflightParams) {
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							Kind: "test-tracker",
						},
						Agent: config.AgentConfig{
							Kind: "test-agent",
						},
					}
				}
				// Both registries return zero-value meta (simulating plain Register).
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc:  func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
				}
				p.AgentRegistry = &stubAgentRegistry{
					getFunc:  func(string) (registry.AgentConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
				}
			},
			wantOK:   true,
			noChecks: []string{"tracker.api_key", "tracker.project", "agent.command"},
		}, {
			name: "adapter validation errors routed to result.Errors",
			modify: func(p *PreflightParams) {
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{
							ValidateTrackerConfig: func(_ registry.TrackerConfigFields) []registry.ValidationDiag {
								return []registry.ValidationDiag{
									{Severity: "error", Check: "test.adapter.check", Message: "adapter error"},
								}
							},
						}
					},
				}
			},
			wantChecks:   []string{"test.adapter.check"},
			noWarnChecks: []string{"test.adapter.check"},
		},
		{
			name: "adapter validation warnings routed to result.Warnings",
			modify: func(p *PreflightParams) {
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{
							ValidateTrackerConfig: func(_ registry.TrackerConfigFields) []registry.ValidationDiag {
								return []registry.ValidationDiag{
									{Severity: "warning", Check: "test.adapter.warn", Message: "adapter warning"},
								}
							},
						}
					},
				}
			},
			wantOK:         true,
			wantWarnChecks: []string{"test.adapter.warn"},
			noChecks:       []string{"test.adapter.warn"},
		},
		{
			name: "nil ValidateTrackerConfig produces no adapter diagnostics",
			modify: func(p *PreflightParams) {
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) registry.AdapterMeta {
						return registry.AdapterMeta{} // ValidateTrackerConfig is nil
					},
				}
			},
			wantOK: true,
		}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			params := validPreflightParams()
			tt.modify(&params)

			result := ValidateDispatchConfig(params)

			if tt.wantOK && !result.OK() {
				t.Fatalf("ValidateDispatchConfig() OK = false, want true; errors: %v", result.Errors)
			}
			if !tt.wantOK && len(tt.wantChecks) > 0 && result.OK() {
				t.Fatalf("ValidateDispatchConfig() OK = true, want false with checks %v", tt.wantChecks)
			}

			for _, check := range tt.wantChecks {
				requireCheck(t, result, check)
			}
			for _, check := range tt.noChecks {
				requireNoCheck(t, result, check)
			}
			for _, check := range tt.wantWarnChecks {
				requireWarnCheck(t, result, check)
			}
			for _, check := range tt.noWarnChecks {
				requireNoWarnCheck(t, result, check)
			}
		})
	}
}

func TestPreflightResult_OK(t *testing.T) {
	t.Parallel()

	t.Run("zero value is OK", func(t *testing.T) {
		t.Parallel()

		var r PreflightResult
		if !r.OK() {
			t.Error("PreflightResult{}.OK() = false, want true")
		}
	})

	t.Run("with errors is not OK", func(t *testing.T) {
		t.Parallel()

		r := PreflightResult{Errors: []PreflightError{{Check: "x", Message: "bad"}}}
		if r.OK() {
			t.Error("PreflightResult{Errors: [...]}.OK() = true, want false")
		}
	})
}

func TestPreflightResult_Error(t *testing.T) {
	t.Parallel()

	t.Run("empty when OK", func(t *testing.T) {
		t.Parallel()

		r := PreflightResult{}
		if got := r.Error(); got != "" {
			t.Errorf("PreflightResult{}.Error() = %q, want %q", got, "")
		}
	})

	t.Run("single error", func(t *testing.T) {
		t.Parallel()

		r := PreflightResult{Errors: []PreflightError{
			{Check: "tracker.kind", Message: "tracker.kind is required"},
		}}
		got := r.Error()
		want := "dispatch preflight failed: tracker.kind is required"
		if got != want {
			t.Errorf("PreflightResult.Error() = %q, want %q", got, want)
		}
	})

	t.Run("multiple errors joined with semicolon", func(t *testing.T) {
		t.Parallel()

		r := PreflightResult{Errors: []PreflightError{
			{Check: "a", Message: "error one"},
			{Check: "b", Message: "error two"},
		}}
		got := r.Error()
		if !strings.HasPrefix(got, "dispatch preflight failed: ") {
			t.Errorf("PreflightResult.Error() = %q, want prefix %q", got, "dispatch preflight failed: ")
		}
		if !strings.Contains(got, "; ") {
			t.Errorf("PreflightResult.Error() = %q, want semicolon separator", got)
		}
		if !strings.Contains(got, "error one") || !strings.Contains(got, "error two") {
			t.Errorf("PreflightResult.Error() = %q, want both messages present", got)
		}
	})
}

func TestValidateConfigForPromotion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		active   []string
		terminal []string
		wantErr  bool
	}{
		{
			name:     "both empty returns error",
			active:   nil,
			terminal: nil,
			wantErr:  true,
		},
		{
			name:     "active non-empty terminal empty returns nil",
			active:   []string{"To Do"},
			terminal: nil,
			wantErr:  false,
		},
		{
			name:     "active empty terminal non-empty returns nil",
			active:   nil,
			terminal: []string{"Done"},
			wantErr:  false,
		},
		{
			name:     "both non-empty returns nil",
			active:   []string{"To Do"},
			terminal: []string{"Done"},
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.ServiceConfig{
				Tracker: config.TrackerConfig{
					ActiveStates:   tt.active,
					TerminalStates: tt.terminal,
				},
			}

			err := ValidateConfigForPromotion(cfg)

			if tt.wantErr {
				if err == nil {
					t.Fatal("ValidateConfigForPromotion() = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateConfigForPromotion() unexpected error: %v", err)
			}
		})
	}
}

func TestValidateDispatchConfig_WorkspaceRootWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce POSIX file permission bits")
	}
	t.Parallel()

	// serviceConfigWithRoot returns a valid ServiceConfig with the given workspace root.
	serviceConfigWithRoot := func(root string) config.ServiceConfig {
		return config.ServiceConfig{
			Tracker: config.TrackerConfig{
				Kind:   "test-tracker",
				APIKey: "secret",
			},
			Agent: config.AgentConfig{
				Kind:    "test-agent",
				Command: "/usr/bin/agent",
			},
			Workspace: config.WorkspaceConfig{
				Root: root,
			},
		}
	}

	tests := []struct {
		name          string
		setup         func(t *testing.T, p *PreflightParams)
		wantOK        bool
		wantChecks    []string
		noChecks      []string
		checkMessages map[string]string // check name → expected substring in Message
	}{
		{
			name: "writable directory",
			setup: func(t *testing.T, p *PreflightParams) {
				t.Helper()
				root := t.TempDir()
				p.ConfigFunc = func() config.ServiceConfig {
					return serviceConfigWithRoot(root)
				}
			},
			wantOK:   true,
			noChecks: []string{"workspace.root_writable"},
		},
		{
			name: "read-only directory",
			setup: func(t *testing.T, p *PreflightParams) {
				t.Helper()
				if os.Getuid() == 0 {
					t.Skip("read-only directory test requires non-root user")
				}
				root := t.TempDir()
				t.Cleanup(func() { _ = os.Chmod(root, 0o750) })
				if err := os.Chmod(root, 0o555); err != nil {
					t.Fatalf("chmod: %v", err)
				}
				p.ConfigFunc = func() config.ServiceConfig {
					return serviceConfigWithRoot(root)
				}
			},
			wantOK:        false,
			wantChecks:    []string{"workspace.root_writable"},
			checkMessages: map[string]string{"workspace.root_writable": "permission denied"},
		},
		{
			name: "non-existent parent writable",
			setup: func(t *testing.T, p *PreflightParams) {
				t.Helper()
				root := filepath.Join(t.TempDir(), "sub", "deep")
				p.ConfigFunc = func() config.ServiceConfig {
					return serviceConfigWithRoot(root)
				}
			},
			wantOK:   true,
			noChecks: []string{"workspace.root_writable"},
		},
		{
			name: "non-existent parent read-only",
			setup: func(t *testing.T, p *PreflightParams) {
				t.Helper()
				if os.Getuid() == 0 {
					t.Skip("read-only directory test requires non-root user")
				}
				parent := t.TempDir()
				t.Cleanup(func() { _ = os.Chmod(parent, 0o750) })
				if err := os.Chmod(parent, 0o555); err != nil {
					t.Fatalf("chmod: %v", err)
				}
				p.ConfigFunc = func() config.ServiceConfig {
					return serviceConfigWithRoot(filepath.Join(parent, "sub"))
				}
			},
			wantOK:        false,
			wantChecks:    []string{"workspace.root_writable"},
			checkMessages: map[string]string{"workspace.root_writable": "permission denied"},
		},
		{
			name: "root is symlink",
			setup: func(t *testing.T, p *PreflightParams) {
				t.Helper()
				realDir := t.TempDir()
				symlinkPath := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(realDir, symlinkPath); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				p.ConfigFunc = func() config.ServiceConfig {
					return serviceConfigWithRoot(symlinkPath)
				}
			},
			wantOK:   true,
			noChecks: []string{"workspace.root_writable"},
		},
		{
			name: "empty root skipped",
			setup: func(t *testing.T, p *PreflightParams) {
				t.Helper()
				p.ConfigFunc = func() config.ServiceConfig {
					return serviceConfigWithRoot("")
				}
			},
			wantOK:   true,
			noChecks: []string{"workspace.root_writable"},
		},
		{
			// Errors are collected, not short-circuited.
			name: "collected with other errors",
			setup: func(t *testing.T, p *PreflightParams) {
				t.Helper()
				if os.Getuid() == 0 {
					t.Skip("read-only directory test requires non-root user")
				}
				root := t.TempDir()
				t.Cleanup(func() { _ = os.Chmod(root, 0o750) })
				if err := os.Chmod(root, 0o555); err != nil {
					t.Fatalf("chmod: %v", err)
				}
				p.ConfigFunc = func() config.ServiceConfig {
					return config.ServiceConfig{
						Tracker: config.TrackerConfig{
							APIKey: "secret",
							// Kind intentionally empty to trigger tracker.kind error.
						},
						Agent: config.AgentConfig{
							Kind:    "test-agent",
							Command: "/usr/bin/agent",
						},
						Workspace: config.WorkspaceConfig{
							Root: root,
						},
					}
				}
			},
			wantOK:     false,
			wantChecks: []string{"tracker.kind", "workspace.root_writable"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			params := validPreflightParams()
			tt.setup(t, &params)

			result := ValidateDispatchConfig(params)

			if tt.wantOK && !result.OK() {
				t.Fatalf("ValidateDispatchConfig() OK = false, want true; errors: %v", result.Errors)
			}
			if !tt.wantOK && len(tt.wantChecks) > 0 && result.OK() {
				t.Fatalf("ValidateDispatchConfig() OK = true, want false with checks %v", tt.wantChecks)
			}
			for _, check := range tt.wantChecks {
				requireCheck(t, result, check)
			}
			for _, check := range tt.noChecks {
				requireNoCheck(t, result, check)
			}
			for check, want := range tt.checkMessages {
				for _, e := range result.Errors {
					if e.Check == check && !strings.Contains(e.Message, want) {
						t.Errorf("error check %q message = %q, want to contain %q", check, e.Message, want)
					}
				}
			}
		})
	}
}
