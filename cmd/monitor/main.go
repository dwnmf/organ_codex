package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"organ_codex/internal/domain"
)

type client struct {
	baseURL string
	http    *http.Client
}

type embeddedOrchestrator struct {
	cmd *exec.Cmd
}

func main() {
	addr := flag.String("addr", "http://localhost:8091", "orchestrator base URL")
	interval := flag.Duration("interval", 2*time.Second, "refresh interval")
	embedded := flag.Bool("embedded", true, "start orchestrator in the same monitor process lifecycle")
	orchestratorBinary := flag.String("orchestrator-bin", "", "path to orchestrator binary (optional in embedded mode)")
	dbPath := flag.String("db", "data/embedded.db", "sqlite db path for embedded orchestrator")
	workspaceRoot := flag.String("workspace", "workspace", "workspace root for embedded orchestrator")
	flag.Parse()

	c := &client{
		baseURL: strings.TrimRight(*addr, "/"),
		http: &http.Client{
			Timeout: 10 * time.Second,
		},
	}

	var embeddedProc *embeddedOrchestrator
	var err error
	if *embedded {
		embeddedProc, err = startEmbeddedOrchestrator(*addr, *orchestratorBinary, *dbPath, *workspaceRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to start embedded orchestrator: %v\n", err)
			os.Exit(1)
		}
		defer embeddedProc.Stop()
	}

	if err := waitHealth(c, 30*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "orchestrator health check failed: %v\n", err)
		os.Exit(1)
	}

	app := tview.NewApplication()
	tasksTable := tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false)
	tasksTable.SetTitle("Tasks (Enter inspect, F5 refresh, F10 quit)").SetBorder(true)

	messagesView := tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)
	messagesView.SetTitle("Messages").SetBorder(true)

	acksView := tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)
	acksView.SetTitle("Acks").SetBorder(true)

	decisionsView := tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)
	decisionsView.SetTitle("Decisions").SetBorder(true)

	agentStateView := tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)
	agentStateView.SetTitle("Agent State").SetBorder(true)

	promptInput := tview.NewInputField().
		SetLabel("Prompt -> Orchestrator: ")
	promptInput.SetBorder(true).SetTitle("Enter = create+start task")

	statusView := tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)
	statusView.SetBorder(true).SetTitle("Status")
	statusView.SetText(fmt.Sprintf(
		"Connected to %s | embedded=%t | shortcuts: F10 quit, F5 refresh, Ctrl+L focus prompt, Ctrl+T focus tasks",
		c.baseURL,
		*embedded,
	))

	rightTop := tview.NewFlex().
		AddItem(messagesView, 0, 2, false).
		AddItem(acksView, 0, 1, false)
	right := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(rightTop, 0, 3, false).
		AddItem(agentStateView, 8, 0, false).
		AddItem(decisionsView, 0, 2, false)

	mainLayout := tview.NewFlex().
		AddItem(tasksTable, 0, 1, false).
		AddItem(right, 0, 2, false)

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(mainLayout, 0, 12, false).
		AddItem(promptInput, 3, 0, true).
		AddItem(statusView, 3, 0, false)

	var selectedTaskID string
	var lastTasks []domain.Task
	var detailsVersion uint64

	setStatusUI := func(msg string) {
		statusView.SetText(msg)
	}
	setStatusAsync := func(msg string) {
		app.QueueUpdateDraw(func() {
			statusView.SetText(msg)
		})
	}

	refreshTasks := func() {
		tasks, err := c.listTasks()
		if err != nil {
			app.QueueUpdateDraw(func() {
				tasksTable.Clear()
				tasksTable.SetCell(0, 0, tview.NewTableCell(fmt.Sprintf("load error: %v", err)).SetTextColor(tview.Styles.ContrastSecondaryTextColor))
			})
			return
		}
		sort.Slice(tasks, func(i, j int) bool {
			return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
		})
		lastTasks = tasks
		app.QueueUpdateDraw(func() {
			renderTasksTable(tasksTable, tasks, selectedTaskID)
		})
	}

	refreshDetailsAsync := func(taskID string) {
		if strings.TrimSpace(taskID) == "" {
			return
		}
		version := atomic.AddUint64(&detailsVersion, 1)
		app.QueueUpdateDraw(func() {
			messagesView.SetText("Loading...")
			acksView.SetText("Loading...")
			agentStateView.SetText("Loading...")
			decisionsView.SetText("Loading...")
		})

		go func(selected string, v uint64) {
			type msgResult struct {
				items []domain.Message
				err   error
			}
			type ackResult struct {
				items []domain.MessageAck
				err   error
			}
			type decisionResult struct {
				items []domain.DecisionLog
				err   error
			}

			msgCh := make(chan msgResult, 1)
			ackCh := make(chan ackResult, 1)
			decisionCh := make(chan decisionResult, 1)

			go func() {
				items, err := c.listTaskMessages(selected, 200)
				msgCh <- msgResult{items: items, err: err}
			}()
			go func() {
				items, err := c.listTaskAcks(selected, 250)
				ackCh <- ackResult{items: items, err: err}
			}()
			go func() {
				items, err := c.listTaskDecisions(selected, 250)
				decisionCh <- decisionResult{items: items, err: err}
			}()

			msgRes := <-msgCh
			ackRes := <-ackCh
			decisionRes := <-decisionCh

			if atomic.LoadUint64(&detailsVersion) != v {
				return
			}
			app.QueueUpdateDraw(func() {
				if selected != selectedTaskID {
					return
				}
				if msgRes.err != nil {
					messagesView.SetText(fmt.Sprintf("error: %v", msgRes.err))
				} else {
					messagesView.SetText(renderMessages(msgRes.items))
				}
				if ackRes.err != nil {
					acksView.SetText(fmt.Sprintf("error: %v", ackRes.err))
				} else {
					acksView.SetText(renderAcks(ackRes.items))
				}
				if decisionRes.err != nil {
					decisionsView.SetText(fmt.Sprintf("error: %v", decisionRes.err))
				} else {
					decisionsView.SetText(renderDecisions(decisionRes.items))
				}
				agentStateView.SetText(renderAgentState(selected, lastTasks, msgRes.items, ackRes.items, decisionRes.items))
			})
		}(taskID, version)
	}

	submitPrompt := func(prompt string) {
		prompt = strings.TrimSpace(prompt)
		if prompt == "" {
			return
		}
		setStatusUI("Creating task from prompt...")
		promptInput.SetText("")
		go func(input string) {
			taskID, err := c.createAndStartTaskFromPrompt(input)
			if err != nil {
				setStatusAsync("Failed to create/start task: " + err.Error())
				return
			}
			selectedTaskID = taskID
			refreshTasks()
			refreshDetailsAsync(selectedTaskID)
			setStatusAsync("Task started: " + taskID)
		}(prompt)
	}

	promptInput.SetDoneFunc(func(key tcell.Key) {
		if key != tcell.KeyEnter {
			return
		}
		submitPrompt(promptInput.GetText())
	})

	tasksTable.SetSelectedFunc(func(row, _ int) {
		if row <= 0 || row > len(lastTasks) {
			return
		}
		selectedTaskID = lastTasks[row-1].ID
		refreshDetailsAsync(selectedTaskID)
	})

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if app.GetFocus() == promptInput {
			if event.Key() == tcell.KeyEscape || event.Key() == tcell.KeyTAB {
				app.SetFocus(tasksTable)
				setStatusUI("Focus -> tasks")
				return nil
			}
			return event
		}

		if event.Key() == tcell.KeyEscape {
			app.SetFocus(tasksTable)
			setStatusUI("Focus -> tasks")
			return nil
		}
		switch event.Key() {
		case tcell.KeyF10:
			app.Stop()
			return nil
		case tcell.KeyF5:
			refreshTasks()
			refreshDetailsAsync(selectedTaskID)
			setStatusUI("Manual refresh complete")
			return nil
		case tcell.KeyCtrlL:
			app.SetFocus(promptInput)
			setStatusUI("Focus -> prompt")
			return nil
		case tcell.KeyCtrlT:
			app.SetFocus(tasksTable)
			setStatusUI("Focus -> tasks")
			return nil
		}
		if event.Key() == tcell.KeyTAB {
			if app.GetFocus() == promptInput {
				app.SetFocus(tasksTable)
			} else {
				app.SetFocus(promptInput)
			}
			return nil
		}
		if event.Key() == tcell.KeyRune {
			app.SetFocus(promptInput)
			return event
		}
		return event
	})

	go func() {
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()

		refreshTasks()
		for _, task := range lastTasks {
			if task.Status == domain.TaskStatusRunning || task.Status == domain.TaskStatusBlocked {
				selectedTaskID = task.ID
				break
			}
		}
		if selectedTaskID != "" {
			refreshDetailsAsync(selectedTaskID)
		}

		for range ticker.C {
			refreshTasks()
			if selectedTaskID == "" && len(lastTasks) > 0 {
				selectedTaskID = lastTasks[0].ID
			}
			refreshDetailsAsync(selectedTaskID)
		}
	}()

	if err := app.SetRoot(root, true).EnableMouse(true).SetFocus(promptInput).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "monitor failed: %v\n", err)
		os.Exit(1)
	}
}

