package hue

import (
	"cmp"
	"context"
	"log/slog"
	"sync"

	"github.com/henry40408/domus/internal/core"
)

// Manager owns the running Hue task so pairing can (re)start it at runtime.
type Manager struct {
	core   *core.Core
	timing Timing
	logger *slog.Logger

	// pairMu serialises StartSynced, so two pairings cannot interleave and
	// leave the lights of the bridge that lost behind. It is separate from mu
	// so status queries are not blocked by a sync.
	pairMu sync.Mutex
	mu     sync.Mutex
	task   *task
}

// task is the goroutine that syncs and follows the event stream.
type task struct {
	integration *Integration
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewManager is a manager with nothing running. A nil logger means
// slog.Default().
func NewManager(c *core.Core, timing Timing, logger *slog.Logger) *Manager {
	return &Manager{core: c, timing: timing, logger: cmp.Or(logger, slog.Default())}
}

// IsRunning reports whether a Hue task is running.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.task != nil
}

// Start (re)starts the task for the bridge at base; it syncs in the background.
func (m *Manager) Start(base, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resetLocked()
	m.startLocked(m.newIntegration(base, key), true)
}

// StartSynced is like Start, but finishes the first sync before returning so
// callers see the lights. The sync runs without any lock held.
func (m *Manager) StartSynced(ctx context.Context, base, key string) {
	m.pairMu.Lock()
	defer m.pairMu.Unlock()
	m.Reset()
	integration := m.newIntegration(base, key)
	integration.syncAndLog(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
	m.startLocked(integration, false)
}

// Reset stops the task, waits until it has exited, and forgets every Hue
// entity. Waiting first means the old task cannot write states after they were
// removed.
func (m *Manager) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resetLocked()
}

func (m *Manager) resetLocked() {
	m.stopLocked()
	m.core.RemoveIntegration(IntegrationName)
	for _, prefix := range []string{entityPrefix, scenePrefix} {
		for _, id := range m.core.EntityIDsWithPrefix(prefix) {
			m.core.RemoveState(id)
		}
	}
}

// stopLocked cancels the running task and waits for it.
func (m *Manager) stopLocked() {
	if m.task == nil {
		return
	}
	m.task.cancel()
	<-m.task.done
	m.task.integration.Close()
	m.task = nil
}

func (m *Manager) newIntegration(base, key string) *Integration {
	return NewIntegration(IntegrationConfig{
		Client: NewClient(base, key, m.timing, m.logger),
		Core:   m.core,
		Timing: m.timing,
		Logger: m.logger,
	})
}

func (m *Manager) startLocked(integration *Integration, firstSync bool) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.core.SetIntegration(IntegrationName, integration)
	go func() {
		defer close(done)
		integration.run(ctx, firstSync)
	}()
	m.task = &task{integration: integration, cancel: cancel, done: done}
}
