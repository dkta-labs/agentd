package top

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
)

func TestParseOptions(t *testing.T) {
	options, err := ParseOptions([]string{"--address", "http://localhost:9000/", "--interval", "750ms", "--once", "--no-clear"}, "http://127.0.0.1:7337")
	if err != nil {
		t.Fatal(err)
	}
	if options.Address != "http://localhost:9000" || options.Interval != 750*time.Millisecond || !options.Once || !options.NoClear {
		t.Fatalf("unexpected options: %#v", options)
	}
	for _, args := range [][]string{
		{"--address", "localhost:7337"},
		{"--address", "http://localhost:7337/jobs"},
		{"--interval", "100ms"},
		{"--interval", "2m"},
		{"extra"},
	} {
		if _, err := ParseOptions(args, "http://127.0.0.1:7337"); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}
}

func TestSnapshotUsesReadOnlyAPIAndFetchesOnlyActiveRuns(t *testing.T) {
	started := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", request.Method)
		}
		requests = append(requests, request.URL.RequestURI())
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/jobs":
			_ = json.NewEncoder(writer).Encode([]Job{
				{ID: "job-active", Name: "active", State: "running", ActiveRunID: "run-current"},
				{ID: "job-paused", Name: "paused", State: "paused"},
			})
		case "/jobs/job-active/runs":
			if request.URL.Query().Get("limit") != "10" {
				t.Fatalf("unexpected limit: %s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(writer).Encode([]Run{{ID: "run-current", State: "running", StartedAt: started}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	snapshot, err := NewClient(server.URL).Snapshot(context.Background(), started.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0] != "/jobs" || requests[1] != "/jobs/job-active/runs?limit=10" {
		t.Fatalf("unexpected requests: %#v", requests)
	}
	if len(snapshot.ActiveRuns) != 1 || snapshot.ActiveRuns["job-active"].ID != "run-current" {
		t.Fatalf("unexpected active runs: %#v", snapshot.ActiveRuns)
	}
}

func TestRenderSortsAndShowsLifecycleTiming(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	last := now.Add(-10 * time.Minute)
	next := now.Add(30 * time.Minute)
	snapshot := Snapshot{
		Address:    "http://127.0.0.1:7337",
		ObservedAt: now,
		Jobs: []Job{
			{ID: "scheduled", Name: "Weekly pulse", State: "scheduled", Iteration: 4, LastRunAt: &last, NextRunAt: &next},
			{ID: "failed", Name: "Broken job", State: "failed", Iteration: 2, LastError: "first failure line\nprivate second line"},
			{ID: "active", Name: "Full candidate checkpoint", GoalKey: "goal/checkpoint", State: "running", Iteration: 3, ActiveRunID: "run_1234567890abcdef", OwnerTarget: "agentd-owner"},
		},
		ActiveRuns: map[string]Run{"active": {ID: "run_1234567890abcdef", State: "running", StartedAt: now.Add(-5*time.Minute - 4*time.Second)}},
	}
	var output bytes.Buffer
	Render(&output, snapshot)
	text := output.String()
	if !strings.Contains(text, "3 jobs  |  1 active  |  1 scheduled  |  1 attention") {
		t.Fatalf("missing summary:\n%s", text)
	}
	running := strings.Index(text, "Full candidate checkpoint")
	failed := strings.Index(text, "Broken job")
	scheduled := strings.Index(text, "Weekly pulse")
	if running < 0 || failed < running || scheduled < failed {
		t.Fatalf("wrong state order:\n%s", text)
	}
	for _, want := range []string{"GOAL", "goal/checkpoint", "run_123456789…", "agentd-owner", "5m04s", "10m00s ago", "in 30m00s", "first failure line"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	scheduledLine := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "Weekly pulse") {
			scheduledLine = line
			break
		}
	}
	if scheduledLine == "" || !strings.Contains(scheduledLine, " - ") {
		t.Fatalf("empty goal was not rendered compactly:\n%s", text)
	}
	if strings.Contains(text, "private second line") {
		t.Fatalf("rendered unbounded multiline error:\n%s", text)
	}
}

func TestRenderSanitizesControlCharacters(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	snapshot := Snapshot{
		Address:    "http://127.0.0.1:7337\x1b[2J",
		ObservedAt: now,
		Jobs: []Job{{
			ID:          "job-control",
			Name:        "visible\tname\nforged\x1b[31m\x7f\u0085\u202e",
			GoalKey:     "goal\tkey\r\nhidden\u200b",
			State:       "running",
			ActiveRunID: "run\tid\x1b[2J\nhidden",
			OwnerTarget: "owner\r\nhidden\u009b",
			LastError:   "\x1b[31mfirst\tline\x00\nprivate second line",
		}},
		ActiveRuns: map[string]Run{},
	}
	var output bytes.Buffer
	Render(&output, snapshot)
	text := output.String()
	for _, r := range text {
		if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029') {
			t.Fatalf("rendered control character U+%04X in %q", r, text)
		}
	}
	if got := strings.Count(text, "\n"); got != 5 {
		t.Fatalf("control payload created extra rows: %d newlines in %q", got, text)
	}
	for _, want := range []string{"http://127.0.0.1:7337[2J", "visible", "goal", "run", "owner", "firstline"} {
		if !strings.Contains(text, want) {
			t.Fatalf("sanitized printable content %q missing:\n%s", want, text)
		}
	}
	if strings.Contains(text, "private second line") {
		t.Fatalf("rendered error detail past first line:\n%s", text)
	}
}

func TestRunOnceReportsUnavailableAgentd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var output bytes.Buffer
	err := Execute(context.Background(), Options{Address: server.URL, Interval: time.Second, Once: true}, &output, false)
	if err == nil || !strings.Contains(err.Error(), "503 Service Unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(output.String(), "unavailable: read jobs") {
		t.Fatalf("missing visible failure:\n%s", output.String())
	}
}

func TestWatchStopsCleanlyOnCancellation(t *testing.T) {
	var count atomic.Int32
	second := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		current := count.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte("[]\n"))
		if current == 2 {
			close(second)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		<-second
		cancel()
	}()
	var output bytes.Buffer
	if err := Execute(ctx, Options{Address: server.URL, Interval: time.Millisecond}, &output, false); err != nil {
		t.Fatal(err)
	}
	if count.Load() < 2 || !strings.Contains(output.String(), "(no jobs)") {
		t.Fatalf("watch did not refresh before cancellation: count=%d output=%q", count.Load(), output.String())
	}
}