func waitHealth(c *client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/healthz", nil)
		if err == nil {
			resp, err := c.http.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode < 300 {
					return nil
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for /healthz")
}

func startEmbeddedOrchestrator(addr string, orchestratorBinary string, dbPath string, workspaceRoot string) (*embeddedOrchestrator, error) {
	parsed, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("parse addr: %w", err)
	}
	port := parsed.Port()
	if port == "" {
		return nil, fmt.Errorf("addr must include explicit port, got %q", addr)
	}
	addrArg := ":" + port

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace root: %w", err)
	}

	var cmd *exec.Cmd
	if strings.TrimSpace(orchestratorBinary) != "" {
		cmd = exec.Command(orchestratorBinary, "--addr", addrArg, "--db", dbPath, "--workspace", workspaceRoot)
	} else {
		self, err := os.Executable()
		if err == nil {
			sibling := filepath.Join(filepath.Dir(self), "orchestrator.exe")
			if fileExists(sibling) {
				cmd = exec.Command(sibling, "--addr", addrArg, "--db", dbPath, "--workspace", workspaceRoot)
			}
		}
		if cmd == nil {
			cmd = exec.Command("go", "run", "./cmd/orchestrator", "--addr", addrArg, "--db", dbPath, "--workspace", workspaceRoot)
			cwd, _ := os.Getwd()
			cmd.Dir = cwd
		}
	}

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start orchestrator process: %w", err)
	}

	proc := &embeddedOrchestrator{cmd: cmd}
	return proc, nil
}

