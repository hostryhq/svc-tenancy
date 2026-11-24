package model

// CodeSpace is 32^3: three characters of a 32-symbol alphabet (capitals and digits without I, O, 0, 1),
// so a registration has 32,768 series codes (ADR-0002 S7).
const CodeSpace = 32768

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// DocTypes are the allowed document types, in issuing order.
var DocTypes = []string{"tax_invoice", "bill_of_supply", "plain_bill", "credit_note"}

// CodeFor maps an allocator position to its three-character series code.
func CodeFor(idx int) string {
	return string([]byte{codeAlphabet[idx/1024%32], codeAlphabet[idx/32%32], codeAlphabet[idx%32]})
}

// IndexOf is the inverse of CodeFor; it returns -1 for a string that is not a code.
func IndexOf(code string) int {
	if len(code) != 3 {
		return -1
	}
	idx := 0
	for i := 0; i < 3; i++ {
		p := -1
		for j := 0; j < 32; j++ {
			if codeAlphabet[j] == code[i] {
				p = j
			}
		}
		if p < 0 {
			return -1
		}
		idx = idx*32 + p
	}
	return idx
}
