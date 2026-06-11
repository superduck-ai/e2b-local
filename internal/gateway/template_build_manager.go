package gateway

import (
	"context"
	"errors"
	"sync"

	"e2b-local/internal/e2bapi"
)

var (
	errTemplateBuildCapacityExhausted = errors.New("template build capacity exhausted")
	errTemplateBuildAlreadyRunning    = errors.New("template build is already running")
	errTemplateBuildManagerStopped    = errors.New("template build manager is stopped")
)

type templateBuildKey struct {
	TemplateID string
	BuildID    string
}

type templateBuildManager struct {
	ctx    context.Context
	cancel context.CancelFunc

	maxConcurrent int

	mu       sync.Mutex
	active   map[templateBuildKey]*templateBuildTask
	stopping bool
	wg       sync.WaitGroup
}

type templateBuildTask struct {
	manager *templateBuildManager
	key     templateBuildKey
	ctx     context.Context
	cancel  context.CancelFunc

	cancelled bool
}

func newTemplateBuildManager(maxConcurrent int) *templateBuildManager {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultTemplateBuildMaxConcurrent
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &templateBuildManager{
		ctx:           ctx,
		cancel:        cancel,
		maxConcurrent: maxConcurrent,
		active:        map[templateBuildKey]*templateBuildTask{},
	}
}

func (m *templateBuildManager) reserve(templateID string, buildID string) (*templateBuildTask, error) {
	key := templateBuildKey{TemplateID: templateID, BuildID: buildID}
	ctx, cancel := context.WithCancel(m.ctx)
	task := &templateBuildTask{
		manager: m,
		key:     key,
		ctx:     ctx,
		cancel:  cancel,
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stopping {
		cancel()
		return nil, errTemplateBuildManagerStopped
	}
	if _, exists := m.active[key]; exists {
		cancel()
		return nil, errTemplateBuildAlreadyRunning
	}
	if len(m.active) >= m.maxConcurrent {
		cancel()
		return nil, errTemplateBuildCapacityExhausted
	}
	m.active[key] = task
	m.wg.Add(1)
	return task, nil
}

func (t *templateBuildTask) goRun(run func(context.Context)) {
	go func() {
		defer t.manager.complete(t)
		run(t.ctx)
	}()
}

func (m *templateBuildManager) complete(task *templateBuildTask) {
	m.mu.Lock()
	if m.active[task.key] == task {
		delete(m.active, task.key)
	}
	m.mu.Unlock()
	task.cancel()
	m.wg.Done()
}

func (m *templateBuildManager) release(task *templateBuildTask) {
	m.mu.Lock()
	if m.active[task.key] == task {
		delete(m.active, task.key)
	}
	m.mu.Unlock()
	task.cancel()
	m.wg.Done()
}

func (m *templateBuildManager) isCurrent(task *templateBuildTask) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[task.key] == task
}

func (m *templateBuildManager) cancelTeamBuilds(teamID string) e2bapi.AdminBuildCancelResult {
	m.mu.Lock()
	tasks := make([]*templateBuildTask, 0, len(m.active))
	for _, task := range m.active {
		if task.cancelled {
			continue
		}
		task.cancelled = true
		tasks = append(tasks, task)
	}
	m.mu.Unlock()

	for _, task := range tasks {
		task.cancel()
	}
	return e2bapi.AdminBuildCancelResult{CancelledCount: len(tasks)}
}

func (m *templateBuildManager) shutdown(ctx context.Context) error {
	m.mu.Lock()
	if !m.stopping {
		m.stopping = true
		m.cancel()
	}
	for _, task := range m.active {
		task.cancelled = true
		task.cancel()
	}
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
