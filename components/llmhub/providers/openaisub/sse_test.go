package openaisub

import (
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/Phong95/aec-agent-go/kernel"
)

// payloadsOf parses input and returns every payload next returns, and the error that
// ended the parse (io.EOF is the normal end and is returned as nil).
func payloadsOf(r io.Reader, maxLine int) ([]string, error) {
	p := newSSEParser(r, maxLine)
	var out []string
	for {
		payload, err := p.next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, payload)
	}
}

// TestSSEParserRules checks the framing rules of the parser (a split on a blank line,
// the first data line, trim, skips) plus CRLF framing and the line cap.
func TestSSEParserRules(t *testing.T) {
	const big = 1 << 30
	for _, tc := range []struct {
		name  string
		input string
		want  []string
	}{
		{"one event", "data: {\"a\":1}\n\n", []string{`{"a":1}`}},
		{"two events", "data: 1\n\ndata: 2\n\n", []string{"1", "2"}},
		{"event line is ignored", "event: x\ndata: 1\n\n", []string{"1"}},
		{"first data line wins", "data: 1\ndata: 2\n\n", []string{"1"}},
		{"an empty first data line hides the second", "data:\ndata: 2\n\ndata: 3\n\n", []string{"3"}},
		{"done marker", "data: [DONE]\n\ndata: 1\n\n", []string{"1"}},
		{"done marker with padding", "data:  [DONE] \n\ndata: 1\n\n", []string{"1"}},
		{"empty payload", "data:\n\ndata:   \n\ndata: 1\n\n", []string{"1"}},
		{"no space after the colon", "data:1\n\n", []string{"1"}},
		{"padding is trimmed with JS rules", "data:\u00a0\ufeff 1 \u2028\n\n", []string{"1"}},
		{"tab padding", "data:\t1\t\n\n", []string{"1"}},
		{"an indented data line is not data", " data: 1\n\ndata: 2\n\n", []string{"2"}},
		{"data is case sensitive", "Data: 1\nDATA: 2\n\ndata: 3\n\n", []string{"3"}},
		{"other fields and comments", "id: 7\nretry: 1\n: comment\ndata: 1\n\n", []string{"1"}},
		{"leading and extra blank lines", "\n\n\ndata: 1\n\n\n\ndata: 2\n\n", []string{"1", "2"}},
		{"crlf framing", "event: x\r\ndata: 1\r\n\r\ndata: 2\r\n\r\n", []string{"1", "2"}},
		{"lf events with crlf lines", "event: x\r\ndata: 1\r\n\ndata: 2\n\n", []string{"1", "2"}},
		{"a lone carriage return line ends the event", "data: 1\n\r\ndata: 2\n\n", []string{"1", "2"}},
		// The end of the body closes an open event, its unterminated last line
		// included.
		{"an unterminated final event is read", "data: 1\n\ndata: 2\n", []string{"1", "2"}},
		{"a final line without a newline is read", "data: 1\n\ndata: 2", []string{"1", "2"}},
		{"a final crlf line without a newline is read", "data: 1\r\n\r\ndata: 2\r", []string{"1", "2"}},
		{"an open event with only a blank-free tail is dropped", "data: 1\n\nevent: x\n", []string{"1"}},
		{"an open event with an empty payload is dropped", "data: 1\n\ndata:", []string{"1"}},
		{"empty input", "", nil},
		{"only blank lines", "\n\n\n", nil},
		{"byte order mark at the start", "\ufeffdata: 1\n\n", []string{"1"}},
		{"byte order mark later is data", "data: 1\n\n\ufeffdata: 2\n\ndata: 3\n\n", []string{"1", "3"}},
		{"invalid bytes become the replacement character", "data: a\xffb\n\n", []string{"a\ufffdb"}},
		{"multi-byte text", "data: Ti\u1ebfng \U0001f600\n\n", []string{"Ti\u1ebfng \U0001f600"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The same input must give the same payloads when it arrives in one read, in
			// two reads cut anywhere, and one byte at a time (a character split across
			// reads is rebuilt before it is decoded).
			readers := map[string]func() io.Reader{
				"one read":      func() io.Reader { return strings.NewReader(tc.input) },
				"one byte each": func() io.Reader { return iotest.OneByteReader(strings.NewReader(tc.input)) },
			}
			for name, mk := range readers {
				got, err := payloadsOf(mk(), big)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if !slices.Equal(got, tc.want) {
					t.Errorf("%s: got %q, want %q", name, got, tc.want)
				}
			}
			for k := 0; k <= len(tc.input); k++ {
				r := io.MultiReader(strings.NewReader(tc.input[:k]), strings.NewReader(tc.input[k:]))
				got, err := payloadsOf(r, big)
				if err != nil || !slices.Equal(got, tc.want) {
					t.Fatalf("cut at %d: got %q (%v), want %q", k, got, err, tc.want)
				}
			}
		})
	}
}

