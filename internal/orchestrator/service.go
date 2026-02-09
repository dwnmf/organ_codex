package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"organ_codex/internal/domain"
)

const orchestratorAgentID = "orchestrator"

type Store interface {
	CreateTask(ctx context.Context, task domain.Task) error
	GetTask(ctx context.Context, taskID string) (domain.Task, error)
	ListTasks(ctx context.Context) ([]domain.Task, error)
	ListUnfinishedTasks(ctx context.Context) ([]domain.Task, error)
	UpdateTaskStatus(ctx context.Context, taskID string, status domain.TaskStatus, lastError string) error
	TouchTask(ctx context.Context, taskID string) error
	IncrementTaskHop(ctx context.Context, taskID string) (int, error)
	CountPendingMessages(ctx context.Context, taskID string) (int, error)
	CountActiveMessages(ctx context.Context, taskID string) (int, error)

	GrantTaskPermission(ctx context.Context, p domain.TaskPermission) error
	GrantAgentChannel(ctx context.Context, ch domain.AgentChannel) error

	CreateMessage(ctx context.Context, msg domain.Message) (bool, error)
	ListDispatchableMessages(ctx context.Context, limit int, now time.Time) ([]domain.Message, error)
	MarkMessageDelivered(ctx context.Context, messageID string) error
	MarkMessageForRetry(ctx context.Context, messageID string, lastError string, retryAt time.Time, maxRetries int) (bool, error)
	AckMessage(ctx context.Context, messageID string, agentID string, result string) error

	LogDecision(ctx context.Context, entry domain.DecisionLog) error
}

type Policy interface {
	CanMessage(ctx context.Context, taskID, fromAgent, toAgent string, msgType domain.MessageType) (bool, string, error)
}

type Bus interface {
	Register(agentID string) <-chan domain.Message
	Unregister(agentID string)
	Publish(msg domain.Message) error
}

type Config struct {
	DispatchInterval time.Duration
	WatchdogInterval time.Duration
	RetryDelay       time.Duration
	MaxRetries       int
	IdleTimeout      time.Duration
	DefaultMaxHops   int
}

func (c Config) withDefaults() Config {
	if c.DispatchInterval <= 0 {
		c.DispatchInterval = 250 * time.Millisecond
	}
	if c.WatchdogInterval <= 0 {
		c.WatchdogInterval = 3 * time.Second
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = 1 * time.Second
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 6
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 30 * time.Second
	}
	if c.DefaultMaxHops <= 0 {
		c.DefaultMaxHops = 8
	}
	return c
}

type Service struct {
	store  Store
	policy Policy
	bus    Bus
	cfg    Config
	logger *log.Logger

	wg sync.WaitGroup
}

func New(store Store, policy Policy, bus Bus, cfg Config, logger *log.Logger) *Service {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = log.Default()
	}
	return &Service{
		store:  store,
		policy: policy,
		bus:    bus,
		cfg:    cfg,
		logger: logger,
	}
}

type CreateTaskInput struct {
	ID           string
	Goal         string
	Scope        string
	OwnerAgent   string
	Priority     int
	DeadlineAt   *time.Time
	BudgetTokens int
	MaxHops      int
}

func (s *Service) Start(ctx context.Context) {
	s.wg.Add(3)
	go func() {
		defer s.wg.Done()
		s.dispatchLoop(ctx)
	}()
	go func() {
		defer s.wg.Done()
		s.watchdogLoop(ctx)
	}()
	go func() {
		defer s.wg.Done()
		s.orchestratorInboxLoop(ctx)
	}()
}

func (s *Service) Wait() {
	s.wg.Wait()
}

