package usagesource

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/registry"
)

func setTempRoot(t *testing.T, root string) {
	t.Helper()
	t.Setenv("TMPDIR", root)
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", root)
		t.Setenv("TEMP", root)
	}
}

func assertTempRootEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", root, err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		t.Errorf("temp root %q contains %v, want empty", root, names)
	}
}

func assertKiroAgentDefinition(t *testing.T, dir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "sortie_verify.json"))
	if err != nil {
		t.Fatalf("ReadFile(sortie_verify.json): %v", err)
	}
	var agent map[string]json.RawMessage
	if err := json.Unmarshal(data, &agent); err != nil {
		t.Fatalf("sortie_verify.json = %q, want a JSON object: %v", data, err)
	}
	if got := string(agent["tools"]); got != "[]" {
		t.Errorf("sortie_verify.json tools = %q, want %q", got, "[]")
	}
	if _, ok := agent["model"]; ok {
		t.Errorf("sortie_verify.json carries a model field, want none: the launch's own --model must stand")
	}
}

func TestEveryDeclarationPassesTheRegistrationCheck(t *testing.T) {
	t.Parallel()

	declarations := map[string]Declaration{DefaultName: Default}
	for _, kind := range Declarations.Kinds() {
		declaration, err := Declarations.Get(kind)
		if err != nil {
			t.Fatalf("Declarations.Get(%q) error = %v", kind, err)
		}
		declarations[kind] = declaration
	}

	for name, declaration := range declarations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if err := checkDeclaration(declaration, struct{}{}); err != nil {
				t.Errorf("checkDeclaration(%q) = %v, want nil", name, err)
			}
		})
	}
}

func TestDeclarationsRefuseWhatTheTransportCannotMeasure(t *testing.T) {
	t.Parallel()

	noPromptSpend := Default
	noPromptSpend.PromptSpend = nil
	nameOnly, mergeOnly := Default, Default
	unreadAmount := Default
	unreadAmount.SessionUpdateAmounts = nil
	readerWithoutAmount := Default
	readerWithoutAmount.Live.Spend.Amounts = nil
	nameOnly.LaunchAddition = LaunchAddition{Name: "ADDITION_VARIABLE"}
	mergeOnly.LaunchAddition = LaunchAddition{Merge: func(current string, _ bool) (string, bool) { return current, true }}
	tests := []struct {
		name        string
		key         string
		declaration Declaration
	}{
		{"zero live source", "refused-zero-live", Declaration{}},
		{"declared spend without a prompt reader", "refused-no-prompt-spend", noPromptSpend},
		{"the reserved default key", DefaultName, Default},
		{"a launch addition with a name and no merge", "refused-addition-name-only", nameOnly},
		{"a launch addition with a merge and no name", "refused-addition-merge-only", mergeOnly},
		{"a live amount with no reader", "refused-amount-no-reader", unreadAmount},
		{"an amount reader with no live amount", "refused-reader-no-amount", readerWithoutAmount},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			panicked := registerPanics(tt.key, tt.declaration)

			if !panicked {
				t.Errorf("Declarations.Register(%q) did not panic, want a refusal", tt.key)
			}
			if tt.key != DefaultName && Declarations.Has(tt.key) {
				t.Errorf("Declarations.Has(%q) = true, want a refused declaration stored nowhere", tt.key)
			}
		})
	}
}

func registerPanics(key string, declaration Declaration) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	Declarations.Register(key, declaration)
	return false
}

