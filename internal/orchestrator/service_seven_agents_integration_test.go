package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"organ_codex/internal/domain"
	"organ_codex/internal/fs"
	"organ_codex/internal/messaging/inproc"
	"organ_codex/internal/policy"
	sqlitestore "organ_codex/internal/store/sqlite"
)

type plannerForSevenAgents struct {
	id        string
	queue     *inproc.Bus
	messenger *Service
	store     *sqlitestore.Store
}

func newPlannerForSevenAgents(queue *inproc.Bus, messenger *Service, store *sqlitestore.Store) *plannerForSevenAgents {
	return &plannerForSevenAgents{
		id:        "planner",
		queue:     queue,
		messenger: messenger,
		store:     store,
	}
}

func (p *plannerForSevenAgents) Start(ctx context.Context) {
	ch := p.queue.Register(p.id)
	go func() {
		defer p.queue.Unregister(p.id)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				p.handleMessage(ctx, msg)
			}
		}
	}()
}

func (p *plannerForSevenAgents) handleMessage(ctx context.Context, msg domain.Message) {
	logDecision(ctx, p.store, msg.TaskID, p.id, "seven_planner_received", "planner received message", map[string]any{
		"type": msg.Type,
		"from": msg.FromAgent,
	})
	if msg.Type != domain.MessageTypeRequest {
		ackWithRetry(ctx, p.store, msg.ID, p.id, "planner ignored unsupported type")
		return
	}
	var req domain.TaskRequestPayload
	if err := json.Unmarshal(msg.Payload, &req); err != nil {
		logDecision(ctx, p.store, msg.TaskID, p.id, "seven_planner_bad_payload", "planner failed to parse request", map[string]any{
			"error": err.Error(),
		})
		ackWithRetry(ctx, p.store, msg.ID, p.id, "bad payload")
		return
	}

	plan := domain.PlanProposalPayload{
		Summary: "seven-agent integration DAG",
		Nodes: []domain.PlanNode{
			{ID: "n1_architect", AgentID: "architect", Type: domain.MessageTypeRequest, Goal: "Design module map", Scope: req.Scope},
			{ID: "n2_backend", AgentID: "backend_coder", Type: domain.MessageTypeRequest, Goal: "Implement backend contracts", Scope: req.Scope, DependsOn: []string{"n1_architect"}},
			{ID: "n3_frontend", AgentID: "frontend_coder", Type: domain.MessageTypeRequest, Goal: "Implement frontend skeleton", Scope: req.Scope, DependsOn: []string{"n2_backend"}},
			{ID: "n4_integrate", AgentID: "integrator", Type: domain.MessageTypeRequest, Goal: "Integrate frontend and backend", Scope: req.Scope, DependsOn: []string{"n3_frontend"}},
			{ID: "n5_qa", AgentID: "qa_reviewer", Type: domain.MessageTypeReview, DependsOn: []string{"n4_integrate"}},
			{ID: "n6_security", AgentID: "security_reviewer", Type: domain.MessageTypeReview, DependsOn: []string{"n5_qa"}},
			{ID: "n7_release", AgentID: "release_manager", Type: domain.MessageTypeRequest, Goal: "Prepare release package", Scope: req.Scope, DependsOn: []string{"n6_security"}},
		},
	}
	payload, _ := json.Marshal(plan)
	if err := p.messenger.EnqueueMessage(ctx, domain.Message{
		TaskID:         msg.TaskID,
		FromAgent:      p.id,
		ToAgent:        "orchestrator",
		Type:           domain.MessageTypePropose,
		Payload:        payload,
		CorrelationID:  msg.CorrelationID,
		IdempotencyKey: "seven-planner-propose-" + msg.ID,
	}); err != nil {
		logDecision(ctx, p.store, msg.TaskID, p.id, "seven_planner_propose_failed", "planner failed to send proposal", map[string]any{
			"error": err.Error(),
		})
		ackWithRetry(ctx, p.store, msg.ID, p.id, "propose failed")
		return
	}
	logDecision(ctx, p.store, msg.TaskID, p.id, "seven_planner_propose_sent", "planner sent seven-agent plan", map[string]any{
		"nodes": len(plan.Nodes),
	})
	ackWithRetry(ctx, p.store, msg.ID, p.id, "plan proposed")
}

