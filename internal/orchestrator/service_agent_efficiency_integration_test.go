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

type efficiencyScenario struct {
	Name                   string
	Goal                   string
	Scope                  string
	Roles                  []efficiencyRole
	ParallelNodeIDs        []string
	MaxDuration            time.Duration
	MaxMessagesPerWorker   float64
	MaxParallelDispatchGap time.Duration
}

type efficiencyRole struct {
	NodeID     string
	AgentID    string
	NodeType   domain.MessageType
	Goal       string
	DependsOn  []string
	Delay      time.Duration
	OutputPath string
}

type plannerForEfficiency struct {
	id       string
	queue    *inproc.Bus
	svc      *Service
	store    *sqlitestore.Store
	scenario efficiencyScenario
}

func newPlannerForEfficiency(queue *inproc.Bus, svc *Service, store *sqlitestore.Store, scenario efficiencyScenario) *plannerForEfficiency {
	return &plannerForEfficiency{
		id:       "planner",
		queue:    queue,
		svc:      svc,
		store:    store,
		scenario: scenario,
	}
}

func (p *plannerForEfficiency) Start(ctx context.Context) {
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

func (p *plannerForEfficiency) handleMessage(ctx context.Context, msg domain.Message) {
	logDecision(ctx, p.store, msg.TaskID, p.id, "eff_planner_message_received", "planner received message", map[string]any{
		"type": msg.Type,
		"from": msg.FromAgent,
	})
	if msg.Type != domain.MessageTypeRequest {
		ackWithRetry(ctx, p.store, msg.ID, p.id, "ignored unsupported type")
		return
	}

	var req domain.TaskRequestPayload
	if err := json.Unmarshal(msg.Payload, &req); err != nil {
		logDecision(ctx, p.store, msg.TaskID, p.id, "eff_planner_bad_payload", "planner failed to parse payload", map[string]any{
			"error": err.Error(),
		})
		ackWithRetry(ctx, p.store, msg.ID, p.id, "bad payload")
		return
	}

	nodes := make([]domain.PlanNode, 0, len(p.scenario.Roles))
	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		scope = p.scenario.Scope
	}
	for _, role := range p.scenario.Roles {
		node := domain.PlanNode{
			ID:        role.NodeID,
			AgentID:   role.AgentID,
			Type:      role.NodeType,
			Goal:      role.Goal,
			Scope:     scope,
			DependsOn: append([]string{}, role.DependsOn...),
		}
		nodes = append(nodes, node)
	}
	proposal := domain.PlanProposalPayload{
		Summary: "efficiency scenario " + p.scenario.Name,
		Nodes:   nodes,
	}
	payload, _ := json.Marshal(proposal)
	if err := p.svc.EnqueueMessage(ctx, domain.Message{
		TaskID:         msg.TaskID,
		FromAgent:      p.id,
		ToAgent:        "orchestrator",
		Type:           domain.MessageTypePropose,
		Payload:        payload,
		CorrelationID:  msg.CorrelationID,
		IdempotencyKey: "eff-planner-propose-" + msg.ID,
	}); err != nil {
		logDecision(ctx, p.store, msg.TaskID, p.id, "eff_planner_propose_failed", "planner failed to send proposal", map[string]any{
			"error": err.Error(),
		})
		ackWithRetry(ctx, p.store, msg.ID, p.id, "proposal send failed")
		return
	}
	logDecision(ctx, p.store, msg.TaskID, p.id, "eff_planner_propose_sent", "planner sent efficiency plan", map[string]any{
		"nodes": len(nodes),
	})
	ackWithRetry(ctx, p.store, msg.ID, p.id, "proposed")
}

type efficiencyNodeAgent struct {
	role         efficiencyRole
	scenarioName string
	queue        *inproc.Bus
	svc          *Service
	store        *sqlitestore.Store
	files        *fs.Gateway
}

