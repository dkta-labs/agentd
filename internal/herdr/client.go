package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"time"
)

type Client struct {
	Binary string
}

type Session struct {
	Name       string `json:"name"`
	Running    bool   `json:"running"`
	SocketPath string `json:"socket_path"`
}

type Snapshot struct {
	Workspaces []Workspace `json:"workspaces"`
	Tabs       []Tab       `json:"tabs"`
	Panes      []Pane      `json:"panes"`
}

type Workspace struct {
	ID          string `json:"workspace_id"`
	Label       string `json:"label"`
	ActiveTabID string `json:"active_tab_id"`
	Focused     bool   `json:"focused"`
	AgentStatus string `json:"agent_status"`
}

type Tab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	AgentStatus string `json:"agent_status"`
}

type Pane struct {
	ID                    string        `json:"pane_id"`
	WorkspaceID           string        `json:"workspace_id"`
	TabID                 string        `json:"tab_id"`
	CWD                   string        `json:"cwd"`
	ForegroundCWD         string        `json:"foreground_cwd"`
	Focused               bool          `json:"focused"`
	Agent                 string        `json:"agent"`
	AgentStatus           string        `json:"agent_status"`
	TerminalTitleStripped string        `json:"terminal_title_stripped"`
	AgentSession          *AgentSession `json:"agent_session"`
}

type AgentSession struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

func (c Client) Sessions(ctx context.Context) ([]Session, error) {
	binary := c.Binary
	if binary == "" {
		binary = "herdr"
	}
	command := exec.CommandContext(ctx, binary, "session", "list", "--json")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list Herdr sessions: %w", err)
	}
	var response struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("decode Herdr session list: %w", err)
	}
	return response.Sessions, nil
}

func (c Client) Snapshot(ctx context.Context, session Session) (Snapshot, error) {
	var result struct {
		Snapshot Snapshot `json:"snapshot"`
	}
	if err := c.Call(ctx, session.SocketPath, "session.snapshot", map[string]any{}, &result); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot Herdr session %q: %w", session.Name, err)
	}
	return result.Snapshot, nil
}

func (Client) Call(ctx context.Context, socketPath, method string, params, result any) error {
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("connect Herdr socket: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	}
	request := struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{ID: "agentd:request", Method: method, Params: params}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return fmt.Errorf("request Herdr %s: %w", method, err)
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return fmt.Errorf("decode Herdr %s: %w", method, err)
	}
	if len(response.Error) > 0 && string(response.Error) != "null" {
		return fmt.Errorf("Herdr %s failed: %s", method, response.Error)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("decode Herdr %s result: %w", method, err)
	}
	return nil
}