func (e *embeddedOrchestrator) Stop() {
	if e == nil || e.cmd == nil || e.cmd.Process == nil {
		return
	}
	_ = e.cmd.Process.Kill()
	_, _ = e.cmd.Process.Wait()
}

func renderTasksTable(table *tview.Table, tasks []domain.Task, selectedTaskID string) {
	table.Clear()
	headers := []string{"Task", "Status", "Owner", "Updated", "Goal"}
	for i, h := range headers {
		table.SetCell(0, i, tview.NewTableCell(h).SetSelectable(false).SetAttributes(tcell.AttrBold))
	}
	for i, t := range tasks {
		row := i + 1
		table.SetCell(row, 0, tview.NewTableCell(shortID(t.ID)))
		table.SetCell(row, 1, tview.NewTableCell(string(t.Status)))
		table.SetCell(row, 2, tview.NewTableCell(t.OwnerAgent))
		table.SetCell(row, 3, tview.NewTableCell(t.UpdatedAt.Format("15:04:05")))
		table.SetCell(row, 4, tview.NewTableCell(trimLine(t.Goal, 64)))
		if t.ID == selectedTaskID {
			table.Select(row, 0)
		}
	}
}

func renderMessages(items []domain.Message) string {
	if len(items) == 0 {
		return "No messages"
	}
	var b strings.Builder
	for _, m := range items {
		b.WriteString(fmt.Sprintf(
			"[%s] %s -> %s  %s  status=%s attempts=%d\n",
			m.CreatedAt.Format("15:04:05"),
			m.FromAgent,
			m.ToAgent,
			m.Type,
			m.Status,
			m.Attempts,
		))
		if m.LastError != "" {
			b.WriteString("  error: " + m.LastError + "\n")
		}
	}
	return b.String()
}

func renderAcks(items []domain.MessageAck) string {
	if len(items) == 0 {
		return "No acks"
	}
	var b strings.Builder
	for _, a := range items {
		b.WriteString(fmt.Sprintf(
			"[%s] agent=%s msg=%s\n  result: %s\n",
			a.AckAt.Format("15:04:05"),
			a.AgentID,
			shortID(a.MessageID),
			trimLine(a.Result, 80),
		))
	}
	return b.String()
}

