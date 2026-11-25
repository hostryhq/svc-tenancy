package gstin

import "testing"

func TestValidateAndNormalise(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"valid known GSTIN", "27AAPFU0939F1ZV", ""},
		{"lower case and spaces normalise to the same key", " 27aapfu0939f1zv ", ""},
		{"bad check character", "27AAPFU0939F1ZX", CodeCheckChar},
		{"bad format", "27AAPFU0939F1", CodeFormat},
		{"no Z at position 14", "27AAPFU0939F1AV", CodeFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Validate(Normalise(tc.in)); got != tc.want {
				t.Fatalf("Validate(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	if Normalise("27 aapfu0939f1zv") != Normalise("27AAPFU0939F1ZV") {
		t.Fatal("variants must normalise to one key")
	}
}
