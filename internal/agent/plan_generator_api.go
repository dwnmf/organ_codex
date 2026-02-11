package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"organ_codex/internal/domain"
)

const (
	defaultReasoningEffort   = "high"
	defaultAPIRetries        = 2
	defaultAPIRetryBackoff   = 1500 * time.Millisecond
	defaultAPITimeout        = 8 * time.Minute
	defaultMaxOutputBytes    = 8 * 1024 * 1024
	defaultMaxOutputTokens   = 24000
	maxHTTPErrorBodyReadSize = 64 * 1024
)

var allowedReasoningEfforts = map[string]struct{}{
	"none":   {},
	"low":    {},
	"medium": {},
	"high":   {},
}

type APIPlanGeneratorConfig struct {
	Endpoint        string
	Model           string
	ReasoningEffort string
	AuthToken       string
	Timeout         time.Duration
	Retries         int
	RetryBackoff    time.Duration
	MaxOutputBytes  int
	MaxOutputTokens int
	Logger          *log.Logger
	Client          *http.Client
}

type APIPlanGenerator struct {
	endpoint        string
	model           string
	reasoningEffort string
	authToken       string
	retries         int
	retryBackoff    time.Duration
	maxOutputBytes  int
	maxOutputTokens int
	logger          *log.Logger
	client          *http.Client
}

func NewAPIPlanGenerator(cfg APIPlanGeneratorConfig) (*APIPlanGenerator, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("empty API endpoint")
	}
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return nil, fmt.Errorf("invalid API endpoint %q: %w", endpoint, err)
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("empty model")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultAPITimeout
	}
	retries := cfg.Retries
	if retries < 0 {
		retries = 0
	}
	if retries == 0 {
		retries = defaultAPIRetries
	}
	retryBackoff := cfg.RetryBackoff
	if retryBackoff <= 0 {
		retryBackoff = defaultAPIRetryBackoff
	}
	maxOutputBytes := cfg.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}
	maxOutputTokens := cfg.MaxOutputTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = defaultMaxOutputTokens
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{
			Timeout: timeout,
		}
	}

	return &APIPlanGenerator{
		endpoint:        endpoint,
		model:           model,
		reasoningEffort: normalizeReasoningEffort(cfg.ReasoningEffort),
		authToken:       strings.TrimSpace(cfg.AuthToken),
		retries:         retries,
		retryBackoff:    retryBackoff,
		maxOutputBytes:  maxOutputBytes,
		maxOutputTokens: maxOutputTokens,
		logger:          cfg.Logger,
		client:          client,
	}, nil
}

func (g *APIPlanGenerator) Generate(ctx context.Context, req domain.WorkRequestPayload) (codexPlan, error) {
	var lastErr error
	for attempt := 1; attempt <= g.retries+1; attempt++ {
		plan, err := g.generateOnce(ctx, req)
		if err == nil {
			return plan, nil
		}
		lastErr = err
		if !isRetryableAPIError(err) || attempt == g.retries+1 {
			break
		}
		wait := time.Duration(attempt) * g.retryBackoff
		g.logger.Printf("coder api generation retry attempt=%d wait=%s reason=%v", attempt, wait, err)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return codexPlan{}, ctx.Err()
		case <-timer.C:
		}
	}
	if lastErr == nil {
		lastErr = errors.New("unknown API generation error")
	}
	return codexPlan{}, lastErr
}