type nodeAgent struct {
	id        string
	queue     *inproc.Bus
	messenger *Service
	store     *sqlitestore.Store
	files     *fs.Gateway
}

func newNodeAgent(id string, queue *inproc.Bus, messenger *Service, store *sqlitestore.Store, files *fs.Gateway) *nodeAgent {
	return &nodeAgent{
		id:        id,
		queue:     queue,
		messenger: messenger,
		store:     store,
		files:     files,
	}
}

func (a *nodeAgent) Start(ctx context.Context) {
	ch := a.queue.Register(a.id)
	go func() {
		defer a.queue.Unregister(a.id)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				a.handleMessage(ctx, msg)
			}
		}
	}()
}

func (a *nodeAgent) handleMessage(ctx context.Context, msg domain.Message) {
	logDecision(ctx, a.store, msg.TaskID, a.id, "seven_agent_message_received", "agent received message", map[string]any{
		"type":    msg.Type,
		"from":    msg.FromAgent,
		"message": msg.ID,
	})

	switch msg.Type {
	case domain.MessageTypeRequest:
		var req domain.WorkRequestPayload
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			a.blockAndAck(ctx, msg, "", "bad REQUEST payload: "+err.Error())
			return
		}
		path := fmt.Sprintf("agents/%s/%s.txt", a.id, req.NodeID)
		content := fmt.Sprintf("agent=%s\nnode=%s\ntype=request\ngoal=%s\nscope=%s\n", a.id, req.NodeID, req.Goal, req.Scope)
		if err := retryTransient(5, 25*time.Millisecond, func() error {
			return a.files.WriteFile(ctx, msg.TaskID, a.id, path, []byte(content))
		}); err != nil {
			a.blockAndAck(ctx, msg, req.NodeID, "write failed: "+err.Error())
			return
		}
		logDecision(ctx, a.store, msg.TaskID, a.id, "seven_agent_file_written", "agent wrote request file", map[string]any{
			"path": path,
			"node": req.NodeID,
		})
		a.sendDoneAndAck(ctx, msg, req.NodeID, "request completed by "+a.id, []string{path})
	case domain.MessageTypeReview:
		var req domain.ReviewRequestPayload
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			a.blockAndAck(ctx, msg, "", "bad REVIEW payload: "+err.Error())
			return
		}
		path := fmt.Sprintf("agents/%s/%s.txt", a.id, req.NodeID)
		content := fmt.Sprintf("agent=%s\nnode=%s\ntype=review\nfiles=%d\nsummary=%s\n", a.id, req.NodeID, len(req.CreatedFiles), req.Summary)
		if err := retryTransient(5, 25*time.Millisecond, func() error {
			return a.files.WriteFile(ctx, msg.TaskID, a.id, path, []byte(content))
		}); err != nil {
			a.blockAndAck(ctx, msg, req.NodeID, "write failed: "+err.Error())
			return
		}
		logDecision(ctx, a.store, msg.TaskID, a.id, "seven_agent_file_written", "agent wrote review file", map[string]any{
			"path": path,
			"node": req.NodeID,
		})
		created := append([]string{}, req.CreatedFiles...)
		created = append(created, path)
		a.sendDoneAndAck(ctx, msg, req.NodeID, "review completed by "+a.id, created)
	default:
		ackWithRetry(ctx, a.store, msg.ID, a.id, "ignored message type")
	}
}

