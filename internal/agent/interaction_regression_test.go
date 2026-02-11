package agent

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"organ_codex/internal/domain"
	"organ_codex/internal/fs"
	"organ_codex/internal/messaging/inproc"
	"organ_codex/internal/orchestrator"
	"organ_codex/internal/policy"
	sqlitestore "organ_codex/internal/store/sqlite"
)

type interactionHarness struct {
	svc           *orchestrator.Service
	store         *sqlitestore.Store
	workspaceRoot string

	planner *Planner
	coder   *Coder
	review  *Reviewer

	runCtx       context.Context
	cancel       context.CancelFunc
	agentsStarted bool
}

func newInteractionHarness(t *testing.T, cfg orchestrator.Config, generator PlanGenerator, startAgents bool) *interactionHarness {
	t.Helper()

	root := t.TempDir()
	dbPath := filepath.Join(root, "state.db")
	workspaceRoot := filepath.Join(root, "workspace")

	store, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		_ = store.Close()
		t.Fatalf("migrate store: %v", err)
	}

	bus := inproc.New(128)
	policyEngine := policy.New(store)
	fileGateway, err := fs.NewGateway(workspaceRoot, policyEngine, store)
	if err != nil {
		_ = store.Close()
		t.Fatalf("new gateway: %v", err)
	}

	svc := orchestrator.New(store, policyEngine, bus, cfg, log.New(io.Discard, "", 0))
	runCtx, cancel := context.WithCancel(context.Background())
	svc.Start(runCtx)

	h := &interactionHarness{
		svc:            svc,
		store:          store,
		workspaceRoot:  workspaceRoot,
		planner:        NewPlanner(bus, svc, store, log.New(io.Discard, "", 0)),
		coder:          NewCoder(bus, svc, store, fileGateway, generator, log.New(io.Discard, "", 0)),
		review:         NewReviewer(bus, svc, store, log.New(io.Discard, "", 0)),
		runCtx:         runCtx,
		cancel:         cancel,
		agentsStarted:  false,
	}
	if startAgents {
		h.StartAgents()
	}
	return h
}

func (h *interactionHarness) StartAgents() {
	if h.agentsStarted {
		return
	}
	h.planner.Start(h.runCtx)
	h.coder.Start(h.runCtx)
	h.review.Start(h.runCtx)
	h.agentsStarted = true
}

func (h *interactionHarness) Close() {
	h.cancel()
	h.svc.Wait()
	_ = h.store.Close()
}

type generatorFunc func(req domain.WorkRequestPayload) (codexPlan, error)

func (f generatorFunc) Generate(_ context.Context, req domain.WorkRequestPayload) (codexPlan, error) {
	return f(req)
}

func TestAgentInteraction_HappyPathPlanToReviewDone(t *testing.T) {
	h := newInteractionHarness(t, orchestrator.Config{
		DispatchInterval: 5 * time.Millisecond,
		WatchdogInterval: 200 * time.Millisecond,
		RetryDelay:       30 * time.Millisecond,
		MaxRetries:       5,
		IdleTimeout:      8 * time.Second,
		DispatchLease:    1200 * time.Millisecond,
		AckTimeout:       2 * time.Second,
	}, generatorFunc(func(_ domain.WorkRequestPayload) (codexPlan, error) {
		return codexPlan{
			Summary: "implemented ui",
			Files: []codexFile{
				{Path: "app/index.html", Content: "<!doctype html><title>ok</title>"},
				{Path: "README.md", Content: "run by opening index.html"},
			},
		}, nil
	}), true)
	defer h.Close()

	ctx := context.Background()
	taskID := createTaskWithRules(t, ctx, h.svc)
	if err := h.svc.StartTask(ctx, taskID); err != nil {
		t.Fatalf("start task: %v", err)
	}

	if err := waitForCondition(6*time.Second, func() (bool, error) {
		msgs, err := h.store.ListTaskMessages(ctx, taskID, 200)
		if err != nil {
			return false, err
		}
		var hasReviewerDone bool
		for _, msg := range msgs {
			if msg.FromAgent == "reviewer" && msg.ToAgent == "orchestrator" && msg.Type == domain.MessageTypeDone {
				hasReviewerDone = true
				break
			}
		}
		return hasReviewerDone, nil
	}); err != nil {
		t.Fatalf("did not observe reviewer DONE: %v context=%s", err, taskDebugContext(t, h.store, taskID))
	}

	task, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status != domain.TaskStatusDone && task.Status != domain.TaskStatusRunning {
		t.Fatalf("unexpected task status=%s context=%s", task.Status, taskDebugContext(t, h.store, taskID))
	}

	content, err := os.ReadFile(filepath.Join(h.workspaceRoot, "app", "index.html"))
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	if !strings.Contains(string(content), "ok") {
		t.Fatalf("unexpected generated file content: %q", string(content))
	}

	msgs, err := h.store.ListTaskMessages(ctx, taskID, 200)
	if err != nil {
		t.Fatalf("list task messages: %v", err)
	}
	var hasReview bool
	var hasReviewerDone bool
	for _, msg := range msgs {
		if msg.ToAgent == "reviewer" && msg.Type == domain.MessageTypeReview {
			hasReview = true
		}
		if msg.FromAgent == "reviewer" && msg.Type == domain.MessageTypeDone {
			hasReviewerDone = true
		}
	}
	if !hasReview || !hasReviewerDone {
		t.Fatalf("expected review workflow messages; hasReview=%t hasReviewerDone=%t", hasReview, hasReviewerDone)
	}
}

