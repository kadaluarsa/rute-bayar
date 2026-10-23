package provider

import (
	"math"
	"strconv"
	"strings"
)

// ParseIDRAmount reads a gateway amount ("150000", "150000.00", 1.5e+06)
// as whole rupiah. It returns 0 when the value is missing, malformed or has
// a non-zero fractional part, which callers treat as "amount not reported".
func ParseIDRAmount(raw string) int64 {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		if n < 0 {
			return 0
		}
		return n
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f) || f > 1e15 {
		return 0
	}
	return int64(f)
}