func TestEffectiveSuppliesTheDefaultMappingOnlyWhenNoneIsDeclared(t *testing.T) {
	t.Parallel()

	own := Default
	own.Live.Spend.LowerBound = false
	own.Live.Spend.CacheInInput = false
	own.Live.Spend.Amounts = nil
	own.SessionUpdateAmounts = nil
	amountsOnly := Declaration{Live: usage.Source{Category: usage.SourceProtocol, Strategy: usage.StrategyLive, Spend: usage.SpendDeclaration{Amounts: []usage.AmountDeclaration{{Unit: "credits", Shape: usage.AmountPerTurn}}}}}
	tests := []struct {
		name              string
		declaration       Declaration
		wantSpend         usage.SpendDeclaration
		wantPromptHas     bool
		wantAmountsReader bool
	}{
		{"windows only", Declaration{Live: usage.Source{Category: usage.SourceProtocol, Strategy: usage.StrategyLive}}, Default.Live.Spend, true, true},
		{"its own token mapping", own, own.Live.Spend, true, false},
		{"an amount alone", amountsOnly, amountsOnly.Live.Spend, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.declaration.Effective()

			if !reflect.DeepEqual(got.Live.Spend, tt.wantSpend) {
				t.Errorf("Effective().Live.Spend = %+v, want %+v", got.Live.Spend, tt.wantSpend)
			}
			if (got.PromptSpend != nil) != tt.wantPromptHas {
				t.Errorf("Effective().PromptSpend set = %v, want %v", got.PromptSpend != nil, tt.wantPromptHas)
			}
			if (got.SessionUpdateAmounts != nil) != tt.wantAmountsReader {
				t.Errorf("Effective().SessionUpdateAmounts set = %v, want %v", got.SessionUpdateAmounts != nil, tt.wantAmountsReader)
			}
		})
	}
}

func TestDeclarationLaunchesFollowTheConfiguredCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		kind   string
		target agentcore.LaunchTarget
		want   bool
	}{
		{"executable with its flag", copilotHandshakeName, agentcore.LaunchTarget{Command: "copilot", Args: []string{"--acp"}}, true},
		{"npm package through a launcher", geminiAgentName, agentcore.LaunchTarget{Command: "npx", Args: []string{"@google/gemini-cli@latest", "--acp"}}, true},
		{"wrapper command", copilotHandshakeName, agentcore.LaunchTarget{Command: "env", Args: []string{"MODEL=x", "copilot", "--acp"}}, true},
		{"npm package of the 2.x line through a launcher", opencodeHandshakeName, agentcore.LaunchTarget{Command: "npx", Args: []string{"-y", "@opencode/cli@2", "acp"}}, true},
		{"another runtime's command", copilotHandshakeName, agentcore.LaunchTarget{Command: "gemini", Args: []string{"--acp"}}, false},
		{"remote command named, ssh as the executable", kiroAgentName, agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: "kiro-cli acp --model x"}, true},
		{"remote command naming nothing", kiroAgentName, agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: "agent --acp"}, false},
		{"runtime named only in the ssh arguments", kiroAgentName, agentcore.LaunchTarget{Command: "ssh", Args: []string{"kiro-cli"}, SSHHost: "worker", RemoteCommand: "agent --acp"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			declaration, err := Declarations.Get(tt.kind)
			if err != nil {
				t.Fatalf("Declarations.Get(%q) error = %v", tt.kind, err)
			}

			if got := declaration.Launches(tt.target); got != tt.want {
				t.Errorf("%q Launches(%+v) = %v, want %v", tt.kind, tt.target, got, tt.want)
			}
		})
	}
}

var verificationExecutables = map[string]string{
	geminiAgentName:   geminiExecutableName,
	copilotAgentName:  copilotExecutableName,
	opencodeAgentName: opencodeExecutableName,
	kiroAgentName:     kiroExecutableName,
}

func verificationClaimArgs(kind string) []string {
	if kind == kiroAgentName {
		return []string{"acp", "--model", "scripted-model"}
	}
	return []string{"--acp"}
}

