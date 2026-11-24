package compression

import (
	"strings"
)

// Encoding is a content coding supported by the middleware.
type Encoding string

// The supported content codings, named as in Content-Encoding.
const (
	Zstd   Encoding = "zstd"
	Brotli Encoding = "br"
	Gzip   Encoding = "gzip"
)

// maxQ is a quality value of 1, in thousandths.
const maxQ = 1000

// unset marks a coding the Accept-Encoding header does not mention.
const unset = -1

// accepted holds the quality, in thousandths, that an Accept-Encoding header
// gives to each coding; unset when not mentioned.
type accepted struct {
	zstd, br, gzip, star int
}

// Negotiate picks the content coding for an Accept-Encoding header value from
// the server preference list. It honours quality values: the acceptable coding
// with the highest q wins and ties go to the earlier entry of preference. A
// q of 0 excludes a coding, an explicit coding beats "*", "*" covers every
// coding not named, and codings not in preference are ignored. A header that is
// empty, "identity" or lists nothing usable, selects no coding. Malformed
// items (an unparsable q) are ignored. The result is false when the response
// should be sent as is.
func Negotiate(acceptEncoding string, preference []Encoding) (Encoding, bool) {
	if acceptEncoding == "" {
		return "", false
	}
	a := parseAccept(acceptEncoding)
	var best Encoding
	bestQ := 0
	for _, e := range preference {
		if q := a.quality(e); q > bestQ {
			best, bestQ = e, q
		}
	}

	return best, bestQ > 0
}

func (a accepted) quality(e Encoding) int {
	q := unset
	switch e {
	case Zstd:
		q = a.zstd
	case Brotli:
		q = a.br
	case Gzip:
		q = a.gzip
	}
	if q == unset {
		q = a.star
	}
	if q == unset {
		return 0
	}

	return q
}

func parseAccept(h string) accepted {
	a := accepted{unset, unset, unset, unset}
	for rest := h; rest != ""; {
		var item string
		item, rest, _ = strings.Cut(rest, ",")
		name, params, _ := strings.Cut(item, ";")
		name = strings.TrimSpace(name)
		q, ok := parseQ(params)
		if !ok {
			continue
		}
		var slot *int
		switch {
		case strings.EqualFold(name, "zstd"):
			slot = &a.zstd
		case strings.EqualFold(name, "br"):
			slot = &a.br
		case strings.EqualFold(name, "gzip"), strings.EqualFold(name, "x-gzip"):
			slot = &a.gzip
		case name == "*":
			slot = &a.star
		default:
			continue
		}
		if *slot == unset {
			*slot = q
		}
	}

	return a
}

// parseQ reads the q parameter from the parameters after the first ";" of an
// Accept-Encoding item. No q means 1. ok is false for a malformed value.
func parseQ(params string) (int, bool) {
	q := maxQ
	for rest := params; rest != ""; {
		var p string
		p, rest, _ = strings.Cut(rest, ";")
		key, val, found := strings.Cut(p, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}

		return parseQValue(strings.TrimSpace(val))
	}

	return q, true
}

// parseQValue parses "0", "1", "0.5", "1.000": digit, optional dot and up to
// three decimals, never above 1.
func parseQValue(v string) (int, bool) {
	if v == "" || (v[0] != '0' && v[0] != '1') {
		return 0, false
	}
	q := int(v[0]-'0') * maxQ
	if len(v) == 1 {
		return q, true
	}
	const maxDecimals = 3
	if v[1] != '.' || len(v) > 2+maxDecimals {
		return 0, false
	}
	scale := maxQ / 10 //nolint:mnd // first decimal is tenths of a thousandth scale
	frac := 0
	for i := 2; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
		frac += int(v[i]-'0') * scale
		scale /= 10
	}
	q += frac
	if q > maxQ {
		return 0, false
	}

	return q, true
}