// TestSSEParserLongLines requires a line longer than the reader buffer to be
// collected whole, and a line above the limit to be an error that ends the stream.
func TestSSEParserLongLines(t *testing.T) {
	long := strings.Repeat("x", 3*readBufferBytes+17)
	got, err := payloadsOf(strings.NewReader("data: "+long+"\n\ndata: next\n\n"), maxLineBytes)
	if err != nil || len(got) != 2 || got[0] != long || got[1] != "next" {
		t.Fatalf("a line above the buffer size: %d payloads, err %v", len(got), err)
	}

	// A limit of 20 bytes: "data: 1234567890123\n" is 20 bytes with its newline.
	atLimit := "data: 1234567890123\n\n"
	if got, err := payloadsOf(strings.NewReader(atLimit), 20); err != nil || !slices.Equal(got, []string{"1234567890123"}) {
		t.Errorf("a line at the limit: %q, %v", got, err)
	}
	over := "data: 12345678901234\n\n"
	got, err = payloadsOf(strings.NewReader("data: 1\n\n"+over), 20)
	var tooLong *lineTooLongError
	if !errors.As(err, &tooLong) || tooLong.limit != 20 || !slices.Equal(got, []string{"1"}) {
		t.Errorf("a line over the limit: payloads %q, error %v", got, err)
	}
	// The limit counts a line that never ends, however it arrives.
	endless := io.MultiReader(strings.NewReader("data: "), iotest.OneByteReader(strings.NewReader(strings.Repeat("y", 100))))
	if _, err := payloadsOf(endless, 50); !errors.As(err, &tooLong) {
		t.Errorf("a line that never ends: %v", err)
	}
}

// TestSSEParserReadError requires an error of the reader in the middle of a line to be
// returned as that error, not as the end of the stream and not as an event.
func TestSSEParserReadError(t *testing.T) {
	boom := errors.New("connection reset")
	r := &failingBody{chunks: []string{"data: 1\n\ndata: 2"}, err: boom}
	got, err := payloadsOf(r, maxLineBytes)
	if !errors.Is(err, boom) || !slices.Equal(got, []string{"1"}) {
		t.Errorf("got %q and %v, want [1] and the reader error", got, err)
	}
}

// chunkReader returns its chunks one per Read.
type chunkReader struct{ chunks []string }

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	if n == len(r.chunks[0]) {
		r.chunks = r.chunks[1:]
	} else {
		r.chunks[0] = r.chunks[0][n:]
	}
	return n, nil
}