func (a *nodeAgent) sendDoneAndAck(ctx context.Context, msg domain.Message, nodeID string, summary string, createdFiles []string) {
	donePayload, _ := json.Marshal(domain.WorkResultPayload{
		NodeID:       nodeID,
		Summary:      summary,
		CreatedFiles: createdFiles,
	})
	if err := retryTransient(5, 25*time.Millisecond, func() error {
		return a.messenger.EnqueueMessage(ctx, domain.Message{
			TaskID:         msg.TaskID,
			FromAgent:      a.id,
			ToAgent:        "orchestrator",
			Type:           domain.MessageTypeDone,
			Payload:        donePayload,
			CorrelationID:  msg.CorrelationID,
			IdempotencyKey: "seven-done-" + a.id + "-" + msg.ID,
		})
	}); err != nil {
		a.blockAndAck(ctx, msg, nodeID, "send DONE failed: "+err.Error())
		return
	}
	logDecision(ctx, a.store, msg.TaskID, a.id, "seven_agent_done_sent", "agent sent DONE", map[string]any{
		"node":  nodeID,
		"files": len(createdFiles),
	})
	ackWithRetry(ctx, a.store, msg.ID, a.id, "done sent")
}

func (a *nodeAgent) blockAndAck(ctx context.Context, msg domain.Message, nodeID string, reason string) {
	payload, _ := json.Marshal(map[string]string{
		"reason":  reason,
		"node_id": nodeID,
	})
	_ = retryTransient(3, 25*time.Millisecond, func() error {
		return a.messenger.EnqueueMessage(ctx, domain.Message{
			TaskID:         msg.TaskID,
			FromAgent:      a.id,
			ToAgent:        "orchestrator",
			Type:           domain.MessageTypeBlocked,
			Payload:        payload,
			CorrelationID:  msg.CorrelationID,
			IdempotencyKey: "seven-blocked-" + a.id + "-" + msg.ID,
		})
	})
	logDecision(ctx, a.store, msg.TaskID, a.id, "seven_agent_blocked_sent", "agent sent BLOCKED", map[string]any{
		"reason": reason,
		"node":   nodeID,
	})
	ackWithRetry(ctx, a.store, msg.ID, a.id, "blocked")
}

func logDecision(ctx context.Context, store *sqlitestore.Store, taskID, actor, action, reason string, payload any) {
	raw := []byte("{}")
	if payload != nil {
		encoded, _ := json.Marshal(payload)
		raw = encoded
	}
	_ = retryTransient(5, 20*time.Millisecond, func() error {
		return store.LogDecision(ctx, domain.DecisionLog{
			TaskID:  taskID,
			Actor:   actor,
			Action:  action,
			Reason:  reason,
			Payload: raw,
		})
	})
}

