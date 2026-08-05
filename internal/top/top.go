package top

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
)

const (
	defaultInterval = 2 * time.Second
	maxResponseSize = 4 << 20
)

type Options struct {
	Address  string
	Interval time.Duration
	Once     bool
	NoClear  bool
}

type Job struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	GoalKey        string     `json:"goalKey"`
	State          string     `json:"state"`
	DesiredState   string     `json:"desiredState"`
	Iteration      int64      `json:"iteration"`
	RunRequested   bool       `json:"runRequested"`
	ActiveRunID    string     `json:"activeRunId"`
	OwnerTarget    string     `json:"ownerTarget"`
	NextRunAt      *time.Time `json:"nextRunAt"`
	LastRunAt      *time.Time `json:"lastRunAt"`
	LastError      string     `json:"lastError"`
	CadenceSeconds int        `json:"cadenceSeconds"`
}

type Run struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

type Snapshot struct {
	Address    string
	ObservedAt time.Time
	Jobs       []Job
	ActiveRuns map[string]Run
}

type Client struct {
	address string
	http    *http.Client
}

func ParseOptions(args []string, defaultAddress string) (Options, error) {
	options := Options{Address: defaultAddress, Interval: defaultInterval}
	flags := flag.NewFlagSet("agentd top", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.Address, "address", options.Address, "Agentd HTTP address")
	flags.DurationVar(&options.Interval, "interval", options.Interval, "refresh interval")
	flags.BoolVar(&options.Once, "once", false, "render one snapshot and exit")
	flags.BoolVar(&options.NoClear, "no-clear", false, "do not clear the terminal between frames")
	if err := flags.Parse(args); err != nil {
		return Options{}, err
	}
	if flags.NArg() != 0 {
		return Options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	address, err := normalizeAddress(options.Address)
	if err != nil {
		return Options{}, err
	}
	options.Address = address
	if options.Interval < 250*time.Millisecond || options.Interval > time.Minute {
		return Options{}, errors.New("interval must be between 250ms and 1m")
	}
	return options, nil
}

func normalizeAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid Agentd address %q", value)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("Agentd address must not contain a path, query, or fragment")
	}
	return strings.TrimRight(value, "/"), nil
}

