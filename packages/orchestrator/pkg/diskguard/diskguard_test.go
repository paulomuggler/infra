package diskguard

import (
	"errors"
	"math"
	"path/filepath"
	"testing"
)

func TestStatResolvesNonExistentPathToAncestor(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not", "created", "yet")

	usage, err := Stat(missing)
	if err != nil {
		t.Fatalf("Stat(%q) = %v, want nil", missing, err)
	}
	if usage.Path != dir {
		t.Fatalf("probed %q, want the nearest existing ancestor %q", usage.Path, dir)
	}
	if usage.TotalBytes == 0 {
		t.Fatal("TotalBytes = 0, want the backing filesystem's size")
	}
}

func TestCheckRefusesBelowFloor(t *testing.T) {
	err := Check(math.MaxUint64/2, "TEST_KNOB", t.TempDir())

	var insufficient *InsufficientDiskError
	if !errors.As(err, &insufficient) {
		t.Fatalf("Check() = %v, want *InsufficientDiskError", err)
	}
	if insufficient.Knob != "TEST_KNOB" {
		t.Fatalf("Knob = %q, want the configured knob name", insufficient.Knob)
	}
}

func TestCheckPassesUnderRealisticFloorAndWhenDisabled(t *testing.T) {
	if err := Check(1, "TEST_KNOB", t.TempDir()); err != nil {
		t.Fatalf("Check(1 byte) = %v, want nil", err)
	}
	if err := Check(0, "TEST_KNOB", "/definitely/not/a/path"); err != nil {
		t.Fatalf("Check(0) = %v, want nil (0 disables the guard)", err)
	}
}
