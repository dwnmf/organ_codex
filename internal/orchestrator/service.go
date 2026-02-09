package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
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
	CountPendingMessages(ctx context.Context, taskID string) (int, error)
	CountActiveMessages(ctx context.Context, taskID string) (int, error)
	ListTaskMessages(ctx context.Context, taskID string, limit int) ([]domain.Message, error)
	ListTaskMessageAcks(ctx context.Context, taskID string, limit int) ([]domain.MessageAck, error)
	ListTaskDecisions(ctx context.Context, taskID string, limit int) ([]domain.DecisionLog, error)
	ListTaskTimeline(ctx context.Context, taskID string, limit int) ([]domain.TimelineEvent, error)

	GrantTaskPermission(ctx context.Context, p domain.TaskPermission) error
	GrantAgentChannel(ctx context.Context, ch domain.AgentChannel) error

	CreateMessage(ctx context.Context, msg domain.Message) (bool, error)
	ListDispatchableMessages(ctx context.Context, limit int, now time.Time) ([]domain.Message, error)
	ClaimMessageForDispatch(ctx context.Context, messageID string, now, leaseUntil time.Time) (bool, error)
	MarkMessageDelivered(ctx context.Context, messageID string, ackDeadline time.Time) (bool, error)
	ListExpiredInFlightMessages(ctx context.Context, limit int, now time.Time) ([]domain.Message, error)
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
	DispatchLease    time.Duration
	AckTimeout       time.Duration
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
	if c.DispatchLease <= 0 {
		c.DispatchLease = 10 * time.Second
	}
	if c.AckTimeout <= 0 {
		c.AckTimeout = 45 * time.Second
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

	planMu sync.Mutex
	plans  map[string]*taskPlanRuntime
}

type taskPlanRuntime struct {
	nodes     map[string]domain.PlanNode
	scheduled map[string]bool
	completed map[string]bool
	results   map[string]domain.WorkResultPayload
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
		plans:  make(map[string]*taskPlanRuntime),
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

func (s *Service) ListTaskMessages(ctx context.Context, taskID string, limit int) ([]domain.Message, error) {
	return s.store.ListTaskMessages(ctx, taskID, limit)
}

func (s *Service) ListTaskMessageAcks(ctx context.Context, taskID string, limit int) ([]domain.MessageAck, error) {
	return s.store.ListTaskMessageAcks(ctx, taskID, limit)
}

func (s *Service) ListTaskDecisions(ctx context.Context, taskID string, limit int) ([]domain.DecisionLog, error) {
	return s.store.ListTaskDecisions(ctx, taskID, limit)
}

func (s *Service) ListTaskTimeline(ctx context.Context, taskID string, limit int) ([]domain.TimelineEvent, error) {
	return s.store.ListTaskTimeline(ctx, taskID, limit)
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
		if strings.Contains(strings.ToLower(msgErr.Error()), "max hops exceeded") {
			s.blockTaskIfNonFinal(ctx, msg.TaskID, "max hops exceeded", map[string]any{
				"message_id": msg.ID,
				"from_agent": msg.FromAgent,
				"to_agent":   msg.ToAgent,
				"type":       msg.Type,
			})
		}
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
	now := time.Now().UTC()
	if err := s.requeueExpiredInFlightMessages(ctx, now); err != nil {
		s.logger.Printf("requeue in-flight messages error: %v", err)
	}

	msgs, err := s.store.ListDispatchableMessages(ctx, 128, now)
	if err != nil {
		return err
	}
	for _, msg := range msgs {
		claimed, claimErr := s.store.ClaimMessageForDispatch(ctx, msg.ID, time.Now().UTC(), time.Now().UTC().Add(s.cfg.DispatchLease))
		if claimErr != nil {
			s.logger.Printf("claim message failed message=%s: %v", msg.ID, claimErr)
			continue
		}
		if !claimed {
			continue
		}

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
				if msg.Type == domain.MessageTypeRequest {
					s.blockTaskIfNonFinal(ctx, msg.TaskID, "message delivery retries exceeded", map[string]any{
						"message_id": msg.ID,
						"from_agent": msg.FromAgent,
						"to_agent":   msg.ToAgent,
						"type":       msg.Type,
						"last_error": err.Error(),
					})
				}
			}
			continue
		}

		updated, err := s.store.MarkMessageDelivered(ctx, msg.ID, time.Now().UTC().Add(s.cfg.AckTimeout))
		if err != nil {
			s.logger.Printf("mark message delivered failed message=%s: %v", msg.ID, err)
			continue
		}
		if !updated {
			s.logger.Printf("mark message delivered skipped message=%s (status changed concurrently)", msg.ID)
		}
	}
	return nil
}

