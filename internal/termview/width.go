package termview

import "golang.org/x/text/width"

// runeWidth is how many columns a character occupies: 2 for the East Asian wide and
// fullwidth forms, 1 for everything else.
//
// ONE ANSWER, TWO BUFFERS. The transcript model already counts wide characters this way —
// counting them as one column slides every wrap after the first Chinese character, which
// is the drift that puts cells on the wrong line. The alternate grid needs the same rule
// for the same reason, and a second copy of it would be a second answer: vim editing a
// Chinese file would line up on the rail and not on the screen, or the other way round.
func runeWidth(r rune) int {
	kind := width.LookupRune(r).Kind()
	if kind == width.EastAsianWide || kind == width.EastAsianFullwidth {
		return 2
	}
	return 1
}

// wideTail marks the second cell of a wide character: it is occupied, but it holds no
// character of its own. A zero rune cannot appear in terminal output, so it is free to mean
// this, and keeping it in the grid means cursor addressing counts the cells the terminal
// counts.
const wideTail rune = 0
