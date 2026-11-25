// Package states is the GST state and union-territory reference table (platform reference data,
// ADR-0003 section 2). Codes follow the GSTIN prefix. The list is maintained here until the tax
// tables of gst-invoicing-india become a shared data set.
package states

// State is one reference row.
type State struct{ Code, Name string }

// All lists the states in code order.
var All = []State{
	{"01", "Jammu and Kashmir"}, {"02", "Himachal Pradesh"}, {"03", "Punjab"}, {"04", "Chandigarh"},
	{"05", "Uttarakhand"}, {"06", "Haryana"}, {"07", "Delhi"}, {"08", "Rajasthan"}, {"09", "Uttar Pradesh"},
	{"10", "Bihar"}, {"11", "Sikkim"}, {"12", "Arunachal Pradesh"}, {"13", "Nagaland"}, {"14", "Manipur"},
	{"15", "Mizoram"}, {"16", "Tripura"}, {"17", "Meghalaya"}, {"18", "Assam"}, {"19", "West Bengal"},
	{"20", "Jharkhand"}, {"21", "Odisha"}, {"22", "Chhattisgarh"}, {"23", "Madhya Pradesh"}, {"24", "Gujarat"},
	{"26", "Dadra and Nagar Haveli and Daman and Diu"}, {"27", "Maharashtra"}, {"29", "Karnataka"}, {"30", "Goa"},
	{"31", "Lakshadweep"}, {"32", "Kerala"}, {"33", "Tamil Nadu"}, {"34", "Puducherry"},
	{"35", "Andaman and Nicobar Islands"}, {"36", "Telangana"}, {"37", "Andhra Pradesh"}, {"38", "Ladakh"},
	{"97", "Other Territory"},
}

var byCode = func() map[string]bool {
	m := map[string]bool{}
	for _, s := range All {
		m[s.Code] = true
	}
	return m
}()

// Valid reports whether code is in the table.
func Valid(code string) bool { return byCode[code] }
