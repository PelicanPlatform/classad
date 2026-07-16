package classad

import "testing"

// TestCountMatches covers countMatches / evalInEachContext, the functions HTCondor
// generates for heterogeneous custom-resource (GPU) matchmaking: arg0 is evaluated
// in the scope of each nested ad in the arg1 list. countMatches counts the
// boolean-true results without propagating undefined/error; evalInEachContext
// returns the list of per-ad results.
func TestCountMatches(t *testing.T) {
	slot := `[
		AvailableGPUs = { GPUs_tag1, GPUs_tag2, GPUs_tag3 };
		GPUs_tag1 = [ Capability = 4.0; GlobalMemoryMb = 8000 ];
		GPUs_tag2 = [ Capability = 5.5; GlobalMemoryMb = 16000 ];
		GPUs_tag3 = [ Capability = 8.0; GlobalMemoryMb = 40000 ];
		RequireGPUs = Capability > 4;
		RequestGPUs = 2
	]`
	ad, err := Parse(slot)
	if err != nil {
		t.Fatal(err)
	}
	intCases := []struct {
		expr string
		want int64
	}{
		{`countMatches(Capability > 4, AvailableGPUs)`, 2},          // 5.5, 8.0
		{`countMatches(Capability >= 4.0, AvailableGPUs)`, 3},       // all
		{`countMatches(GlobalMemoryMb >= 16000, AvailableGPUs)`, 2}, // 16000, 40000
		{`countMatches(Capability > 100, AvailableGPUs)`, 0},        // none
		{`countMatches(MY.RequireGPUs, AvailableGPUs)`, 2},          // attr ref -> bound expr Capability > 4
		{`countMatches(Undef, AvailableGPUs)`, 0},                   // unbound projection -> not counted
		{`countMatches(Capability > 4, DoesNotExist)`, 0},           // undefined list -> 0, not error
		// The generated matchmaking shape: enough GPUs satisfy the requirement.
		{`countMatches(MY.RequireGPUs, AvailableGPUs) >= RequestGPUs`, 1}, // true == 1 (bool-equiv)
	}
	for _, tc := range intCases {
		expr, err := ParseExpr(tc.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.expr, err)
		}
		v := expr.Eval(ad)
		if b, ok := v.BoolValue(); ok == nil { // the >= case yields a boolean
			got := int64(0)
			if b {
				got = 1
			}
			if got != tc.want {
				t.Errorf("%s => %v, want %d", tc.expr, v, tc.want)
			}
			continue
		}
		got, gerr := v.IntValue()
		if gerr != nil || got != tc.want {
			t.Errorf("%s => %v, want %d", tc.expr, v, tc.want)
		}
	}

	// evalInEachContext returns the list of per-ad projections; sum/max fold over it.
	for _, tc := range []struct {
		expr string
		want float64
	}{
		{`sum(evalInEachContext(Capability, AvailableGPUs))`, 17.5},
		{`max(evalInEachContext(Capability, AvailableGPUs))`, 8.0},
		{`min(evalInEachContext(GlobalMemoryMb, AvailableGPUs))`, 8000},
	} {
		expr, err := ParseExpr(tc.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.expr, err)
		}
		v := expr.Eval(ad)
		f, ferr := v.RealValue()
		if ferr != nil {
			if i, ierr := v.IntValue(); ierr == nil {
				f = float64(i)
			} else {
				t.Errorf("%s => %v (not numeric)", tc.expr, v)
				continue
			}
		}
		if f != tc.want {
			t.Errorf("%s => %v, want %v", tc.expr, v, tc.want)
		}
	}
}