func TestVerificationAdditionsConformToTheRegistryContract(t *testing.T) {
	kinds := VerificationAdditions.Kinds()
	if len(kinds) == 0 {
		t.Fatal("VerificationAdditions.Kinds() = empty, want at least one registered addition")
	}

	for _, kind := range kinds {
		executable, ok := verificationExecutables[kind]
		if !ok {
			t.Errorf("verificationExecutables[%q] missing, want the executable that names this addition's runtime", kind)
			continue
		}
		root := t.TempDir()
		setTempRoot(t, root)

		constructor, err := VerificationAdditions.Get(kind)
		if err != nil {
			t.Fatalf("VerificationAdditions.Get(%q) error = %v", kind, err)
		}
		if !constructor().Context().Declared() {
			t.Errorf("%s Context().Declared() = false, want true", kind)
		}

		refusals := map[string]agentcore.LaunchTarget{
			"remote launch": {Command: "ssh", RemoteCommand: executable + " --acp", SSHHost: "worker"},
			"unnamed":       {Command: "/opt/sortie-neutral/sortie-neutral-launch", Args: []string{"--acp"}},
		}
		for name, target := range refusals {
			refused := constructor()
			if switches, claimed := refused.Claim(target); claimed {
				t.Errorf("%s Claim(%s) = %v, true, want a refusal", kind, name, switches)
			}
			assertTempRootEmpty(t, root)
			refused.Close()
			assertTempRootEmpty(t, root)
		}

		claimant := constructor()
		switches, claimed := claimant.Claim(agentcore.LaunchTarget{Command: executable, Args: verificationClaimArgs(kind)})
		if !claimed || len(switches.Env)+len(switches.Args) == 0 {
			t.Fatalf("%s Claim(%s --acp) = %v, %v, want a claim with at least one switch", kind, executable, switches, claimed)
		}
		claimant.Close()
		assertTempRootEmpty(t, root)
		claimant.Close()
		assertTempRootEmpty(t, root)
	}
}

func TestClaimVerificationRefusesAnAmbiguousOrOperatorSetLaunch(t *testing.T) {
	tests := []struct {
		name        string
		target      agentcore.LaunchTarget
		environ     []string
		wantClaimed bool
	}{
		{"a named launch with nothing set", agentcore.LaunchTarget{Command: "copilot", Args: []string{"--acp"}}, []string{"PATH=/bin", "OPENCODE_CONFIG=/x"}, true},
		{"a bare flag in Args", agentcore.LaunchTarget{Command: "copilot", Args: []string{"--acp", "--available-tools", "bash"}}, nil, false},
		{"a flag with a value in Args", agentcore.LaunchTarget{Command: "copilot", Args: []string{"--acp", "--no-custom-instructions=true"}}, nil, false},
		{"an environment name set", agentcore.LaunchTarget{Command: "opencode", Args: []string{"acp"}}, []string{"OPENCODE_CONFIG_CONTENT={}"}, false},
		{"an environment name set in another case", agentcore.LaunchTarget{Command: "opencode", Args: []string{"acp"}}, []string{"opencode_config_content={}"}, false},
		{"a file-backed switch already set", agentcore.LaunchTarget{Command: "gemini", Args: []string{"--acp"}}, []string{"GEMINI_SYSTEM_MD=/etc/system.md"}, false},
		{"two runtimes named", agentcore.LaunchTarget{Command: "gemini", Args: []string{"copilot"}}, nil, false},
		{"kiro with a model", agentcore.LaunchTarget{Command: "kiro-cli", Args: []string{"acp", "--model", "x"}}, nil, true},
		{"kiro without a model", agentcore.LaunchTarget{Command: "kiro-cli", Args: []string{"acp"}}, nil, false},
		{"kiro with an agent already chosen", agentcore.LaunchTarget{Command: "kiro-cli", Args: []string{"acp", "--model", "x", "--agent", "foo"}}, nil, false},
		{"kiro with its config directory already set", agentcore.LaunchTarget{Command: "kiro-cli", Args: []string{"acp", "--model", "x"}}, []string{"KIRO_AGENT_CONFIG_DIR=/etc/kiro"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			setTempRoot(t, root)

			addition, switches, claimed := ClaimVerification(tt.target, tt.environ)
			if claimed != tt.wantClaimed {
				t.Fatalf("ClaimVerification(%v, %v) claimed = %v, want %v", tt.target, tt.environ, claimed, tt.wantClaimed)
			}
			if claimed {
				if len(switches.Env)+len(switches.Args) == 0 {
					t.Errorf("ClaimVerification(%v) switches = %v, want at least one", tt.target, switches)
				}
				addition.Close()
			}
			assertTempRootEmpty(t, root)
		})
	}
}

