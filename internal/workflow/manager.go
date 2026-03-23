package workflow

import (
	"context"
	"fmt"
	"log/slog"
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
// [config.NewServiceConfig] succeeds and before config promotion. If it
// returns a non-nil error, the new config is rejected and the
// last-known-good config is retained. This allows the caller to enforce
// domain-level invariants without leaking domain knowledge into the
// Manager. Implementations must be safe for concurrent use and must
// treat the supplied [config.ServiceConfig] as read-only; they must not
// mutate the config value or any data reachable from it.
type ValidateFunc func(config.ServiceConfig) error

// ManagerOption configures optional behavior on [NewManager].
type ManagerOption func(*Manager)

// WithValidateFunc sets a validation callback that gates config
// promotion. See [ValidateFunc] for the contract.
func WithValidateFunc(fn ValidateFunc) ManagerOption {
	return func(m *Manager) { m.validateFunc = fn }
}

// Manager watches a workflow file for changes and maintains the current
// effective configuration. Obtain the latest config and prompt template
// via [Manager.Config] and [Manager.PromptTemplate]. Safe for concurrent
// use.
type Manager struct {
	path         string
	logger       *slog.Logger
	validateFunc ValidateFunc

	mu            sync.RWMutex
	currentConfig config.ServiceConfig
	currentPrompt *prompt.Template
	lastLoadErr   error

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

	cfg, tmpl, err := m.loadPipeline()
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

// LastLoadError returns the error from the most recent reload attempt,
// or nil if the last reload succeeded. Safe for concurrent use.
func (m *Manager) LastLoadError() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastLoadErr
}

// Reload synchronously re-reads the workflow file, parses config and
// prompt, and swaps the effective values. On error the previous config
// is retained and the error is returned. This supports the orchestrator's
// defensive re-validation before dispatch. Safe for concurrent use.
func (m *Manager) Reload() error {
	cfg, tmpl, err := m.loadPipeline()
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
		return fmt.Errorf("workflow.Manager: Start called more than once")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	dir := filepath.Dir(m.path)
	if err := w.Add(dir); err != nil {
		w.Close() //nolint:errcheck // best-effort cleanup on startup failure
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
			m.logger.Error("workflow watcher error", slog.Any("error", err))
		case <-timer.C:
			m.reload()
		}
	}
}

func (m *Manager) reload() {
	cfg, tmpl, err := m.loadPipeline()
	if err != nil {
		m.logger.Error("workflow reload failed", slog.Any("error", err), slog.String("path", m.path))
		m.mu.Lock()
		m.lastLoadErr = err
		m.mu.Unlock()
		return
	}

	if m.validateFunc != nil {
		if err := m.validateFunc(cfg); err != nil {
			m.logger.Error("workflow reload rejected by validation",
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
	m.lastLoadErr = nil
	m.mu.Unlock()
	m.logger.Info("workflow reloaded", slog.String("path", m.path))
}

// loadPipeline runs the full Load → NewServiceConfig → Parse pipeline
// and returns the results. Factored out to share between reload and
// Reload.
func (m *Manager) loadPipeline() (config.ServiceConfig, *prompt.Template, error) {
	wf, err := Load(m.path)
	if err != nil {
		return config.ServiceConfig{}, nil, err
	}

	cfg, err := config.NewServiceConfig(wf.Config)
	if err != nil {
		return config.ServiceConfig{}, nil, err
	}

	tmpl, err := prompt.Parse(wf.PromptTemplate, m.path, wf.FrontMatterLines)
	if err != nil {
		return config.ServiceConfig{}, nil, err
	}

	return cfg, tmpl, nil
}