func TestAgentInteraction_BlockedWhenCoderProducesOnlyRejectedPaths(t *testing.T) {
	h := newInteractionHarness(t, orchestrator.Config{
		DispatchInterval: 5 * time.Millisecond,
		WatchdogInterval: 200 * time.Millisecond,
		RetryDelay:       30 * time.Millisecond,
		MaxRetries:       5,
		IdleTimeout:      8 * time.Second,
		DispatchLease:    1200 * time.Millisecond,
		AckTimeout:       2 * time.Second,
	}, generatorFunc(func(_ domain.WorkRequestPayload) (codexPlan, error) {
		return codexPlan{
			Summary: "invalid plan output",
			Files: []codexFile{
				{Path: "../escape.txt", Content: "bad"},
				{Path: "/tmp/abs.txt", Content: "bad"},
			},
		}, nil
	}), true)
	defer h.Close()

	ctx := context.Background()
	taskID := createTaskWithRules(t, ctx, h.svc)
	if err := h.svc.StartTask(ctx, taskID); err != nil {
		t.Fatalf("start task: %v", err)
	}

	if err := waitForCondition(6*time.Second, func() (bool, error) {
		msgs, err := h.store.ListTaskMessages(ctx, taskID, 200)
		if err != nil {
			return false, err
		}
		for _, msg := range msgs {
			if msg.FromAgent == "coder" && msg.ToAgent == "orchestrator" && msg.Type == domain.MessageTypeBlocked {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("did not observe coder BLOCKED: %v context=%s", err, taskDebugContext(t, h.store, taskID))
	}

	entries, err := os.ReadDir(h.workspaceRoot)
	if err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("workspace is expected to stay empty, got %d entries", len(entries))
	}

	msgs, err := h.store.ListTaskMessages(ctx, taskID, 200)
	if err != nil {
		t.Fatalf("list task messages: %v", err)
	}
	var hasCoderBlocked bool
	for _, msg := range msgs {
		if msg.FromAgent == "coder" && msg.ToAgent == "orchestrator" && msg.Type == domain.MessageTypeBlocked {
			hasCoderBlocked = true
			break
		}
	}
	if !hasCoderBlocked {
		t.Fatalf("expected coder BLOCKED message to orchestrator")
	}

	task, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status != domain.TaskStatusBlocked && task.Status != domain.TaskStatusRunning {
		t.Fatalf("unexpected task status=%s context=%s", task.Status, taskDebugContext(t, h.store, taskID))
	}
}

func TestAgentInteraction_RecoversAfterLateAgentRegistrationAndRetries(t *testing.T) {
	h := newInteractionHarness(t, orchestrator.Config{
		DispatchInterval: 5 * time.Millisecond,
		WatchdogInterval: 30 * time.Second,
		RetryDelay:       1 * time.Second,
		MaxRetries:       20,
		IdleTimeout:      60 * time.Second,
		DispatchLease:    10 * time.Second,
		AckTimeout:       30 * time.Second,
	}, generatorFunc(func(_ domain.WorkRequestPayload) (codexPlan, error) {
		return codexPlan{
			Summary: "implemented after retries",
			Files: []codexFile{
				{Path: "late/index.html", Content: "<html>late start</html>"},
			},
		}, nil
	}), false)
	defer h.Close()

	ctx := context.Background()
	taskID := createTaskWithRules(t, ctx, h.svc)
	if err := h.svc.StartTask(ctx, taskID); err != nil {
		t.Fatalf("start task: %v", err)
	}

	time.Sleep(250 * time.Millisecond)
	h.StartAgents()

	if err := waitForCondition(10*time.Second, func() (bool, error) {
		msgs, err := h.store.ListTaskMessages(ctx, taskID, 200)
		if err != nil {
			return false, err
		}
		var hasCoderDone bool
		var hasReviewerDone bool
		var hasBlocked bool
		for _, msg := range msgs {
			if msg.FromAgent == "coder" && msg.ToAgent == "orchestrator" && msg.Type == domain.MessageTypeDone {
				hasCoderDone = true
			}
			if msg.FromAgent == "reviewer" && msg.ToAgent == "orchestrator" && msg.Type == domain.MessageTypeDone {
				hasReviewerDone = true
			}
			if msg.Type == domain.MessageTypeBlocked {
				hasBlocked = true
			}
		}
		if hasBlocked {
			return false, nil
		}
		return hasCoderDone && hasReviewerDone, nil
	}); err != nil {
		t.Fatalf("did not observe completion DONE messages: %v context=%s", err, taskDebugContext(t, h.store, taskID))
	}

	msgs, err := h.store.ListTaskMessages(ctx, taskID, 200)
	if err != nil {
		t.Fatalf("list task messages: %v", err)
	}
	task, err := h.store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	var hasCoderDone bool
	var hasReviewerDone bool
	var hasBlocked bool
	var startAttempts int
	for _, msg := range msgs {
		if msg.CorrelationID == "task-start-"+taskID && msg.ToAgent == "planner" && msg.Type == domain.MessageTypeRequest {
			startAttempts = msg.Attempts
		}
		if msg.FromAgent == "coder" && msg.ToAgent == "orchestrator" && msg.Type == domain.MessageTypeDone {
			hasCoderDone = true
		}
		if msg.FromAgent == "reviewer" && msg.ToAgent == "orchestrator" && msg.Type == domain.MessageTypeDone {
			hasReviewerDone = true
		}
		if msg.Type == domain.MessageTypeBlocked {
			hasBlocked = true
		}
	}
	if startAttempts == 0 {
		t.Fatalf("expected non-zero retry attempts for task-start message")
	}
	if hasBlocked {
		t.Fatalf("unexpected BLOCKED messages context=%s", taskDebugContext(t, h.store, taskID))
	}
	if !hasCoderDone || !hasReviewerDone {
		t.Fatalf("expected DONE messages from coder and reviewer context=%s", taskDebugContext(t, h.store, taskID))
	}
	if task.Status != domain.TaskStatusDone && task.Status != domain.TaskStatusRunning {
		t.Fatalf("unexpected task status=%s context=%s", task.Status, taskDebugContext(t, h.store, taskID))
	}
}

func createTaskWithRules(t *testing.T, ctx context.Context, svc *orchestrator.Service) string {
	t.Helper()

	task, err := svc.CreateTask(ctx, orchestrator.CreateTaskInput{
		Goal:       "Build a tiny app",
		Scope:      "Create runnable files",
		OwnerAgent: "planner",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	for _, perm := range []domain.TaskPermission{
		{
			TaskID:      task.ID,
			AgentID:     "coder",
			Effect:      domain.PermissionEffectAllow,
			Operation:   domain.FileOperationCreate,
			PathPattern: "**",
		},
		{
			TaskID:      task.ID,
			AgentID:     "coder",
			Effect:      domain.PermissionEffectAllow,
			Operation:   domain.FileOperationWrite,
			PathPattern: "**",
		},
	} {
		if err := svc.GrantPermission(ctx, perm); err != nil {
			t.Fatalf("grant permission: %v", err)
		}
	}

	for _, ch := range []domain.AgentChannel{
		{
			TaskID:       task.ID,
			FromAgent:    "planner",
			ToAgent:      "orchestrator",
			AllowedTypes: []domain.MessageType{domain.MessageTypePropose, domain.MessageTypeEscalate},
			MaxMsgs:      20,
		},
		{
			TaskID:       task.ID,
			FromAgent:    "coder",
			ToAgent:      "orchestrator",
			AllowedTypes: []domain.MessageType{domain.MessageTypeDone, domain.MessageTypeBlocked},
			MaxMsgs:      20,
		},
		{
			TaskID:       task.ID,
			FromAgent:    "reviewer",
			ToAgent:      "orchestrator",
			AllowedTypes: []domain.MessageType{domain.MessageTypeDone, domain.MessageTypeBlocked},
			MaxMsgs:      20,
		},
	} {
		if err := svc.GrantChannel(ctx, ch); err != nil {
			t.Fatalf("grant channel: %v", err)
		}
	}

	return task.ID
}

func waitTaskFinalStatus(t *testing.T, store *sqlitestore.Store, taskID string, timeout time.Duration) domain.TaskStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := store.GetTask(context.Background(), taskID)
		if err == nil && (task.Status == domain.TaskStatusDone || task.Status == domain.TaskStatusBlocked || task.Status == domain.TaskStatusFailed || task.Status == domain.TaskStatusCanceled) {
			return task.Status
		}
		time.Sleep(15 * time.Millisecond)
	}
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("get task after timeout: %v", err)
	}
	return task.Status
}

func waitForCondition(timeout time.Duration, check func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, err := check()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("condition was not met within %s", timeout)
}

func taskDebugContext(t *testing.T, store *sqlitestore.Store, taskID string) string {
	t.Helper()
	ctx := context.Background()
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		return "get_task_error=" + err.Error()
	}
	decisions, err := store.ListTaskDecisions(ctx, taskID, 50)
	if err != nil {
		return "list_decisions_error=" + err.Error()
	}
	msgs, err := store.ListTaskMessages(ctx, taskID, 50)
	if err != nil {
		return "list_messages_error=" + err.Error()
	}
	items := make([]string, 0, len(decisions))
	for _, item := range decisions {
		items = append(items, item.Actor+":"+item.Action+":"+item.Reason)
	}
	msgItems := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		msgItems = append(msgItems, msg.ID+":"+string(msg.Status)+":attempts="+strconv.Itoa(msg.Attempts)+":to="+msg.ToAgent)
	}
	return "last_error=" + task.LastError + " decisions=" + strings.Join(items, "|") + " messages=" + strings.Join(msgItems, "|")
}
