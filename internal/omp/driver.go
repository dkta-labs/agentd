package omp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dkta-labs/agentd/internal/runtime"
)

const maxFrameBytes = 1 << 20

type Driver struct {
	Binary string
}

type Session struct {
	id        string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	sink      runtime.Sink
	writes    chan []byte
	done      chan struct{}
	close     sync.Once
	pendingMu sync.Mutex
	pending   map[string]chan map[string]any
	counter   atomic.Uint64
}

func (d Driver) Start(ctx context.Context, spec runtime.SessionSpec, sink runtime.Sink) (runtime.Session, error) {
	binary := d.Binary
	if binary == "" {
		binary = "omp"
	}
	cmd := exec.CommandContext(ctx, binary, "--mode", "rpc")
	cmd.Dir = spec.WorkspacePath
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open OMP stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open OMP stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open OMP stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start OMP: %w", err)
	}

	session := &Session{
		id:      spec.ID,
		cmd:     cmd,
		stdin:   stdin,
		sink:    sink,
		writes:  make(chan []byte, 32),
		done:    make(chan struct{}),
		pending: make(map[string]chan map[string]any),
	}
	ready := make(chan error, 1)
	go session.writeLoop()
	go session.readStdout(stdout, ready)
	go session.readStderr(stderr)
	go session.wait()

	select {
	case err := <-ready:
		if err != nil {
			_ = session.Close(context.Background())
			return nil, err
		}
	case <-time.After(15 * time.Second):
		_ = session.Close(context.Background())
		return nil, errors.New("timed out waiting for OMP RPC ready frame")
	case <-ctx.Done():
		_ = session.Close(context.Background())
		return nil, context.Cause(ctx)
	}

	handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := session.requireSuccess(handshakeCtx, map[string]any{
		"id":              session.nextID(),
		"type":            "negotiate_protocol",
		"protocolVersion": 2,
	}); err != nil {
		_ = session.Close(context.Background())
		return nil, fmt.Errorf("negotiate OMP RPC protocol v2: %w", err)
	}
	if err := session.requireSuccess(handshakeCtx, map[string]any{
		"id":    session.nextID(),
		"type":  "set_subagent_subscription",
		"level": "events",
	}); err != nil {
		_ = session.Close(context.Background())
		return nil, fmt.Errorf("subscribe to OMP subagent events: %w", err)
	}
	return session, nil
}

func (s *Session) Send(ctx context.Context, input runtime.Input) error {
	if input.Text == "" {
		return errors.New("input text must not be empty")
	}
	command := map[string]any{
		"id":      input.ID,
		"type":    string(input.Mode),
		"message": input.Text,
	}
	if command["id"] == "" {
		command["id"] = s.nextID()
	}
	return s.sendFrameContext(ctx, command)
}

func (s *Session) Respond(ctx context.Context, response runtime.InteractionResponse) error {
	if response.RequestID == "" {
		return errors.New("interaction request id must not be empty")
	}
	variants := 0
	frame := map[string]any{
		"type": "extension_ui_response",
		"id":   response.RequestID,
	}
	if response.Value != nil {
		variants++
		frame["value"] = *response.Value
	}
	if response.Confirmed != nil {
		variants++
		frame["confirmed"] = *response.Confirmed
	}
	if response.Cancelled {
		variants++
		frame["cancelled"] = true
	}
	if variants != 1 {
		return errors.New("interaction response requires exactly one value, confirmation, or cancellation")
	}
	return s.sendFrameContext(ctx, frame)
}

func (s *Session) Abort(ctx context.Context) error {
	return s.sendFrameContext(ctx, map[string]any{"id": s.nextID(), "type": "abort"})
}

func (s *Session) Close(ctx context.Context) error {
	var closeErr error
	s.close.Do(func() {
		_ = s.stdin.Close()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-s.done:
		case <-ctx.Done():
			closeErr = context.Cause(ctx)
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
		case <-timer.C:
			closeErr = errors.New("timed out waiting for OMP to exit")
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
		}
	})
	return closeErr
}

func (s *Session) requireSuccess(ctx context.Context, frame map[string]any) error {
	id, _ := frame["id"].(string)
	if id == "" {
		return errors.New("RPC command id must not be empty")
	}
	responseChannel := make(chan map[string]any, 1)
	s.pendingMu.Lock()
	s.pending[id] = responseChannel
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
	}()

	if err := s.sendFrameContext(ctx, frame); err != nil {
		return err
	}
	select {
	case response := <-responseChannel:
		success, _ := response["success"].(bool)
		if success {
			return nil
		}
		message, _ := response["error"].(string)
		if message == "" {
			message = "RPC command failed"
		}
		return errors.New(message)
	case <-s.done:
		return errors.New("OMP exited before responding to RPC command")
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (s *Session) resolvePending(frame map[string]any) {
	if frameType, _ := frame["type"].(string); frameType != "response" {
		return
	}
	id, _ := frame["id"].(string)
	if id == "" {
		return
	}
	s.pendingMu.Lock()
	responseChannel := s.pending[id]
	s.pendingMu.Unlock()
	if responseChannel != nil {
		select {
		case responseChannel <- frame:
		default:
		}
	}
}

func (s *Session) sendFrameContext(ctx context.Context, frame any) error {
	encoded, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode OMP RPC frame: %w", err)
	}
	encoded = append(encoded, '\n')
	select {
	case s.writes <- encoded:
		return nil
	case <-s.done:
		return errors.New("OMP session has exited")
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (s *Session) writeLoop() {
	for {
		select {
		case frame := <-s.writes:
			if _, err := s.stdin.Write(frame); err != nil {
				s.publish("rpc_write_error", map[string]string{"error": err.Error()})
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *Session) readStdout(stdout io.Reader, ready chan<- error) {
	decoder := newFrameDecoder(stdout)
	readySeen := false
	for {
		frame, err := decoder.Next()
		if err != nil {
			if !readySeen {
				ready <- fmt.Errorf("read OMP ready frame: %w", err)
			} else if !errors.Is(err, io.EOF) {
				s.publish("rpc_protocol_error", map[string]string{"error": err.Error()})
			}
			return
		}
		frameType, _ := frame["type"].(string)
		if !readySeen {
			if frameType != "ready" {
				ready <- fmt.Errorf("expected OMP ready frame, received %q", frameType)
				return
			}
			readySeen = true
			ready <- nil
		}
		s.resolvePending(frame)
		s.publish(frameType, frame)
	}
}

func (s *Session) readStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		s.publish("stderr", map[string]string{"text": scanner.Text()})
	}
	if err := scanner.Err(); err != nil {
		s.publish("stderr_error", map[string]string{"error": err.Error()})
	}
}

func (s *Session) wait() {
	err := s.cmd.Wait()
	payload := map[string]any{"success": err == nil}
	if err != nil {
		payload["error"] = err.Error()
	}
	s.publish("process_exit", payload)
	close(s.done)
}

func (s *Session) publish(eventType string, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = s.sink.Publish(context.Background(), runtime.Event{
		SessionID: s.id,
		Type:      eventType,
		Payload:   encoded,
		CreatedAt: time.Now().UTC(),
	})
}

func (s *Session) nextID() string {
	return fmt.Sprintf("agentd-%d", s.counter.Add(1))
}
