package jobs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/envpilot/contracts/domain"
	"github.com/envpilot/runner/internal/app"
	"github.com/envpilot/runner/internal/scm"
	"github.com/envpilot/runner/internal/store"
)

type Type string

const (
	TypeCreateEnvironment Type = "create_environment"
	TypeDeleteEnvironment Type = "delete_environment"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusIgnored   Status = "ignored"
)

type Executor interface {
	CreateEnvironment(ctx context.Context, req domain.CreateEnvironmentRequest) (domain.Environment, error)
	DeleteEnvironment(ctx context.Context, id string, force bool) (domain.Environment, error)
	GetEnvironment(id string) (domain.Environment, error)
}

type EnvironmentReserver interface {
	ReserveEnvironment(ctx context.Context, req domain.CreateEnvironmentRequest) (domain.Environment, error)
}

type ProjectResolver interface {
	ResolveProjectByRepository(provider string, repo string) (domain.Project, bool, error)
	ResolveProjectByIntegrationID(provider string, integrationID string) (domain.Project, bool, error)
}

type Job struct {
	ID            string                          `json:"id"`
	Type          Type                            `json:"type"`
	Status        Status                          `json:"status"`
	EnvironmentID string                          `json:"environmentId"`
	Event         scm.PullRequestEvent            `json:"event"`
	Request       domain.CreateEnvironmentRequest `json:"request"`
	Result        *domain.Environment             `json:"result,omitempty"`
	Error         string                          `json:"error,omitempty"`
	Attempts      int                             `json:"attempts"`
	MaxAttempts   int                             `json:"maxAttempts"`
	NextRunAt     *time.Time                      `json:"nextRunAt,omitempty"`
	CreatedAt     time.Time                       `json:"createdAt"`
	StartedAt     *time.Time                      `json:"startedAt,omitempty"`
	CompletedAt   *time.Time                      `json:"completedAt,omitempty"`
}

type Option func(*Manager)

func WithRetryDelay(delay time.Duration) Option {
	return func(m *Manager) {
		m.retryDelay = delay
	}
}

func WithMaxAttempts(attempts int) Option {
	return func(m *Manager) {
		if attempts > 0 {
			m.maxAttempts = attempts
		}
	}
}

func WithProjectResolver(resolver ProjectResolver) Option {
	return func(m *Manager) {
		m.projects = resolver
	}
}

func WithStore(store Store) Option {
	return func(m *Manager) {
		if store != nil {
			m.store = store
		}
	}
}

func WithQueue(queue Queue) Option {
	return func(m *Manager) {
		if queue != nil {
			m.queue = queue
		}
	}
}

type Manager struct {
	executor    Executor
	projects    ProjectResolver
	store       Store
	queue       Queue
	now         func() time.Time
	retryDelay  time.Duration
	maxAttempts int
	mu          sync.RWMutex
	nextID      int64
	wake        chan struct{}
}

func NewManager(executor Executor, options ...Option) *Manager {
	manager := &Manager{
		executor:    executor,
		retryDelay:  5 * time.Second,
		maxAttempts: 3,
		now: func() time.Time {
			return time.Now().UTC()
		},
		store: NewMemoryStore(),
		queue: NewMemoryQueue(),
		wake:  make(chan struct{}, 1),
	}
	for _, option := range options {
		option(manager)
	}
	manager.bootstrap()
	return manager
}