func (s *Service) requeueExpiredInFlightMessages(ctx context.Context, now time.Time) error {
	expired, err := s.store.ListExpiredInFlightMessages(ctx, 128, now)
	if err != nil {
		return err
	}
	for _, msg := range expired {
		reason := "message in-flight lease expired"
		if msg.Status == domain.MessageStatusDelivered {
			reason = "message ack timeout exceeded"
		}
		shouldRetry, retryErr := s.store.MarkMessageForRetry(
			ctx,
			msg.ID,
			reason,
			time.Now().UTC().Add(s.cfg.RetryDelay),
			s.cfg.MaxRetries,
		)
		if retryErr != nil {
			s.logger.Printf("retry expired in-flight failed message=%s: %v", msg.ID, retryErr)
			continue
		}
		if shouldRetry {
			_ = s.store.LogDecision(ctx, domain.DecisionLog{
				TaskID: msg.TaskID,
				Actor:  orchestratorAgentID,
				Action: "message_requeued",
				Reason: reason,
				Payload: mustJSON(map[string]any{
					"message_id": msg.ID,
					"from_agent": msg.FromAgent,
					"to_agent":   msg.ToAgent,
					"type":       msg.Type,
					"status":     msg.Status,
				}),
			})
			continue
		}

		_ = s.store.LogDecision(ctx, domain.DecisionLog{
			TaskID: msg.TaskID,
			Actor:  orchestratorAgentID,
			Action: "message_failed",
			Reason: "max retries reached",
			Payload: mustJSON(map[string]any{
				"message_id": msg.ID,
				"from_agent": msg.FromAgent,
				"to_agent":   msg.ToAgent,
				"type":       msg.Type,
				"status":     msg.Status,
				"last_error": reason,
			}),
		})
		if msg.Type == domain.MessageTypeRequest || msg.Type == domain.MessageTypeReview {
			s.blockTaskIfNonFinal(ctx, msg.TaskID, "message retry budget exhausted", map[string]any{
				"message_id": msg.ID,
				"from_agent": msg.FromAgent,
				"to_agent":   msg.ToAgent,
				"type":       msg.Type,
				"last_error": reason,
			})
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
	task, err := s.store.GetTask(ctx, msg.TaskID)
	if err == nil && isFinalStatus(task.Status) {
		_ = s.store.LogDecision(ctx, domain.DecisionLog{
			TaskID: msg.TaskID,
			Actor:  orchestratorAgentID,
			Action: "final_message_ignored",
			Reason: "task already in final status",
			Payload: mustJSON(map[string]any{
				"task_status": task.Status,
				"type":        msg.Type,
				"from_agent":  msg.FromAgent,
				"message_id":  msg.ID,
			}),
		})
		return
	}

	switch msg.Type {
	case domain.MessageTypePropose:
		s.handlePlanProposalMessage(ctx, msg, task)
	case domain.MessageTypeDone:
		if s.handlePlanNodeDone(ctx, msg) {
			return
		}
		var result domain.WorkResultPayload
		if err := json.Unmarshal(msg.Payload, &result); err != nil {
			s.blockTaskIfNonFinal(ctx, msg.TaskID, "invalid DONE payload", map[string]any{
				"message_id": msg.ID,
				"from_agent": msg.FromAgent,
				"error":      err.Error(),
			})
			return
		}
		if strings.TrimSpace(result.Summary) == "" || len(result.CreatedFiles) == 0 {
			s.blockTaskIfNonFinal(ctx, msg.TaskID, "DONE payload missing summary/files", map[string]any{
				"message_id":    msg.ID,
				"from_agent":    msg.FromAgent,
				"summary_empty": strings.TrimSpace(result.Summary) == "",
				"files_count":   len(result.CreatedFiles),
			})
			return
		}
		_ = s.store.UpdateTaskStatus(ctx, msg.TaskID, domain.TaskStatusDone, "")
		_ = s.store.LogDecision(ctx, domain.DecisionLog{
			TaskID:  msg.TaskID,
			Actor:   orchestratorAgentID,
			Action:  "task_done",
			Reason:  "received valid DONE from agent",
			Payload: msg.Payload,
		})
		s.clearTaskPlan(msg.TaskID)
	case domain.MessageTypeBlocked, domain.MessageTypeEscalate:
		s.blockTaskIfNonFinal(ctx, msg.TaskID, "received escalation message", map[string]any{
			"type":       msg.Type,
			"from_agent": msg.FromAgent,
			"payload":    json.RawMessage(msg.Payload),
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

func (s *Service) handlePlanProposalMessage(ctx context.Context, msg domain.Message, task domain.Task) {
	if msg.FromAgent != "planner" {
		s.blockTaskIfNonFinal(ctx, msg.TaskID, "plan proposal allowed only from planner", map[string]any{
			"from_agent": msg.FromAgent,
			"message_id": msg.ID,
		})
		return
	}

	if strings.TrimSpace(task.ID) == "" {
		t, err := s.store.GetTask(ctx, msg.TaskID)
		if err != nil {
			s.blockTaskIfNonFinal(ctx, msg.TaskID, "task not found for plan proposal", map[string]any{
				"error": err.Error(),
			})
			return
		}
		task = t
	}

	var proposal domain.PlanProposalPayload
	if err := json.Unmarshal(msg.Payload, &proposal); err != nil {
		s.blockTaskIfNonFinal(ctx, msg.TaskID, "invalid PROPOSE payload", map[string]any{
			"message_id": msg.ID,
			"from_agent": msg.FromAgent,
			"error":      err.Error(),
		})
		return
	}
	plan, err := buildTaskPlanRuntime(proposal)
	if err != nil {
		s.blockTaskIfNonFinal(ctx, msg.TaskID, "invalid plan proposal", map[string]any{
			"message_id": msg.ID,
			"from_agent": msg.FromAgent,
			"error":      err.Error(),
		})
		return
	}

	s.planMu.Lock()
	if _, exists := s.plans[msg.TaskID]; exists {
		s.planMu.Unlock()
		_ = s.store.LogDecision(ctx, domain.DecisionLog{
			TaskID: msg.TaskID,
			Actor:  orchestratorAgentID,
			Action: "plan_proposal_ignored",
			Reason: "task already has active plan",
			Payload: mustJSON(map[string]any{
				"from_agent": msg.FromAgent,
				"message_id": msg.ID,
			}),
		})
		return
	}
	s.plans[msg.TaskID] = plan
	s.planMu.Unlock()

	_ = s.store.LogDecision(ctx, domain.DecisionLog{
		TaskID: msg.TaskID,
		Actor:  orchestratorAgentID,
		Action: "plan_registered",
		Reason: "plan accepted from planner",
		Payload: mustJSON(map[string]any{
			"from_agent": msg.FromAgent,
			"nodes":      len(plan.nodes),
			"summary":    proposal.Summary,
		}),
	})

	if err := s.dispatchReadyPlanNodes(ctx, msg.TaskID, task); err != nil {
		s.blockTaskIfNonFinal(ctx, msg.TaskID, "failed to dispatch planned nodes", map[string]any{
			"error": err.Error(),
		})
	}
}

func (s *Service) handlePlanNodeDone(ctx context.Context, msg domain.Message) bool {
	if !s.hasTaskPlan(msg.TaskID) {
		return false
	}

	var result domain.WorkResultPayload
	if err := json.Unmarshal(msg.Payload, &result); err != nil {
		s.blockTaskIfNonFinal(ctx, msg.TaskID, "invalid plan DONE payload", map[string]any{
			"message_id": msg.ID,
			"from_agent": msg.FromAgent,
			"error":      err.Error(),
		})
		return true
	}
	if strings.TrimSpace(result.NodeID) == "" {
		s.blockTaskIfNonFinal(ctx, msg.TaskID, "plan DONE missing node_id", map[string]any{
			"message_id": msg.ID,
			"from_agent": msg.FromAgent,
		})
		return true
	}

	node, alreadyDone, err := s.completePlanNode(msg.TaskID, result, msg.FromAgent)
	if err != nil {
		s.blockTaskIfNonFinal(ctx, msg.TaskID, "plan node completion failed", map[string]any{
			"message_id": msg.ID,
			"from_agent": msg.FromAgent,
			"node_id":    result.NodeID,
			"error":      err.Error(),
		})
		return true
	}
	if alreadyDone {
		return true
	}

	_ = s.store.LogDecision(ctx, domain.DecisionLog{
		TaskID: msg.TaskID,
		Actor:  orchestratorAgentID,
		Action: "plan_node_done",
		Reason: "planned node completed",
		Payload: mustJSON(map[string]any{
			"node_id":       node.ID,
			"agent_id":      node.AgentID,
			"message_id":    msg.ID,
			"created_files": result.CreatedFiles,
			"summary":       trimText(result.Summary, 160),
		}),
	})

	if s.isTaskPlanCompleted(msg.TaskID) {
		s.finalizeTaskFromPlan(ctx, msg.TaskID)
		return true
	}

	task, err := s.store.GetTask(ctx, msg.TaskID)
	if err != nil {
		s.blockTaskIfNonFinal(ctx, msg.TaskID, "failed to load task for next planned nodes", map[string]any{
			"error": err.Error(),
		})
		return true
	}
	if err := s.dispatchReadyPlanNodes(ctx, msg.TaskID, task); err != nil {
		s.blockTaskIfNonFinal(ctx, msg.TaskID, "failed to dispatch next planned node", map[string]any{
			"node_id": result.NodeID,
			"error":   err.Error(),
		})
	}
	return true
}

func buildTaskPlanRuntime(proposal domain.PlanProposalPayload) (*taskPlanRuntime, error) {
	if len(proposal.Nodes) == 0 {
		return nil, fmt.Errorf("plan has no nodes")
	}
	nodes := make(map[string]domain.PlanNode, len(proposal.Nodes))
	for _, node := range proposal.Nodes {
		node.ID = strings.TrimSpace(node.ID)
		node.AgentID = strings.TrimSpace(node.AgentID)
		if node.ID == "" {
			return nil, fmt.Errorf("plan node id is required")
		}
		if node.AgentID == "" {
			return nil, fmt.Errorf("plan node %s has empty agent_id", node.ID)
		}
		if node.Type == "" {
			return nil, fmt.Errorf("plan node %s has empty type", node.ID)
		}
		if node.Type != domain.MessageTypeRequest && node.Type != domain.MessageTypeReview {
			return nil, fmt.Errorf("plan node %s has unsupported type %s", node.ID, node.Type)
		}
		if _, exists := nodes[node.ID]; exists {
			return nil, fmt.Errorf("duplicate plan node id %s", node.ID)
		}
		deps := make([]string, 0, len(node.DependsOn))
		for _, dep := range node.DependsOn {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if dep == node.ID {
				return nil, fmt.Errorf("plan node %s depends on itself", node.ID)
			}
			deps = append(deps, dep)
		}
		node.DependsOn = deps
		nodes[node.ID] = node
	}
	for _, node := range nodes {
		for _, dep := range node.DependsOn {
			if _, ok := nodes[dep]; !ok {
				return nil, fmt.Errorf("plan node %s depends on unknown node %s", node.ID, dep)
			}
		}
	}
	if hasPlanCycle(nodes) {
		return nil, fmt.Errorf("plan dependency graph has a cycle")
	}
	return &taskPlanRuntime{
		nodes:     nodes,
		scheduled: make(map[string]bool, len(nodes)),
		completed: make(map[string]bool, len(nodes)),
		results:   make(map[string]domain.WorkResultPayload, len(nodes)),
	}, nil
}

func hasPlanCycle(nodes map[string]domain.PlanNode) bool {
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var dfs func(id string) bool
	dfs = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, dep := range nodes[id].DependsOn {
			if dfs(dep) {
				return true
			}
		}
		visiting[id] = false
		visited[id] = true
		return false
	}
	for id := range nodes {
		if dfs(id) {
			return true
		}
	}
	return false
}

func (s *Service) dispatchReadyPlanNodes(ctx context.Context, taskID string, task domain.Task) error {
	type dispatchNode struct {
		node       domain.PlanNode
		depResults []domain.WorkResultPayload
	}

	ready := make([]dispatchNode, 0)
	s.planMu.Lock()
	plan, ok := s.plans[taskID]
	if !ok {
		s.planMu.Unlock()
		return nil
	}
	for nodeID, node := range plan.nodes {
		if plan.scheduled[nodeID] || plan.completed[nodeID] {
			continue
		}
		depsDone := true
		depResults := make([]domain.WorkResultPayload, 0, len(node.DependsOn))
		for _, dep := range node.DependsOn {
			if !plan.completed[dep] {
				depsDone = false
				break
			}
			depResults = append(depResults, plan.results[dep])
		}
		if !depsDone {
			continue
		}
		plan.scheduled[nodeID] = true
		ready = append(ready, dispatchNode{
			node:       node,
			depResults: depResults,
		})
	}
	s.planMu.Unlock()

	for _, item := range ready {
		payload, err := buildPlanNodePayload(task, item.node, item.depResults)
		if err != nil {
			return err
		}
		if err := s.EnqueueMessage(ctx, domain.Message{
			TaskID:         taskID,
			FromAgent:      orchestratorAgentID,
			ToAgent:        item.node.AgentID,
			Type:           item.node.Type,
			Payload:        payload,
			CorrelationID:  fmt.Sprintf("plan-%s-node-%s", taskID, item.node.ID),
			IdempotencyKey: fmt.Sprintf("plan-%s-node-%s", taskID, item.node.ID),
		}); err != nil {
			return err
		}
		_ = s.store.LogDecision(ctx, domain.DecisionLog{
			TaskID: taskID,
			Actor:  orchestratorAgentID,
			Action: "plan_node_dispatched",
			Reason: "ready plan node dispatched",
			Payload: mustJSON(map[string]any{
				"node_id":    item.node.ID,
				"agent_id":   item.node.AgentID,
				"type":       item.node.Type,
				"depends_on": item.node.DependsOn,
			}),
		})
	}
	return nil
}

func buildPlanNodePayload(task domain.Task, node domain.PlanNode, depResults []domain.WorkResultPayload) ([]byte, error) {
	switch node.Type {
	case domain.MessageTypeRequest:
		goal := strings.TrimSpace(node.Goal)
		if goal == "" {
			goal = task.Goal
		}
		scope := strings.TrimSpace(node.Scope)
		if scope == "" {
			scope = task.Scope
		}
		payload, _ := json.Marshal(domain.WorkRequestPayload{
			NodeID: node.ID,
			Goal:   goal,
			Scope:  scope,
			AcceptanceCriteria: []string{
				"Result must include runnable project files, not only text description",
				"All generated files must stay inside approved workspace scope",
				"Provide concise summary and list of created files",
			},
		})
		return payload, nil
	case domain.MessageTypeReview:
		filesSet := map[string]struct{}{}
		summaries := make([]string, 0, len(depResults))
		for _, dep := range depResults {
			if txt := strings.TrimSpace(dep.Summary); txt != "" {
				summaries = append(summaries, txt)
			}
			for _, f := range dep.CreatedFiles {
				filesSet[f] = struct{}{}
			}
		}
		files := make([]string, 0, len(filesSet))
		for f := range filesSet {
			files = append(files, f)
		}
		sort.Strings(files)
		payload, _ := json.Marshal(domain.ReviewRequestPayload{
			NodeID:       node.ID,
			Summary:      strings.Join(summaries, "\n\n"),
			CreatedFiles: files,
		})
		return payload, nil
	default:
		return nil, fmt.Errorf("unsupported plan node type %s", node.Type)
	}
}

func (s *Service) hasTaskPlan(taskID string) bool {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	_, ok := s.plans[taskID]
	return ok
}

func (s *Service) completePlanNode(taskID string, result domain.WorkResultPayload, fromAgent string) (domain.PlanNode, bool, error) {
	s.planMu.Lock()
	defer s.planMu.Unlock()

	plan, ok := s.plans[taskID]
	if !ok {
		return domain.PlanNode{}, false, fmt.Errorf("plan is not registered for task %s", taskID)
	}
	node, ok := plan.nodes[result.NodeID]
	if !ok {
		return domain.PlanNode{}, false, fmt.Errorf("unknown plan node %s", result.NodeID)
	}
	if node.AgentID != fromAgent {
		return domain.PlanNode{}, false, fmt.Errorf("node %s is assigned to %s, got %s", node.ID, node.AgentID, fromAgent)
	}
	if plan.completed[node.ID] {
		return node, true, nil
	}
	plan.completed[node.ID] = true
	plan.results[node.ID] = result
	return node, false, nil
}

func (s *Service) isTaskPlanCompleted(taskID string) bool {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	plan, ok := s.plans[taskID]
	if !ok {
		return false
	}
	return len(plan.completed) == len(plan.nodes)
}

func (s *Service) finalizeTaskFromPlan(ctx context.Context, taskID string) {
	aggregated, ok := s.collectAndClearPlanResult(taskID)
	if !ok {
		return
	}
	if strings.TrimSpace(aggregated.Summary) == "" || len(aggregated.CreatedFiles) == 0 {
		s.blockTaskIfNonFinal(ctx, taskID, "plan completed without valid output", map[string]any{
			"summary_empty": strings.TrimSpace(aggregated.Summary) == "",
			"files_count":   len(aggregated.CreatedFiles),
		})
		return
	}
	_ = s.store.UpdateTaskStatus(ctx, taskID, domain.TaskStatusDone, "")
	_ = s.store.LogDecision(ctx, domain.DecisionLog{
		TaskID:  taskID,
		Actor:   orchestratorAgentID,
		Action:  "task_done",
		Reason:  "all plan nodes completed",
		Payload: mustJSON(aggregated),
	})
}

func (s *Service) collectAndClearPlanResult(taskID string) (domain.WorkResultPayload, bool) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	plan, ok := s.plans[taskID]
	if !ok {
		return domain.WorkResultPayload{}, false
	}
	if len(plan.completed) != len(plan.nodes) {
		return domain.WorkResultPayload{}, false
	}

	summaries := make([]string, 0, len(plan.results))
	filesSet := map[string]struct{}{}
	nodeIDs := make([]string, 0, len(plan.results))
	for nodeID := range plan.results {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	for _, nodeID := range nodeIDs {
		item := plan.results[nodeID]
		if txt := strings.TrimSpace(item.Summary); txt != "" {
			summaries = append(summaries, fmt.Sprintf("[%s] %s", nodeID, txt))
		}
		for _, f := range item.CreatedFiles {
			filesSet[f] = struct{}{}
		}
	}
	files := make([]string, 0, len(filesSet))
	for f := range filesSet {
		files = append(files, f)
	}
	sort.Strings(files)

	delete(s.plans, taskID)
	return domain.WorkResultPayload{
		Summary:      strings.Join(summaries, "\n"),
		CreatedFiles: files,
	}, true
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
			if active == 0 {
				failedAcks, err := s.collectFailureAcks(ctx, task.ID)
				if err != nil {
					s.logger.Printf("watchdog read acks task=%s error: %v", task.ID, err)
				} else if len(failedAcks) >= 3 {
					s.blockTaskIfNonFinal(ctx, task.ID, "ack failure threshold reached", map[string]any{
						"failures": failedAcks,
					})
					continue
				}
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

func (s *Service) collectFailureAcks(ctx context.Context, taskID string) ([]map[string]any, error) {
	acks, err := s.store.ListTaskMessageAcks(ctx, taskID, 24)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(acks))
	for _, ack := range acks {
		lc := strings.ToLower(strings.TrimSpace(ack.Result))
		if lc == "" {
			continue
		}
		if strings.Contains(lc, "failed") ||
			strings.Contains(lc, "error") ||
			strings.Contains(lc, "denied") ||
			strings.Contains(lc, "bad payload") ||
			strings.Contains(lc, "no files") ||
			strings.Contains(lc, "empty plan") {
			out = append(out, map[string]any{
				"message_id": ack.MessageID,
				"agent_id":   ack.AgentID,
				"result":     ack.Result,
				"ack_at":     ack.AckAt,
			})
		}
	}
	return out, nil
}

func (s *Service) blockTaskIfNonFinal(ctx context.Context, taskID string, reason string, payload map[string]any) {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return
	}
	if isFinalStatus(task.Status) {
		return
	}
	_ = s.store.UpdateTaskStatus(ctx, taskID, domain.TaskStatusBlocked, reason)
	_ = s.store.LogDecision(ctx, domain.DecisionLog{
		TaskID:  taskID,
		Actor:   orchestratorAgentID,
		Action:  "task_blocked",
		Reason:  reason,
		Payload: mustJSON(payload),
	})
	s.clearTaskPlan(taskID)
}

func (s *Service) clearTaskPlan(taskID string) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	delete(s.plans, taskID)
}

func isFinalStatus(status domain.TaskStatus) bool {
	return status == domain.TaskStatusDone ||
		status == domain.TaskStatusFailed ||
		status == domain.TaskStatusCanceled
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func trimText(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy")
}
