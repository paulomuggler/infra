package main

import (
	"testing"
	"time"
)

func TestCheckMinAge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		minAge       time.Duration
		acknowledged bool
		wantErr      bool
	}{
		{name: "unset defers to the builder", minAge: unsetMinAge},
		{name: "the builder default is well clear", minAge: 2 * time.Hour},
		{name: "exactly the safe floor is allowed", minAge: safeMinAge},
		{name: "below the floor is refused", minAge: 30 * time.Second, wantErr: true},
		{name: "no floor at all is refused", minAge: 0, wantErr: true},
		{name: "below the floor is allowed once acknowledged", minAge: 0, acknowledged: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := checkMinAge(tt.minAge, tt.acknowledged)
			if tt.wantErr && err == nil {
				t.Fatalf("checkMinAge(%s, %v) = nil, want refusal", tt.minAge, tt.acknowledged)
			}

			if !tt.wantErr && err != nil {
				t.Fatalf("checkMinAge(%s, %v) = %v, want nil", tt.minAge, tt.acknowledged, err)
			}
		})
	}
}

// The refusal has to be actionable on its own: it names the flag to add and why
// the floor is there, because whoever hits it is mid-incident.
func TestCheckMinAgeRefusalNamesTheFlagAndTheHazard(t *testing.T) {
	t.Parallel()

	err := checkMinAge(0, false)
	if err == nil {
		t.Fatal("expected a refusal")
	}

	for _, want := range []string{acknowledgeFlag, "pausing", "silent"} {
		if !contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %s", want, err)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}
