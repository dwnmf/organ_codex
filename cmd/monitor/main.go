package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"organ_codex/internal/domain"
)

type client struct {
	baseURL string
	http    *http.Client
}

func main() {
	addr := flag.String("addr", "http://localhost:8091", "orchestrator base URL")
	interval := flag.Duration("interval", 2*time.Second, "refresh interval")
	flag.Parse()

	c := &client{
		baseURL: strings.TrimRight(*addr, "/"),
		http: &http.Client{
			Timeout: 6 * time.Second,
		},
	}

	app := tview.NewApplication()
	tasksTable := tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false)
	tasksTable.SetTitle("Tasks (Enter to inspect, q to quit)").SetBorder(true)

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

	rightTop := tview.NewFlex().
		AddItem(messagesView, 0, 2, false).
		AddItem(acksView, 0, 1, false)
	right := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(rightTop, 0, 3, false).
		AddItem(decisionsView, 0, 2, false)

	layout := tview.NewFlex().
		AddItem(tasksTable, 0, 1, true).
		AddItem(right, 0, 2, false)

	var selectedTaskID string
	var lastTasks []domain.Task

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

	refreshDetails := func() {
		if selectedTaskID == "" {
			return
		}
		msgs, err := c.listTaskMessages(selectedTaskID, 120)
		if err != nil {
			app.QueueUpdateDraw(func() {
				messagesView.SetText(fmt.Sprintf("error: %v", err))
			})
		} else {
			app.QueueUpdateDraw(func() {
				messagesView.SetText(renderMessages(msgs))
			})
		}

		acks, err := c.listTaskAcks(selectedTaskID, 160)
		if err != nil {
			app.QueueUpdateDraw(func() {
				acksView.SetText(fmt.Sprintf("error: %v", err))
			})
		} else {
			app.QueueUpdateDraw(func() {
				acksView.SetText(renderAcks(acks))
			})
		}

		decisions, err := c.listTaskDecisions(selectedTaskID, 120)
		if err != nil {
			app.QueueUpdateDraw(func() {
				decisionsView.SetText(fmt.Sprintf("error: %v", err))
			})
		} else {
			app.QueueUpdateDraw(func() {
				decisionsView.SetText(renderDecisions(decisions))
			})
		}
	}

	tasksTable.SetSelectedFunc(func(row, _ int) {
		if row <= 0 || row > len(lastTasks) {
			return
		}
		selectedTaskID = lastTasks[row-1].ID
		refreshDetails()
	})

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case 'q', 'Q':
			app.Stop()
			return nil
		case 'r', 'R':
			refreshTasks()
			refreshDetails()
			return nil
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
			refreshDetails()
		}

		for range ticker.C {
			refreshTasks()
			if selectedTaskID == "" && len(lastTasks) > 0 {
				selectedTaskID = lastTasks[0].ID
			}
			refreshDetails()
		}
	}()

	if err := app.SetRoot(layout, true).EnableMouse(true).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "monitor failed: %v\n", err)
		os.Exit(1)
	}
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
	}
	return b.String()
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
