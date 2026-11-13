package mcp

import (
	"context"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/task"
)

func (s *Server) startTransfer(ctx context.Context, kind task.Kind, session, path, localPath string, resources ...task.ResourceRef) (*task.Task, context.Context, func(), error) {
	workCtx, cancel := context.WithCancel(context.Background())
	authority := auth.BackgroundWorkAuthority{}
	switch kind {
	case task.Upload:
		authority.Operations = []string{"file.write"}
	case task.Download:
		authority.Operations = []string{"file.read"}
	case task.Copy:
		authority.Operations = []string{"file.read", "file.write"}
	}
	for _, resource := range resources {
		if resource.HostID != "" {
			authority.Hosts = append(authority.Hosts, resource.HostID)
		}
	}
	release, err := auth.RegisterBackgroundWorkFor(ctx, cancel, authority)
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
