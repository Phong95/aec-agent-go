package jsstr

import (
	"math"
	"time"
)

// maxDateMs is the largest absolute time value of a JS Date (8.64e15 ms).
const maxDateMs = 8.64e15

// ParseDate is the subset of Date.parse (and so of new Date(string)) that the memory
// pipeline needs, which is the ECMAScript date time string format and its RFC 3339
// relatives, returning the time value in milliseconds since the epoch or NaN:
//
//	date  = YYYY | YYYY-MM | YYYY-MM-DD | (+|-)YYYYYY[-MM[-DD]]
//	time  = (T | t | space) HH:mm[:ss[.f+]]   an hour of 24 only with 00:00:00.000
//	zone  = Z | z | (+|-)hh:mm | (+|-)hhmm
//
// A date without a time is UTC, a date time without a zone is in loc (JS local time;
// nil means time.Local), and a fraction keeps its first three digits. The calendar
// is as lenient as V8's: a month must be 1 to 12 and a day 1 to 31, and a day past
// the end of the month rolls into the next month ("2026-02-30" is 2 March). A value
// beyond 8.64e15 ms in either direction is NaN.
//
// Date.parse also falls back to a legacy parser for strings that are not in this
// format ("Mar 4 2026", "2026/03/04", "2026-3-4", a bare "12", surrounding
// whitespace, a trailing "-", fullwidth digits). Go does not parse any of them: they
// are NaN here. The recorded corpus tests/testdata/golden/jsstr-date-parse.json names
// every such row, and a caller that stores such a date reads it as "never expires".
func ParseDate(s string, loc *time.Location) float64 {
	if loc == nil {
		loc = time.Local
	}
	p := dateParser{s: s}

	year, ok := p.year()
	if !ok {
		return math.NaN()
	}
	month, day := 1, 1
	if p.eat('-') {
		if month, ok = p.digits(2); !ok || month < 1 || month > 12 {
			return math.NaN()
		}
		if p.eat('-') {
			if day, ok = p.digits(2); !ok || day < 1 || day > 31 {
				return math.NaN()
			}
		}
	}

	hour, minute, sec, nanos := 0, 0, 0, 0
	zone := time.UTC
	offsetMs := int64(0)
	if !p.done() {
		if !p.eat('T') && !p.eat('t') && !p.eat(' ') {
			return math.NaN()
		}
		if hour, ok = p.digits(2); !ok || hour > 24 || !p.eat(':') {
			return math.NaN()
		}
		if minute, ok = p.digits(2); !ok || minute > 59 {
			return math.NaN()
		}
		if p.eat(':') {
			if sec, ok = p.digits(2); !ok || sec > 59 {
				return math.NaN()
			}
			if p.eat('.') {
				if nanos, ok = p.fraction(); !ok {
					return math.NaN()
				}
			}
		}
		if hour == 24 && (minute != 0 || sec != 0 || nanos != 0) {
			return math.NaN()
		}
		switch {
		case p.done():
			zone = loc
		case p.eat('Z') || p.eat('z'):
			// UTC
		default:
			sign := int64(1)
			switch {
			case p.eat('+'):
			case p.eat('-'):
				sign = -1
			default:
				return math.NaN()
			}
			oh, ok := p.digits(2)
			if !ok || oh > 23 {
				return math.NaN()
			}
			p.eat(':')
			om, ok := p.digits(2)
			if !ok || om > 59 {
				return math.NaN()
			}
			offsetMs = sign * int64(oh*60+om) * 60_000
		}
		if !p.done() {
			return math.NaN()
		}
	}

	t := time.Date(year, time.Month(month), day, hour, minute, sec, nanos, zone)
	ms := t.UnixMilli() - offsetMs
	if math.Abs(float64(ms)) > maxDateMs {
		return math.NaN()
	}
	return float64(ms)
}

// dateParser walks a date string byte by byte; every accepted character is ASCII.
type dateParser struct {
	s string
	i int
}

func (p *dateParser) done() bool { return p.i >= len(p.s) }

// eat consumes c when it is next.
func (p *dateParser) eat(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

// digits reads exactly n ASCII digits.
func (p *dateParser) digits(n int) (int, bool) {
	if p.i+n > len(p.s) {
		return 0, false
	}
	v := 0
	for k := range n {
		c := p.s[p.i+k]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	p.i += n
	return v, true
}

// year reads YYYY or the signed six digit form. The negative zero year -000000 is
// rejected, as ECMAScript says; a plus sign with only four digits is not an ISO year
// (V8 reads it through its legacy path), so it is NaN here.
func (p *dateParser) year() (int, bool) {
	if p.i < len(p.s) && p.s[p.i] == '-' {
		p.i++
		y, ok := p.digits(6)
		if !ok || y == 0 {
			return 0, false
		}
		return -y, true
	}
	if p.i < len(p.s) && p.s[p.i] == '+' {
		p.i++
		return p.digits(6)
	}
	return p.digits(4)
}

// fraction reads the digits after the decimal point (at least one) and returns the
// value in nanoseconds with the first three digits kept.
func (p *dateParser) fraction() (int, bool) {
	start := p.i
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	digits := p.s[start:p.i]
	if digits == "" {
		return 0, false
	}
	ms := 0
	for k := range 3 {
		ms *= 10
		if k < len(digits) {
			ms += int(digits[k] - '0')
		}
	}
	return ms * 1_000_000, true
}