func (m *Manager) Run(ctx context.Context) {
	if err := m.Recover(ctx); err != nil {
		// Keep the worker alive; list/get endpoints still expose stored state.
	}
	for {
		processed, err := m.ProcessNext(ctx)
		if err != nil {
			// Job state already captures the error. Keep the worker alive.
		}
		if processed {
			continue
		}

		timer := time.NewTimer(m.sleepDuration())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (m *Manager) List() []Job {
	items, err := m.store.List()
	if err != nil {
		return nil
	}
	sortJobs(items)
	return items
}

func (m *Manager) Get(id string) (Job, bool) {
	job, err := m.store.Get(id)
	return job, err == nil
}

func (m *Manager) SubmitSCMEvent(ctx context.Context, event scm.PullRequestEvent) (Job, error) {
	if existing, ok := m.findExistingEventJob(event); ok {
		return existing, nil
	}

	project, err := m.resolveProjectBinding(event)
	if err != nil {
		return Job{}, err
	}
	if !m.shouldProcessEvent(event, project) {
		job, err := m.newIgnoredJob(event, project)
		if err != nil {
			return Job{}, err
		}
		if err := m.save(job); err != nil {
			return Job{}, err
		}
		return job, nil
	}

	job, err := m.newJob(event, project)
	if err != nil {
		return Job{}, err
	}
	if job.Type == TypeCreateEnvironment && job.Status != StatusIgnored {
		if reserver, ok := m.executor.(EnvironmentReserver); ok {
			env, err := reserver.ReserveEnvironment(ctx, job.Request)
			if err != nil {
				return Job{}, err
			}
			if env.ID != "" {
				job.EnvironmentID = env.ID
			}
		}
	}
	if err := m.save(job); err != nil {
		return Job{}, err
	}
	if job.Status != StatusIgnored {
		if err := m.enqueue(ctx, job.ID, true); err != nil {
			return Job{}, err
		}
	}
	return job, nil
}

func (m *Manager) SubmitCreateEnvironment(ctx context.Context, req domain.CreateEnvironmentRequest, event scm.PullRequestEvent) (Job, error) {
	if existing, ok := m.findActiveCreateJob(req.ID); ok {
		return existing, nil
	}

	job, err := m.newCreateJob(req, event)
	if err != nil {
		return Job{}, err
	}
	if reserver, ok := m.executor.(EnvironmentReserver); ok {
		env, err := reserver.ReserveEnvironment(ctx, job.Request)
		if err != nil {
			return Job{}, err
		}
		if env.ID != "" {
			job.EnvironmentID = env.ID
			job.Request.ID = env.ID
		}
		if env.Project != "" {
			job.Request.Project = env.Project
		}
	}
	if err := m.save(job); err != nil {
		return Job{}, err
	}
	if err := m.enqueue(ctx, job.ID, true); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (m *Manager) ProcessNext(ctx context.Context) (bool, error) {
	job, ok := m.nextReadyJob()
	if !ok {
		return false, nil
	}
	_, err := m.execute(ctx, job.ID)
	return true, err
}

func (m *Manager) QueueDepth() int {
	depth, err := m.queue.Depth(context.Background())
	if err != nil {
		return 0
	}
	return depth
}

func (m *Manager) newJob(event scm.PullRequestEvent, project domain.Project) (Job, error) {
	now := m.now()
	request := event.CreateEnvironmentRequest(project.ProductID, project.ID)
	jobType := TypeCreateEnvironment
	status := StatusQueued

	switch event.Action {
	case scm.ActionOpen, scm.ActionUpdate:
		jobType = TypeCreateEnvironment
	case scm.ActionClose:
		jobType = TypeDeleteEnvironment
	case scm.ActionIgnore:
		status = StatusIgnored
	default:
		return Job{}, fmt.Errorf("unsupported scm event action %q", event.Action)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("job-%06d", m.nextID)

	return Job{
		ID:            id,
		Type:          jobType,
		Status:        status,
		EnvironmentID: request.ID,
		Event:         event,
		Request:       request,
		Attempts:      0,
		MaxAttempts:   m.maxAttempts,
		CreatedAt:     now,
	}, nil
}

func (m *Manager) newCreateJob(req domain.CreateEnvironmentRequest, event scm.PullRequestEvent) (Job, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("job-%06d", m.nextID)

	return Job{
		ID:            id,
		Type:          TypeCreateEnvironment,
		Status:        StatusQueued,
		EnvironmentID: req.ID,
		Event:         event,
		Request:       req,
		Attempts:      0,
		MaxAttempts:   m.maxAttempts,
		CreatedAt:     now,
	}, nil
}

func (m *Manager) resolveProjectBinding(event scm.PullRequestEvent) (domain.Project, error) {
	if m.projects == nil {
		return domain.Project{}, nil
	}
	event.InstallationID = strings.TrimSpace(event.InstallationID)
	project, ok, err := m.projects.ResolveProjectByRepository(string(event.Provider), event.Repo)
	if err != nil {
		return domain.Project{}, err
	}
	if ok {
		return project, nil
	}
	if event.InstallationID == "" {
		return domain.Project{}, nil
	}
	projectByIntegration, ok, err := m.projects.ResolveProjectByIntegrationID(string(event.Provider), event.InstallationID)
	if err != nil || !ok {
		return domain.Project{}, err
	}
	return projectByIntegration, nil
}

func (m *Manager) shouldProcessEvent(event scm.PullRequestEvent, project domain.Project) bool {
	if project.ID == "" {
		return true
	}
	if event.Action == scm.ActionClose || event.Action == scm.ActionIgnore {
		return true
	}
	if !project.WebhookAllowDraftPRs && event.Draft {
		return false
	}
	if !matchesAnyBranchFilter(event.Branch, project.WebhookBranchFilters) {
		return false
	}
	if !hasAnyMatchingLabel(event.Labels, project.WebhookLabels) {
		return false
	}
	return true
}

func (m *Manager) findExistingEventJob(event scm.PullRequestEvent) (Job, bool) {
	key := event.DeduplicationKey()
	if key == "" {
		return Job{}, false
	}
	items, err := m.store.List()
	if err != nil {
		return Job{}, false
	}
	for _, item := range items {
		if item.Event.DeduplicationKey() == key {
			return item, true
		}
	}
	return Job{}, false
}

func (m *Manager) findActiveCreateJob(environmentID string) (Job, bool) {
	environmentID = strings.TrimSpace(environmentID)
	if environmentID == "" {
		return Job{}, false
	}
	items, err := m.store.List()
	if err != nil {
		return Job{}, false
	}
	for _, item := range items {
		if item.Type != TypeCreateEnvironment {
			continue
		}
		if item.EnvironmentID != environmentID && item.Request.ID != environmentID {
			continue
		}
		if item.Status == StatusQueued || item.Status == StatusRunning {
			return item, true
		}
	}
	return Job{}, false
}

func (m *Manager) newIgnoredJob(event scm.PullRequestEvent, project domain.Project) (Job, error) {
	request := event.CreateEnvironmentRequest(project.ProductID, project.ID)
	jobType, err := jobTypeForAction(event.Action)
	if err != nil {
		return Job{}, err
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("job-%06d", m.nextID)
	return Job{
		ID:            id,
		Type:          jobType,
		Status:        StatusIgnored,
		EnvironmentID: request.ID,
		Event:         event,
		Request:       request,
		Attempts:      0,
		MaxAttempts:   m.maxAttempts,
		CreatedAt:     now,
	}, nil
}

func jobTypeForAction(action scm.EventAction) (Type, error) {
	switch action {
	case scm.ActionOpen, scm.ActionUpdate:
		return TypeCreateEnvironment, nil
	case scm.ActionClose:
		return TypeDeleteEnvironment, nil
	case scm.ActionIgnore:
		return TypeCreateEnvironment, nil
	default:
		return "", fmt.Errorf("unsupported scm event action %q", action)
	}
}

func matchesAnyBranchFilter(branch string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	normalizedBranch := strings.ToLower(strings.TrimSpace(branch))
	if normalizedBranch == "" {
		return false
	}
	for _, filter := range filters {
		candidate := strings.ToLower(strings.TrimSpace(filter))
		if candidate == "" {
			continue
		}
		if candidate == "*" || candidate == normalizedBranch {
			return true
		}
		matched, err := path.Match(candidate, normalizedBranch)
		if err == nil && matched {
			return true
		}
	}
	return false
}

func hasAnyMatchingLabel(labels []string, required []string) bool {
	if len(required) == 0 {
		return true
	}
	if len(labels) == 0 {
		return false
	}

	requiredPatterns := make([]string, 0, len(required))
	for _, requiredLabel := range required {
		requiredLabel = strings.ToLower(strings.TrimSpace(requiredLabel))
		if requiredLabel == "" {
			continue
		}
		requiredPatterns = append(requiredPatterns, requiredLabel)
	}
	if len(requiredPatterns) == 0 {
		return true
	}

	normalizedLabels := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" {
			continue
		}
		normalizedLabels = append(normalizedLabels, label)
	}
	if len(normalizedLabels) == 0 {
		return false
	}

	for _, pattern := range requiredPatterns {
		for _, label := range normalizedLabels {
			matched, err := path.Match(pattern, label)
			if err == nil && matched {
				return true
			}
		}
	}
	return false
}

func (m *Manager) nextReadyJob() (Job, bool) {
	id, ok, err := m.queue.Dequeue(context.Background())
	if err != nil || !ok {
		return Job{}, false
	}
	job, err := m.store.Get(id)
	if err != nil {
		return Job{}, false
	}
	if job.NextRunAt != nil && m.now().Before(*job.NextRunAt) {
		_ = m.enqueue(context.Background(), job.ID, false)
		return Job{}, false
	}
	if job.Status != StatusQueued && job.Status != StatusRunning {
		return Job{}, false
	}
	return job, true
}

func (m *Manager) execute(ctx context.Context, id string) (Job, error) {
	job, ok := m.Get(id)
	if !ok {
		return Job{}, fmt.Errorf("job %q not found", id)
	}

	now := m.now()
	job.Status = StatusRunning
	job.Attempts++
	job.StartedAt = &now
	job.NextRunAt = nil
	if err := m.save(job); err != nil {
		return Job{}, err
	}

	var env domain.Environment
	var err error
	switch job.Type {
	case TypeCreateEnvironment:
		env, err = m.executor.CreateEnvironment(ctx, job.Request)
		if isConflict(err) {
			env, err = m.executor.GetEnvironment(job.EnvironmentID)
		}
	case TypeDeleteEnvironment:
		env, err = m.executor.DeleteEnvironment(ctx, job.EnvironmentID, true)
		if errors.Is(err, store.ErrNotFound) {
			err = nil
		}
	default:
		err = fmt.Errorf("unsupported job type %q", job.Type)
	}

	if err != nil {
		job.Error = err.Error()
		if job.Attempts < job.MaxAttempts {
			nextRunAt := m.now().Add(m.retryDelay)
			job.Status = StatusQueued
			job.NextRunAt = &nextRunAt
			if saveErr := m.save(job); saveErr != nil {
				return job, saveErr
			}
			if enqueueErr := m.enqueue(ctx, job.ID, false); enqueueErr != nil {
				return job, enqueueErr
			}
			return job, err
		}
		completedAt := m.now()
		job.Status = StatusFailed
		job.CompletedAt = &completedAt
		_ = m.save(job)
		return job, err
	}

	completedAt := m.now()
	job.Status = StatusSucceeded
	job.Error = ""
	job.CompletedAt = &completedAt
	if env.ID != "" {
		job.Result = &env
	}
	if err := m.save(job); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (m *Manager) enqueue(ctx context.Context, id string, wake bool) error {
	if err := m.queue.Enqueue(ctx, id); err != nil {
		return err
	}
	if !wake {
		return nil
	}

	select {
	case m.wake <- struct{}{}:
	default:
	}
	return nil
}

func (m *Manager) save(job Job) error {
	return m.store.Save(job)
}

func (m *Manager) Recover(ctx context.Context) error {
	items, err := m.store.List()
	if err != nil {
		return err
	}
	now := m.now()
	for _, item := range items {
		if !isRecoverableStatus(item.Status) {
			continue
		}
		job := recoverableJob(item, now)
		if err := m.save(job); err != nil {
			return err
		}
		if err := m.enqueue(ctx, job.ID, true); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Retry(ctx context.Context, id string) (Job, error) {
	job, err := m.store.Get(id)
	if err != nil {
		return Job{}, err
	}
	if job.Status != StatusFailed {
		return Job{}, fmt.Errorf("job %q is not failed", id)
	}
	job.Status = StatusQueued
	job.Error = ""
	job.NextRunAt = nil
	job.CompletedAt = nil
	if err := m.save(job); err != nil {
		return Job{}, err
	}
	if err := m.enqueue(ctx, job.ID, true); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (m *Manager) bootstrap() {
	items, err := m.store.List()
	if err != nil {
		return
	}
	m.nextID = maxNumericJobID(items)
}

func (m *Manager) sleepDuration() time.Duration {
	items, err := m.store.List()
	if err != nil {
		return time.Second
	}
	now := m.now()
	shortest := time.Second
	for _, job := range items {
		if job.Status != StatusQueued {
			continue
		}
		if job.NextRunAt == nil {
			return 100 * time.Millisecond
		}
		wait := job.NextRunAt.Sub(now)
		if wait < 0 {
			return 100 * time.Millisecond
		}
		if wait < shortest {
			shortest = wait
		}
	}
	return shortest
}

func isConflict(err error) bool {
	var target app.ConflictError
	return errors.As(err, &target)
}
