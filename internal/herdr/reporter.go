package herdr

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/dkta-labs/agentd/internal/config"
	"github.com/dkta-labs/agentd/internal/supervisor"
)

type Reporter struct {
	Binary string
}

func (r Reporter) Report(ctx context.Context, workspace config.Workspace, visibility supervisor.Visibility) error {
	workspaceID, ok := herdrWorkspaceID(workspace.ID)
	if !ok {
		return nil
	}
	binary := strings.TrimSpace(r.Binary)
	if binary == "" {
		binary = "herdr"
	}
	args := []string{
		"workspace", "report-metadata", workspaceID,
		"--source", "agentd",
		"--token", "agentd_status=" + visibility.Status,
		"--token", "agentd_job=" + visibility.JobName,
		"--token", "agentd_result=" + visibility.Result,
		"--token", "agentd_job_id=" + visibility.JobID,
	}
	if visibility.RunID == "" {
		args = append(args, "--clear-token", "agentd_run")
	} else {
		args = append(args, "--token", "agentd_run="+visibility.RunID)
	}
	if visibility.Evidence == "" {
		args = append(args, "--clear-token", "agentd_evidence")
	} else {
		args = append(args, "--token", "agentd_evidence="+visibility.Evidence)
	}
	output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("report Herdr metadata: %w", ctx.Err())
	}
	message := strings.TrimSpace(string(output))
	if len(message) > 512 {
		message = message[:512]
	}
	if message == "" {
		return fmt.Errorf("report Herdr metadata: %w", err)
	}
	return fmt.Errorf("report Herdr metadata: %w: %s", err, message)
}

func herdrWorkspaceID(id string) (string, bool) {
	parts := strings.Split(id, ":")
	if len(parts) < 4 || parts[0] != "herdr" || strings.TrimSpace(parts[2]) == "" {
		return "", false
	}
	return parts[2], true
}