func TestVerificationSwitchesPerRuntime(t *testing.T) {
	tests := []struct {
		name           string
		kind           string
		args           []string
		wantArgs       []string
		wantEnv        []string
		wantPromptIn   string
		wantAgentDirIn string
		wantKeeps      []registry.VerificationResidual
	}{
		{name: "copilot narrows the tools and drops custom instructions", kind: copilotAgentName,
			wantArgs:  []string{"--available-tools", "view", "--no-custom-instructions"},
			wantKeeps: []registry.VerificationResidual{registry.VerificationKeepsProjectInstructions, registry.VerificationKeepsUserInstructions}},
		{name: "opencode denies every tool through inline configuration", kind: opencodeAgentName,
			wantEnv: []string{"OPENCODE_CONFIG_CONTENT={\"permission\":{\"*\":\"deny\"}}"}},
		{name: "gemini points at a private system prompt file", kind: geminiAgentName,
			wantPromptIn: "GEMINI_SYSTEM_MD="},
		{name: "kiro selects a tool-less agent for a spaced --model", kind: kiroAgentName, args: []string{"acp", "--model", "x"},
			wantArgs: []string{"--agent", "sortie_verify"}, wantAgentDirIn: "KIRO_AGENT_CONFIG_DIR="},
		{name: "kiro selects a tool-less agent for an attached --model", kind: kiroAgentName, args: []string{"acp", "--model=x"},
			wantArgs: []string{"--agent", "sortie_verify"}, wantAgentDirIn: "KIRO_AGENT_CONFIG_DIR="},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			setTempRoot(t, root)
			constructor, err := VerificationAdditions.Get(tt.kind)
			if err != nil {
				t.Fatalf("VerificationAdditions.Get(%q) error = %v", tt.kind, err)
			}

			addition := constructor()
			for _, residual := range tt.wantKeeps {
				if !addition.Context().Keeps(residual) {
					t.Errorf("%s Context().Keeps(%q) = false, want true", tt.kind, residual)
				}
			}
			args := tt.args
			if args == nil {
				args = []string{"--acp"}
			}
			switches, claimed := addition.Claim(agentcore.LaunchTarget{Command: verificationExecutables[tt.kind], Args: args})
			if !claimed {
				t.Fatalf("%s Claim() claimed = false, want true", tt.kind)
			}
			if !slices.Equal(switches.Args, tt.wantArgs) {
				t.Errorf("%s Claim() Args = %q, want %q", tt.kind, switches.Args, tt.wantArgs)
			}
			if tt.wantPromptIn == "" && tt.wantAgentDirIn == "" && !slices.Equal(switches.Env, tt.wantEnv) {
				t.Errorf("%s Claim() Env = %q, want %q", tt.kind, switches.Env, tt.wantEnv)
			}
			if tt.wantPromptIn != "" {
				if len(switches.Env) != 1 || !strings.HasPrefix(switches.Env[0], tt.wantPromptIn) {
					t.Fatalf("%s Claim() Env = %q, want one %s entry", tt.kind, switches.Env, tt.wantPromptIn)
				}
				prompt, err := os.ReadFile(strings.TrimPrefix(switches.Env[0], tt.wantPromptIn))
				if err != nil {
					t.Fatalf("ReadFile(%s): %v", switches.Env[0], err)
				}
				if string(prompt) != agentcore.VerificationSystemPrompt {
					t.Errorf("%s system prompt file = %q, want %q", tt.kind, prompt, agentcore.VerificationSystemPrompt)
				}
			}

			if tt.wantAgentDirIn != "" {
				if len(switches.Env) != 1 || !strings.HasPrefix(switches.Env[0], tt.wantAgentDirIn) {
					t.Fatalf("%s Claim() Env = %q, want one %s entry", tt.kind, switches.Env, tt.wantAgentDirIn)
				}
				assertKiroAgentDefinition(t, strings.TrimPrefix(switches.Env[0], tt.wantAgentDirIn))
			}

			addition.Close()
			assertTempRootEmpty(t, root)
		})
	}
}
