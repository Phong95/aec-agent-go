package jsstr

import "strings"

// DecodeUTF8 turns bytes into text the way Node's Buffer.toString("utf8") does, which
// is the WHATWG UTF-8 decoder: every maximal ill-formed subpart of the input becomes ONE
// U+FFFD. A subpart is the longest prefix of a character that is still well formed, so a
// euro sign (E2 82 AC) cut after two bytes is one U+FFFD, not two, and a run of stray
// continuation bytes is one U+FFFD each. The input is cut in the middle of a character
// by the workspace tools (headTail cuts command output at byte offsets), which is why
// this exact rule matters.
//
// It is not strings.ToValidUTF8, which merges a whole run of invalid bytes into a single
// replacement, and it is not what a range loop over a string gives, which reports one
// U+FFFD per invalid byte. Valid input is returned unchanged.
func DecodeUTF8(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	var (
		codePoint   rune
		bytesSeen   int
		bytesNeeded int
		lower       byte = 0x80
		upper       byte = 0xBF
	)
	for i := 0; i < len(b); {
		c := b[i]
		if bytesNeeded == 0 {
			switch {
			case c <= 0x7F:
				sb.WriteByte(c)
			case c >= 0xC2 && c <= 0xDF:
				bytesNeeded = 1
				codePoint = rune(c & 0x1F)
			case c >= 0xE0 && c <= 0xEF:
				switch c {
				case 0xE0:
					lower = 0xA0
				case 0xED:
					upper = 0x9F
				}
				bytesNeeded = 2
				codePoint = rune(c & 0x0F)
			case c >= 0xF0 && c <= 0xF4:
				switch c {
				case 0xF0:
					lower = 0x90
				case 0xF4:
					upper = 0x8F
				}
				bytesNeeded = 3
				codePoint = rune(c & 0x07)
			default:
				sb.WriteRune('\ufffd')
			}
			i++
			continue
		}
		if c < lower || c > upper {
			// The sequence ended early: one U+FFFD for the subpart read so far, and the
			// byte that broke it is decoded again from the start.
			codePoint, bytesNeeded, bytesSeen = 0, 0, 0
			lower, upper = 0x80, 0xBF
			sb.WriteRune('\ufffd')
			continue
		}
		lower, upper = 0x80, 0xBF
		codePoint = codePoint<<6 | rune(c&0x3F)
		bytesSeen++
		i++
		if bytesSeen == bytesNeeded {
			sb.WriteRune(codePoint)
			codePoint, bytesNeeded, bytesSeen = 0, 0, 0
		}
	}
	if bytesNeeded != 0 {
		// The input ends inside a character: one U+FFFD for the incomplete subpart.
		sb.WriteRune('\ufffd')
	}
	return sb.String()
}
