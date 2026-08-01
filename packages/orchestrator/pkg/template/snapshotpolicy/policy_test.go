package snapshotpolicy

import "testing"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Policy
		wantErr bool
	}{
		{in: "", want: ""},
		{in: "leaf-only", want: LeafOnly},
		{in: "all-layers", want: AllLayers},
		{in: "LEAF-ONLY", wantErr: true},
		{in: "none", wantErr: true},
	} {
		got, err := Parse(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) = %q, want error", tc.in, got)
			}

			continue
		}
		if err != nil {
			t.Errorf("Parse(%q) returned %v", tc.in, err)

			continue
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPersistMemfile(t *testing.T) {
	for _, tc := range []struct {
		policy  Policy
		isFinal bool
		want    bool
	}{
		{policy: LeafOnly, isFinal: false, want: false},
		{policy: LeafOnly, isFinal: true, want: true},
		{policy: AllLayers, isFinal: false, want: true},
		{policy: AllLayers, isFinal: true, want: true},
		// An unset policy must not silently drop memfiles: only leaf-only does.
		{policy: "", isFinal: false, want: true},
	} {
		if got := tc.policy.PersistMemfile(tc.isFinal); got != tc.want {
			t.Errorf("Policy(%q).PersistMemfile(%v) = %v, want %v", tc.policy, tc.isFinal, got, tc.want)
		}
	}
}
