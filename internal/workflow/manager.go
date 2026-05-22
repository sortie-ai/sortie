package workflow

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/prompt"
)

const debounceInterval = 50 * time.Millisecond

// ValidateFunc is a caller-supplied validation function invoked after
// [config.NewServiceConfig] succeeds and before config promotion.
//
// If it returns a non-nil error the new config is rejected and the
// last-known-good config is retained. Implementations must be safe for
// concurrent use and must treat the supplied [config.ServiceConfig] as
// read-only.
type ValidateFunc func(config.ServiceConfig) error

// ManagerOption configures optional behavior on [NewManager].
type ManagerOption func(*Manager)

// WithValidateFunc sets a validation callback that gates config
// promotion. See [ValidateFunc] for the contract.
func WithValidateFunc(fn ValidateFunc) ManagerOption {
	return func(m *Manager) { m.validateFunc = fn }
}

// WithAgentKindProbe sets the agent-kind registry probe used by
// dispatch-rule validation at workflow load time. The probe receives
// an agent kind string and returns true when the kind is currently
// registered. Callers wire the closure to the registry pointer; the
// workflow package never imports the registry package itself.
//
// When the probe is nil (the default), the dispatch builder treats
// every agent kind as registered. The orchestrator's startup
// preflight still rejects unknown kinds as a defense-in-depth gate.
func WithAgentKindProbe(probe func(kind string) bool) ManagerOption {
	return func(m *Manager) { m.agentKindProbe = probe }
}

// Manager watches a workflow file for changes and maintains the current
// effective configuration. The latest config and prompt template are
// available via [Manager.Config] and [Manager.PromptTemplate]. Safe for
// concurrent use.
type Manager struct {
	path           string
	logger         *slog.Logger
	validateFunc   ValidateFunc
	agentKindProbe func(kind string) bool

	mu                   sync.RWMutex
	currentConfig        config.ServiceConfig
	currentPrompt        *prompt.Template
	currentTemplateIndex map[string]*prompt.Template
	lastLoadErr          error

	watcher *fsnotify.Watcher
	done    chan struct{}
	stopped sync.Once
	wg      sync.WaitGroup
	started atomic.Bool
}

// NewManager creates a [Manager] for the workflow file at path. It
// performs a synchronous initial load — if the file cannot be loaded or
// the config is invalid, NewManager returns an error so the caller can
// fail startup. The logger is used for reload diagnostics. Options are
// applied after construction; see [WithValidateFunc].
func NewManager(path string, logger *slog.Logger, opts ...ManagerOption) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}

	m := &Manager{
		path:   path,
		logger: logger,
		done:   make(chan struct{}),
	}
	for _, o := range opts {
		o(m)
	}

	cfg, tmpl, index, err := m.loadPipeline()
	if err != nil {
		return nil, err
	}

	if m.validateFunc != nil {
		if err := m.validateFunc(cfg); err != nil {
			return nil, err
		}
	}

	m.currentConfig = cfg
	m.currentPrompt = tmpl
	m.currentTemplateIndex = index
	return m, nil
}

// Config returns the current effective [config.ServiceConfig]. If the
// most recent reload failed, this returns the last successfully loaded
// config. Safe for concurrent use.
func (m *Manager) Config() config.ServiceConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentConfig
}

// PromptTemplate returns the current compiled prompt template. If the
// most recent reload failed, this returns the last successfully parsed
// template. Safe for concurrent use.
func (m *Manager) PromptTemplate() *prompt.Template {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentPrompt
}

// PromptTemplateByID returns the parsed prompt template registered
// under id. The empty-string key returns the WORKFLOW.md body
// template. Returns nil when the id is unknown. Safe for concurrent
// use.
func (m *Manager) PromptTemplateByID(id string) *prompt.Template {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.currentTemplateIndex == nil {
		if id == "" {
			return m.currentPrompt
		}
		return nil
	}
	if tmpl, ok := m.currentTemplateIndex[id]; ok {
		return tmpl
	}
	return nil
}