func (s *Service) CreateTask(ctx context.Context, in CreateTaskInput) (domain.Task, error) {
	if in.ID == "" {
		in.ID = uuid.NewString()
	}
	if in.OwnerAgent == "" {
		in.OwnerAgent = "planner"
	}
	maxHops := in.MaxHops
	if maxHops <= 0 {
		maxHops = s.cfg.DefaultMaxHops
	}
	task := domain.Task{
		ID:           in.ID,
		Goal:         in.Goal,
		Scope:        in.Scope,
		OwnerAgent:   in.OwnerAgent,
		Status:       domain.TaskStatusPlanned,
		Priority:     in.Priority,
		DeadlineAt:   in.DeadlineAt,
		BudgetTokens: in.BudgetTokens,
		MaxHops:      maxHops,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := s.store.CreateTask(ctx, task); err != nil {
		return domain.Task{}, err
	}
	_ = s.store.LogDecision(ctx, domain.DecisionLog{
		TaskID:  task.ID,
		Actor:   orchestratorAgentID,
		Action:  "task_created",
		Reason:  "task created by orchestrator",
		Payload: mustJSON(task),
	})
	return task, nil
}

func (s *Service) GetTask(ctx context.Context, taskID string) (domain.Task, error) {
	return s.store.GetTask(ctx, taskID)
}

func (s *Service) ListTasks(ctx context.Context) ([]domain.Task, error) {
	return s.store.ListTasks(ctx)
}

func (s *Service) GrantPermission(ctx context.Context, permission domain.TaskPermission) error {
	if err := s.store.GrantTaskPermission(ctx, permission); err != nil {
		return err
	}
	_ = s.store.LogDecision(ctx, domain.DecisionLog{
		TaskID:  permission.TaskID,
		Actor:   orchestratorAgentID,
		Action:  "permission_granted",
		Reason:  "permission rule granted",
		Payload: mustJSON(permission),
	})
	return nil
}

func (s *Service) GrantChannel(ctx context.Context, channel domain.AgentChannel) error {
	if err := s.store.GrantAgentChannel(ctx, channel); err != nil {
		return err
	}
	_ = s.store.LogDecision(ctx, domain.DecisionLog{
		TaskID:  channel.TaskID,
		Actor:   orchestratorAgentID,
		Action:  "channel_granted",
		Reason:  "agent channel granted",
		Payload: mustJSON(channel),
	})
	return nil
}

func (s *Service) StartTask(ctx context.Context, taskID string) error {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if task.Status == domain.TaskStatusDone || task.Status == domain.TaskStatusFailed || task.Status == domain.TaskStatusCanceled {
		return fmt.Errorf("task %s is already final (%s)", task.ID, task.Status)
	}
	if err := s.store.UpdateTaskStatus(ctx, taskID, domain.TaskStatusRunning, ""); err != nil {
		return err
	}

	payload, _ := json.Marshal(domain.TaskRequestPayload{
		Goal:  task.Goal,
		Scope: task.Scope,
		AcceptanceCriteria: []string{
			"Result must include runnable project files, not only a text description",
			"All produced files must stay inside approved workspace scope",
			"Provide concise summary of what was created",
		},
	})
	return s.EnqueueMessage(ctx, domain.Message{
		TaskID:         taskID,
		FromAgent:      orchestratorAgentID,
		ToAgent:        task.OwnerAgent,
		Type:           domain.MessageTypeRequest,
		Payload:        payload,
		CorrelationID:  "task-start-" + taskID,
		IdempotencyKey: "task-start-" + taskID,
	})
}

func (s *Service) EnqueueMessage(ctx context.Context, msg domain.Message) error {
	if msg.TaskID == "" || msg.FromAgent == "" || msg.ToAgent == "" || msg.Type == "" {
		return fmt.Errorf("invalid message: required fields are empty")
	}
	if msg.ID == "" {
		msg.ID = uuid.NewString()
	}
	if msg.IdempotencyKey == "" {
		msg.IdempotencyKey = msg.ID
	}
	if msg.CorrelationID == "" {
		msg.CorrelationID = msg.TaskID
	}
	if msg.Payload == nil {
		msg.Payload = []byte("{}")
	}
	if msg.Status == "" {
		msg.Status = domain.MessageStatusPending
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	if msg.NextAttemptAt.IsZero() {
		msg.NextAttemptAt = msg.CreatedAt
	}

	task, err := s.store.GetTask(ctx, msg.TaskID)
	if err != nil {
		return fmt.Errorf("get task for message: %w", err)
	}
	if task.Status == domain.TaskStatusDone || task.Status == domain.TaskStatusFailed || task.Status == domain.TaskStatusCanceled {
		return fmt.Errorf("task %s is final state (%s), no new messages accepted", task.ID, task.Status)
	}

	if msg.FromAgent != orchestratorAgentID {
		var allowed bool
		var reason string
		var err error
		for attempt := 0; attempt < 6; attempt++ {
			allowed, reason, err = s.policy.CanMessage(ctx, msg.TaskID, msg.FromAgent, msg.ToAgent, msg.Type)
			if err == nil {
				break
			}
			if !isSQLiteBusy(err) {
				break
			}
			time.Sleep(time.Duration(30*(attempt+1)) * time.Millisecond)
		}
		if err != nil {
			return fmt.Errorf("message policy check: %w", err)
		}
		if !allowed {
			_ = s.store.LogDecision(ctx, domain.DecisionLog{
				TaskID: msg.TaskID,
				Actor:  orchestratorAgentID,
				Action: "message_denied",
				Reason: reason,
				Payload: mustJSON(map[string]string{
					"from": string(msg.FromAgent),
					"to":   string(msg.ToAgent),
					"type": string(msg.Type),
				}),
			})
			return fmt.Errorf("message denied: %s", reason)
		}
		hopCount, err := s.store.IncrementTaskHop(ctx, msg.TaskID)
		if err != nil {
			return fmt.Errorf("increment task hop: %w", err)
		}
		if hopCount > task.MaxHops {
			_ = s.store.UpdateTaskStatus(ctx, msg.TaskID, domain.TaskStatusBlocked, "max hops exceeded")
			_ = s.store.LogDecision(ctx, domain.DecisionLog{
				TaskID: msg.TaskID,
				Actor:  orchestratorAgentID,
				Action: "task_blocked",
				Reason: "max hops exceeded",
				Payload: mustJSON(map[string]any{
					"hop_count": hopCount,
					"max_hops":  task.MaxHops,
				}),
			})
			return fmt.Errorf("max hops exceeded for task %s", msg.TaskID)
		}
	}

	var created bool
	var msgErr error
	for attempt := 0; attempt < 6; attempt++ {
		created, msgErr = s.store.CreateMessage(ctx, msg)
		if msgErr == nil {
			break
		}
		if !isSQLiteBusy(msgErr) {
			break
		}
		time.Sleep(time.Duration(30*(attempt+1)) * time.Millisecond)
	}
	if msgErr != nil {
		return msgErr
	}
	if !created {
		return nil
	}
	return nil
}

func (s *Service) dispatchLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.DispatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.dispatchOnce(ctx); err != nil {
				s.logger.Printf("dispatch loop error: %v", err)
			}
		}
	}
}

