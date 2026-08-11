package restated

import "testing"

// Each declaration below restates the three-implementer cardinality AND
// compares it against a measurement, in each of the three spellings.

func TestConstForm(t *testing.T) {
	all := []Shape{&Square{}, &Circle{}}
	const wantShapes = 3 // WANT
	if len(all) != wantShapes {
		t.Fatal("count drifted")
	}
}

func TestVarForm(t *testing.T) {
	all := []Shape{&Square{}}
	wantVar := 3 // WANT
	if len(all) != wantVar {
		t.Fatal("count drifted")
	}
}

func TestShortForm(t *testing.T) {
	seen := 0
	for range []int{1, 2} {
		seen++
	}
	wantShort := 3 // WANT
	if seen != wantShort {
		t.Fatal("count drifted")
	}
}