func NewClient(address string) *Client {
	return &Client{
		address: strings.TrimRight(address, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

func (client *Client) Snapshot(ctx context.Context, now time.Time) (Snapshot, error) {
	var jobs []Job
	if err := client.getJSON(ctx, "/jobs", &jobs); err != nil {
		return Snapshot{}, fmt.Errorf("read jobs: %w", err)
	}
	activeRuns := make(map[string]Run)
	for _, job := range jobs {
		if job.ActiveRunID == "" {
			continue
		}
		var runs []Run
		path := "/jobs/" + url.PathEscape(job.ID) + "/runs?limit=10"
		if err := client.getJSON(ctx, path, &runs); err != nil {
			return Snapshot{}, fmt.Errorf("read active run for %s: %w", job.Name, err)
		}
		for _, run := range runs {
			if run.ID == job.ActiveRunID {
				activeRuns[job.ID] = run
				break
			}
		}
	}
	return Snapshot{Address: client.address, ObservedAt: now, Jobs: jobs, ActiveRuns: activeRuns}, nil
}

func (client *Client) getJSON(ctx context.Context, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.address+path, nil)
	if err != nil {
		return err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Agentd returned %s", response.Status)
	}
	reader := io.LimitReader(response.Body, maxResponseSize+1)
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("decode response: multiple JSON values")
		}
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func Execute(ctx context.Context, options Options, output io.Writer, interactive bool) error {
	client := NewClient(options.Address)
	ticker := time.NewTicker(options.Interval)
	defer ticker.Stop()
	for {
		snapshot, err := client.Snapshot(ctx, time.Now())
		if ctx.Err() != nil {
			return nil
		}
		if interactive && !options.NoClear {
			_, _ = io.WriteString(output, "\x1b[H\x1b[2J")
		}
		if err != nil {
			fmt.Fprintf(output, "AGENTD TOP  %s  %s\n\nunavailable: %s\n", time.Now().Format("2006-01-02 15:04:05"), sanitize(options.Address), sanitize(err.Error()))
		} else {
			Render(output, snapshot)
		}
		if options.Once {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func Render(output io.Writer, snapshot Snapshot) {
	sort.SliceStable(snapshot.Jobs, func(left, right int) bool {
		lp, rp := statePriority(snapshot.Jobs[left].State), statePriority(snapshot.Jobs[right].State)
		if lp != rp {
			return lp < rp
		}
		return strings.ToLower(snapshot.Jobs[left].Name) < strings.ToLower(snapshot.Jobs[right].Name)
	})
	active, scheduled, attention := 0, 0, 0
	for _, job := range snapshot.Jobs {
		switch job.State {
		case "starting", "running", "stopping":
			active++
		case "scheduled":
			scheduled++
		}
		if job.State == "failed" || job.State == "interrupted" || job.LastError != "" {
			attention++
		}
	}
	fmt.Fprintf(output, "AGENTD TOP  %s  %s\n", snapshot.ObservedAt.Local().Format("2006-01-02 15:04:05"), sanitize(snapshot.Address))
	fmt.Fprintf(output, "%d jobs  |  %d active  |  %d scheduled  |  %d attention\n\n", len(snapshot.Jobs), active, scheduled, attention)
	if len(snapshot.Jobs) == 0 {
		io.WriteString(output, "(no jobs)\n")
		return
	}
	writer := tabwriter.NewWriter(output, 0, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "STATE\tJOB\tGOAL\tITER\tACTIVE RUN\tOWNER\tELAPSED\tLAST\tNEXT\tDETAIL")
	for _, job := range snapshot.Jobs {
		runID, elapsed := "-", "-"
		if job.ActiveRunID != "" {
			runID = truncate(job.ActiveRunID, 14)
			if run, ok := snapshot.ActiveRuns[job.ID]; ok && !run.StartedAt.IsZero() {
				elapsed = shortDuration(snapshot.ObservedAt.Sub(run.StartedAt))
			} else {
				elapsed = "?"
			}
		}
		owner := "-"
		if job.OwnerTarget != "" {
			owner = truncate(job.OwnerTarget, 22)
		}
		goal := "-"
		if job.GoalKey != "" {
			goal = truncate(job.GoalKey, 20)
		}
		last, next := "-", "-"
		if job.LastRunAt != nil {
			last = age(snapshot.ObservedAt, *job.LastRunAt)
		}
		if job.NextRunAt != nil {
			next = relativeFuture(snapshot.ObservedAt, *job.NextRunAt)
		}
		detail := firstLine(job.LastError)
		if detail == "" && job.RunRequested {
			detail = "run requested"
		}
		if detail == "" && job.CadenceSeconds > 0 && job.State == "paused" {
			detail = "cadence paused"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			sanitize(strings.ToUpper(job.State)), truncate(job.Name, 34), goal, job.Iteration, runID, owner,
			elapsed, last, next, truncate(detail, 42))
	}
	_ = writer.Flush()
}

func statePriority(state string) int {
	switch state {
	case "starting", "running", "stopping":
		return 0
	case "failed", "interrupted":
		return 1
	case "scheduled":
		return 2
	case "paused", "stopped":
		return 3
	default:
		return 4
	}
}

func shortDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	value = value.Round(time.Second)
	if value < time.Minute {
		return fmt.Sprintf("%ds", int(value.Seconds()))
	}
	if value < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(value/time.Minute), int(value%time.Minute/time.Second))
	}
	if value < 24*time.Hour {
		return fmt.Sprintf("%dh%02dm", int(value/time.Hour), int(value%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dd%02dh", int(value/(24*time.Hour)), int(value%(24*time.Hour)/time.Hour))
}

func age(now, value time.Time) string {
	if value.After(now) {
		return "now"
	}
	return shortDuration(now.Sub(value)) + " ago"
}

func relativeFuture(now, value time.Time) string {
	if !value.After(now) {
		return "due"
	}
	return "in " + shortDuration(value.Sub(now))
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = value[:index]
	}
	return sanitize(value)
}

func truncate(value string, limit int) string {
	value = sanitize(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}

func sanitize(value string) string {
	var sanitized strings.Builder
	sanitized.Grow(len(value))
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			continue
		}
		sanitized.WriteRune(r)
	}
	return sanitized.String()
}