func TestSevenAgents_FullInteractionAndLogging(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	workspaceRoot := filepath.Join(dir, "workspace")

	store, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate store: %v", err)
	}

	bus := inproc.New(256)
	policyEngine := policy.New(store)
	files, err := fs.NewGateway(workspaceRoot, policyEngine, store)
	if err != nil {
		t.Fatalf("create gateway: %v", err)
	}

	svc := New(store, policyEngine, bus, Config{
		DispatchInterval: 5 * time.Millisecond,
		WatchdogInterval: 30 * time.Second,
		RetryDelay:       20 * time.Millisecond,
		MaxRetries:       8,
		IdleTimeout:      15 * time.Second,
		DispatchLease:    10 * time.Second,
		AckTimeout:       30 * time.Second,
		DefaultMaxHops:   50,
	}, log.New(io.Discard, "", 0))

	runCtx, cancel := context.WithCancel(context.Background())
	svc.Start(runCtx)
	defer svc.Wait()
	defer cancel()

	planner := newPlannerForSevenAgents(bus, svc, store)
	planner.Start(runCtx)

	agentIDs := []string{
		"architect",
		"backend_coder",
		"frontend_coder",
		"integrator",
		"qa_reviewer",
		"security_reviewer",
		"release_manager",
	}
	for _, id := range agentIDs {
		newNodeAgent(id, bus, svc, store, files).Start(runCtx)
	}

	task, err := svc.CreateTask(context.Background(), CreateTaskInput{
		Goal:       "Build and validate release",
		Scope:      "multi-agent integration",
		OwnerAgent: "planner",
		MaxHops:    50,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	for _, id := range agentIDs {
		for _, op := range []domain.FileOperation{domain.FileOperationCreate, domain.FileOperationWrite} {
			if err := svc.GrantPermission(context.Background(), domain.TaskPermission{
				TaskID:      task.ID,
				AgentID:     id,
				Effect:      domain.PermissionEffectAllow,
				Operation:   op,
				PathPattern: "agents/**",
			}); err != nil {
				t.Fatalf("grant permission agent=%s op=%s: %v", id, op, err)
			}
		}
	}

	if err := svc.GrantChannel(context.Background(), domain.AgentChannel{
		TaskID:       task.ID,
		FromAgent:    "planner",
		ToAgent:      "orchestrator",
		AllowedTypes: []domain.MessageType{domain.MessageTypePropose, domain.MessageTypeEscalate},
		MaxMsgs:      20,
	}); err != nil {
		t.Fatalf("grant planner channel: %v", err)
	}
	for _, id := range agentIDs {
		if err := svc.GrantChannel(context.Background(), domain.AgentChannel{
			TaskID:       task.ID,
			FromAgent:    id,
			ToAgent:      "orchestrator",
			AllowedTypes: []domain.MessageType{domain.MessageTypeDone, domain.MessageTypeBlocked},
			MaxMsgs:      20,
		}); err != nil {
			t.Fatalf("grant channel for %s: %v", id, err)
		}
	}

	if err := svc.StartTask(context.Background(), task.ID); err != nil {
		t.Fatalf("start task: %v", err)
	}

	status := waitTaskToFinalStatus(t, store, task.ID, 12*time.Second)
	if status != domain.TaskStatusDone {
		msgs, err := store.ListTaskMessages(context.Background(), task.ID, 500)
		if err != nil {
			t.Fatalf("status=%s and list messages failed: %v", status, err)
		}
		allDone := true
		for _, id := range agentIDs {
			hasDone := false
			for _, msg := range msgs {
				if msg.FromAgent == id && msg.ToAgent == "orchestrator" && msg.Type == domain.MessageTypeDone {
					hasDone = true
					break
				}
			}
			if !hasDone {
				allDone = false
				break
			}
		}
		if !allDone {
			t.Fatalf("status=%s want=%s debug=%s", status, domain.TaskStatusDone, taskDebugSnapshot(t, store, task.ID))
		}
	}

	expectedFiles := map[string]string{
		"architect":         "agents/architect/n1_architect.txt",
		"backend_coder":     "agents/backend_coder/n2_backend.txt",
		"frontend_coder":    "agents/frontend_coder/n3_frontend.txt",
		"integrator":        "agents/integrator/n4_integrate.txt",
		"qa_reviewer":       "agents/qa_reviewer/n5_qa.txt",
		"security_reviewer": "agents/security_reviewer/n6_security.txt",
		"release_manager":   "agents/release_manager/n7_release.txt",
	}

	for agentID, rel := range expectedFiles {
		raw, err := os.ReadFile(filepath.Join(workspaceRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read file for %s (%s): %v", agentID, rel, err)
		}
		if !strings.Contains(string(raw), "agent="+agentID) {
			t.Fatalf("file %s does not contain agent marker", rel)
		}
	}

	msgs, err := store.ListTaskMessages(context.Background(), task.ID, 500)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	decisions, err := store.ListTaskDecisions(context.Background(), task.ID, 1000)
	if err != nil {
		t.Fatalf("list decisions: %v", err)
	}

	assertAgentMessageFlow(t, msgs, "planner", domain.MessageTypeRequest, domain.MessageTypePropose)
	for _, id := range agentIDs {
		assertAgentMessageFlow(t, msgs, id, domain.MessageTypeRequest, domain.MessageTypeDone, domain.MessageTypeReview)
		assertDecisionByActorAction(t, decisions, id, "seven_agent_message_received")
		assertDecisionByActorAction(t, decisions, id, "seven_agent_file_written")
		assertDecisionByActorAction(t, decisions, id, "seven_agent_done_sent")
	}
	assertDecisionByActorAction(t, decisions, "planner", "seven_planner_propose_sent")
}

func waitTaskToFinalStatus(t *testing.T, store *sqlitestore.Store, taskID string, timeout time.Duration) domain.TaskStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := store.GetTask(context.Background(), taskID)
		if err == nil && (task.Status == domain.TaskStatusDone || task.Status == domain.TaskStatusBlocked || task.Status == domain.TaskStatusFailed || task.Status == domain.TaskStatusCanceled) {
			return task.Status
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("get task after timeout: %v", err)
	}
	return task.Status
}

func assertAgentMessageFlow(t *testing.T, msgs []domain.Message, agentID string, inboundType domain.MessageType, outboundType domain.MessageType, optionalInbound ...domain.MessageType) {
	t.Helper()
	var hasInbound bool
	var hasOutbound bool
	allowedInbound := map[domain.MessageType]struct{}{
		inboundType: {},
	}
	for _, typ := range optionalInbound {
		allowedInbound[typ] = struct{}{}
	}
	for _, msg := range msgs {
		if msg.ToAgent == agentID {
			if _, ok := allowedInbound[msg.Type]; ok {
				hasInbound = true
			}
		}
		if msg.FromAgent == agentID && msg.ToAgent == "orchestrator" && msg.Type == outboundType {
			hasOutbound = true
		}
	}
	if !hasInbound {
		t.Fatalf("agent=%s does not have expected inbound message", agentID)
	}
	if !hasOutbound {
		t.Fatalf("agent=%s does not have expected outbound message type=%s", agentID, outboundType)
	}
}

func assertDecisionByActorAction(t *testing.T, items []domain.DecisionLog, actor string, action string) {
	t.Helper()
	for _, item := range items {
		if item.Actor == actor && item.Action == action {
			return
		}
	}
	t.Fatalf("missing decision actor=%s action=%s", actor, action)
}

func retryTransient(attempts int, delay time.Duration, fn func() error) error {
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := fn(); err != nil {
			lastErr = err
			if !isTransientErr(err) || i == attempts-1 {
				return err
			}
			time.Sleep(delay)
			continue
		}
		return nil
	}
	return lastErr
}

