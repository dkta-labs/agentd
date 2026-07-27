package fleet

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/dkta-labs/agentd/internal/runtime"
)

type Driver struct {
	Local   runtime.Driver
	Service *Service
}

func (d Driver) Start(ctx context.Context, spec runtime.SessionSpec, sink runtime.Sink) (runtime.Session, error) {
	if d.Service != nil && d.Service.Enabled() {
		if node, err := d.Service.Select(spec.WorkspaceID); err == nil {
			d.Service.Attach(spec.ID, node.ID, sink)
			startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := d.Service.Dispatch(startCtx, node.ID, "start", spec.ID, StartPayload{WorkspaceID: spec.WorkspaceID, HarnessID: "omp"}); err != nil {
				d.Service.Detach(spec.ID)
				return nil, err
			}
			return &remoteSession{service: d.Service, node: node, sessionID: spec.ID}, nil
		}
	}
	if d.Local == nil {
		return nil, errors.New("no local runtime or compatible worker is available")
	}
	started, err := d.Local.Start(ctx, spec, sink)
	if err != nil {
		return nil, err
	}
	hostname, _ := os.Hostname()
	return &localSession{Session: started, placement: runtime.Placement{NodeID: "local", NodeName: hostname, Reason: "local workspace available"}}, nil
}

type localSession struct {
	runtime.Session
	placement runtime.Placement
}

func (s *localSession) Placement() runtime.Placement { return s.placement }
func (s *localSession) AttachLocally() bool          { return true }

type remoteSession struct {
	service   *Service
	node      Node
	sessionID string
}

func (s *remoteSession) Placement() runtime.Placement {
	return runtime.Placement{NodeID: s.node.ID, NodeName: s.node.Name, Reason: "online worker has the requested workspace", Remote: true}
}

func (s *remoteSession) AttachLocally() bool { return false }

func (s *remoteSession) Send(ctx context.Context, input runtime.Input) error {
	return s.service.Dispatch(ctx, s.node.ID, "input", s.sessionID, InputPayload{ID: input.ID, IdempotencyKey: input.IdempotencyKey, Mode: string(input.Mode), Text: input.Text})
}

func (s *remoteSession) Respond(ctx context.Context, response runtime.InteractionResponse) error {
	return s.service.Dispatch(ctx, s.node.ID, "respond", s.sessionID, InteractionPayload{RequestID: response.RequestID, Value: response.Value, Confirmed: response.Confirmed, Cancelled: response.Cancelled})
}

func (s *remoteSession) Abort(ctx context.Context) error {
	return s.service.Dispatch(ctx, s.node.ID, "abort", s.sessionID, struct{}{})
}

func (s *remoteSession) Close(ctx context.Context) error {
	defer s.service.Detach(s.sessionID)
	return s.service.Dispatch(ctx, s.node.ID, "close", s.sessionID, struct{}{})
}