func newEfficiencyNodeAgent(role efficiencyRole, scenarioName string, queue *inproc.Bus, svc *Service, store *sqlitestore.Store, files *fs.Gateway) *efficiencyNodeAgent {
	return &efficiencyNodeAgent{
		role:         role,
		scenarioName: scenarioName,
		queue:        queue,
		svc:          svc,
		store:        store,
		files:        files,
	}
}

func (a *efficiencyNodeAgent) Start(ctx context.Context) {
	ch := a.queue.Register(a.role.AgentID)
	go func() {
		defer a.queue.Unregister(a.role.AgentID)
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

func (a *efficiencyNodeAgent) handleMessage(ctx context.Context, msg domain.Message) {
	logDecision(ctx, a.store, msg.TaskID, a.role.AgentID, "eff_agent_message_received", "agent received message", map[string]any{
		"type":    msg.Type,
		"message": msg.ID,
		"from":    msg.FromAgent,
	})

	switch msg.Type {
	case domain.MessageTypeRequest:
		var req domain.WorkRequestPayload
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			a.blockAndAck(ctx, msg, "bad REQUEST payload: "+err.Error(), "")
			return
		}
		a.processAndSendDone(ctx, msg, req.NodeID, req.Goal, req.Scope, nil, "request completed")
	case domain.MessageTypeReview:
		var req domain.ReviewRequestPayload
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			a.blockAndAck(ctx, msg, "bad REVIEW payload: "+err.Error(), "")
			return
		}
		a.processAndSendDone(ctx, msg, req.NodeID, req.Summary, "review", req.CreatedFiles, "review completed")
	default:
		ackWithRetry(ctx, a.store, msg.ID, a.role.AgentID, "ignored message type")
	}
}

func (a *efficiencyNodeAgent) processAndSendDone(ctx context.Context, msg domain.Message, nodeID string, goal string, scope string, inheritedFiles []string, summaryPrefix string) {
	if nodeID == "" {
		nodeID = a.role.NodeID
	}
	if err := waitWithContext(ctx, a.role.Delay); err != nil {
		a.blockAndAck(ctx, msg, "context canceled before completion", nodeID)
		return
	}

	content := fmt.Sprintf("scenario=%s\nagent=%s\nnode=%s\ngoal=%s\nscope=%s\n", a.scenarioName, a.role.AgentID, nodeID, goal, scope)
	if err := retryTransient(5, 25*time.Millisecond, func() error {
		return a.files.WriteFile(ctx, msg.TaskID, a.role.AgentID, a.role.OutputPath, []byte(content))
	}); err != nil {
		a.blockAndAck(ctx, msg, "write failed: "+err.Error(), nodeID)
		return
	}
	logDecision(ctx, a.store, msg.TaskID, a.role.AgentID, "eff_agent_file_written", "agent wrote scenario artifact", map[string]any{
		"path": a.role.OutputPath,
		"node": nodeID,
	})

	createdFiles := append([]string{}, inheritedFiles...)
	createdFiles = append(createdFiles, a.role.OutputPath)

	donePayload, _ := json.Marshal(domain.WorkResultPayload{
		NodeID:       nodeID,
		Summary:      summaryPrefix + " by " + a.role.AgentID,
		CreatedFiles: createdFiles,
	})
	if err := retryTransient(5, 25*time.Millisecond, func() error {
		return a.svc.EnqueueMessage(ctx, domain.Message{
			TaskID:         msg.TaskID,
			FromAgent:      a.role.AgentID,
			ToAgent:        "orchestrator",
			Type:           domain.MessageTypeDone,
			Payload:        donePayload,
			CorrelationID:  msg.CorrelationID,
			IdempotencyKey: "eff-done-" + a.role.AgentID + "-" + msg.ID,
		})
	}); err != nil {
		a.blockAndAck(ctx, msg, "send DONE failed: "+err.Error(), nodeID)
		return
	}
	logDecision(ctx, a.store, msg.TaskID, a.role.AgentID, "eff_agent_done_sent", "agent sent DONE", map[string]any{
		"node": nodeID,
	})
	ackWithRetry(ctx, a.store, msg.ID, a.role.AgentID, "done sent")
}