// FilePath returns the base filename of the workflow file (e.g.
// "WORKFLOW.md"). The full directory path is stripped to avoid
// exposing sensitive directory structure.
func (m *Manager) FilePath() string {
	return filepath.Base(m.path)
}

// WorkflowAbsPath returns the absolute path to the workflow file.
// The returned value is immutable after construction.
func (m *Manager) WorkflowAbsPath() string {
	return m.path
}

// LastLoadError returns the error from the most recent reload attempt,
// or nil if the last reload succeeded. Safe for concurrent use.
func (m *Manager) LastLoadError() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastLoadErr
}

func (m *Manager) currentLogger() *slog.Logger {
	m.mu.RLock()
	l := m.logger
	m.mu.RUnlock()
	return l
}

// SetLogger replaces the logger used for reload diagnostics and watcher
// errors. Safe for concurrent use — the file-watcher goroutine may be
// running. A nil argument is treated as [slog.Default]. After return,
// all subsequent log calls from the watcher use the new logger.
func (m *Manager) SetLogger(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	m.mu.Lock()
	m.logger = logger
	m.mu.Unlock()
}

// Reload synchronously re-reads the workflow file, parses config and
// prompt, and swaps the effective values. On error the previous config
// is retained and the error is returned. This supports the orchestrator's
// defensive re-validation before dispatch. Safe for concurrent use.
func (m *Manager) Reload() error {
	cfg, tmpl, index, err := m.loadPipeline()
	if err != nil {
		m.mu.Lock()
		m.lastLoadErr = err
		m.mu.Unlock()
		return err
	}

	if m.validateFunc != nil {
		if err := m.validateFunc(cfg); err != nil {
			m.mu.Lock()
			m.lastLoadErr = err
			m.mu.Unlock()
			return err
		}
	}

	m.mu.Lock()
	m.currentConfig = cfg
	m.currentPrompt = tmpl
	m.currentTemplateIndex = index
	m.lastLoadErr = nil
	m.mu.Unlock()
	return nil
}

// Start begins watching the workflow file for changes. It spawns a
// background goroutine that listens for filesystem events and reloads
// the config on change. The goroutine exits when ctx is cancelled or
// [Manager.Stop] is called. Start must be called at most once.
//
// The watcher monitors the parent directory rather than the file itself
// so that atomic-rename saves (vim, sed -i) are detected via Create
// events. This does not detect Kubernetes ConfigMap symlink swaps; the
// orchestrator's defensive re-validation before dispatch covers that gap.
func (m *Manager) Start(ctx context.Context) error {
	if !m.started.CompareAndSwap(false, true) {
		return fmt.Errorf("start called more than once")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	dir := filepath.Dir(m.path)
	if err := w.Add(dir); err != nil {
		w.Close() //nolint:errcheck,gosec // best-effort cleanup on startup failure
		return err
	}

	m.watcher = w
	m.wg.Add(1)
	go m.watch(ctx)
	return nil
}

// Stop stops the filesystem watcher and waits for the background
// goroutine to exit. Safe to call multiple times.
func (m *Manager) Stop() {
	m.stopped.Do(func() { close(m.done) })
	m.wg.Wait()
}

func (m *Manager) watch(ctx context.Context) {
	// wg.Done runs after watcher.Close (LIFO defer order), so by the
	// time Stop()'s wg.Wait() returns the watcher is already closed.
	// Ownership of m.watcher belongs exclusively to this goroutine from
	// the moment Start() returns.
	defer m.wg.Done()
	defer m.watcher.Close() //nolint:errcheck // best-effort cleanup in defer

	targetName := filepath.Base(m.path)
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.done:
			return
		case event, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != targetName {
				continue
			}
			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
				continue
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(debounceInterval)
		case err, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
			m.currentLogger().Error("workflow watcher error", slog.Any("error", err))
		case <-timer.C:
			m.reload()
		}
	}
}