func renderDecisions(items []domain.DecisionLog) string {
	if len(items) == 0 {
		return "No decisions"
	}
	var b strings.Builder
	for _, d := range items {
		b.WriteString(fmt.Sprintf(
			"[%s] %s %s\n  reason: %s\n",
			d.CreatedAt.Format("15:04:05"),
			d.Actor,
			d.Action,
			trimLine(d.Reason, 100),
		))
		if detail := decisionPayloadSummary(d.Payload); detail != "" {
			b.WriteString("  payload: " + trimLine(detail, 160) + "\n")
		}
	}
	return b.String()
}

type agentStateLine struct {
	Agent      string
	State      string
	LastAction string
	LastReason string
	LastAt     time.Time
	LastAck    string
	InFlight   int
}

func renderAgentState(
	taskID string,
	tasks []domain.Task,
	messages []domain.Message,
	acks []domain.MessageAck,
	decisions []domain.DecisionLog,
) string {
	if strings.TrimSpace(taskID) == "" {
		return "No task selected"
	}

	taskStatus := "unknown"
	for _, t := range tasks {
		if t.ID == taskID {
			taskStatus = string(t.Status)
			break
		}
	}

	lines := map[string]*agentStateLine{
		"planner":      {Agent: "planner", State: "idle"},
		"coder":        {Agent: "coder", State: "idle"},
		"orchestrator": {Agent: "orchestrator", State: "idle"},
	}

	ackedByAgent := map[string]map[string]bool{}
	lastAckByAgent := map[string]domain.MessageAck{}
	for _, ack := range acks {
		if _, ok := ackedByAgent[ack.AgentID]; !ok {
			ackedByAgent[ack.AgentID] = map[string]bool{}
		}
		ackedByAgent[ack.AgentID][ack.MessageID] = true
		if prev, ok := lastAckByAgent[ack.AgentID]; !ok || ack.AckAt.After(prev.AckAt) {
			lastAckByAgent[ack.AgentID] = ack
		}
	}

	for _, msg := range messages {
		line, ok := lines[msg.ToAgent]
		if !ok {
			continue
		}
		acked := false
		if byAgent, ok := ackedByAgent[msg.ToAgent]; ok {
			acked = byAgent[msg.ID]
		}
		if (msg.Status == domain.MessageStatusPending || msg.Status == domain.MessageStatusDelivered) && !acked {
			line.InFlight++
		}
	}

	for _, d := range decisions {
		line, ok := lines[d.Actor]
		if !ok {
			continue
		}
		if line.LastAt.IsZero() || d.CreatedAt.After(line.LastAt) {
			line.LastAt = d.CreatedAt
			line.LastAction = d.Action
			line.LastReason = d.Reason
			line.State = classifyAgentState(d.Actor, d.Action, taskStatus)
		}
	}

	for agentID, ack := range lastAckByAgent {
		line, ok := lines[agentID]
		if !ok {
			continue
		}
		line.LastAck = trimLine(ack.Result, 64)
	}

	order := []string{"orchestrator", "planner", "coder"}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Task: %s  status=%s\n", shortID(taskID), taskStatus))
	for _, id := range order {
		line := lines[id]
		lastAt := "-"
		if !line.LastAt.IsZero() {
			lastAt = line.LastAt.Format("15:04:05")
		}
		b.WriteString(fmt.Sprintf(
			"%-12s state=%-12s inflight=%d last=%s action=%s\n",
			line.Agent, line.State, line.InFlight, lastAt, trimLine(line.LastAction, 28),
		))
		if line.LastReason != "" {
			b.WriteString("  reason: " + trimLine(line.LastReason, 120) + "\n")
		}
		if line.LastAck != "" {
			b.WriteString("  ack: " + line.LastAck + "\n")
		}
	}
	return b.String()
}