func (a *efficiencyNodeAgent) blockAndAck(ctx context.Context, msg domain.Message, reason string, nodeID string) {
	payload, _ := json.Marshal(map[string]string{
		"reason":  reason,
		"node_id": nodeID,
	})
	_ = retryTransient(3, 25*time.Millisecond, func() error {
		return a.svc.EnqueueMessage(ctx, domain.Message{
			TaskID:         msg.TaskID,
			FromAgent:      a.role.AgentID,
			ToAgent:        "orchestrator",
			Type:           domain.MessageTypeBlocked,
			Payload:        payload,
			CorrelationID:  msg.CorrelationID,
			IdempotencyKey: "eff-blocked-" + a.role.AgentID + "-" + msg.ID,
		})
	})
	logDecision(ctx, a.store, msg.TaskID, a.role.AgentID, "eff_agent_blocked_sent", "agent sent BLOCKED", map[string]any{
		"node":   nodeID,
		"reason": reason,
	})
	ackWithRetry(ctx, a.store, msg.ID, a.role.AgentID, "blocked")
}

func waitWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func TestAgentEfficiency_MultiTaskInteraction_5to7Agents(t *testing.T) {
	for _, scenario := range buildEfficiencyScenarios() {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			runAgentEfficiencyScenario(t, scenario)
		})
	}
}

