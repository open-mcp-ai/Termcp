package mcp

import (
	"context"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/pkg/api"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// BATCH TARGETS: several objects named in one call, answered one slot per object.
//
// WHY THE COMMA. It is already this surface's batch spelling — `run a,b $1`,
// `read a,b` and `input a,b` split the target the same way — and it keeps the words
// out of the positional positions, which are the ones that change what a statement
// acts on when a value lands in them (issue #199: `close s:1 $1` closed the whole
// session). A comma list is ONE word, so it cannot be mistaken for anything else.
//
// WHY ONE SLOT PER ENTRY, not one body with a table (#339, #341). A slot carries a
// code, and the code is what a caller branches on: with a single aggregate slot,
// "erased 4 of 6" has to be either a 200 (which hides the two failures) or a 409
// (which claims nothing worked). One slot per target says exactly what happened to
// each, in the order the caller wrote them, and the reply's IsError is true if any
// entry failed — the rule term/envelope already documents.
//
// PARTIAL FAILURE NEVER ROLLS BACK. Every entry runs independently: an id that no
// longer resolves does not stop the one after it, and what was erased stays erased.

// splitTargetList splits a comma-separated target into its entries, or reports that
// there is only one (a plain target, which must keep its old single path).
func splitTargetList(target string) (entries []string, isList bool) {
	if !strings.Contains(target, ",") {
		return nil, false
	}
	for _, part := range strings.Split(target, ",") {
		if part = strings.TrimSpace(part); part != "" {
			entries = append(entries, part)
		}
	}
	return entries, true
}

// closeTargets expands one `close` into the objects it names, or refuses the call
// that would mean two things at once.
//
// THE THREE SPELLINGS ARE NOT SYNONYMS, and each is checked where it can go wrong:
//
//	close a1f3:1,b2c4:1      explicit list; each entry is closed as written
//	close a1f3 --index 1,2,3 channels of ONE session, spelled out
//	close a1f3 --all         the container (`close a1f3` already means this)
//
// `--all` on a CHANNEL HANDLE IS REFUSED rather than upgraded. `close a1f3:1 --all`
// reads like "everything under this handle", and the thing under it is the whole
// session — the exact widening `close <session> --force` used to do silently (#132,
// #199). The refusal names `close a1f3`, which says what it does.
func closeTargets(stmt directive.Statement) ([]string, *envelope.Slot) {
	list, isList := splitTargetList(stmt.Target)
	if !isList {
		list = []string{stmt.Target}
	}
	indexFlag := stmt.FlagState("index") != directive.FlagAbsent
	all := boolFlag(stmt, "all")
	refuse := func(body string) *envelope.Slot {
		s := badRequest(stmt, body)
		return &s
	}

	if indexFlag && all {
		return nil, refuse("`--index` names channels and `--all` names the whole session: one call cannot mean " +
			"both.\nPick one: `close " + stmt.Target + " --index 1,2,3` or `close " + stmt.Target + " --all`.")
	}
	if indexFlag {
		if len(list) != 1 {
			return nil, refuse("`--index` belongs to ONE session: the channels it names are that session's, so a " +
				"target list beside it would be ambiguous.\n" +
				"Write the handles out instead: `close a1f3:1,b2c4:1`.")
		}
		if directive.NamesAChannel(list[0]) {
			return nil, refuse("`" + list[0] + "` IS a channel, and `--index` names channels of a session.\n" +
				"Either list the channels: `close " + list[0] + ",<session>:<n>` — or name them from the " +
				"session: `close " + directive.SplitHandleSession(list[0]) + " --index 1,2,3`.")
		}
		raw, _ := stmt.Flag("index")
		var out []string
		for _, part := range strings.Split(raw, ",") {
			idx := strings.TrimSpace(part)
			if idx == "" {
				continue
			}
			if _, ok := directive.ChannelIndex(idx); !ok {
				return nil, refuse("`--index " + raw + "`: `" + idx + "` is not a channel index. Channels are " +
					"numbered from 1 and the number is what `ls <session>` prints.")
			}
			out = append(out, list[0]+":"+idx)
		}
		if len(out) == 0 {
			return nil, refuse("`--index` needs at least one channel number: `close " + list[0] + " --index 1,2,3`.")
		}
		return out, nil
	}
	if all {
		if len(list) != 1 {
			return nil, refuse("`--all` names ONE session (`close a1f3 --all`), so it cannot sit beside a target list.\n" +
				"Drop the flag: `close a1f3,b2c4` closes both containers.")
		}
		if directive.NamesAChannel(list[0]) {
			return nil, refuse("`" + list[0] + "` is a channel, and `--all` would close the session it belongs to — " +
				"every other channel on that link with it.\n" +
				"Say so without the handle: `close " + directive.SplitHandleSession(list[0]) +
				"` (that is what `--all` means), or close just this channel: `close " + list[0] + "`.")
		}
		// The container close IS what `close <session>` does; --all states it.
		return list, nil
	}
	return list, nil
}

// closeBatch answers a multi-target close, one slot per target, in the order the
// caller wrote them. ok=false means "this was not a batch" and the caller keeps its
// single-target path byte for byte.
func (s *Server) closeBatch(ctx context.Context, stmt directive.Statement, win termArgs) ([]envelope.Slot, bool) {
	targets, bad := closeTargets(stmt)
	if bad != nil {
		return []envelope.Slot{*bad}, true
	}
	if len(targets) <= 1 && !boolFlag(stmt, "all") && stmt.FlagState("index") == directive.FlagAbsent {
		return nil, false
	}
	slots := make([]envelope.Slot, 0, len(targets))
	for _, target := range targets {
		single := stmt
		single.Target = target
		// THE BATCH FLAGS DO NOT SURVIVE THE EXPANSION: `--index`/`--all` described
		// how the list was built, and the entry they became is just a handle. Leaving
		// them set would make directiveClose refuse the channel it was handed (a
		// channel with `--grace` is meaningless, and the flag is checked there).
		single.Flags = map[string]*string{}
		for k, v := range stmt.Flags {
			if k == "index" || k == "all" {
				continue
			}
			single.Flags[k] = v
		}
		slot := s.directiveClose(ctx, single, win)
		if slot.Headers == nil {
			slot.Headers = map[string]string{}
		}
		// THE SLOT SAYS WHICH TARGET IT ANSWERS. Every slot of a batch echoes the same
		// Path (there is one line), so without this header a failure in the third block
		// cannot be attributed to the third handle.
		slot.Headers["Batch"] = target
		slots = append(slots, slot)
	}
	return slots, true
}

// rmBatch answers a multi-target `rm`, one slot per target.
//
// Running sessions are refused PER ENTRY and the rest still run, which is the
// boundary #339 draws: "clear the archive" must never take a live session with it,
// and a batch that contains one is told which one without the others being spared.
func (s *Server) rmBatch(ctx context.Context, stmt directive.Statement, win termArgs) ([]envelope.Slot, bool) {
	if boolFlag(stmt, "dead") {
		return []envelope.Slot{s.rmArchiveSweep(ctx, stmt)}, true
	}
	list, isList := splitTargetList(stmt.Target)
	if !isList {
		return nil, false
	}
	slots := make([]envelope.Slot, 0, len(list))
	for _, target := range list {
		single := stmt
		single.Target = target
		slot := s.directiveRemove(ctx, single, win)
		if slot.Headers == nil {
			slot.Headers = map[string]string{}
		}
		slot.Headers["Batch"] = target
		slots = append(slots, slot)
	}
	return slots, true
}

// rmArchiveSweep erases every session that has ENDED — the "clear the archive" call
// #339 asks for — and answers one slot per entry.
//
// IT NEVER TOUCHES A RUNNING SESSION, not even with --force: --force exists so a
// caller can erase a live session it named, and this call names its targets by
// STATE. Reading --force as "include the running ones" would make the one call whose
// whole purpose is "the finished ones" able to take live work with it, which is the
// thing #339 drew the line around.
//
// NOTHING TO ERASE IS A 200 WITH NO SLOT PER ENTRY, to stay with the rule that a
// 204 means "ran, delivered nothing": a sweep that found nothing did not run anything.
func (s *Server) rmArchiveSweep(ctx context.Context, stmt directive.Statement) envelope.Slot {
	if boolFlag(stmt, "force") {
		return badRequest(stmt, "`--force` erases a RUNNING session the caller named, and `--dead` names its "+
			"targets by state: it clears what has already ended and never touches a live session.\n"+
			"A flag that cannot apply is refused rather than dropped, because `--dead --force` reads as "+
			"\"clear the archive, including whatever is still running\". To erase one live session, name it: "+
			"`rm <session> --force`.")
	}
	dry := boolFlag(stmt, "dry-run")
	var ended []api.Session
	for _, info := range s.sessMgr.ListAll() {
		if info.Status != api.SessionRunning {
			ended = append(ended, info)
		}
	}
	if len(ended) == 0 {
		return envelope.Slot{Code: 200, Headers: map[string]string{
			"Path": stmt.Path,
		}, Body: "no finished sessions to clear: every session on this instance is running.\n" +
			"`close <session>` stops one and keeps it readable; this call erases the records of sessions " +
			"that have already ended."}
	}
	// The slots are built one per session by the same code the named batch uses, so
	// the per-entry rules (running refusal, force, dry-run) cannot drift between the
	// two spellings.
	slots := make([]envelope.Slot, 0, len(ended))
	for _, info := range ended {
		single := stmt
		single.Target = info.ID
		slot := s.directiveRemove(ctx, single, termArgs{})
		if slot.Headers == nil {
			slot.Headers = map[string]string{}
		}
		slot.Headers["Batch"] = info.ID
		if info.Name != "" {
			slot.Headers["Name"] = info.Name
		}
		slots = append(slots, slot)
	}
	return batchReplySlot(stmt, slots, dry)
}

// batchReplySlot folds per-entry slots into the single slot a sweep answers with.
//
// A SWEEP IS ONE QUESTION ("what is left in the archive"), and answering it with
// forty status lines would make the caller count blocks to find the two that failed.
// The per-entry outcomes are here as text, the code is the worst one, and the count
// line says how it went at a glance.
func batchReplySlot(stmt directive.Statement, slots []envelope.Slot, dry bool) envelope.Slot {
	var b strings.Builder
	failed := 0
	verb := "erased"
	if dry {
		verb = "would erase"
	}
	for _, slot := range slots {
		id := slot.Headers["Batch"]
		switch {
		case slot.Code >= 400:
			failed++
			b.WriteString("FAIL  " + id + "  " + firstLineOf(slot.Body) + "\n")
		default:
			b.WriteString("ok    " + id + "  " + verb + "\n")
		}
	}
	b.WriteString("\n" + strconv.Itoa(len(slots)-failed) + " " + verb + ", " + strconv.Itoa(failed) + " failed")
	if failed > 0 {
		b.WriteString(" — a failed entry is the only one that did not run; the rest are gone.")
	}
	b.WriteString("\nThe session directory goes with the record: manifests, log.bin and log.jsonl. " +
		"This cannot be undone, and `close <session>` (which keeps the transcript) is the other choice.")
	headers := map[string]string{"Path": stmt.Path, "Count": strconv.Itoa(len(slots))}
	if dry {
		headers["Dry-Run"] = "true"
	}
	code := 200
	if failed > 0 {
		code = 409
		headers["Error-Code"] = CodeConflict
	}
	return envelope.Slot{Code: code, Headers: headers, Body: b.String()}
}