func (m *Manager) reload() {
	logger := m.currentLogger()
	cfg, tmpl, index, err := m.loadPipeline()
	if err != nil {
		logger.Error("workflow reload failed", slog.Any("error", err), slog.String("path", m.path))
		m.mu.Lock()
		m.lastLoadErr = err
		m.mu.Unlock()
		return
	}

	if m.validateFunc != nil {
		if err := m.validateFunc(cfg); err != nil {
			logger.Error("workflow reload rejected by validation",
				slog.Any("error", err), slog.String("path", m.path))
			m.mu.Lock()
			m.lastLoadErr = err
			m.mu.Unlock()
			return
		}
	}

	m.mu.Lock()
	m.currentConfig = cfg
	m.currentPrompt = tmpl
	m.currentTemplateIndex = index
	m.lastLoadErr = nil
	m.mu.Unlock()
	logger.Info("workflow reloaded", slog.String("path", m.path))
}

// loadPipeline runs the full Load -> NewServiceConfig -> dispatch
// build -> Parse pipeline. Returns the parsed service config, the
// body template, and the per-template index keyed by resolved
// absolute path (empty key holds the body template). On any error
// the caller retains the previous values and surfaces the error via
// [Manager.LastLoadError].
func (m *Manager) loadPipeline() (config.ServiceConfig, *prompt.Template, map[string]*prompt.Template, error) {
	wf, err := Load(m.path)
	if err != nil {
		return config.ServiceConfig{}, nil, nil, err
	}

	cfg, err := config.NewServiceConfig(wf.Config)
	if err != nil {
		return config.ServiceConfig{}, nil, nil, err
	}

	probe := m.agentKindProbe
	if probe == nil {
		// Permissive default: callers without an orchestrator
		// dependency (dryrun, tests) accept any agent kind. The
		// startup preflight rejects unknown kinds as a second gate.
		probe = func(string) bool { return true }
	}

	dispatchCfg, err := config.BuildDispatchConfig(wf.Config, filepath.Dir(m.path), probe)
	if err != nil {
		return config.ServiceConfig{}, nil, nil, err
	}
	cfg.SetDispatch(dispatchCfg)

	tmpl, err := prompt.Parse(wf.PromptTemplate, m.path, wf.FrontMatterLines)
	if err != nil {
		return config.ServiceConfig{}, nil, nil, err
	}

	index, err := loadPerRuleTemplates(dispatchCfg, tmpl)
	if err != nil {
		return config.ServiceConfig{}, nil, nil, err
	}

	return cfg, tmpl, index, nil
}

// loadPerRuleTemplates reads and parses every unique per-rule
// template referenced by the dispatch config. The returned index is
// keyed by resolved absolute path; the empty-string key holds the
// already-parsed body template.
func loadPerRuleTemplates(dispatchCfg config.DispatchConfig, body *prompt.Template) (map[string]*prompt.Template, error) {
	index := map[string]*prompt.Template{"": body}

	unique := make(map[string]struct{})
	for _, rule := range dispatchCfg.Rules {
		if rule.Selection.TemplateID == "" {
			continue
		}
		unique[rule.Selection.TemplateID] = struct{}{}
	}
	if dispatchCfg.Default.TemplateID != "" {
		unique[dispatchCfg.Default.TemplateID] = struct{}{}
	}

	for absPath := range unique {
		bodyBytes, err := os.ReadFile(absPath) //nolint:gosec // path was resolved and validated by BuildDispatchConfig
		if err != nil {
			return nil, &prompt.TemplateError{
				Kind:   prompt.ErrTemplateParse,
				Source: absPath,
				Err:    fmt.Errorf("read per-rule template: %w", err),
			}
		}
		if hasFrontMatterMarker(bodyBytes) {
			return nil, &prompt.TemplateError{
				Kind:   prompt.ErrTemplateParse,
				Source: absPath,
				Err:    fmt.Errorf("per-rule templates must not carry front matter"),
			}
		}
		parsed, err := prompt.Parse(string(bodyBytes), absPath, 0)
		if err != nil {
			return nil, err
		}
		index[absPath] = parsed
	}

	return index, nil
}

// hasFrontMatterMarker reports whether the byte buffer starts with
// the YAML front matter delimiter, after skipping leading whitespace.
func hasFrontMatterMarker(b []byte) bool {
	trimmed := bytes.TrimLeft(b, " \t\r\n")
	return bytes.HasPrefix(trimmed, []byte("---"))
}
