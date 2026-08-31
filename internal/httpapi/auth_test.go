package httpapi

import "testing"

func TestValidPIN(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "four digits", value: "0427", want: true},
		{name: "too short", value: "427", want: false},
		{name: "too long", value: "04270", want: false},
		{name: "letters", value: "12a4", want: false},
		{name: "unicode digits", value: "１２３４", want: false},
		{name: "spaces", value: " 123", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validPIN(test.value); got != test.want {
				t.Fatalf("validPIN(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}
