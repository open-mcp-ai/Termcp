package encoding

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
)

// The charsets a session's output can be in.
//
// A TERMINAL IS NOT A UTF-8 DEVICE. On a Chinese Windows the console code page is
// 936, so every byte a shell writes there is GBK: a path, a compiler diagnostic, a
// `dir` listing. The daemon used to assume UTF-8 everywhere, which turned that into
// three separate failures — a read that refused the whole window, a `--grep` that
// scanned bytes against a UTF-8 pattern and found nothing, and a JSON-RPC frame from
// an extension that could not be read at all.
//
// The names are the ones a caller can look up: IANA names, plus the code-page
// numbers Windows users say out loud (cp936 is GBK, cp65001 is UTF-8).
var charsets = map[string]encoding.Encoding{
	"":        nil, // UTF-8: no decoder needed
	"utf-8":   nil,
	"utf8":    nil,
	"cp65001": nil,

	"gbk": simplifiedchinese.GBK,
	// GB2312 IS A CHARACTER SET, NOT THIS ENCODING. What a caller means by
	// "--encoding gb2312" is the code page their zh-CN console writes, which is
	// CP936 — and GBK is CP936's superset, so every GB2312 byte decodes the same
	// way. It used to map to HZGB2312, which is the 7-bit ESCAPE encoding from
	// mail: fed a console's bytes it returned plausible-looking garbage with no
	// error at all, which is the one failure this package exists to prevent.
	"gb2312":     simplifiedchinese.GBK,
	"euc-cn":     simplifiedchinese.GBK,
	"hz-gb-2312": simplifiedchinese.HZGB2312, // the escape form, under its own name
	"hzgb2312":   simplifiedchinese.HZGB2312,
	"gb18030":    simplifiedchinese.GB18030,
	"cp936":      simplifiedchinese.GBK,
	"big5":       traditionalchinese.Big5,
	"cp950":      traditionalchinese.Big5,
	"shift_jis":  japanese.ShiftJIS,
	"shift-jis":  japanese.ShiftJIS,
	"sjis":       japanese.ShiftJIS,
	// CP932 IS NOT SHIFT_JIS, and this is the closest thing in the standard library:
	// Windows-31J adds vendor extensions and fills some of the 0x80-0xA0 and
	// 0xFD-0xFF ranges, which decode here as replacement characters. The name is
	// kept because it is what a Windows caller writes and refusing it would leave
	// them with nothing; the difference is stated rather than hidden.
	"cp932":        japanese.ShiftJIS,
	"windows-31j":  japanese.ShiftJIS,
	"euc-jp":       japanese.EUCJP,
	"euc-kr":       korean.EUCKR,
	"cp949":        korean.EUCKR,
	"windows-1252": charmap.Windows1252,
	"cp1252":       charmap.Windows1252,
	"latin-1":      charmap.ISO8859_1,
	"iso-8859-1":   charmap.ISO8859_1,

	// UTF-16 IS WHAT WINDOWS WRITES BY DEFAULT, which is why it is here and not in a
	// footnote (issue #177): PowerShell 5.1's `>` and `Out-File` with no -Encoding write
	// UTF-16LE with a BOM, so "redirect a long command to a file and poll it" — the
	// pattern this tool recommends for work that outlives a session — produced a file
	// this build could not read. The reply said `mode: text` and handed back `\xff\xfe`
	// soup: true, and useless.
	//
	// `utf-16` (no suffix) is the BOM-sniffing form and the one to prefer: the caller
	// usually does not know how the file was written, and the BOM does. It defaults to
	// little-endian without one, which is what a Windows tool writes.
	"utf-16":   unicode.UTF16(unicode.LittleEndian, unicode.UseBOM),
	"utf16":    unicode.UTF16(unicode.LittleEndian, unicode.UseBOM),
	"utf-16le": unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM),
	"utf16le":  unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM),
	"utf-16be": unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM),
	"utf16be":  unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM),
}

// KnownCharset reports whether name is a charset this build can decode, and what it
// normalises to. The second answer is what goes in a reply, so a caller that guessed
// `CP936` is told the spelling the rest of the replies use.
func KnownCharset(name string) (string, bool) {
	key := normaliseCharset(name)
	if _, ok := charsets[key]; !ok {
		return "", false
	}
	if key == "" || key == "utf8" || key == "cp65001" {
		return "utf-8", true
	}
	return key, true
}

