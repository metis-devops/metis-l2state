package statefile

import (
	"math/big"
	"strings"
	"testing"
)

func TestConversionAmountBounds(t *testing.T) {
	maxUint256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)).String()
	base := Conversion{OriginalSupply: maxUint256, BalanceSum: maxUint256, MigratedNative: maxUint256, RemainingSupply: "0", SelfBalance: "0"}
	if err := validateConversion(base); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "-1", "+1", "00", strings.Repeat("9", 78), strings.Repeat("1", 1<<18)} {
		bad := base
		bad.OriginalSupply = value
		if err := validateConversion(bad); err == nil {
			t.Fatal("accepted invalid uint256 decimal")
		}
	}
}

func TestOversizedConversionAllocationBound(t *testing.T) {
	c := Conversion{OriginalSupply: strings.Repeat("1", 1<<18)}
	var err error
	allocations := testing.AllocsPerRun(2, func() { err = validateConversion(c) })
	if err == nil {
		t.Fatal("accepted oversized amount")
	}
	// A generous constant rejection budget, not an exact allocator count.
	// Parsing this numeral first used thousands of allocations (~157 MB).
	if allocations > 16 {
		t.Fatalf("oversized numeral amplified parser work: %.0f allocations", allocations)
	}
}

// Untrusted footer numerals must not cause quadratic big-integer parsing work.
func BenchmarkRejectOversizedConversion(b *testing.B) {
	c := Conversion{OriginalSupply: strings.Repeat("1", 1<<18)}
	b.ReportAllocs()
	for b.Loop() {
		if err := validateConversion(c); err == nil {
			b.Fatal("accepted oversized amount")
		}
	}
}
