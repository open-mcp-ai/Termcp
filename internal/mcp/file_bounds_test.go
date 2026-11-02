package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/authorization"
	"github.com/open-mcp-ai/termcp/internal/task"
)

func boundedToolDocument(t *testing.T, wire string) map[string]any {
	t.Helper()
	var result struct {
		IsError bool                    `json:"isError"`
		Content []struct{ Text string } `json:"content"`
	}
	if err := json.Unmarshal([]byte(wire), &result); err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("file tool failed: %s", wire)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(result.Content[0].Text), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func awaitBoundedTransfer(t *testing.T, s *Server, id string) {
	t.Helper()
	if id == "" {
		t.Fatal("transfer has no receipt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, err := s.tasks.Await(ctx, id, 10*time.Second)
	if err != nil || v.Status != task.Done {
		t.Fatalf("bounded transfer did not finish: %+v %v", v, err)
	}
}

func fileBoundGrant(forms []string, limit int64) authorization.Grant {
	return authorization.Grant{Effect: authorization.EffectAllow,
		Constraints: &authorization.Constraints{Forms: forms, MaxBytes: &limit}}
}

func TestAgentBoundedFileReadsAcrossMCPTransports(t *testing.T) {
	for _, transport := range []string{"stream", "sse"} {
		t.Run(transport, func(t *testing.T) {
			p := v2Policy("file.read")
			p.Actions["file.read"] = fileBoundGrant([]string{"inline"}, 4)
			h := newV2Harness(t, p)
			sess := h.session("internal")
			cli := h.client(transport)
			path := filepath.Join(t.TempDir(), "source.txt")
			if err := os.WriteFile(path, []byte("ABCDEFGHIJ"), 0600); err != nil {
				t.Fatal(err)
			}
			// Existing DSL reads have no small execution-time bound. A display
			// window or an unused flag cannot supply authorization evidence.
			for _, suffix := range []string{"", " --bytes 4"} {
				out := v2Term(t, cli, "read "+sess.ID+" file $1"+suffix, path)
				if !statusLineIs(out, "403") || strings.Contains(out, "ABCDEFGHIJ") {
					t.Fatalf("unbounded DSL read reached data: %s", out)
				}
			}
			if out := v2Term(t, cli, "read "+sess.ID+" file $1 --length 4", path); !statusLineIs(out, "400") {
				t.Fatal("authorization changed the existing DSL read syntax", out)
			}
			unused := filepath.Join(t.TempDir(), "must-not-be-created.txt")
			for _, mode := range []string{"text", "hex"} {
				doc := boundedToolDocument(t, h.tool("file_read", argsOf("session_id", sess.ID, "remote_path", path,
					"mode", mode, "offset", 2, "length", 4, "local_path", unused)))
				want := "CDEF"
				if mode == "hex" {
					want = "43444546"
				}
				if doc["data"] != want || doc["bytes_read"] != float64(4) {
					t.Fatal("internal file_read exceeded its byte evidence", doc)
				}
			}
			if _, err := os.Stat(unused); !os.IsNotExist(err) || len(h.server.tasks.List()) != 0 {
				t.Fatal("inline local_path created local work", err)
			}
			p.Actions["file.read"] = fileBoundGrant([]string{"inline"}, 8<<20)
			h.adminRequest("PATCH", "/auth/groups/"+h.groupID, map[string]any{"revision": 1, "policy": p}, nil)
			cli = h.client(transport)
			if out := v2Term(t, cli, "read "+sess.ID+" file $1", path); !statusLineIs(out, "200") || !strings.Contains(out, "ABCDEFGHIJ") {
				t.Fatal("existing DSL read failed under its proven inline cap", out)
			}
		})
	}
}

func TestAgentBoundedFileTransfersCheckBothEndpoints(t *testing.T) {
	for _, transport := range []string{"stream", "sse"} {
		t.Run(transport, func(t *testing.T) {
			p := v2Policy("file.read", "file.write")
			for _, id := range []string{"file.read", "file.write"} {
				p.Actions[id] = fileBoundGrant([]string{"local", "pipe"}, 4)
			}
			h := newV2Harness(t, p)
			sess := h.session("internal")
			cli := h.client(transport)
			dir := t.TempDir()
			source, destination := filepath.Join(dir, "source.txt"), filepath.Join(dir, "destination.txt")
			if err := os.WriteFile(source, []byte("ABCDEFGHIJ"), 0600); err != nil {
				t.Fatal(err)
			}
			// Keep the original DSL pipeline syntax. Its destination is literal
			// so these permission tests do not depend on parser changes.
			pipeDestination := " | write " + sess.ID + " file " + strconv.Quote(filepath.ToSlash(destination))
			for _, suffix := range []string{"", " --length 5", " --length 0"} {
				if out := v2Term(t, cli, "write "+sess.ID+" file $2 --local-path $1"+suffix, source, destination); !statusLineIs(out, "403") {
					t.Fatal("upload without sufficient byte evidence passed", out)
				}
			}
			for _, suffix := range []string{"", " --bytes 5", " --bytes 0"} {
				if out := v2Term(t, cli, "read "+sess.ID+" file $1"+suffix+pipeDestination, source); !statusLineIs(out, "403") {
					t.Fatal("pipe without sufficient byte evidence passed", out)
				}
			}
			if out := h.tool("file_read", argsOf("session_id", sess.ID, "remote_path", source,
				"mode", "file", "local_path", destination)); !strings.Contains(out, "forbidden") {
				t.Fatal("unbounded local download passed", out)
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) || len(h.server.tasks.List()) != 0 {
				t.Fatal("denied transfer started local work", err)
			}
			// Upload and download use the exact same range evidence at both ends.
			out := v2Term(t, cli, "write "+sess.ID+" file $2 --local-path $1 --length 4", source, destination)
			if !statusLineIs(out, "202") {
				t.Fatal("bounded upload refused", out)
			}
			awaitBoundedTransfer(t, h.server, headerValueOr(out, "Task", ""))
			if data, err := os.ReadFile(destination); err != nil || string(data) != "ABCD" {
				t.Fatal("upload exceeded its authorization evidence", string(data), err)
			}
			download := filepath.Join(dir, "download.txt")
			doc := boundedToolDocument(t, h.tool("file_read", argsOf("session_id", sess.ID, "remote_path", source,
				"mode", "file", "local_path", download, "offset", 2, "length", 4)))
			awaitBoundedTransfer(t, h.server, doc["task_id"].(string))
			if data, err := os.ReadFile(download); err != nil || string(data) != "CDEF" {
				t.Fatal("download exceeded its authorization evidence", string(data), err)
			}
			out = v2Term(t, cli, "read "+sess.ID+" file $1 --offset 2 --bytes 4"+pipeDestination, source)
			if !statusLineIs(out, "202") {
				t.Fatal("bounded file pipe refused", out)
			}
			awaitBoundedTransfer(t, h.server, headerValueOr(out, "Task", ""))
			if data, err := os.ReadFile(destination); err != nil || string(data) != "CDEF" {
				t.Fatal("pipe exceeded its authorization evidence", string(data), err)
			}
			// Tighten each endpoint independently. A grant at the other endpoint
			// cannot compensate for insufficient source or destination permission.
			before := len(h.server.tasks.List())
			for i, id := range []string{"file.read", "file.write"} {
				for _, action := range []string{"file.read", "file.write"} {
					p.Actions[action] = fileBoundGrant([]string{"local", "pipe"}, 4)
				}
				p.Actions[id] = fileBoundGrant([]string{"local", "pipe"}, 3)
				h.adminRequest("PATCH", "/auth/groups/"+h.groupID, map[string]any{"revision": i + 1, "policy": p}, nil)
				cli = h.client(transport)
				if out := v2Term(t, cli, "write "+sess.ID+" file $2 --local-path $1 --length 4", source, destination); !statusLineIs(out, "403") {
					t.Fatalf("%s upload bound was bypassed: %s", id, out)
				}
				if out := v2Term(t, cli, "read "+sess.ID+" file $1 --bytes 4"+pipeDestination, source); !statusLineIs(out, "403") {
					t.Fatalf("%s pipe bound was bypassed: %s", id, out)
				}
			}
			if len(h.server.tasks.List()) != before {
				t.Fatal("endpoint denial still created a task")
			}
		})
	}
}

func TestAgentInlineWritesCannotHideBytesWithLength(t *testing.T) {
	p := v2Policy("file.write", "file.replace")
	p.Actions["file.write"] = fileBoundGrant([]string{"inline"}, 4)
	h := newV2Harness(t, p)
	sess := h.session("internal")
	path := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if out := h.tool("file_write", argsOf("session_id", sess.ID, "remote_path", path, "data", "12345", "length", 1)); !strings.Contains(out, "forbidden") {
		t.Fatal("ignored inline length supplied a false bound", out)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "original" {
		t.Fatal("denied inline write modified the file", err)
	}
	unused := filepath.Join(t.TempDir(), "unused.txt")
	_ = boundedToolDocument(t, h.tool("file_replace", argsOf("session_id", sess.ID, "remote_path", path,
		"old_text", "original", "new_text", "replaced", "local_path", unused)))
	if data, err := os.ReadFile(path); err != nil || string(data) != "replaced" {
		t.Fatal("replacement was classified as a local transfer", err)
	}
	if _, err := os.Stat(unused); !os.IsNotExist(err) {
		t.Fatal("replacement touched unused local_path", err)
	}
}