// TestSubscriptionTranscriptsAtEveryOffset cuts every recorded transcript into two
// reads at every byte offset, and into single bytes, and requires the chunks to equal
// the recorded chunks each time: the output of the stream does not depend on where
// the network happens to split it (inside an event name, inside a JSON string, inside
// a multi-byte character). The reference harness gives the same chunks for any cut
// (wiregen checks that before it records an entry).
func TestSubscriptionTranscriptsAtEveryOffset(t *testing.T) {
	var f streamFixture
	loadFixture(t, "llm-subscription-streams.json", &f)
	if len(f.Entries) == 0 {
		t.Fatal("the stream fixture has no entries")
	}
	collect := func(t *testing.T, r io.Reader) []string {
		t.Helper()
		var chunks []kernel.StreamChunk
		readEvents(t.Context(), r, maxLineBytes, func(c kernel.StreamChunk, err error) bool {
			if err != nil {
				t.Fatalf("the error slot carried %v", err)
			}
			chunks = append(chunks, c)
			return true
		}, nil)
		return chunkTexts(t, chunks)
	}
	for _, e := range f.Entries {
		if e.ReadError != "" {
			continue
		}
		want := f.expected(t, e)
		t.Run(e.Name, func(t *testing.T) {
			finals := finalArguments(decodeChunks(t, want))
			wantTexts := normalizeChunks(t, rawTexts(want.Out), finals)
			joined := strings.Join(decodeChunks(t, e), "")
			check := func(what string, r io.Reader) {
				t.Helper()
				if got := normalizeChunks(t, collect(t, r), finals); !slices.Equal(got, wantTexts) {
					t.Fatalf("%s: chunks differ\n got: %s\nwant: %s", what, strings.Join(got, "\n      "), strings.Join(wantTexts, "\n      "))
				}
			}
			check("single bytes", iotest.OneByteReader(strings.NewReader(joined)))
			for k := 0; k <= len(joined); k++ {
				check("cut at "+strconv.Itoa(k), &chunkReader{chunks: []string{joined[:k], joined[k:]}})
			}
		})
	}
}

// chunkTexts encodes chunks like llmtest.ChunkTexts, without needing a testing.TB
// helper in a loop over thousands of cuts.
func chunkTexts(t *testing.T, chunks []kernel.StreamChunk) []string {
	t.Helper()
	out := make([]string, len(chunks))
	for i, c := range chunks {
		b, err := kernel.EncodeJSON(c)
		if err != nil {
			t.Fatalf("encode chunk %d: %v", i, err)
		}
		out[i] = string(b)
	}
	return out
}

// TestStreamStateOutputIndexKeys pins the map semantics of the call lookup: the
// string "0" and the number 0 are different keys, and a delta looks the call
// up by item id first and by output index second.
func TestStreamStateOutputIndexKeys(t *testing.T) {
	s := newStreamState()
	zero := 0.0
	five := 5.0
	id0, idB, idUnknown, id5 := "0", "fc_b", "unknown", "5"
	emit := func(kernel.StreamChunk) bool { return true }
	s.handle(streamEvent{Type: "response.output_item.added", OutputIndex: &five, Item: &outputItem{Type: "function_call", ID: "0", CallID: "A", Name: "alpha"}}, emit)
	s.handle(streamEvent{Type: "response.output_item.added", OutputIndex: &zero, Item: &outputItem{Type: "function_call", ID: "fc_b", CallID: "B", Name: "beta"}}, emit)

	for _, tc := range []struct {
		name string
		ev   streamEvent
		want string
	}{
		{"string id 0", streamEvent{ItemID: &id0}, "A"},
		{"number index 0", streamEvent{OutputIndex: &zero}, "B"},
		{"string id that spells an index", streamEvent{ItemID: &id5}, ""},
		{"id wins over index", streamEvent{ItemID: &idB, OutputIndex: &five}, "B"},
		{"unknown id falls to the index", streamEvent{ItemID: &idUnknown, OutputIndex: &five}, "A"},
		{"neither", streamEvent{}, ""},
	} {
		call, ok := s.lookup(tc.ev)
		if (tc.want == "") == ok || call.id != tc.want {
			t.Errorf("%s: got %q (found %v), want %q", tc.name, call.id, ok, tc.want)
		}
	}
}
