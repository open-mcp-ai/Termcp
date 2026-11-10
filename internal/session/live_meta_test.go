package session

import (
	"sync"
	"testing"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// A RECORD IS ONE SNAPSHOT, NOT A SET OF FIELDS READ AT DIFFERENT MOMENTS. A rename
// and a status change must never be observable as a mixture: a caller that sees the
// new name also sees everything that was true next to it, and a caller that sees the
// old name sees the old record whole.
//
// The check is not "eventually consistent" — it is that the pair (name, status) always
// comes from a record that existed. Renaming between two names while the status flips
// between two values means a reader may only ever see one of the four combinations the
// writer actually published, never the cross product.
func TestInfoReturnsOneCoherentRecord(t *testing.T) {
	s := &Session{Session: api.Session{ID: "sess-coherence", Status: api.SessionRunning}}
	s.loadLive()
	s.updateLive(func(st *liveMeta) { st.name = "name-0" })

	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			name := "name-0"
			if i%2 == 1 {
				name = "name-1"
			}
			status := api.SessionRunning
			if i%2 == 1 {
				status = api.SessionExited
			}
			s.updateLive(func(st *liveMeta) {
				st.name = name
				st.status = status
			})
		}
	}()
	// Reported from the reading goroutine through a channel: t.Fatalf must run on
	// the test's own goroutine.
	bad := make(chan string, 1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			info := s.Info()
			// Both fields change in the same transition, so only the two published
			// pairs are legal.
			// Only the two published pairs; the initial record is the first of them.
			legal := (info.Name == "name-0" && info.Status == api.SessionRunning) ||
				(info.Name == "name-1" && info.Status == api.SessionExited)
			if !legal {
				select {
				case bad <- "Info returned a mixture no writer published: name=" + info.Name + " status=" + string(info.Status):
				default:
				}
				return
			}
		}
	}()
	wg.Wait()
	select {
	case msg := <-bad:
		t.Fatal(msg)
	default:
	}
}

// THE EMBEDDED COPY IS DELIBERATELY BLANK. If a future change starts writing metadata
// on the embedded api.Session again, readers would silently see a stale record; this
// pins that the authoritative values come from live.
func TestInfoReadsLiveNotTheEmbeddedCopy(t *testing.T) {
	s := &Session{Session: api.Session{ID: "sess-blank", Status: api.SessionRunning, Name: "old"}}
	s.updateLive(func(st *liveMeta) {
		st.name = "new"
		st.status = api.SessionExited
	})
	info := s.Info()
	if info.Name != "new" || info.Status != api.SessionExited {
		t.Fatalf("Info = {%q, %q}, want the live record", info.Name, info.Status)
	}
	if info.ID != "sess-blank" {
		t.Fatalf("Info lost the immutable half: id=%q", info.ID)
	}
}
