package mcp

import (
	"context"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/task"
)

func (s *Server) startTransfer(ctx context.Context, kind task.Kind, session, path, localPath string, resources ...task.ResourceRef) (*task.Task, context.Context, func(), error) {
	workCtx, cancel := context.WithCancel(context.Background())
	release, err := auth.RegisterBackgroundWork(ctx, cancel)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	if err = workCtx.Err(); err != nil {
		release()
		cancel()
		return nil, nil, nil, err
	}
	transfer := s.tasks.Start(kind, session, path, localPath, cancel, resources...)
	finish := func() { release(); cancel() }
	return transfer, workCtx, finish, nil
}

// Closing an I/O client to interrupt cancellation may return a transport error;
// the receipt should still describe the authoritative cancellation reason.
func (s *Server) finishTransfer(ctx context.Context, id string, err error) {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	s.tasks.Finish(id, err)
}
