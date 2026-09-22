// Package clock is the project's single definition of "what time is it".
//
// Every timestamp termcp stores or serves is Unix milliseconds: log.jsonl marks,
// session and shell manifests, forward and notification metadata, SFTP mod_time.
// That unit is chosen here and nowhere else — a caller never writes
// time.Now().UnixMilli() itself, so changing the unit (to seconds, to a
// monotonic stamp) is a change to this file and not a hunt through the tree.
//
// The same reasoning covers the conversions: Millis/Time/Since keep the
// arithmetic in one place instead of re-deriving it at each call site.
package clock

import "time"

// Now returns the current time as Unix milliseconds.
func Now() int64 { return time.Now().UnixMilli() }

// Millis converts a time.Time to Unix milliseconds. Use it for timestamps that
// originate outside the project (a file's mtime, an SFTP attribute) so they
// enter the wire format through the same unit as Now.
func Millis(t time.Time) int64 { return t.UnixMilli() }

// Time converts a Unix-millisecond stamp back to a time.Time.
func Time(ms int64) time.Time { return time.UnixMilli(ms) }

// Since returns how long has elapsed since the given Unix-millisecond stamp.
func Since(ms int64) time.Duration { return time.Since(Time(ms)) }
