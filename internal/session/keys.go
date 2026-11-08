package session

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownKey reports a key name this build does not have.
//
// It is a SENTINEL rather than a message because the caller's two failures on this
// path are opposites and arrive through the same call: an unknown key name is the
// caller's argument being wrong (400 — fix the line), while a channel whose
// process has finished is the resource's state (409 — the line was fine). A
// surface that can only read the text cannot tell them apart, so it answered 500
// for both.
var ErrUnknownKey = errors.New("unknown key")

// unknownKeyError carries ErrUnknownKey without saying so in the message.
//
// The text is the whole hint (it lists the key names), so it must not gain a
// sentinel's words on the way out; `Is` is what makes it recognisable. This is the
// one shape that gives both a clean message and errors.Is.
type unknownKeyError struct{ key string }

func (e *unknownKeyError) Error() string {
	return fmt.Sprintf("unknown key %q; supported: enter, tab, esc (or escape), up, down, left, right, backspace, delete, home, end, ctrl+c, ctrl+d, ctrl+z, ctrl+l, ctrl+u, ctrl+w", e.key)
}
func (e *unknownKeyError) Is(target error) bool { return target == ErrUnknownKey }

// KeyBytes maps a named key to terminal input bytes.
// For enter: PTY uses CR (\r); pipe uses LF or CRLF based on shell family.
func KeyBytes(key string, pty bool, enterCRLF bool) ([]byte, error) {
	k := strings.ToLower(strings.TrimSpace(key))
	switch k {
	case "enter":
		if pty {
			return []byte{'\r'}, nil
		}
		if enterCRLF {
			return []byte{'\r', '\n'}, nil
		}
		return []byte{'\n'}, nil
	case "tab":
		return []byte{'\t'}, nil
	case "esc", "escape":
		return []byte{0x1b}, nil
	case "up":
		return []byte{0x1b, '[', 'A'}, nil
	case "down":
		return []byte{0x1b, '[', 'B'}, nil
	case "right":
		return []byte{0x1b, '[', 'C'}, nil
	case "left":
		return []byte{0x1b, '[', 'D'}, nil
	case "backspace":
		return []byte{0x7f}, nil
	case "delete":
		return []byte{0x1b, '[', '3', '~'}, nil
	case "home":
		return []byte{0x1b, '[', 'H'}, nil
	case "end":
		return []byte{0x1b, '[', 'F'}, nil
	case "ctrl+c":
		return []byte{0x03}, nil
	case "ctrl+d":
		return []byte{0x04}, nil
	case "ctrl+z":
		return []byte{0x1a}, nil
	case "ctrl+l":
		return []byte{0x0c}, nil
	case "ctrl+u":
		return []byte{0x15}, nil
	case "ctrl+w":
		return []byte{0x17}, nil
	default:
		return nil, &unknownKeyError{key: key}
	}
}
