package githubhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxPayloadBytes = 1 << 20

type receiptLedger interface {
	Acquire(context.Context, string, string, time.Time) (AcquireResult, error)
	MarkDelivered(context.Context, string, string, time.Time) error
	Release(context.Context, string, string) error
}

type Dispatcher interface {
	Run(context.Context, string) error
}

type Server struct {
	rules      map[string][]Rule
	secret     []byte
	ledger     receiptLedger
	dispatcher Dispatcher
	now        func() time.Time
}

type githubPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest *struct {
		Merged bool `json:"merged"`
	} `json:"pull_request"`
}

type response struct {
	Status     string `json:"status"`
	Matched    int    `json:"matched"`
	Dispatched int    `json:"dispatched"`
	Duplicates int    `json:"duplicates"`
}

func NewServer(rules []Rule, secret []byte, ledger receiptLedger, dispatcher Dispatcher) (*Server, error) {
	if len(rules) == 0 || len(secret) == 0 || ledger == nil || dispatcher == nil {
		return nil, errors.New("rules, secret, ledger, and dispatcher are required")
	}
	indexed := make(map[string][]Rule)
	for _, rule := range rules {
		indexed[rule.Event] = append(indexed[rule.Event], rule)
	}
	return &Server{rules: indexed, secret: secret, ledger: ledger, dispatcher: dispatcher, now: time.Now}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /github", s.github)
	return mux
}

func (s *Server) github(writer http.ResponseWriter, request *http.Request) {
	event := strings.TrimSpace(request.Header.Get("X-GitHub-Event"))
	deliveryID := strings.TrimSpace(request.Header.Get("X-GitHub-Delivery"))
	if !validName(event) || !validName(deliveryID) {
		writeError(writer, http.StatusBadRequest, "valid GitHub event and delivery headers are required")
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "content type must be application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxPayloadBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(writer, http.StatusRequestEntityTooLarge, "payload exceeds one MiB")
			return
		}
		writeError(writer, http.StatusBadRequest, "cannot read payload")
		return
	}
	if !validSignature(s.secret, body, request.Header.Get("X-Hub-Signature-256")) {
		writeError(writer, http.StatusUnauthorized, "invalid webhook signature")
		return
	}
	var payload githubPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(writer, http.StatusBadRequest, "payload must be valid JSON")
		return
	}
	matched := matchingRules(s.rules[event], payload)
	if len(matched) == 0 {
		writeJSON(writer, http.StatusAccepted, response{Status: "ignored"})
		return
	}
	result := response{Status: "accepted", Matched: len(matched)}
	retryable := false
	for _, rule := range matched {
		acquired, err := s.ledger.Acquire(request.Context(), deliveryID, rule.ID, s.now())
		if err != nil {
			retryable = true
			continue
		}
		switch acquired {
		case Duplicate:
			result.Duplicates++
			continue
		case InProgress:
			retryable = true
			continue
		}
		if err := s.dispatcher.Run(request.Context(), rule.JobID); err != nil {
			_ = s.ledger.Release(context.WithoutCancel(request.Context()), deliveryID, rule.ID)
			retryable = true
			continue
		}
		if err := s.ledger.MarkDelivered(context.WithoutCancel(request.Context()), deliveryID, rule.ID, s.now()); err != nil {
			retryable = true
			continue
		}
		result.Dispatched++
	}
	if retryable {
		result.Status = "retry"
		writer.Header().Set("Retry-After", "30")
		writeJSON(writer, http.StatusServiceUnavailable, result)
		return
	}
	writeJSON(writer, http.StatusAccepted, result)
}

func matchingRules(rules []Rule, payload githubPayload) []Rule {
	matched := make([]Rule, 0, len(rules))
	for _, rule := range rules {
		if rule.Action != payload.Action || rule.Repository != payload.Repository.FullName {
			continue
		}
		if rule.Merged != nil {
			if payload.PullRequest == nil || payload.PullRequest.Merged != *rule.Merged {
				continue
			}
		}
		matched = append(matched, rule)
	}
	return matched
}

func validSignature(secret, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}

type AgentdDispatcher struct {
	baseURL string
	client  *http.Client
}

func NewAgentdDispatcher(baseURL string, client *http.Client) (*AgentdDispatcher, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return nil, errors.New("valid Agentd base URL is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &AgentdDispatcher{baseURL: strings.TrimRight(baseURL, "/"), client: client}, nil
}

func (d *AgentdDispatcher) Run(ctx context.Context, jobID string) error {
	endpoint := d.baseURL + "/jobs/" + url.PathEscape(jobID) + "/run"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := d.client.Do(request)
	if err != nil {
		return fmt.Errorf("request Agentd run: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 32<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Agentd run request returned %s", response.Status)
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}
