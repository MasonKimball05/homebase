package supervisor

import (
	"context"
	"sync"

	"github.com/MasonKimball05/homebase/internal/config"
)

// Manager runs every app from the config.
type Manager struct {
	apps   []*App
	byName map[string]*App
}

func NewManager(cfg config.Config) *Manager {
	m := &Manager{byName: map[string]*App{}}
	for _, ac := range cfg.Apps {
		a := NewApp(ac, cfg.LogDir)
		m.apps = append(m.apps, a)
		m.byName[ac.Name] = a
	}
	return m
}

// Run supervises all apps until ctx is cancelled, then waits for every app
// to stop, so none are left running without a supervisor.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, a := range m.apps {
		wg.Go(func() { a.Run(ctx) })
	}
	wg.Wait()
}

func (m *Manager) App(name string) (*App, bool) {
	a, ok := m.byName[name]
	return a, ok
}

// Statuses returns every app's status in config order.
func (m *Manager) Statuses() []Status {
	out := make([]Status, len(m.apps))
	for i, a := range m.apps {
		out[i] = a.Status()
	}
	return out
}
