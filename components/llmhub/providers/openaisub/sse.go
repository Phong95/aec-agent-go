package openaisub

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// maxLineBytes is the longest line (terminator included) the parser accepts.
// Without a bound, a server that never sends a newline would grow the buffer until
// memory runs out. 32 MiB is the limit the vendor SDK scanners use for the other
// providers, far above any real event (a function call with a whole
// file as arguments is a few megabytes).
const maxLineBytes = 32 << 20

// readBufferBytes is the size of the buffered reader; a longer line is collected
// from several reads, up to the line limit.
const readBufferBytes = 64 << 10

// byteOrderMark is the UTF-8 encoding of U+FEFF. The parser drops one at the start
// of the stream.
var byteOrderMark = []byte("\xef\xbb\xbf")

// lineTooLongError is the error next returns for a line above the limit.
type lineTooLongError struct{ limit int }

func (e *lineTooLongError) Error() string {
	return fmt.Sprintf("the event stream has a line longer than %d bytes", e.limit)
}

// sseParser reads the events of a Responses stream. Its framing is not a general
// SSE reader:
//   - an event ends at a blank line (the text between two "\n\n"); a trailing "\r"
//     is dropped from every line first, so CRLF framing works too (CONTEXT C4);
//   - only the first line that starts with "data:" counts, even when its payload is
//     empty; "event:" lines and every later data line are ignored;
//   - the payload is the rest of that line trimmed of whitespace, Unicode spaces,
//     U+FEFF and line separators included;
//   - an event whose payload is empty or [DONE] is skipped;
//   - an event still open at the end of the stream is read as if a blank line
//     followed, its unterminated last line included, so a server that closes the
//     body right after its last event loses nothing, and a tail cut
//     inside its JSON is skipped like any event that is not JSON.
//
// The parser reads whole lines as bytes and decodes each line alone, so a
// multi-byte character that arrives split across two reads is rebuilt before it is
// decoded.
type sseParser struct {
	br      *bufio.Reader
	maxLine int
	started bool
	buf     []byte
	// eof is set once the reader ended: every later readLine returns io.EOF.
	eof bool
}

// newSSEParser reads events from r. maxLine is the line limit in bytes.
func newSSEParser(r io.Reader, maxLine int) *sseParser {
	return &sseParser{br: bufio.NewReaderSize(r, readBufferBytes), maxLine: maxLine}
}

// readLine returns the next line without its "\n" and without a trailing "\r". A
// last line with no "\n" is returned as a line; after it, readLine returns io.EOF.
func (s *sseParser) readLine() ([]byte, error) {
	if s.eof {
		return nil, io.EOF
	}
	s.buf = s.buf[:0]
	for {
		part, err := s.br.ReadSlice('\n')
		if len(s.buf)+len(part) > s.maxLine {
			s.buf = nil
			return nil, &lineTooLongError{limit: s.maxLine}
		}
		s.buf = append(s.buf, part...)
		switch {
		case err == nil, errors.Is(err, io.EOF) && len(s.buf) > 0:
			if err != nil {
				s.eof = true
			}
			line := bytes.TrimSuffix(s.buf, []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			if !s.started {
				s.started = true
				line = bytes.TrimPrefix(line, byteOrderMark)
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			if errors.Is(err, io.EOF) {
				s.eof = true
			}
			return nil, err
		}
	}
}

// next returns the payload of the next event that has one, or io.EOF when the stream
// ends. Any other error is a read failure or a line above the limit.
func (s *sseParser) next() (string, error) {
	var (
		have    bool
		payload string
	)
	for {
		line, err := s.readLine()
		if err != nil {
			// The end of the stream closes an open event, as a blank line would.
			if errors.Is(err, io.EOF) && have && payload != "" && payload != "[DONE]" {
				return payload, nil
			}
			return "", err
		}
		if len(line) == 0 {
			// A blank line ends the event.
			if have && payload != "" && payload != "[DONE]" {
				return payload, nil
			}
			have, payload = false, ""
			continue
		}
		if !have && bytes.HasPrefix(line, []byte("data:")) {
			have = true
			payload = jsstr.Trim(strings.ToValidUTF8(string(line[len("data:"):]), "\ufffd"))
		}
		// A very long line was copied once into buf and once into payload; give the
		// buffer back so a stream of normal events does not keep the largest one.
		if cap(s.buf) > 4*readBufferBytes {
			s.buf = nil
		}
	}
}
