package web

import (
	"strings"
	"unicode/utf16"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// This file decodes a page body the way web_extract does: charsetOf reads the label of the
// Content-Type header and decodeBody decodes the bytes with it, following the WHATWG
// TextDecoder rules. The supported encodings are the ones a page is realistically served
// in: UTF-8 (the default), UTF-16 little and big endian, and the windows-1252 family
// (latin1, iso-8859-1, ascii and their other labels, which TextDecoder maps to
// windows-1252). Any other label (shift_jis, gbk, koi8-r, ...) falls back to UTF-8.

// Encodings decodeBody supports.
const (
	encUTF8        = "utf-8"
	encUTF16LE     = "utf-16le"
	encUTF16BE     = "utf-16be"
	encWindows1252 = "windows-1252"
)

// encodingLabels maps the WHATWG encoding labels of the supported encodings to the
// encoding. TextDecoder reports the same names (latin1 and ascii report windows-1252,
// utf-16 and unicode report utf-16le).
var encodingLabels = map[string]string{
	"unicode-1-1-utf-8": encUTF8, "unicode11utf8": encUTF8, "unicode20utf8": encUTF8, "utf-8": encUTF8, "utf8": encUTF8, "x-unicode20utf8": encUTF8,
	"csunicode": encUTF16LE, "iso-10646-ucs-2": encUTF16LE, "ucs-2": encUTF16LE, "unicode": encUTF16LE, "unicodefeff": encUTF16LE, "utf-16": encUTF16LE, "utf-16le": encUTF16LE,
	"unicodefffe": encUTF16BE, "utf-16be": encUTF16BE,
	"ansi_x3.4-1968": encWindows1252, "ascii": encWindows1252, "cp1252": encWindows1252, "cp819": encWindows1252, "csisolatin1": encWindows1252,
	"ibm819": encWindows1252, "iso-8859-1": encWindows1252, "iso-ir-100": encWindows1252, "iso8859-1": encWindows1252, "iso88591": encWindows1252,
	"iso_8859-1": encWindows1252, "iso_8859-1:1987": encWindows1252, "l1": encWindows1252, "latin1": encWindows1252, "us-ascii": encWindows1252,
	"windows-1252": encWindows1252, "x-cp1252": encWindows1252,
}

// windows1252High is the windows-1252 code page for the bytes 0x80 to 0x9F (the WHATWG
// table: the five bytes the code page leaves undefined map to the C1 control of the same
// value). Every other byte maps to the code point of the same value.
var windows1252High = [32]rune{
	0x20AC, 0x0081, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x008D, 0x017D, 0x008F,
	0x0090, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x009D, 0x017E, 0x0178,
}

// charsetLabel is /charset=["']?([\w-]+)/i.exec(contentType)?.[1]?.toLowerCase(): the label
// of the first `charset=` that is followed by an optional quote and at least one word
// character or hyphen ("" when there is none).
func charsetLabel(contentType string) string {
	lower := asciiLower(contentType)
	for from := 0; ; {
		i := strings.Index(lower[from:], "charset=")
		if i < 0 {
			return ""
		}
		p := from + i + len("charset=")
		if p < len(lower) && (lower[p] == '"' || lower[p] == '\'') {
			p++
		}
		start := p
		for p < len(lower) && isLabelChar(lower[p]) {
			p++
		}
		if p > start {
			return lower[start:p]
		}
		from += i + 1
	}
}

func isLabelChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// charsetOf is the encoding for a Content-Type header: UTF-8 when there is no label or the
// label is not one of the supported encodings.
func charsetOf(contentType string) string {
	if enc, ok := encodingLabels[charsetLabel(contentType)]; ok {
		return enc
	}
	return encUTF8
}

// decodeBody decodes a page body like TextDecoder with the encoding of the Content-Type
// header: a byte order mark is stripped, and malformed input becomes U+FFFD (one per
// maximal ill-formed UTF-8 subpart, one per unpaired UTF-16 surrogate or trailing odd
// byte). A body cut in the middle of a character, which the byte cap does, ends in one
// U+FFFD, like the final flush of a streaming TextDecoder.
func decodeBody(b []byte, contentType string) string {
	switch charsetOf(contentType) {
	case encUTF16LE:
		return decodeUTF16(b, false)
	case encUTF16BE:
		return decodeUTF16(b, true)
	case encWindows1252:
		return decodeWindows1252(b)
	}
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	return jsstr.DecodeUTF8(b)
}

func decodeUTF16(b []byte, bigEndian bool) string {
	if len(b) >= 2 && (bigEndian && b[0] == 0xFE && b[1] == 0xFF || !bigEndian && b[0] == 0xFF && b[1] == 0xFE) {
		b = b[2:]
	}
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			units = append(units, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	// utf16.Decode turns an unpaired surrogate into U+FFFD, like the WHATWG decoder.
	var sb strings.Builder
	sb.Grow(len(b))
	sb.WriteString(string(utf16.Decode(units)))
	if len(b)%2 == 1 {
		sb.WriteString(replacementChar)
	}
	return sb.String()
}

func decodeWindows1252(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		if c >= 0x80 && c <= 0x9F {
			sb.WriteRune(windows1252High[c-0x80])
		} else {
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}

// asciiLower folds A-Z only, so the byte offsets of the lowered copy stay equal to the
// original. JS toLowerCase can change the length of a string (U+0130 becomes two UTF-16
// units), and the HTML scanners find tags in the lowered copy and slice the original with
// the offsets; the port deliberately keeps them aligned (htmltext.go).
func asciiLower(s string) string {
	for i := range len(s) {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}
