// Package page holds cursor pagination helpers: an opaque cursor carrying the last id seen.
package page

import (
	"encoding/base64"
	"encoding/json"

	"github.com/hostryhq/go-common/apperr"
)

type cursor struct {
	After string `json:"after"`
}

// Encode returns the opaque cursor for the last id of a page.
func Encode(after string) string {
	b, _ := json.Marshal(cursor{After: after})
	return base64.RawURLEncoding.EncodeToString(b)
}

// Decode returns the id a cursor points after ("" for no cursor).
func Decode(c *string) (string, error) {
	if c == nil || *c == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(*c)
	var cur cursor
	if err != nil || json.Unmarshal(b, &cur) != nil {
		return "", apperr.New(apperr.Validation, "validation", "invalid cursor").WithFields(apperr.FieldError{Field: "cursor", Code: "invalid"})
	}
	return cur.After, nil
}

// Limit validates a requested page size against a default and maximum.
func Limit(l *int, def, max int) (int, error) {
	if l == nil {
		return def, nil
	}
	if *l < 1 || *l > max {
		return 0, apperr.New(apperr.Validation, "validation", "invalid page size").WithFields(apperr.FieldError{Field: "limit", Code: "out_of_range"})
	}
	return *l, nil
}
