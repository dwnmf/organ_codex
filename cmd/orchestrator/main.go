package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"organ_codex/internal/agent"
	"organ_codex/internal/config"
	"organ_codex/internal/domain"
	"organ_codex/internal/fs"
	"organ_codex/internal/messaging/inproc"
	"organ_codex/internal/orchestrator"
	"organ_codex/internal/policy"
	sqlitestore "organ_codex/internal/store/sqlite"
)

type app struct {
	cfg          config.Config
	orchestrator *orchestrator.Service
}

func main() {
	configPath := flag.String("config", "", "path to config.toml (default: ~/.codex/config.toml)")
	addrFlag := flag.String("addr", "", "http listen address override")
	dbPathFlag := flag.String("db", "", "sqlite database path override")
	workspaceFlag := flag.String("workspace", "", "workspace root for file operations override")
	demo := flag.Bool("demo", false, "bootstrap a demo task on startup")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	addr := firstNonEmpty(*addrFlag, cfg.Orchestrator.Addr, ":8091")
	dbPath := firstNonEmpty(*dbPathFlag, cfg.Orchestrator.DBPath, "data/organ_codex.db")
	workspaceRoot := firstNonEmpty(*workspaceFlag, cfg.Orchestrator.WorkspaceRoot, "workspace")
	dbPath = filepath.Clean(dbPath)
	workspaceRoot = filepath.Clean(workspaceRoot)

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("create db directory: %v", err)
	}
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		log.Fatalf("create workspace directory: %v", err)
	}

	store, err := sqlitestore.Open(dbPath)
	if err != nil {
		log.Fatalf("open sqlite store: %v", err)
	}
	defer func() {
		_ = store.Close()
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := store.Migrate(ctx); err != nil {
		log.Fatalf("migrate sqlite: %v", err)
	}

	bus := inproc.New(256)
	policyEngine := policy.New(store)
	files, err := fs.NewGateway(workspaceRoot, policyEngine, store)
	if err != nil {
		log.Fatalf("create file gateway: %v", err)
	}

	orchCfg := orchestrator.Config{
		DispatchInterval: durationMS(cfg.Orchestrator.DispatchIntervalMS, 250*time.Millisecond),
		WatchdogInterval: durationMS(cfg.Orchestrator.WatchdogIntervalMS, 3*time.Second),
		RetryDelay:       durationMS(cfg.Orchestrator.RetryDelayMS, 1*time.Second),
		MaxRetries:       intOrDefault(cfg.Orchestrator.MaxRetries, 6),
		IdleTimeout:      durationMS(cfg.Orchestrator.IdleTimeoutMS, 45*time.Second),
		DefaultMaxHops:   intOrDefault(cfg.Orchestrator.DefaultMaxHops, 8),
	}
	orch := orchestrator.New(store, policyEngine, bus, orchCfg, log.Default())
	orch.Start(ctx)

	planner := agent.NewPlanner(bus, orch, store, log.Default())
	coder := agent.NewCoder(bus, orch, store, files, "codex", workspaceRoot, log.Default())
	planner.Start(ctx)
	coder.Start(ctx)

	if *demo {
		if err := bootstrapDemo(ctx, orch); err != nil {
			log.Printf("demo bootstrap failed: %v", err)
		}
	}

	a := &app{
		cfg:          cfg,
		orchestrator: orch,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/config", a.handleConfig)
	mux.HandleFunc("/tasks", a.handleTasks)
	mux.HandleFunc("/tasks/", a.handleTaskByID)

	server := &http.Server{
		Addr:              addr,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf(
		"organ_codex started addr=%s db=%s workspace=%s model=%s provider=%s",
		addr,
		dbPath,
		workspaceRoot,
		cfg.Model,
		cfg.ModelProvider,
	)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server failed: %v", err)
	}
}

func (a *app) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (a *app) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"path": a.cfg.Path,
		"raw":  a.cfg.Raw,
	})
}