// Decode turns bytes in the named charset into UTF-8.
//
// An unknown name is an error rather than a silent UTF-8 read: the caller asked for
// an interpretation, and answering with a different one is how mojibake gets
// attributed to the wrong layer. The empty name means UTF-8, which needs no work.
func Decode(name string, data []byte) ([]byte, error) {
	enc, ok := charsets[normaliseCharset(name)]
	if !ok {
		return nil, &UnknownCharsetError{Name: name}
	}
	if enc == nil {
		return data, nil
	}
	// x/text's decoder maps bytes it cannot represent to U+FFFD, which is exactly
	// the substitution this package's Render refuses. A caller that asked for GBK and
	// handed us something else is told, rather than handed replacement characters.
	out, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(out) {
		return nil, &UnknownCharsetError{Name: name, Detail: "the bytes are not valid " + name}
	}
	return out, nil
}

// BOMCharset names the charset a UTF-16 byte order mark proves, or "" when the data
// does not start with one.
//
// IT IS SPLIT OUT BECAUSE THE ANSWER HAS TWO USES, and they must not drift. `Sniff`
// orders it before its round-trip loop (a BOM is proof; a UTF-16 round trip is not).
// A text file read uses it for a different question: a NUL byte normally means binary,
// but a PowerShell `>` redirect writes UTF-16, where every ASCII character has one --
// so the BOM is what tells "wide-charset text" from "binary" (#291). One table, two
// callers.
func BOMCharset(data []byte) string {
	if len(data) < 2 {
		return ""
	}
	switch {
	case data[0] == 0xFF && data[1] == 0xFE:
		return "utf-16le"
	case data[0] == 0xFE && data[1] == 0xFF:
		return "utf-16be"
	}
	return ""
}

// Sniff names the charset these bytes are, when it can be established rather than
// guessed.
//
// The test is a ROUND TRIP: decode, encode, and require the original bytes back. That
// is what separates "these bytes are GBK" from "these bytes happen to decode as
// GBK" — a claim the daemon is willing to put in an error message only because the
// bytes confirm it. Order matters and is stated: GBK before Big5, because a zh-CN
// Windows console is the case being diagnosed and GBK's byte ranges are a superset of
// Big5's common ones.
//
// It returns "" when nothing round-trips. That is a real answer: the bytes are not
// text in a charset this build knows, and the caller is told nothing rather than
// told a guess.
func Sniff(data []byte) string {
	// A UTF-16 BYTE ORDER MARK IS PROOF, NOT A GUESS, so it is answered before the
	// round-trip loop below. UTF-16 decoding is permissive — almost any even-length byte
	// string "decodes" — so a round trip proves nothing there, while the two bytes the
	// writer itself chose are unambiguous. This is the case issue #177 is about: the
	// caller cannot see how the file was written, and the file says.
	if name := BOMCharset(data); name != "" {
		return name
	}
	for _, name := range []string{"gbk", "gb18030", "big5", "shift_jis", "euc-kr"} {
		enc := charsets[name]
		if enc == nil {
			continue
		}
		out, err := enc.NewDecoder().Bytes(data)
		if err != nil || !utf8.Valid(out) {
			continue
		}
		back, err := enc.NewEncoder().Bytes(out)
		if err != nil || !bytes.Equal(back, data) {
			continue
		}
		return name
	}
	return ""
}

// UnknownCharsetError names a charset this build cannot decode.
type UnknownCharsetError struct {
	Name   string
	Detail string
}

func (e *UnknownCharsetError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return "unknown charset " + e.Name + "; this build reads " + SupportedCharsets()
}

// advertised is what the refusal offers: ONE NAME PER ENCODING, spelled the way a
// caller is most likely to have it. It is a curated list rather than a dump of the
// table, because the table holds six spellings of GBK and a caller reading a
// refusal needs to know which encodings exist, not which aliases do.
//
// IT CANNOT DRIFT, and that is the point of the test next to it: every name here is
// resolved against the table, and every entry in the table must share its encoder
// with one of them. The hand-written sentence this replaces had already gone stale —
// it named big5, shift_jis and euc-kr while silently leaving out gb2312, cp950,
// cp949, euc-jp, latin-1 and utf-16le, every one of them accepted.
var advertised = []string{
	"utf-8", "gbk", "gb18030", "hz-gb-2312", "big5", "shift_jis", "euc-jp",
	"euc-kr", "windows-1252", "latin-1", "utf-16", "utf-16le", "utf-16be",
}

// SupportedCharsets lists the encodings this build can decode, as the refusal
// offers them.
func SupportedCharsets() string { return strings.Join(advertised, ", ") }

func normaliseCharset(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
