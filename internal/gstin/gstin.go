// Package gstin normalises and validates GSTINs (ADR-0003 A1.1 item 1). The check
// character is the standard mod-36 Luhn variant over the first 14 characters.
package gstin

import (
	"regexp"
	"strings"
	"unicode"
)

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

var format = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z]$`)

// Error codes returned by Validate (field error codes of the API).
const (
	CodeFormat    = "gstin_format"
	CodeCheckChar = "gstin_check_character"
)

// Normalise upper-cases and removes all white space, so case and spacing variants share one key.
func Normalise(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToUpper(r)
	}, s)
}

// CheckChar returns the check character for the first 14 characters of a GSTIN.
func CheckChar(first14 string) byte {
	sum := 0
	for i := 0; i < len(first14); i++ {
		v := strings.IndexByte(alphabet, first14[i])
		f := 1
		if i%2 == 1 {
			f = 2
		}
		p := v * f
		sum += p/36 + p%36
	}
	return alphabet[(36-sum%36)%36]
}

// Validate checks an already normalised GSTIN and returns "" when valid, otherwise a field error code.
func Validate(n string) string {
	if !format.MatchString(n) {
		return CodeFormat
	}
	if CheckChar(n[:14]) != n[14] {
		return CodeCheckChar
	}
	return ""
}

// State returns the two-digit state code embedded in a GSTIN.
func State(n string) string { return n[:2] }
