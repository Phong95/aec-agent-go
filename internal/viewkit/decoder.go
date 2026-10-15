package viewkit

import "slices"

// IncompleteTail returns how many bytes at the end of b are the start of a UTF-8
// sequence that more bytes could still complete: a lead byte followed by fewer
// continuation bytes than it needs, each of them valid so far (the WHATWG bounds
// for the second byte of E0, ED, F0 and F4 apply). It returns 0 when b ends on a
// complete character, on ASCII, or on bytes that no continuation could repair.
// The result is at most 3.
//
// Decoder holds these bytes back until the next write, and the host exec output
// cap uses it to cut a stream on a character boundary.
func IncompleteTail(b []byte) int {
	// A sequence is at most four bytes, so only one of the last three bytes can
	// be the lead byte of an incomplete one.
	for back := 1; back <= 3 && back <= len(b); back++ {
		c := b[len(b)-back]
		if c >= 0x80 && c <= 0xBF {
			continue // a continuation byte: keep looking for its lead byte
		}
		need := sequenceLength(c)
		if need == 0 || back >= need {
			return 0 // ASCII, a byte that cannot lead, or a sequence that is complete
		}
		lower, upper := byte(0x80), byte(0xBF)
		switch c {
		case 0xE0:
			lower = 0xA0
		case 0xED:
			upper = 0x9F
		case 0xF0:
			lower = 0x90
		case 0xF4:
			upper = 0x8F
		}
		for _, next := range b[len(b)-back+1:] {
			if next < lower || next > upper {
				return 0
			}
			lower, upper = 0x80, 0xBF
		}
		return back
	}
	return 0
}

// sequenceLength is the length of the UTF-8 sequence a lead byte starts, and 0 for
// a byte that cannot start one (ASCII, a continuation byte, C0, C1, F5 to FF).
func sequenceLength(lead byte) int {
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		return 2
	case lead >= 0xE0 && lead <= 0xEF:
		return 3
	case lead >= 0xF0 && lead <= 0xF4:
		return 4
	}
	return 0
}

// Decoder turns a stream of bytes into text a piece at a time, with the semantics
// of Node's StringDecoder: an incomplete character at the end of a write is held
// back until the next write completes it, an invalid byte becomes U+FFFD the WHATWG
// way, and End flushes a dangling partial character as one U+FFFD. The pieces
// returned by Write and End always add up to LossyUTF8 of the whole stream, however
// the stream was split into writes.
//
// A decoder that handled every chunk on its own would turn a character split across
// two pipe reads into replacement characters; this decoder is incremental. A Decoder is not safe for concurrent use; the zero value is ready.
type Decoder struct {
	pending []byte
}

// Write returns the text of everything written so far that cannot change with
// later bytes.
func (d *Decoder) Write(p []byte) string {
	data := p
	if len(d.pending) > 0 {
		data = slices.Concat(d.pending, p)
	}
	keep := IncompleteTail(data)
	cut := len(data) - keep
	text := LossyUTF8(data[:cut])
	d.pending = slices.Clone(data[cut:])
	return text
}

// End returns the text of the held-back bytes (one U+FFFD for a partial
// character, as the decoder does at the end of a stream) and resets the Decoder.
func (d *Decoder) End() string {
	text := LossyUTF8(d.pending)
	d.pending = nil
	return text
}