func (a *app) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tasks, err := a.orchestrator.ListTasks(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, tasks)
	case http.MethodPost:
		var req struct {
			Goal            string `json:"goal"`
			Scope           string `json:"scope"`
			OwnerAgent      string `json:"owner_agent"`
			Priority        int    `json:"priority"`
			DeadlineSeconds int    `json:"deadline_seconds"`
			BudgetTokens    int    `json:"budget_tokens"`
			MaxHops         int    `json:"max_hops"`
			AutoStart       bool   `json:"auto_start"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %w", err))
			return
		}
		if strings.TrimSpace(req.Goal) == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("goal is required"))
			return
		}

		var deadline *time.Time
		if req.DeadlineSeconds > 0 {
			v := time.Now().UTC().Add(time.Duration(req.DeadlineSeconds) * time.Second)
			deadline = &v
		}
		task, err := a.orchestrator.CreateTask(r.Context(), orchestrator.CreateTaskInput{
			Goal:         req.Goal,
			Scope:        req.Scope,
			OwnerAgent:   req.OwnerAgent,
			Priority:     req.Priority,
			DeadlineAt:   deadline,
			BudgetTokens: req.BudgetTokens,
			MaxHops:      req.MaxHops,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if req.AutoStart {
			if err := a.orchestrator.StartTask(r.Context(), task.ID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		writeJSON(w, http.StatusCreated, task)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *app) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/tasks/")
	parts := strings.Split(trimmed, "/")
	taskID := parts[0]
	if taskID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("task id is required"))
		return
	}

	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		task, err := a.orchestrator.GetTask(r.Context(), taskID)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, task)
		return
	}

	action := parts[1]
	switch action {
	case "start":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := a.orchestrator.StartTask(r.Context(), taskID); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "started", "task_id": taskID})
	case "permissions":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			AgentID     string `json:"agent_id"`
			Effect      string `json:"effect"`
			Operation   string `json:"operation"`
			PathPattern string `json:"path_pattern"`
			TTLSeconds  int    `json:"ttl_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %w", err))
			return
		}
		if req.AgentID == "" || req.Effect == "" || req.Operation == "" || req.PathPattern == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("agent_id, effect, operation, path_pattern are required"))
			return
		}
		var expires *time.Time
		if req.TTLSeconds > 0 {
			v := time.Now().UTC().Add(time.Duration(req.TTLSeconds) * time.Second)
			expires = &v
		}
		perm := domain.TaskPermission{
			TaskID:      taskID,
			AgentID:     req.AgentID,
			Effect:      domain.PermissionEffect(req.Effect),
			Operation:   domain.FileOperation(req.Operation),
			PathPattern: req.PathPattern,
			ExpiresAt:   expires,
		}
		if err := a.orchestrator.GrantPermission(r.Context(), perm); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"status": "permission granted"})
	case "channels":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			FromAgent    string   `json:"from_agent"`
			ToAgent      string   `json:"to_agent"`
			AllowedTypes []string `json:"allowed_types"`
			MaxMsgs      int      `json:"max_msgs"`
			TTLSeconds   int      `json:"ttl_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json body: %w", err))
			return
		}
		if req.FromAgent == "" || req.ToAgent == "" || len(req.AllowedTypes) == 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("from_agent, to_agent, allowed_types are required"))
			return
		}
		types := make([]domain.MessageType, 0, len(req.AllowedTypes))
		for _, item := range req.AllowedTypes {
			types = append(types, domain.MessageType(item))
		}
		var expires *time.Time
		if req.TTLSeconds > 0 {
			v := time.Now().UTC().Add(time.Duration(req.TTLSeconds) * time.Second)
			expires = &v
		}
		channel := domain.AgentChannel{
			TaskID:       taskID,
			FromAgent:    req.FromAgent,
			ToAgent:      req.ToAgent,
			AllowedTypes: types,
			MaxMsgs:      req.MaxMsgs,
			ExpiresAt:    expires,
		}
		if err := a.orchestrator.GrantChannel(r.Context(), channel); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"status": "channel granted"})
	case "messages":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		limit := queryInt(r, "limit", 200)
		items, err := a.orchestrator.ListTaskMessages(r.Context(), taskID, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case "acks":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		limit := queryInt(r, "limit", 400)
		items, err := a.orchestrator.ListTaskMessageAcks(r.Context(), taskID, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case "decisions":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		limit := queryInt(r, "limit", 300)
		items, err := a.orchestrator.ListTaskDecisions(r.Context(), taskID, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown action: %s", action))
	}
}

func bootstrapDemo(ctx context.Context, orch *orchestrator.Service) error {
	task, err := orch.CreateTask(ctx, orchestrator.CreateTaskInput{
		Goal:         "Build initial orchestrator draft",
		Scope:        "Use planner and coder with direct messaging",
		OwnerAgent:   "planner",
		BudgetTokens: 5000,
		MaxHops:      10,
	})
	if err != nil {
		return err
	}

	expires := time.Now().UTC().Add(2 * time.Hour)
	perm := []domain.TaskPermission{
		{
			TaskID:      task.ID,
			AgentID:     "coder",
			Effect:      domain.PermissionEffectAllow,
			Operation:   domain.FileOperationCreate,
			PathPattern: "**",
			ExpiresAt:   &expires,
		},
		{
			TaskID:      task.ID,
			AgentID:     "coder",
			Effect:      domain.PermissionEffectAllow,
			Operation:   domain.FileOperationWrite,
			PathPattern: "**",
			ExpiresAt:   &expires,
		},
	}
	for _, item := range perm {
		if err := orch.GrantPermission(ctx, item); err != nil {
			return err
		}
	}

	channels := []domain.AgentChannel{
		{
			TaskID:       task.ID,
			FromAgent:    "planner",
			ToAgent:      "coder",
			AllowedTypes: []domain.MessageType{domain.MessageTypeRequest},
			MaxMsgs:      20,
			ExpiresAt:    &expires,
		},
		{
			TaskID:       task.ID,
			FromAgent:    "coder",
			ToAgent:      "planner",
			AllowedTypes: []domain.MessageType{domain.MessageTypeDone},
			MaxMsgs:      20,
			ExpiresAt:    &expires,
		},
		{
			TaskID:       task.ID,
			FromAgent:    "planner",
			ToAgent:      "orchestrator",
			AllowedTypes: []domain.MessageType{domain.MessageTypeDone, domain.MessageTypeEscalate},
			MaxMsgs:      20,
			ExpiresAt:    &expires,
		},
		{
			TaskID:       task.ID,
			FromAgent:    "coder",
			ToAgent:      "orchestrator",
			AllowedTypes: []domain.MessageType{domain.MessageTypeBlocked},
			MaxMsgs:      20,
			ExpiresAt:    &expires,
		},
	}
	for _, ch := range channels {
		if err := orch.GrantChannel(ctx, ch); err != nil {
			return err
		}
	}

	if err := orch.StartTask(ctx, task.ID); err != nil {
		return err
	}
	log.Printf("demo task started id=%s", task.ID)
	return nil
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{
		"error": err.Error(),
	})
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func durationMS(v int, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return time.Duration(v) * time.Millisecond
}

func intOrDefault(v int, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func queryInt(r *http.Request, key string, def int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return def
	}
	return v
}
