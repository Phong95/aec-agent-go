package web

import "testing"

// Expected values below were produced by the real TextDecoder of Node 22.19 with the calls
// a capped page read makes (decode(chunk, {stream: true}) then decode()).
func TestCharsets(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        []byte
		want        string
	}{
		{"utf-8 without a label", "text/html", []byte("h\xc3\xa9llo"), "h\u00e9llo"},
		{"utf-8 with a BOM", "text/html; charset=utf-8", []byte("\xef\xbb\xbfhi"), "hi"},
		{"utf-8 without a BOM", "text/html; charset=UTF-8", []byte("hi"), "hi"},
		{"utf-8 keeps a second BOM", "text/html; charset=utf-8", []byte("\xef\xbb\xbfA\xef\xbb\xbf"), "A\ufeff"},
		{"utf-8 truncated trailing sequence is one U+FFFD", "text/plain; charset=utf-8", []byte("\xe2\x82"), "\ufffd"},
		{"utf-8 invalid byte in the middle", "text/plain", []byte("A\xe2\x82A"), "A\ufffdA"},
		{"utf-8 two stray bytes", "text/plain", []byte("\xff\xfeA"), "\ufffd\ufffdA"},
		{"utf-16le with a BOM", "text/html; charset=utf-16le", []byte{0xff, 0xfe, 0x41, 0, 0x3d, 0xd8, 0x00, 0xde, 0x61}, "A\U0001F600\ufffd"},
		{"utf-16 is little endian", "text/html; charset=utf-16", []byte{0xff, 0xfe, 0x41, 0}, "A"},
		{"unicode is utf-16le", "text/html; charset=unicode", []byte{0x41, 0}, "A"},
		{"utf-16le unpaired high surrogate", "text/html; charset=utf-16le", []byte{0x41, 0, 0x3d, 0xd8, 0x61, 0}, "A\ufffda"},
		{"utf-16le trailing high surrogate", "text/html; charset=utf-16le", []byte{0x41, 0, 0x3d, 0xd8}, "A\ufffd"},
		{"utf-16le unpaired low surrogate", "text/html; charset=utf-16le", []byte{0x00, 0xde, 0x41, 0}, "\ufffdA"},
		{"utf-16le keeps a big endian BOM as U+FFFE", "text/html; charset=utf-16le", []byte{0xfe, 0xff}, "\ufffe"},
		{"utf-16be with a BOM", "text/html; charset=utf-16be", []byte{0xfe, 0xff, 0, 0x41, 0xd8, 0x3d, 0xde, 0x00, 0, 0x42, 0x61}, "A\U0001F600B\ufffd"},
		{"utf-16be keeps a little endian BOM as U+FFFE", "text/html; charset=utf-16be", []byte{0xff, 0xfe, 0, 0x41}, "\ufffeA"},
		{
			"windows-1252 high bytes", "text/html; charset=windows-1252",
			[]byte{0x41, 0x80, 0x81, 0x8d, 0x8f, 0x90, 0x93, 0x94, 0x9d, 0x9f, 0xa0, 0xe9, 0xff},
			"A\u20ac\u0081\u008d\u008f\u0090\u201c\u201d\u009d\u0178\u00a0\u00e9\u00ff",
		},
		{"latin1 is windows-1252", "text/html; charset=latin1", []byte{0x93, 0x41}, "\u201cA"},
		{"iso-8859-1 is windows-1252", "text/html; charset=ISO-8859-1", []byte{0x93, 0x41}, "\u201cA"},
		{"ascii is windows-1252", "text/plain; charset=ascii", []byte{0x80}, "\u20ac"},
		{"us-ascii is windows-1252", "text/plain; charset=us-ascii", []byte{0x80}, "\u20ac"},
		{"windows-1252 does not strip a UTF-8 BOM", "text/plain; charset=windows-1252", []byte{0xef, 0xbb, 0xbf, 0x41}, "\u00ef\u00bb\u00bfA"},
		{"a quoted label", `text/html; charset="iso-8859-1"`, []byte{0xe9}, "\u00e9"},
		{"a single-quoted label", "text/html; charset='utf-16le'", []byte{0x41, 0}, "A"},
		{"an unknown label falls back to utf-8", "text/html; charset=bogus", []byte("h\xc3\xa9"), "h\u00e9"},
		{"a label of another converter falls back to utf-8", "text/html; charset=shift_jis", []byte("h\xc3\xa9"), "h\u00e9"},
		{"an empty label is utf-8", "text/html; charset=", []byte("h\xc3\xa9"), "h\u00e9"},
		{"the first usable charset wins", "text/html; charset=; x=1; charset=latin1", []byte{0x93}, "\u201c"},
		{"an empty body", "text/html; charset=utf-16le", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decodeBody(c.body, c.contentType); got != c.want {
				t.Fatalf("decodeBody(% x, %q) = %+q, want %+q", c.body, c.contentType, got, c.want)
			}
		})
	}
}

func TestCharsetLabel(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"text/html", ""},
		{"text/html; charset=UTF-8", "utf-8"},
		{`text/html; CHARSET="Latin1"`, "latin1"},
		{"text/html;charset=utf-8;x=y", "utf-8"},
		{"text/html; charset=;", ""},
		{"text/html; charset=\"", ""},
		{"charset=a_b-c d", "a_b-c"},
	} {
		if got := charsetLabel(c.in); got != c.want {
			t.Errorf("charsetLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := charsetOf("text/html; charset=latin1"); got != encWindows1252 {
		t.Errorf("charsetOf(latin1) = %q", got)
	}
}
