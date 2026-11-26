package sftp

import "strings"

// THE WIRE FORM OF A WINDOWS PATH, AND THE ONE PLACE IT IS DECIDED.
//
// SFTP is POSIX-shaped: the wire form of `C:/tc/x` is `/C:/tc/x`. A caller who types
// the path the way Windows shows it — `C:\tc\x` — gets a request the server resolves
// against its working directory, and the error names a doubled path ("open
// C:\Users\Administrator\C:\tc\...: The filename, directory name, or volume label syntax
// is incorrect"). That sentence is about the file NAME, so the caller reads it as "my
// path is wrong" while the path is fine and only the SPELLING is not (issue #186).
//
// WHY THIS IS NARROW ON PURPOSE. `\` is an ordinary byte in a POSIX filename: on this
// project's own platform `read <session> file '/tmp/x/C:\tc.txt'` reads a file whose
// name really does contain a colon and a backslash, and it works today. Rewriting every
// backslash would silently read a DIFFERENT file — and a silent wrong answer is far
// more expensive than an error, because it is only discovered when the conclusion built
// on it turns out wrong. So the rewrite happens only where the reading is not in doubt:
//
//	a drive-prefixed path that contains a backslash   C:\tc\x   -> /C:/tc/x
//	the wire form typed with backslashes              /C:\tc\x  -> /C:/tc/x
//
// Both are anchored at the first byte, which gives the escape hatch for free: a
// relative path beginning with anything else — including `./C:\tc\x`, the spelling for
// "yes, a file whose name contains a backslash" — is left exactly as typed. A
// drive-RELATIVE path (`C:tc\x`, no separator after the colon) is not matched either:
// it means "on drive C, relative to that drive's current directory", which is a
// different path and not one this layer can resolve.
func WireSpelling(p string) (string, bool) {
	if !strings.ContainsRune(p, '\\') {
		return p, false
	}
	switch {
	case len(p) >= 3 && isDriveLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/'):
		// The spelling a caller copies out of Explorer, or out of a PowerShell prompt.
		return "/" + strings.ReplaceAll(p, `\`, "/"), true
	case len(p) >= 4 && p[0] == '/' && isDriveLetter(p[1]) && p[2] == ':' && p[3] == '\\':
		// Already absolute and already wire-shaped, except for the separators: a caller
		// who was told to prefix the slash and did, but kept typing backslashes.
		return strings.ReplaceAll(p, `\`, "/"), true
	}
	return p, false
}

// isDriveLetter reports whether b can start a Windows drive specifier.
func isDriveLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
