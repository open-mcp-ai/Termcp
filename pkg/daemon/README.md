# Managing a service from a Go CLI

Import `github.com/open-mcp-ai/termcp/pkg/daemon`. Each invocation constructs a
manager with the **same name, private data directory, and service endpoint**.
The constructor does not start anything. The default executable is the calling
binary; its `serve` command must run in the foreground (do not detach twice).

```go
mgr := daemon.New(daemon.Config{
    Name:      "indexer",
    DataDir:   privateStateDir,
    BuildArgs: func() []string { return []string{"serve"} },
    Probe:     probeIndexer,
    RequestStop: stopIndexer,
    ReadyWait: 10 * time.Second,
})
instance, err := mgr.Start(ctx) // returns an existing ready instance, or starts one
```

`probeIndexer` and `stopIndexer` are application functions of type
`func(context.Context) error`. A probe returns nil only for the expected service,
`daemon.ErrNotRunning` for definite absence, and another error for an unknown
state. Return a permission error, not `ErrNotRunning`, when credentials fail.
`daemon.HTTPProbe(url)` is a convenience for a **dedicated**, direct health URL:
it accepts 2xx, refuses redirects, and cannot identify the service behind a
shared port. Use an authenticated implementation for endpoints that can be
reused by other services.

Use `mgr.Status(ctx)` in the `status` subcommand. Check `status.Err` before
reading `status.Running`: unreachable and stopped are different. Use
`mgr.Stop(ctx)` in the `stop` subcommand. Supply an authenticated, instance-specific
shutdown request (for example, an HTTP POST with private credentials) that does
not follow redirects. The manager does not kill a process using a saved PID.
Without a stop callback, stopping a running instance returns an error.

Concurrent operations may return `daemon.ErrBusy`; retry after the current CLI
call finishes. The `.lock` file is kept even when the OS lock is released. Do
not remove it to bypass a busy result. PID files are bookkeeping, not authority
to signal a process. If a launch was interrupted and state is indeterminate,
verify the service and the process before manually repairing its records.

`daemon.NewIdleWatcher` wraps an HTTP handler to track in-flight requests;
set `Exempt` for management probes before serving. `daemon.PortReachable` tests
only for a TCP listener. Termcp's own `/api.md` fingerprint and its authenticated
HTTP stop path remain specific to Termcp.