func (s *Service) dispatchOnce(ctx context.Context) error {
	msgs, err := s.store.ListDispatchableMessages(ctx, 128, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, msg := range msgs {
		if err := s.bus.Publish(msg); err != nil {
			shouldRetry, retryErr := s.store.MarkMessageForRetry(
				ctx,
				msg.ID,
				err.Error(),
				time.Now().UTC().Add(s.cfg.RetryDelay),
				s.cfg.MaxRetries,
			)
			if retryErr != nil {
				s.logger.Printf("retry update failed message=%s: %v", msg.ID, retryErr)
				continue
			}
			if !shouldRetry {
				_ = s.store.LogDecision(ctx, domain.DecisionLog{
					TaskID: msg.TaskID,
					Actor:  orchestratorAgentID,
					Action: "message_failed",
					Reason: "max retries reached",
					Payload: mustJSON(map[string]any{
						"message_id": msg.ID,
						"to_agent":   msg.ToAgent,
						"last_error": err.Error(),
					}),
				})
			}
			continue
		}

		if err := s.store.MarkMessageDelivered(ctx, msg.ID); err != nil {
			s.logger.Printf("mark message delivered failed message=%s: %v", msg.ID, err)
		}
	}
	return nil
}

func (s *Service) orchestratorInboxLoop(ctx context.Context) {
	ch := s.bus.Register(orchestratorAgentID)
	defer s.bus.Unregister(orchestratorAgentID)

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			s.handleOrchestratorMessage(ctx, msg)
		}
	}
}

