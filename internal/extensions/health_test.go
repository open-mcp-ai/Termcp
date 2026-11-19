package extensions

import "testing"

func TestExtensionHealthCountsOnlyResidentReplacements(t *testing.T) {
	var s extState
	s.want() // an acquire might fail before a channel starts
	if v := s.snapshot(); v.LastActiveAt != 0 || v.Restarts != 0 {
		t.Fatalf("request without launch: %+v", v)
	}
	s.attach("session-a", "child-a")
	s.attach("session-a", "child-a") // forwarding retry on the existing child
	s.addExtra("session-a", "extra-a", "", "")
	s.addExtra("session-a", "extra-b", "", "")
	if v := s.snapshot(); v.LastActiveAt == 0 || v.Restarts != 0 {
		t.Fatalf("first launch/retry/extra: %+v", v)
	}
	s.takeAttachment()
	s.attach("session-b", "child-b")
	if v := s.snapshot(); v.Restarts != 1 {
		t.Fatalf("replacement: %+v", v)
	}
	multi := foldStatus([]Status{{Status: StatusProbing, Health: Health{Restarts: 2, LastActiveAt: 10, PID: 123}}, {Health: Health{Restarts: 3, LastActiveAt: 20}}})
	if multi.Restarts != 5 || multi.LastActiveAt != 20 || multi.PID != 0 || multi.ChannelOpen != nil {
		t.Fatalf("multi-host summary: %+v", multi)
	}
}