func ackWithRetry(ctx context.Context, store *sqlitestore.Store, messageID, agentID, result string) {
	_ = retryTransient(5, 20*time.Millisecond, func() error {
		return store.AckMessage(ctx, messageID, agentID, result)
	})
}

func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "busy")
}

func taskDebugSnapshot(t *testing.T, store *sqlitestore.Store, taskID string) string {
	t.Helper()
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		return "get_task_error=" + err.Error()
	}
	msgs, err := store.ListTaskMessages(context.Background(), taskID, 200)
	if err != nil {
		return "list_messages_error=" + err.Error()
	}
	decisions, err := store.ListTaskDecisions(context.Background(), taskID, 200)
	if err != nil {
		return "list_decisions_error=" + err.Error()
	}
	msgParts := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		msgParts = append(msgParts, fmt.Sprintf("%s:%s->%s:%s:%s:a%d", msg.ID, msg.FromAgent, msg.ToAgent, msg.Type, msg.Status, msg.Attempts))
	}
	decisionParts := make([]string, 0, len(decisions))
	for _, item := range decisions {
		decisionParts = append(decisionParts, fmt.Sprintf("%s:%s:%s", item.Actor, item.Action, item.Reason))
	}
	return fmt.Sprintf("task_status=%s last_error=%s messages=%s decisions=%s",
		task.Status,
		task.LastError,
		strings.Join(msgParts, "|"),
		strings.Join(decisionParts, "|"),
	)
}