func runAgentEfficiencyScenario(t *testing.T, scenario efficiencyScenario) {
	t.Helper()

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
		IdleTimeout:      12 * time.Second,
		DispatchLease:    10 * time.Second,
		AckTimeout:       30 * time.Second,
		DefaultMaxHops:   64,
	}, log.New(io.Discard, "", 0))

	runCtx, cancel := context.WithCancel(context.Background())
	svc.Start(runCtx)
	defer svc.Wait()
	defer cancel()

	newPlannerForEfficiency(bus, svc, store, scenario).Start(runCtx)
	for _, role := range scenario.Roles {
		newEfficiencyNodeAgent(role, scenario.Name, bus, svc, store, files).Start(runCtx)
	}

	task, err := svc.CreateTask(context.Background(), CreateTaskInput{
		Goal:       scenario.Goal,
		Scope:      scenario.Scope,
		OwnerAgent: "planner",
		MaxHops:    64,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	for _, role := range scenario.Roles {
		for _, op := range []domain.FileOperation{domain.FileOperationCreate, domain.FileOperationWrite} {
			if err := svc.GrantPermission(context.Background(), domain.TaskPermission{
				TaskID:      task.ID,
				AgentID:     role.AgentID,
				Effect:      domain.PermissionEffectAllow,
				Operation:   op,
				PathPattern: "deliverables/**",
			}); err != nil {
				t.Fatalf("grant permission agent=%s op=%s: %v", role.AgentID, op, err)
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
	for _, role := range scenario.Roles {
		if err := svc.GrantChannel(context.Background(), domain.AgentChannel{
			TaskID:       task.ID,
			FromAgent:    role.AgentID,
			ToAgent:      "orchestrator",
			AllowedTypes: []domain.MessageType{domain.MessageTypeDone, domain.MessageTypeBlocked},
			MaxMsgs:      20,
		}); err != nil {
			t.Fatalf("grant channel for %s: %v", role.AgentID, err)
		}
	}

	startedAt := time.Now()
	if err := svc.StartTask(context.Background(), task.ID); err != nil {
		t.Fatalf("start task: %v", err)
	}

	expectedMessages := 2 + 2*len(scenario.Roles)
	var msgs []domain.Message
	var acks []domain.MessageAck
	var decisions []domain.DecisionLog
	if err := waitForCondition(12*time.Second, 60*time.Millisecond, func() (bool, error) {
		currentMsgs, err := store.ListTaskMessages(context.Background(), task.ID, 800)
		if err != nil {
			return false, err
		}
		currentAcks, err := store.ListTaskMessageAcks(context.Background(), task.ID, 800)
		if err != nil {
			return false, err
		}
		currentDecisions, err := store.ListTaskDecisions(context.Background(), task.ID, 1200)
		if err != nil {
			return false, err
		}
		msgs = currentMsgs
		acks = currentAcks
		decisions = currentDecisions
		if len(msgs) < expectedMessages {
			return false, nil
		}
		workerDoneCount := countWorkerDoneMessages(msgs, scenario.Roles)
		if workerDoneCount != len(scenario.Roles) {
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Fatalf("scenario=%s did not converge: %v debug=%s", scenario.Name, err, taskDebugSnapshot(t, store, task.ID))
	}
	elapsed := time.Since(startedAt)
	if elapsed > scenario.MaxDuration {
		t.Fatalf("scenario=%s elapsed=%s exceeds max=%s", scenario.Name, elapsed, scenario.MaxDuration)
	}
	status := waitTaskToFinalStatus(t, store, task.ID, 2*time.Second)

	for _, role := range scenario.Roles {
		raw, err := os.ReadFile(filepath.Join(workspaceRoot, filepath.FromSlash(role.OutputPath)))
		if err != nil {
			t.Fatalf("read output for %s (%s): %v", role.AgentID, role.OutputPath, err)
		}
		content := string(raw)
		if !strings.Contains(content, "agent="+role.AgentID) {
			t.Fatalf("output %s missing agent marker", role.OutputPath)
		}
		if !strings.Contains(content, "node="+role.NodeID) {
			t.Fatalf("output %s missing node marker", role.OutputPath)
		}
	}

	if len(msgs) < expectedMessages || len(msgs) > expectedMessages+2 {
		t.Fatalf("scenario=%s messages=%d expected range=[%d,%d]", scenario.Name, len(msgs), expectedMessages, expectedMessages+2)
	}
	ackCoverage := float64(len(acks)) / float64(len(msgs))
	if ackCoverage < 0.45 {
		t.Fatalf("scenario=%s ack coverage=%.2f below 0.45", scenario.Name, ackCoverage)
	}

	var proposeCount int
	var workerDoneCount int
	var blockedCount int
	var retriedCount int
	var unsettledCount int
	unsettledDetails := make([]string, 0, 4)
	roleByAgent := make(map[string]efficiencyRole, len(scenario.Roles))
	for _, role := range scenario.Roles {
		roleByAgent[role.AgentID] = role
	}
	for _, msg := range msgs {
		if msg.FromAgent == "planner" && msg.ToAgent == "orchestrator" && msg.Type == domain.MessageTypePropose {
			proposeCount++
		}
		if _, ok := roleByAgent[msg.FromAgent]; ok && msg.ToAgent == "orchestrator" && msg.Type == domain.MessageTypeDone {
			workerDoneCount++
		}
		if msg.Type == domain.MessageTypeBlocked || msg.Type == domain.MessageTypeEscalate {
			blockedCount++
		}
		if msg.Attempts > 1 {
			retriedCount++
		}
		if msg.Status == domain.MessageStatusPending || msg.Status == domain.MessageStatusDispatching || msg.Status == domain.MessageStatusFailed {
			unsettledCount++
			unsettledDetails = append(unsettledDetails, fmt.Sprintf("%s:%s->%s:%s:%s", msg.ID, msg.FromAgent, msg.ToAgent, msg.Type, msg.Status))
		}
	}
	if proposeCount != 1 {
		t.Fatalf("scenario=%s planner propose count=%d want=1", scenario.Name, proposeCount)
	}
	if workerDoneCount != len(scenario.Roles) {
		t.Fatalf("scenario=%s worker DONE count=%d want=%d", scenario.Name, workerDoneCount, len(scenario.Roles))
	}
	if blockedCount != 0 {
		t.Fatalf("scenario=%s has blocked/escalated messages=%d", scenario.Name, blockedCount)
	}
	if retriedCount != 0 {
		t.Fatalf("scenario=%s retried messages=%d, expected retry-free interaction", scenario.Name, retriedCount)
	}
	if unsettledCount != 0 {
		t.Fatalf("scenario=%s unsettled messages=%d details=%s debug=%s", scenario.Name, unsettledCount, strings.Join(unsettledDetails, "|"), taskDebugSnapshot(t, store, task.ID))
	}

	assertAgentMessageFlow(t, msgs, "planner", domain.MessageTypeRequest, domain.MessageTypePropose)
	for _, role := range scenario.Roles {
		assertAgentMessageFlow(t, msgs, role.AgentID, role.NodeType, domain.MessageTypeDone)
		assertDecisionByActorAction(t, decisions, role.AgentID, "eff_agent_message_received")
		assertDecisionByActorAction(t, decisions, role.AgentID, "eff_agent_file_written")
		assertDecisionByActorAction(t, decisions, role.AgentID, "eff_agent_done_sent")
	}
	assertDecisionByActorAction(t, decisions, "planner", "eff_planner_propose_sent")

	assertParallelDispatchBeforeWorkerCompletion(t, scenario, msgs)

	messagesPerWorker := float64(len(msgs)) / float64(len(scenario.Roles))
	if messagesPerWorker > scenario.MaxMessagesPerWorker {
		t.Fatalf("scenario=%s messages/worker=%.2f exceeds max=%.2f", scenario.Name, messagesPerWorker, scenario.MaxMessagesPerWorker)
	}

	t.Logf("scenario=%s agents=%d status=%s elapsed=%s messages=%d acks=%d ack_coverage=%.2f messages_per_worker=%.2f",
		scenario.Name,
		len(scenario.Roles),
		status,
		elapsed,
		len(msgs),
		len(acks),
		ackCoverage,
		messagesPerWorker,
	)
}

func waitForCondition(timeout time.Duration, pollInterval time.Duration, fn func() (bool, error)) error {
	if pollInterval <= 0 {
		pollInterval = 20 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, err := fn()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		time.Sleep(pollInterval)
	}
	ok, err := fn()
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return fmt.Errorf("condition timeout after %s", timeout)
}

func countWorkerDoneMessages(msgs []domain.Message, roles []efficiencyRole) int {
	roleByAgent := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		roleByAgent[role.AgentID] = struct{}{}
	}
	var count int
	for _, msg := range msgs {
		if msg.ToAgent != "orchestrator" || msg.Type != domain.MessageTypeDone {
			continue
		}
		if _, ok := roleByAgent[msg.FromAgent]; ok {
			count++
		}
	}
	return count
}

func assertParallelDispatchBeforeWorkerCompletion(t *testing.T, scenario efficiencyScenario, msgs []domain.Message) {
	t.Helper()
	if len(scenario.ParallelNodeIDs) == 0 {
		return
	}

	agentByNode := make(map[string]string, len(scenario.Roles))
	for _, role := range scenario.Roles {
		agentByNode[role.NodeID] = role.AgentID
	}

	parallelAgents := make(map[string]struct{}, len(scenario.ParallelNodeIDs))
	for _, nodeID := range scenario.ParallelNodeIDs {
		agentID, ok := agentByNode[nodeID]
		if !ok {
			t.Fatalf("scenario=%s missing parallel node=%s in role map", scenario.Name, nodeID)
		}
		parallelAgents[agentID] = struct{}{}
	}

	dispatchTimes := make(map[string]time.Time, len(parallelAgents))
	var firstDone time.Time
	for _, msg := range msgs {
		if _, ok := parallelAgents[msg.ToAgent]; ok && msg.FromAgent == "orchestrator" &&
			(msg.Type == domain.MessageTypeRequest || msg.Type == domain.MessageTypeReview) {
			if existing, seen := dispatchTimes[msg.ToAgent]; !seen || msg.CreatedAt.Before(existing) {
				dispatchTimes[msg.ToAgent] = msg.CreatedAt
			}
		}
		if _, ok := parallelAgents[msg.FromAgent]; ok && msg.ToAgent == "orchestrator" && msg.Type == domain.MessageTypeDone {
			if firstDone.IsZero() || msg.CreatedAt.Before(firstDone) {
				firstDone = msg.CreatedAt
			}
		}
	}

	if len(dispatchTimes) != len(parallelAgents) {
		t.Fatalf("scenario=%s parallel dispatch count=%d want=%d", scenario.Name, len(dispatchTimes), len(parallelAgents))
	}
	if firstDone.IsZero() {
		t.Fatalf("scenario=%s missing DONE from parallel agents", scenario.Name)
	}

	var minDispatch time.Time
	var maxDispatch time.Time
	for _, ts := range dispatchTimes {
		if minDispatch.IsZero() || ts.Before(minDispatch) {
			minDispatch = ts
		}
		if maxDispatch.IsZero() || ts.After(maxDispatch) {
			maxDispatch = ts
		}
		if ts.After(firstDone) {
			t.Fatalf("scenario=%s parallel node dispatched at %s after first DONE at %s", scenario.Name, ts, firstDone)
		}
	}
	if maxDispatch.Sub(minDispatch) > scenario.MaxParallelDispatchGap {
		t.Fatalf("scenario=%s parallel dispatch gap=%s exceeds max=%s", scenario.Name, maxDispatch.Sub(minDispatch), scenario.MaxParallelDispatchGap)
	}
}

func buildEfficiencyScenarios() []efficiencyScenario {
	return []efficiencyScenario{
		{
			Name:                   "agents_5_html_tetris_conditional",
			Goal:                   "Create conditional Tetris in HTML with keyboard controls and score tracking",
			Scope:                  "static html/css/js bundle for browser",
			MaxDuration:            8 * time.Second,
			MaxMessagesPerWorker:   3.0,
			MaxParallelDispatchGap: 350 * time.Millisecond,
			ParallelNodeIDs:        []string{"n2_gameplay", "n3_ui"},
			Roles: []efficiencyRole{
				{NodeID: "n1_architecture", AgentID: "architect", NodeType: domain.MessageTypeRequest, Goal: "Design Tetris module boundaries", Delay: 90 * time.Millisecond, OutputPath: "deliverables/tetris/architecture.md"},
				{NodeID: "n2_gameplay", AgentID: "gameplay_engineer", NodeType: domain.MessageTypeRequest, Goal: "Implement conditional tetromino movement rules in JS", DependsOn: []string{"n1_architecture"}, Delay: 140 * time.Millisecond, OutputPath: "deliverables/tetris/gameplay.js"},
				{NodeID: "n3_ui", AgentID: "ui_engineer", NodeType: domain.MessageTypeRequest, Goal: "Build HTML board and control panel", DependsOn: []string{"n1_architecture"}, Delay: 140 * time.Millisecond, OutputPath: "deliverables/tetris/index.html"},
				{NodeID: "n4_integrate", AgentID: "integrator", NodeType: domain.MessageTypeRequest, Goal: "Connect UI events with gameplay state", DependsOn: []string{"n2_gameplay", "n3_ui"}, Delay: 120 * time.Millisecond, OutputPath: "deliverables/tetris/bootstrap.js"},
				{NodeID: "n5_qa", AgentID: "qa_reviewer", NodeType: domain.MessageTypeReview, DependsOn: []string{"n4_integrate"}, Delay: 90 * time.Millisecond, OutputPath: "deliverables/tetris/qa_report.md"},
			},
		},
		{
			Name:                   "agents_6_html_kanban_other_task",
			Goal:                   "Build Kanban board in HTML with drag-drop and local state persistence",
			Scope:                  "single-page app with reusable components",
			MaxDuration:            9 * time.Second,
			MaxMessagesPerWorker:   3.0,
			MaxParallelDispatchGap: 350 * time.Millisecond,
			ParallelNodeIDs:        []string{"n2_data", "n3_frontend"},
			Roles: []efficiencyRole{
				{NodeID: "n1_architecture", AgentID: "architect", NodeType: domain.MessageTypeRequest, Goal: "Define domain model and board layout", Delay: 90 * time.Millisecond, OutputPath: "deliverables/kanban/architecture.md"},
				{NodeID: "n2_data", AgentID: "data_engineer", NodeType: domain.MessageTypeRequest, Goal: "Implement storage adapter and card schema", DependsOn: []string{"n1_architecture"}, Delay: 140 * time.Millisecond, OutputPath: "deliverables/kanban/data.js"},
				{NodeID: "n3_frontend", AgentID: "frontend_engineer", NodeType: domain.MessageTypeRequest, Goal: "Implement drag-drop HTML interactions", DependsOn: []string{"n1_architecture"}, Delay: 140 * time.Millisecond, OutputPath: "deliverables/kanban/index.html"},
				{NodeID: "n4_integrate", AgentID: "integrator", NodeType: domain.MessageTypeRequest, Goal: "Wire board state to interactions", DependsOn: []string{"n2_data", "n3_frontend"}, Delay: 120 * time.Millisecond, OutputPath: "deliverables/kanban/app.js"},
				{NodeID: "n5_qa", AgentID: "qa_reviewer", NodeType: domain.MessageTypeReview, DependsOn: []string{"n4_integrate"}, Delay: 90 * time.Millisecond, OutputPath: "deliverables/kanban/qa_report.md"},
				{NodeID: "n6_security", AgentID: "security_reviewer", NodeType: domain.MessageTypeReview, DependsOn: []string{"n5_qa"}, Delay: 90 * time.Millisecond, OutputPath: "deliverables/kanban/security_report.md"},
			},
		},
		{
			Name:                   "agents_7_docs_portal_other_task",
			Goal:                   "Create multi-page HTML docs portal with search and release package",
			Scope:                  "docs site plus release artifact manifest",
			MaxDuration:            10 * time.Second,
			MaxMessagesPerWorker:   3.0,
			MaxParallelDispatchGap: 400 * time.Millisecond,
			ParallelNodeIDs:        []string{"n2_backend", "n3_frontend"},
			Roles: []efficiencyRole{
				{NodeID: "n1_architecture", AgentID: "architect", NodeType: domain.MessageTypeRequest, Goal: "Design docs information architecture", Delay: 90 * time.Millisecond, OutputPath: "deliverables/docs/architecture.md"},
				{NodeID: "n2_backend", AgentID: "backend_engineer", NodeType: domain.MessageTypeRequest, Goal: "Build search index and metadata schema", DependsOn: []string{"n1_architecture"}, Delay: 140 * time.Millisecond, OutputPath: "deliverables/docs/search_index.json"},
				{NodeID: "n3_frontend", AgentID: "frontend_engineer", NodeType: domain.MessageTypeRequest, Goal: "Build static docs layout and navigation", DependsOn: []string{"n1_architecture"}, Delay: 140 * time.Millisecond, OutputPath: "deliverables/docs/index.html"},
				{NodeID: "n4_integrate", AgentID: "integrator", NodeType: domain.MessageTypeRequest, Goal: "Integrate docs layout with search index", DependsOn: []string{"n2_backend", "n3_frontend"}, Delay: 120 * time.Millisecond, OutputPath: "deliverables/docs/app.js"},
				{NodeID: "n5_qa", AgentID: "qa_reviewer", NodeType: domain.MessageTypeReview, DependsOn: []string{"n4_integrate"}, Delay: 90 * time.Millisecond, OutputPath: "deliverables/docs/qa_report.md"},
				{NodeID: "n6_security", AgentID: "security_reviewer", NodeType: domain.MessageTypeReview, DependsOn: []string{"n5_qa"}, Delay: 90 * time.Millisecond, OutputPath: "deliverables/docs/security_report.md"},
				{NodeID: "n7_release", AgentID: "release_manager", NodeType: domain.MessageTypeRequest, Goal: "Prepare release manifest for deployment", DependsOn: []string{"n6_security"}, Delay: 100 * time.Millisecond, OutputPath: "deliverables/docs/release_manifest.json"},
			},
		},
	}
}