func classifyAgentState(actor, action, taskStatus string) string {
	switch actor {
	case "coder":
		switch action {
		case "codex_exec_started", "codex_exec_progress":
			return "working"
		case "file_written":
			return "writing"
		case "done_sent":
			return "done"
		case "codex_exec_failed", "no_files_created", "done_send_failed", "file_write_failed", "file_path_invalid":
			return "error"
		default:
			return "idle"
		}
	case "planner":
		switch action {
		case "forwarded_to_coder", "message_received":
			return "coordinating"
		case "notified_orchestrator_done":
			return "done"
		case "forward_failed", "notify_orchestrator_failed":
			return "error"
		default:
			return "idle"
		}
	case "orchestrator":
		switch taskStatus {
		case string(domain.TaskStatusRunning):
			return "running"
		case string(domain.TaskStatusDone):
			return "done"
		case string(domain.TaskStatusBlocked):
			return "blocked"
		case string(domain.TaskStatusFailed):
			return "failed"
		default:
			return "idle"
		}
	default:
		return "unknown"
	}
}

func decisionPayloadSummary(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || trimmed == "{}" {
		return ""
	}

	var kv map[string]any
	if err := json.Unmarshal(payload, &kv); err == nil {
		keys := make([]string, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, kv[k]))
		}
		return strings.Join(parts, ", ")
	}
	return trimmed
}

func (c *client) createAndStartTaskFromPrompt(prompt string) (string, error) {
	createReq := map[string]any{
		"goal":          prompt,
		"scope":         "Autonomous implementation from prompt",
		"owner_agent":   "planner",
		"priority":      10,
		"budget_tokens": 30000,
		"max_hops":      12,
		"auto_start":    false,
	}
	var task domain.Task
	if err := c.postJSON("/tasks", createReq, &task); err != nil {
		return "", err
	}

	perms := []map[string]any{
		{"agent_id": "coder", "effect": "allow", "operation": "create", "path_pattern": "**", "ttl_seconds": 7200},
		{"agent_id": "coder", "effect": "allow", "operation": "write", "path_pattern": "**", "ttl_seconds": 7200},
	}
	for _, p := range perms {
		if err := c.postJSON(fmt.Sprintf("/tasks/%s/permissions", task.ID), p, nil); err != nil {
			return "", err
		}
	}

	channels := []map[string]any{
		{"from_agent": "planner", "to_agent": "coder", "allowed_types": []string{"REQUEST"}, "max_msgs": 40, "ttl_seconds": 7200},
		{"from_agent": "coder", "to_agent": "planner", "allowed_types": []string{"DONE"}, "max_msgs": 40, "ttl_seconds": 7200},
		{"from_agent": "planner", "to_agent": "orchestrator", "allowed_types": []string{"DONE", "ESCALATE"}, "max_msgs": 40, "ttl_seconds": 7200},
		{"from_agent": "coder", "to_agent": "orchestrator", "allowed_types": []string{"BLOCKED"}, "max_msgs": 40, "ttl_seconds": 7200},
	}
	for _, ch := range channels {
		if err := c.postJSON(fmt.Sprintf("/tasks/%s/channels", task.ID), ch, nil); err != nil {
			return "", err
		}
	}

	if err := c.postJSON(fmt.Sprintf("/tasks/%s/start", task.ID), map[string]any{}, nil); err != nil {
		return "", err
	}
	return task.ID, nil
}

func (c *client) listTasks() ([]domain.Task, error) {
	var out []domain.Task
	if err := c.getJSON("/tasks", &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) listTaskMessages(taskID string, limit int) ([]domain.Message, error) {
	var out []domain.Message
	if err := c.getJSON(fmt.Sprintf("/tasks/%s/messages?limit=%d", taskID, limit), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) listTaskAcks(taskID string, limit int) ([]domain.MessageAck, error) {
	var out []domain.MessageAck
	if err := c.getJSON(fmt.Sprintf("/tasks/%s/acks?limit=%d", taskID, limit), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) listTaskDecisions(taskID string, limit int) ([]domain.DecisionLog, error) {
	var out []domain.DecisionLog
	if err := c.getJSON(fmt.Sprintf("/tasks/%s/decisions?limit=%d", taskID, limit), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) getJSON(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("http %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return err
	}
	return nil
}

func (c *client) postJSON(path string, in any, out any) error {
	var payload io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("http %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return err
	}
	return nil
}

func trimLine(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit-3] + "..."
}

func shortID(v string) string {
	if len(v) <= 8 {
		return v
	}
	return v[:8]
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func combineErrors(errs ...error) error {
	var parts []string
	for _, err := range errs {
		if err == nil {
			continue
		}
		parts = append(parts, err.Error())
	}
	if len(parts) == 0 {
		return nil
	}
	return errors.New(strings.Join(parts, "; "))
}
