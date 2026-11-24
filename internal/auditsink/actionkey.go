// Package auditsink holds what the Postgres and in-memory vendor audit sinks share: the mapping from an
// operation id to the stable action key carried by tenancy.vendor_access.recorded (ADR-0013 V5).
package auditsink

import (
	"strings"
	"unicode"
)

var vendorKeys = map[string]string{
	"createOrganisation":            "vendor.organisation.created",
	"verifyTaxRegistration":         "vendor.registration.verified",
	"getOrganisationDirectoryEntry": "vendor.organisation.read",
	"listOrganisationDirectory":     "vendor.organisation.listed",
}

var supportKeys = map[string]string{
	"getCurrentOrganisation":  "support.read.organisation",
	"listBrands":              "support.read.brands",
	"getBrand":                "support.read.brands",
	"listOutlets":             "support.read.outlets",
	"getOutlet":               "support.read.outlets",
	"listTaxRegistrations":    "support.read.registrations",
	"getTaxRegistration":      "support.read.registrations",
	"listDocumentSeries":      "support.read.series",
	"getOrganisationSettings": "support.read.settings",
}

// ActionKey returns the stable action key (pattern ^[a-z]+(\.[a-z_]+)+$) for an operation id and token
// type ("vendor" or "support").
func ActionKey(operationID, tokenType string) string {
	if tokenType == "support" {
		if k, ok := supportKeys[operationID]; ok {
			return k
		}
	} else if k, ok := vendorKeys[operationID]; ok {
		return k
	}
	var b strings.Builder
	b.WriteString("op.")
	for i, r := range operationID {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