func (s *Service) handleOrchestratorMessage(ctx context.Context, msg domain.Message) {
	_ = s.store.AckMessage(ctx, msg.ID, orchestratorAgentID, "received")
	_ = s.store.TouchTask(ctx, msg.TaskID)

	switch msg.Type {
	case domain.MessageTypeDone:
		_ = s.store.UpdateTaskStatus(ctx, msg.TaskID, domain.TaskStatusDone, "")
		_ = s.store.LogDecision(ctx, domain.DecisionLog{
			TaskID:  msg.TaskID,
			Actor:   orchestratorAgentID,
			Action:  "task_done",
			Reason:  "received DONE from agent",
			Payload: msg.Payload,
		})
	case domain.MessageTypeBlocked, domain.MessageTypeEscalate:
		_ = s.store.UpdateTaskStatus(ctx, msg.TaskID, domain.TaskStatusBlocked, string(msg.Payload))
		_ = s.store.LogDecision(ctx, domain.DecisionLog{
			TaskID:  msg.TaskID,
			Actor:   orchestratorAgentID,
			Action:  "task_blocked",
			Reason:  "received escalation message",
			Payload: msg.Payload,
		})
	default:
		_ = s.store.LogDecision(ctx, domain.DecisionLog{
			TaskID:  msg.TaskID,
			Actor:   orchestratorAgentID,
			Action:  "orchestrator_message_received",
			Reason:  "non-terminal message received by orchestrator",
			Payload: msg.Payload,
		})
	}
}

func (s *Service) watchdogLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.WatchdogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.watchdogOnce(ctx)
		}
	}
}

func (s *Service) watchdogOnce(ctx context.Context) {
	tasks, err := s.store.ListUnfinishedTasks(ctx)
	if err != nil {
		s.logger.Printf("watchdog list unfinished tasks error: %v", err)
		return
	}
	now := time.Now().UTC()
	for _, task := range tasks {
		if task.DeadlineAt != nil && now.After(*task.DeadlineAt) &&
			task.Status != domain.TaskStatusDone &&
			task.Status != domain.TaskStatusFailed &&
			task.Status != domain.TaskStatusCanceled {
			_ = s.store.UpdateTaskStatus(ctx, task.ID, domain.TaskStatusFailed, "deadline exceeded")
			_ = s.store.LogDecision(ctx, domain.DecisionLog{
				TaskID:  task.ID,
				Actor:   orchestratorAgentID,
				Action:  "task_failed",
				Reason:  "deadline exceeded",
				Payload: mustJSON(map[string]any{"deadline_at": task.DeadlineAt}),
			})
			continue
		}

		if task.Status == domain.TaskStatusRunning {
			active, err := s.store.CountActiveMessages(ctx, task.ID)
			if err != nil {
				s.logger.Printf("watchdog count active task=%s error: %v", task.ID, err)
				continue
			}
			if active == 0 && now.Sub(task.UpdatedAt) > s.cfg.IdleTimeout {
				_ = s.store.UpdateTaskStatus(ctx, task.ID, domain.TaskStatusBlocked, "idle timeout exceeded")
				_ = s.store.LogDecision(ctx, domain.DecisionLog{
					TaskID: task.ID,
					Actor:  orchestratorAgentID,
					Action: "task_blocked",
					Reason: "idle timeout exceeded",
					Payload: mustJSON(map[string]any{
						"idle_timeout_sec": int(s.cfg.IdleTimeout.Seconds()),
					}),
				})
			}
		}
	}
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy")
}