func (g *APIPlanGenerator) generateOnce(ctx context.Context, req domain.WorkRequestPayload) (codexPlan, error) {
	payload := responsesRequest{
		Model:        g.model,
		Instructions: responsesInstructions,
		Stream:       true,
		Reasoning:    &responsesReasoning{Effort: g.reasoningEffort},
		Input: []responsesInputMessage{
			{
				Role: "user",
				Content: []responsesInputContent{
					{Type: "input_text", Text: buildCodexPrompt(req)},
				},
			},
		},
		MaxOutputTokens: g.maxOutputTokens,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return codexPlan{}, fmt.Errorf("marshal responses request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, bytes.NewReader(body))
	if err != nil {
		return codexPlan{}, fmt.Errorf("create API request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if g.authToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+g.authToken)
	}

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return codexPlan{}, fmt.Errorf("responses api request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxHTTPErrorBodyReadSize))
		if readErr != nil {
			return codexPlan{}, fmt.Errorf("responses api status=%d and read body failed: %w", resp.StatusCode, readErr)
		}
		return codexPlan{}, apiHTTPError{
			statusCode: resp.StatusCode,
			body:       strings.TrimSpace(string(body)),
		}
	}

	raw, err := readResponsesStream(resp.Body, g.maxOutputBytes)
	if err != nil {
		return codexPlan{}, fmt.Errorf("read responses stream: %w", err)
	}
	plan, err := parseCodexOutput([]byte(raw))
	if err != nil {
		return codexPlan{}, fmt.Errorf("parse model output: %w; output: %s", err, trim(raw, 800))
	}
	return plan, nil
}

func normalizeReasoningEffort(value string) string {
	effort := strings.ToLower(strings.TrimSpace(value))
	if effort == "" {
		return defaultReasoningEffort
	}
	if _, ok := allowedReasoningEfforts[effort]; !ok {
		return defaultReasoningEffort
	}
	return effort
}

func isRetryableAPIError(err error) bool {
	var statusErr apiHTTPError
	if errors.As(err, &statusErr) {
		return statusErr.statusCode == http.StatusTooManyRequests || statusErr.statusCode >= http.StatusInternalServerError
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return false
}

func readResponsesStream(body io.Reader, maxBytes int) (string, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxBytes+64*1024)

	var output strings.Builder
	var dataLines []string
	processEvent := func(lines []string) error {
		if len(lines) == 0 {
			return nil
		}
		data := strings.TrimSpace(strings.Join(lines, "\n"))
		if data == "" || data == "[DONE]" {
			return nil
		}
		var event responsesStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("unmarshal stream event: %w", err)
		}
		if event.Error != nil {
			return fmt.Errorf("responses stream error: %s", event.Error.Message)
		}
		if event.Response != nil && event.Response.Error != nil {
			return fmt.Errorf("responses completion error: %s", event.Response.Error.Message)
		}
		switch event.Type {
		case "response.output_text.delta":
			if output.Len()+len(event.Delta) > maxBytes {
				return fmt.Errorf("responses output exceeds %d bytes", maxBytes)
			}
			output.WriteString(event.Delta)
		case "response.completed":
			if output.Len() == 0 && event.Response != nil {
				text := extractCompletedResponseText(event.Response)
				if output.Len()+len(text) > maxBytes {
					return fmt.Errorf("responses output exceeds %d bytes", maxBytes)
				}
				output.WriteString(text)
			}
		}
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := processEvent(dataLines); err != nil {
				return "", err
			}
			dataLines = dataLines[:0]
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if err := processEvent(dataLines); err != nil {
		return "", err
	}
	text := strings.TrimSpace(output.String())
	if text == "" {
		return "", fmt.Errorf("empty output stream")
	}
	return text, nil
}

func extractCompletedResponseText(resp *responsesEventResponse) string {
	if resp == nil {
		return ""
	}
	var out strings.Builder
	for _, item := range resp.Output {
		for _, part := range item.Content {
			if part.Type == "output_text" || part.Type == "text" {
				out.WriteString(part.Text)
			}
		}
	}
	return out.String()
}

type responsesRequest struct {
	Model           string                  `json:"model"`
	Instructions    string                  `json:"instructions"`
	Stream          bool                    `json:"stream"`
	Reasoning       *responsesReasoning     `json:"reasoning,omitempty"`
	Input           []responsesInputMessage `json:"input"`
	MaxOutputTokens int                     `json:"max_output_tokens,omitempty"`
}

type responsesReasoning struct {
	Effort string `json:"effort"`
}

type responsesInputMessage struct {
	Role    string                  `json:"role"`
	Content []responsesInputContent `json:"content"`
}

type responsesInputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesStreamEvent struct {
	Type     string                  `json:"type"`
	Delta    string                  `json:"delta,omitempty"`
	Response *responsesEventResponse `json:"response,omitempty"`
	Error    *responsesAPIError      `json:"error,omitempty"`
}

type responsesEventResponse struct {
	Error  *responsesAPIError    `json:"error,omitempty"`
	Output []responsesOutputItem `json:"output,omitempty"`
}

type responsesOutputItem struct {
	Type    string                   `json:"type"`
	Content []responsesOutputContent `json:"content,omitempty"`
}

type responsesOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesAPIError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}

type apiHTTPError struct {
	statusCode int
	body       string
}

func (e apiHTTPError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("responses api status=%d", e.statusCode)
	}
	return fmt.Sprintf("responses api status=%d body=%s", e.statusCode, e.body)
}

const responsesInstructions = `You are implementing files for a coding task.
Return only valid JSON. Do not wrap output in markdown fences.
Required top-level JSON shape:
{
  "summary": "short summary",
  "files": [
    {"path": "relative/path.ext", "content": "file text"}
  ]
}
Paths must be relative and must not contain ".." or start with "/".`
