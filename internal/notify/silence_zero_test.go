package notify

import "testing"

// `--for 0` must mean ZERO, not "the caller said nothing".
//
// The parameter arrives as an optional integer, where absent and zero look alike.
// Register used to take an int and read `<= 0` as "unspecified", so the one value
// that asks for the opposite of waiting was quietly replaced by the three-second
// default — and the reply did not echo the wait, so the caller could not tell.
func TestSilenceZeroIsHonoured(t *testing.T) {
	m := NewManager(nil)
	zero := 0
	rule, err := m.Register("s", "sh", ChannelResource, EventSilence, &zero, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rule.SilenceSec != 0 {
		t.Fatalf("SilenceSec = %d, want 0", rule.SilenceSec)
	}
}

// An absent --for keeps the documented default.
func TestSilenceDefaultWhenAbsent(t *testing.T) {
	m := NewManager(nil)
	rule, err := m.Register("s", "sh", ChannelResource, EventSilence, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rule.SilenceSec != defaultSilenceSec {
		t.Fatalf("SilenceSec = %d, want the default %d", rule.SilenceSec, defaultSilenceSec)
	}
}

// An explicit value is kept, and the upper bound still applies so a typo cannot
// park a timer for hours.
func TestSilenceValueIsKeptAndCapped(t *testing.T) {
	m := NewManager(nil)
	seven := 7
	rule, err := m.Register("s", "sh", ChannelResource, EventSilence, &seven, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rule.SilenceSec != 7 {
		t.Fatalf("SilenceSec = %d, want 7", rule.SilenceSec)
	}

	huge := maxSilenceSec + 1000
	capped, err := m.Register("s", "sh2", ChannelResource, EventSilence, &huge, nil)
	if err != nil {
		t.Fatal(err)
	}
	if capped.SilenceSec != maxSilenceSec {
		t.Fatalf("SilenceSec = %d, want the cap %d", capped.SilenceSec, maxSilenceSec)
	}
}

// silence_seconds is meaningless for the other events and must not leak into them.
func TestSilenceSecondsIsZeroForOtherEvents(t *testing.T) {
	m := NewManager(nil)
	five := 5
	for _, ev := range []Event{EventOutput, EventExit} {
		rule, err := m.Register("s", "sh-"+string(ev), ChannelResource, ev, &five, nil)
		if err != nil {
			t.Fatal(err)
		}
		if rule.SilenceSec != 0 {
			t.Errorf("%s rule carries SilenceSec = %d, want 0", ev, rule.SilenceSec)
		}
	}
}
