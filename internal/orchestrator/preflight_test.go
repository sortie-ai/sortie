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
	metaFunc func(string) (registry.TrackerMeta, bool)
}

func (s *stubTrackerRegistry) Get(kind string) (registry.TrackerConstructor, error) {
	return s.getFunc(kind)
}

func (s *stubTrackerRegistry) Meta(kind string) (registry.TrackerMeta, bool) {
	return s.metaFunc(kind)
}

// stubAgentRegistry implements the AgentRegistry interface in
// PreflightParams with configurable Get and Meta behavior.
type stubAgentRegistry struct {
	getFunc  func(string) (registry.AgentConstructor, error)
	metaFunc func(string) (registry.AgentMeta, bool)
}

func (s *stubAgentRegistry) Get(kind string) (registry.AgentConstructor, error) {
	return s.getFunc(kind)
}

func (s *stubAgentRegistry) Meta(kind string) (registry.AgentMeta, bool) {
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
			metaFunc: func(string) (registry.TrackerMeta, bool) { return registry.TrackerMeta{}, true },
		},
		AgentRegistry: &stubAgentRegistry{
			getFunc:  func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(string) (registry.AgentMeta, bool) { return registry.AgentMeta{}, true },
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

// stateCollisionErrors returns the result's errors whose check is one of
// the two state-collision keys, in the order the preflight emitted them.
// Filtering keeps the assertion independent of the credential and adapter
// diagnostics the same run may also report.
func stateCollisionErrors(t *testing.T, result PreflightResult) []PreflightError {
	t.Helper()
	var got []PreflightError
	for _, e := range result.Errors {
		if e.Check == "tracker.handoff_state" || e.Check == "tracker.in_progress_state" {
			got = append(got, e)
		}
	}
	return got
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
					metaFunc: func(string) (registry.TrackerMeta, bool) {
						return registry.TrackerMeta{RequiresAPIKey: true}, true
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
					metaFunc: func(string) (registry.TrackerMeta, bool) {
						return registry.TrackerMeta{RequiresProject: true}, true
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
					metaFunc: func(string) (registry.TrackerMeta, bool) {
						return registry.TrackerMeta{RequiresProject: false}, true
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
					metaFunc: func(string) (registry.TrackerMeta, bool) { return registry.TrackerMeta{}, true },
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
					metaFunc: func(string) (registry.AgentMeta, bool) {
						return registry.AgentMeta{RequiresCommand: true}, true
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
					metaFunc: func(string) (registry.AgentMeta, bool) {
						return registry.AgentMeta{RequiresCommand: false}, true
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
					metaFunc: func(string) (registry.AgentMeta, bool) {
						return registry.AgentMeta{RequiresCommand: true}, true
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
					metaFunc: func(string) (registry.AgentMeta, bool) { return registry.AgentMeta{}, true },
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
					metaFunc: func(string) (registry.AgentMeta, bool) { return registry.AgentMeta{}, true },
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
					metaFunc: func(string) (registry.TrackerMeta, bool) {
						return registry.TrackerMeta{RequiresAPIKey: false}, true
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
					metaFunc: func(string) (registry.TrackerMeta, bool) {
						return registry.TrackerMeta{RequiresAPIKey: true}, true
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
					metaFunc: func(string) (registry.TrackerMeta, bool) { return registry.TrackerMeta{}, true },
				}
				p.AgentRegistry = &stubAgentRegistry{
					getFunc:  func(string) (registry.AgentConstructor, error) { return nil, nil },
					metaFunc: func(string) (registry.AgentMeta, bool) { return registry.AgentMeta{}, true },
				}
			},
			wantOK:   true,
			noChecks: []string{"tracker.api_key", "tracker.project", "agent.command"},
		}, {
			name: "adapter validation errors routed to result.Errors",
			modify: func(p *PreflightParams) {
				p.TrackerRegistry = &stubTrackerRegistry{
					getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
					metaFunc: func(string) (registry.TrackerMeta, bool) {
						return registry.TrackerMeta{
							ValidateTrackerConfig: func(_ registry.TrackerConfigFields) []registry.ValidationDiag {
								return []registry.ValidationDiag{
									{Severity: "error", Check: "test.adapter.check", Message: "adapter error"},
								}
							},
						}, true
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
					metaFunc: func(string) (registry.TrackerMeta, bool) {
						return registry.TrackerMeta{
							ValidateTrackerConfig: func(_ registry.TrackerConfigFields) []registry.ValidationDiag {
								return []registry.ValidationDiag{
									{Severity: "warning", Check: "test.adapter.warn", Message: "adapter warning"},
								}
							},
						}, true
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
					metaFunc: func(string) (registry.TrackerMeta, bool) {
						return registry.TrackerMeta{}, true // ValidateTrackerConfig is nil
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

// TestValidateDispatchConfig_QueryFilterPropagation covers the one
// generic-layer plumbing addition: registry.TrackerConfigFields.QueryFilter
// must reach the tracker hook populated from cfg.Tracker.QueryFilter, and a
// validator that never reads it must see no change in its diagnostics.
func TestValidateDispatchConfig_QueryFilterPropagation(t *testing.T) {
	t.Parallel()

	t.Run("hook receives the configured QueryFilter", func(t *testing.T) {
		t.Parallel()

		var captured registry.TrackerConfigFields
		params := validPreflightParams()
		params.ConfigFunc = func() config.ServiceConfig {
			return config.ServiceConfig{
				Tracker: config.TrackerConfig{
					Kind:        "test-tracker",
					APIKey:      "secret",
					QueryFilter: "scope=assigned_to_me",
				},
				Agent: config.AgentConfig{
					Kind:    "test-agent",
					Command: "/usr/bin/agent",
				},
			}
		}
		params.TrackerRegistry = &stubTrackerRegistry{
			getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
			metaFunc: func(string) (registry.TrackerMeta, bool) {
				return registry.TrackerMeta{
					ValidateTrackerConfig: func(fields registry.TrackerConfigFields) []registry.ValidationDiag {
						captured = fields
						return nil
					},
				}, true
			},
		}

		result := ValidateDispatchConfig(params)

		if !result.OK() {
			t.Fatalf("ValidateDispatchConfig() OK = false, want true; errors: %v", result.Errors)
		}
		if captured.QueryFilter != "scope=assigned_to_me" {
			t.Errorf("ValidateDispatchConfig() captured fields.QueryFilter = %q, want %q",
				captured.QueryFilter, "scope=assigned_to_me")
		}
	})

	t.Run("a validator that ignores QueryFilter emits identical diagnostics regardless of its value", func(t *testing.T) {
		t.Parallel()

		validate := func(_ registry.TrackerConfigFields) []registry.ValidationDiag { return nil }

		for _, queryFilter := range []string{"", "scope=assigned_to_me"} {
			params := validPreflightParams()
			params.ConfigFunc = func() config.ServiceConfig {
				return config.ServiceConfig{
					Tracker: config.TrackerConfig{
						Kind:        "test-tracker",
						APIKey:      "secret",
						QueryFilter: queryFilter,
					},
					Agent: config.AgentConfig{
						Kind:    "test-agent",
						Command: "/usr/bin/agent",
					},
				}
			}
			params.TrackerRegistry = &stubTrackerRegistry{
				getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
				metaFunc: func(string) (registry.TrackerMeta, bool) {
					return registry.TrackerMeta{ValidateTrackerConfig: validate}, true
				},
			}

			result := ValidateDispatchConfig(params)

			if !result.OK() || len(result.Warnings) != 0 {
				t.Errorf("ValidateDispatchConfig() with QueryFilter=%q = OK:%v warnings:%v, want OK with no diagnostics",
					queryFilter, result.OK(), result.Warnings)
			}
		}
	})
}

// TestValidateDispatchConfig_APIVersionPropagation mirrors
// TestValidateDispatchConfig_QueryFilterPropagation for the other
// generic-layer plumbing addition: registry.TrackerConfigFields.APIVersion
// must reach the tracker hook populated from cfg.Tracker.APIVersion, and a
// validator that never reads it must see no change in its diagnostics.
func TestValidateDispatchConfig_APIVersionPropagation(t *testing.T) {
	t.Parallel()

	t.Run("hook receives the configured APIVersion", func(t *testing.T) {
		t.Parallel()

		var captured registry.TrackerConfigFields
		params := validPreflightParams()
		params.ConfigFunc = func() config.ServiceConfig {
			return config.ServiceConfig{
				Tracker: config.TrackerConfig{
					Kind:       "test-tracker",
					APIKey:     "secret",
					APIVersion: "2",
				},
				Agent: config.AgentConfig{
					Kind:    "test-agent",
					Command: "/usr/bin/agent",
				},
			}
		}
		params.TrackerRegistry = &stubTrackerRegistry{
			getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
			metaFunc: func(string) (registry.TrackerMeta, bool) {
				return registry.TrackerMeta{
					ValidateTrackerConfig: func(fields registry.TrackerConfigFields) []registry.ValidationDiag {
						captured = fields
						return nil
					},
				}, true
			},
		}

		result := ValidateDispatchConfig(params)

		if !result.OK() {
			t.Fatalf("ValidateDispatchConfig() OK = false, want true; errors: %v", result.Errors)
		}
		if captured.APIVersion != "2" {
			t.Errorf("ValidateDispatchConfig() captured fields.APIVersion = %q, want %q",
				captured.APIVersion, "2")
		}
	})

	t.Run("a validator that ignores APIVersion emits identical diagnostics regardless of its value", func(t *testing.T) {
		t.Parallel()

		validate := func(_ registry.TrackerConfigFields) []registry.ValidationDiag { return nil }

		for _, apiVersion := range []string{"", "2"} {
			params := validPreflightParams()
			params.ConfigFunc = func() config.ServiceConfig {
				return config.ServiceConfig{
					Tracker: config.TrackerConfig{
						Kind:       "test-tracker",
						APIKey:     "secret",
						APIVersion: apiVersion,
					},
					Agent: config.AgentConfig{
						Kind:    "test-agent",
						Command: "/usr/bin/agent",
					},
				}
			}
			params.TrackerRegistry = &stubTrackerRegistry{
				getFunc: func(string) (registry.TrackerConstructor, error) { return nil, nil },
				metaFunc: func(string) (registry.TrackerMeta, bool) {
					return registry.TrackerMeta{ValidateTrackerConfig: validate}, true
				},
			}

			result := ValidateDispatchConfig(params)

			if !result.OK() || len(result.Warnings) != 0 {
				t.Errorf("ValidateDispatchConfig() with APIVersion=%q = OK:%v warnings:%v, want OK with no diagnostics",
					apiVersion, result.OK(), result.Warnings)
			}
		}
	})
}

// TestValidateDispatchConfig_AgentConfigValidation covers the agent-side
// validation channel: a kind reached only through a dispatch rule, the
// documented deterministic emission order across the default and
// rule-referenced kinds, a registered-but-unreferenced kind drawing no
// diagnostic, and the severity-to-diagnostic-kind mapping.
func TestValidateDispatchConfig_AgentConfigValidation(t *testing.T) {
	t.Parallel()

	t.Run("a kind reached only through a dispatch rule draws the same verdict the default kind would", func(t *testing.T) {
		t.Parallel()

		params := validPreflightParams()
		params.ConfigFunc = func() config.ServiceConfig {
			return config.ServiceConfig{
				Tracker: config.TrackerConfig{Kind: "test-tracker", APIKey: "secret"},
				Agent:   config.AgentConfig{Kind: "test-agent", Command: "/usr/bin/agent"},
				Dispatch: config.DispatchConfig{
					Rules: []config.DispatchRule{
						{Selection: config.DispatchSelection{AgentKind: "rule-agent"}},
					},
				},
			}
		}
		params.AgentRegistry = &stubAgentRegistry{
			getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(kind string) (registry.AgentMeta, bool) {
				if kind == "rule-agent" {
					return registry.AgentMeta{ValidateAgentConfig: func(fields registry.AgentConfigFields) []registry.ValidationDiag {
						return []registry.ValidationDiag{{Severity: "error", Check: "rule.check", Message: "bad rule kind " + fields.Kind}}
					}}, true
				}
				return registry.AgentMeta{}, true
			},
		}

		result := ValidateDispatchConfig(params)

		requireCheck(t, result, "rule.check")
	})

	t.Run("diagnostics are emitted in the documented deterministic order", func(t *testing.T) {
		t.Parallel()

		params := validPreflightParams()
		params.ConfigFunc = func() config.ServiceConfig {
			return config.ServiceConfig{
				Tracker: config.TrackerConfig{Kind: "test-tracker", APIKey: "secret"},
				Agent:   config.AgentConfig{Kind: "kind-a", Command: "/usr/bin/agent"},
				Dispatch: config.DispatchConfig{
					Default: config.DispatchSelection{AgentKind: "kind-b"},
					Rules: []config.DispatchRule{
						{Selection: config.DispatchSelection{AgentKind: "kind-c"}},
						{Selection: config.DispatchSelection{AgentKind: "kind-a"}},
					},
				},
			}
		}
		params.AgentRegistry = &stubAgentRegistry{
			getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(string) (registry.AgentMeta, bool) {
				return registry.AgentMeta{ValidateAgentConfig: func(fields registry.AgentConfigFields) []registry.ValidationDiag {
					return []registry.ValidationDiag{{Severity: "error", Check: fields.Kind + ".check", Message: fields.Kind}}
				}}, true
			},
		}

		result := ValidateDispatchConfig(params)

		var gotChecks []string
		for _, e := range result.Errors {
			if strings.HasSuffix(e.Check, ".check") {
				gotChecks = append(gotChecks, e.Check)
			}
		}
		want := []string{"kind-a.check", "kind-b.check", "kind-c.check"}
		if len(gotChecks) != len(want) {
			t.Fatalf("ValidateDispatchConfig() agent checks = %v, want exactly %v (default kind first, then rule kinds in rule order, deduplicated)", gotChecks, want)
		}
		for i, wantCheck := range want {
			if gotChecks[i] != wantCheck {
				t.Errorf("ValidateDispatchConfig() checks[%d] = %q, want %q", i, gotChecks[i], wantCheck)
			}
		}
	})

	t.Run("a registered kind the configuration never references draws no diagnostic", func(t *testing.T) {
		t.Parallel()

		params := validPreflightParams()
		params.AgentRegistry = &stubAgentRegistry{
			getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(kind string) (registry.AgentMeta, bool) {
				if kind == "unused-kind" {
					return registry.AgentMeta{ValidateAgentConfig: func(_ registry.AgentConfigFields) []registry.ValidationDiag {
						return []registry.ValidationDiag{{Severity: "error", Check: "unused.check", Message: "should never fire"}}
					}}, true
				}
				return registry.AgentMeta{}, true
			},
		}

		result := ValidateDispatchConfig(params)

		requireNoCheck(t, result, "unused.check")
	})

	t.Run("warning severity maps to PreflightWarning, anything else maps to PreflightError", func(t *testing.T) {
		t.Parallel()

		params := validPreflightParams()
		params.AgentRegistry = &stubAgentRegistry{
			getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(string) (registry.AgentMeta, bool) {
				return registry.AgentMeta{ValidateAgentConfig: func(_ registry.AgentConfigFields) []registry.ValidationDiag {
					return []registry.ValidationDiag{
						{Severity: "warning", Check: "agent.warn", Message: "advisory"},
						{Severity: "error", Check: "agent.err", Message: "fatal"},
						{Severity: "", Check: "agent.other", Message: "anything else maps to an error"},
					}
				}}, true
			},
		}

		result := ValidateDispatchConfig(params)

		requireWarnCheck(t, result, "agent.warn")
		requireCheck(t, result, "agent.err")
		requireCheck(t, result, "agent.other")
		requireNoWarnCheck(t, result, "agent.err")
		requireNoWarnCheck(t, result, "agent.other")
	})
}

// TestValidateDispatchConfig_MCPConfigWarning covers the five states an
// mcp_config value in an agent kind's block can draw the "agent.mcp_config"
// warning from: a non-injecting kind with the value set, a non-injecting
// kind without it, an injecting kind with it, a kind the registry reports
// unregistered, and a registered non-injecting kind whose adapter supplies
// no config validator. The last state is mock's own shape and is the one
// that reddens if the check is folded behind ValidateDispatchConfig's
// existing "no validator" early continue, since that continue would skip
// the warning for exactly the kind most likely to carry mcp_config in
// practice.
func TestValidateDispatchConfig_MCPConfigWarning(t *testing.T) {
	t.Parallel()

	t.Run("a non-injecting kind with mcp_config set draws one warning and no error", func(t *testing.T) {
		t.Parallel()

		params := validPreflightParams()
		params.ConfigFunc = func() config.ServiceConfig {
			cfg := config.ServiceConfig{
				Tracker: config.TrackerConfig{Kind: "test-tracker", APIKey: "secret"},
				Agent:   config.AgentConfig{Kind: "test-agent", Command: "/usr/bin/agent"},
			}
			cfg.SetExtensionSection("test-agent", map[string]any{"mcp_config": "/ws/.sortie/mcp.json"})
			return cfg
		}
		params.AgentRegistry = &stubAgentRegistry{
			getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(string) (registry.AgentMeta, bool) {
				return registry.AgentMeta{MCPInjection: registry.MCPInjectionUnsupported}, true
			},
		}

		result := ValidateDispatchConfig(params)

		requireWarnCheck(t, result, "agent.mcp_config")
		requireNoCheck(t, result, "agent.mcp_config")
		if !result.OK() {
			t.Errorf("ValidateDispatchConfig().OK() = false, want true: a warning must not block dispatch")
		}

		// Exactly one warning per affected kind, and it names the kind,
		// so an operator reading it knows which block to edit.
		var got []PreflightWarning
		for _, w := range result.Warnings {
			if w.Check == "agent.mcp_config" {
				got = append(got, w)
			}
		}
		if len(got) != 1 {
			t.Fatalf("ValidateDispatchConfig() produced %d agent.mcp_config warnings, want 1: %v", len(got), got)
		}
		if !strings.Contains(got[0].Message, "test-agent") {
			t.Errorf("warning message = %q, want it to name the agent kind %q", got[0].Message, "test-agent")
		}
	})

	t.Run("a non-injecting kind without mcp_config draws no warning", func(t *testing.T) {
		t.Parallel()

		params := validPreflightParams()
		params.AgentRegistry = &stubAgentRegistry{
			getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(string) (registry.AgentMeta, bool) {
				return registry.AgentMeta{MCPInjection: registry.MCPInjectionUnsupported}, true
			},
		}

		result := ValidateDispatchConfig(params)

		requireNoWarnCheck(t, result, "agent.mcp_config")
	})

	t.Run("an injecting kind with mcp_config draws no warning", func(t *testing.T) {
		t.Parallel()

		params := validPreflightParams()
		params.ConfigFunc = func() config.ServiceConfig {
			cfg := config.ServiceConfig{
				Tracker: config.TrackerConfig{Kind: "test-tracker", APIKey: "secret"},
				Agent:   config.AgentConfig{Kind: "test-agent", Command: "/usr/bin/agent"},
			}
			cfg.SetExtensionSection("test-agent", map[string]any{"mcp_config": "/ws/.sortie/mcp.json"})
			return cfg
		}
		params.AgentRegistry = &stubAgentRegistry{
			getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(string) (registry.AgentMeta, bool) {
				return registry.AgentMeta{MCPInjection: registry.MCPInjectionSupported}, true
			},
		}

		result := ValidateDispatchConfig(params)

		requireNoWarnCheck(t, result, "agent.mcp_config")
	})

	t.Run("a kind reached only through a dispatch rule that the registry reports unregistered draws no warning", func(t *testing.T) {
		t.Parallel()

		params := validPreflightParams()
		params.ConfigFunc = func() config.ServiceConfig {
			cfg := config.ServiceConfig{
				Tracker: config.TrackerConfig{Kind: "test-tracker", APIKey: "secret"},
				Agent:   config.AgentConfig{Kind: "test-agent", Command: "/usr/bin/agent"},
				Dispatch: config.DispatchConfig{
					Rules: []config.DispatchRule{
						{Selection: config.DispatchSelection{AgentKind: "rule-agent"}},
					},
				},
			}
			cfg.SetExtensionSection("rule-agent", map[string]any{"mcp_config": "/ws/.sortie/mcp.json"})
			return cfg
		}
		params.AgentRegistry = &stubAgentRegistry{
			getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(kind string) (registry.AgentMeta, bool) {
				if kind == "rule-agent" {
					return registry.AgentMeta{}, false
				}
				return registry.AgentMeta{MCPInjection: registry.MCPInjectionUnsupported}, true
			},
		}

		result := ValidateDispatchConfig(params)

		requireNoWarnCheck(t, result, "agent.mcp_config")
	})

	t.Run("mock's shape: registered, no config validator, block sets mcp_config draws one warning", func(t *testing.T) {
		t.Parallel()

		params := validPreflightParams()
		params.ConfigFunc = func() config.ServiceConfig {
			cfg := config.ServiceConfig{
				Tracker: config.TrackerConfig{Kind: "test-tracker", APIKey: "secret"},
				Agent:   config.AgentConfig{Kind: "test-agent", Command: "/usr/bin/agent"},
				Dispatch: config.DispatchConfig{
					Rules: []config.DispatchRule{
						{Selection: config.DispatchSelection{AgentKind: "mock"}},
					},
				},
			}
			cfg.SetExtensionSection("mock", map[string]any{"mcp_config": "/ws/.sortie/mcp.json"})
			return cfg
		}
		params.AgentRegistry = &stubAgentRegistry{
			getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(kind string) (registry.AgentMeta, bool) {
				if kind == "mock" {
					return registry.AgentMeta{MCPInjection: registry.MCPInjectionUnsupported}, true
				}
				return registry.AgentMeta{}, true
			},
		}

		result := ValidateDispatchConfig(params)

		requireWarnCheck(t, result, "agent.mcp_config")
		requireNoCheck(t, result, "agent.mcp_config")
	})
}

// TestValidateDispatchConfig_DefaultedTrackerStates covers the collision
// rules re-run against the effective state lists, the ones a tracker
// adapter fills from its own fallback when the workflow list is empty.
// The default lists are declared here rather than read from a real
// registration: this package links no adapter package, and the stub
// registry is what makes every shape reachable, including the two-list
// one the workflow promotion gate rejects before the preflight runs.
func TestValidateDispatchConfig_DefaultedTrackerStates(t *testing.T) {
	t.Parallel()

	githubActive := []string{"backlog", "in-progress", "review"}
	githubTerminal := []string{"done", "wontfix"}

	tests := []struct {
		name         string
		tracker      config.TrackerConfig
		meta         registry.TrackerMeta
		unregistered bool
		want         []PreflightError
	}{
		{
			name: "handoff state collides with the defaulted active list",
			tracker: config.TrackerConfig{
				Kind:           "github",
				HandoffState:   "review",
				TerminalStates: []string{"done"},
			},
			meta: registry.TrackerMeta{
				DefaultActiveStates:   githubActive,
				DefaultTerminalStates: githubTerminal,
			},
			want: []PreflightError{{
				Check:   "tracker.handoff_state",
				Message: `"review" collides with active state "review"; tracker.active_states is empty, so the "github" adapter falls back to its own active states`,
			}},
		},
		{
			name: "handoff state collides with the defaulted terminal list",
			tracker: config.TrackerConfig{
				Kind:         "github",
				HandoffState: "done",
				ActiveStates: []string{"backlog"},
			},
			meta: registry.TrackerMeta{
				DefaultActiveStates:   githubActive,
				DefaultTerminalStates: githubTerminal,
			},
			want: []PreflightError{{
				Check:   "tracker.handoff_state",
				Message: `"done" collides with terminal state "done"; tracker.terminal_states is empty, so the "github" adapter falls back to its own terminal states`,
			}},
		},
		{
			name: "in progress state collides with the defaulted terminal list",
			tracker: config.TrackerConfig{
				Kind:            "github",
				InProgressState: "done",
				ActiveStates:    []string{"done", "backlog"},
			},
			meta: registry.TrackerMeta{
				DefaultActiveStates:   githubActive,
				DefaultTerminalStates: githubTerminal,
			},
			want: []PreflightError{{
				Check:   "tracker.in_progress_state",
				Message: `"done" collides with terminal state "done"; tracker.terminal_states is empty, so the "github" adapter falls back to its own terminal states`,
			}},
		},
		{
			name: "comparison is case-insensitive and each side keeps its own casing",
			tracker: config.TrackerConfig{
				Kind:           "github",
				HandoffState:   "REVIEW",
				TerminalStates: []string{"done"},
			},
			meta: registry.TrackerMeta{
				DefaultActiveStates:   githubActive,
				DefaultTerminalStates: githubTerminal,
			},
			want: []PreflightError{{
				Check:   "tracker.handoff_state",
				Message: `"REVIEW" collides with active state "review"; tracker.active_states is empty, so the "github" adapter falls back to its own active states`,
			}},
		},
		{
			name: "an explicitly empty active list behaves like an absent key",
			tracker: config.TrackerConfig{
				Kind:           "github",
				HandoffState:   "review",
				ActiveStates:   []string{},
				TerminalStates: []string{"done"},
			},
			meta: registry.TrackerMeta{
				DefaultActiveStates:   githubActive,
				DefaultTerminalStates: githubTerminal,
			},
			want: []PreflightError{{
				Check:   "tracker.handoff_state",
				Message: `"review" collides with active state "review"; tracker.active_states is empty, so the "github" adapter falls back to its own active states`,
			}},
		},
		{
			name: "no diagnostic when the adapter declares no default",
			tracker: config.TrackerConfig{
				Kind:           "github",
				HandoffState:   "review",
				TerminalStates: []string{"done"},
			},
		},
		{
			name: "no diagnostic when the defaulted list does not collide",
			tracker: config.TrackerConfig{
				Kind:           "github",
				HandoffState:   "awaiting-human",
				TerminalStates: []string{"done"},
			},
			meta: registry.TrackerMeta{
				DefaultActiveStates:   githubActive,
				DefaultTerminalStates: githubTerminal,
			},
		},
		{
			name: "no diagnostic when both lists are written",
			tracker: config.TrackerConfig{
				Kind:           "github",
				HandoffState:   "review",
				ActiveStates:   []string{"backlog"},
				TerminalStates: []string{"done"},
			},
			meta: registry.TrackerMeta{
				DefaultActiveStates:   githubActive,
				DefaultTerminalStates: githubTerminal,
			},
		},
		{
			name: "no diagnostic when the written lists already violate a rule",
			tracker: config.TrackerConfig{
				Kind:         "github",
				HandoffState: "review",
				ActiveStates: []string{"review"},
			},
			meta: registry.TrackerMeta{
				DefaultActiveStates:   githubActive,
				DefaultTerminalStates: githubTerminal,
			},
		},
		{
			name: "both lists defaulted reports one diagnostic per list",
			tracker: config.TrackerConfig{
				Kind:         "github",
				HandoffState: "review",
			},
			meta: registry.TrackerMeta{
				DefaultActiveStates:   []string{"review"},
				DefaultTerminalStates: []string{"review", "done"},
			},
			want: []PreflightError{
				{
					Check:   "tracker.handoff_state",
					Message: `"review" collides with active state "review"; tracker.active_states is empty, so the "github" adapter falls back to its own active states`,
				},
				{
					Check:   "tracker.handoff_state",
					Message: `"review" collides with terminal state "review"; tracker.terminal_states is empty, so the "github" adapter falls back to its own terminal states`,
				},
			},
		},
		{
			name: "both lists defaulted with an in progress state reports nothing",
			tracker: config.TrackerConfig{
				Kind:            "github",
				HandoffState:    "review",
				InProgressState: "in-progress",
			},
			meta: registry.TrackerMeta{
				DefaultActiveStates:   []string{"review"},
				DefaultTerminalStates: []string{"review", "done"},
			},
		},
		{
			name: "no diagnostic for an unregistered kind",
			tracker: config.TrackerConfig{
				Kind:           "nonexistent",
				HandoffState:   "review",
				TerminalStates: []string{"done"},
			},
			unregistered: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			params := validPreflightParams()
			params.ConfigFunc = func() config.ServiceConfig {
				return config.ServiceConfig{
					Tracker: tt.tracker,
					Agent: config.AgentConfig{
						Kind:    "test-agent",
						Command: "/usr/bin/agent",
					},
				}
			}
			params.TrackerRegistry = &stubTrackerRegistry{
				getFunc: func(kind string) (registry.TrackerConstructor, error) {
					if tt.unregistered {
						return nil, &registry.RegistryError{Dimension: "tracker", Kind: kind}
					}
					return nil, nil
				},
				metaFunc: func(string) (registry.TrackerMeta, bool) {
					return tt.meta, !tt.unregistered
				},
			}

			result := ValidateDispatchConfig(params)

			got := stateCollisionErrors(t, result)
			if len(got) != len(tt.want) {
				t.Fatalf("ValidateDispatchConfig() state collision errors = %v, want %v", got, tt.want)
			}
			for i, want := range tt.want {
				if got[i].Check != want.Check {
					t.Errorf("ValidateDispatchConfig() errors[%d].Check = %q, want %q", i, got[i].Check, want.Check)
				}
				if got[i].Message != want.Message {
					t.Errorf("ValidateDispatchConfig() errors[%d].Message = %q, want %q", i, got[i].Message, want.Message)
				}
			}
			if len(tt.want) > 0 && result.OK() {
				t.Errorf("ValidateDispatchConfig() OK = true, want false: a state collision is error severity")
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
