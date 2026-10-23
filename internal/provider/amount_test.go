package provider

import "testing"

func TestParseIDRAmount(t *testing.T) {
	cases := map[string]int64{
		"150000": 150000, "150000.00": 150000, " 99000.0 ": 99000, "1.5e+06": 1500000,
		"": 0, "abc": 0, "150000.50": 0, "-1": 0,
	}
	for in, want := range cases {
		if got := ParseIDRAmount(in); got != want {
			t.Errorf("ParseIDRAmount(%q) = %d, want %d", in, got, want)
		}
	}
}
