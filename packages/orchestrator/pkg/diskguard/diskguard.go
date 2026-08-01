// Package diskguard enforces a free-space floor on the host filesystems that
// template builds write to.
//
// Upstream e2b-infra keeps template artifacts in cloud object storage, so host
// disk pressure is not a failure mode it guards against. This deployment runs
// STORAGE_PROVIDER=Local, which turns every template build into a ~1.7 GB write
// to the host filesystem that nothing collects. Left unguarded that reaches
// zero, at which point E2B's own Postgres dies and the platform wedges without
// being able to report why. Refusing a build while headroom remains converts
// that wedge into a loud, diagnosable error.
package diskguard

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dustin/go-humanize"
	"golang.org/x/sys/unix"
)

// Usage is the capacity of the filesystem backing a path.
type Usage struct {
	// Path is the path actually probed. It is the requested path, or its
	// nearest existing ancestor when the requested path does not exist yet.
	Path string
	// Device is the filesystem's device ID, so callers can recognise paths
	// that share a filesystem and probe it once.
	Device uint64
	// FreeBytes is space available to unprivileged writers, i.e. what df
	// reports as "Avail" — it excludes the filesystem's reserved blocks.
	FreeBytes uint64
	// TotalBytes is the filesystem's total size.
	TotalBytes uint64
}

// Stat reports the capacity of the filesystem backing path. A path that does
// not exist yet resolves to its nearest existing ancestor, which is the
// filesystem it will be created on.
func Stat(path string) (Usage, error) {
	probe, err := nearestExisting(path)
	if err != nil {
		return Usage{}, err
	}

	var statfs unix.Statfs_t
	if err := unix.Statfs(probe, &statfs); err != nil {
		return Usage{}, fmt.Errorf("failed to stat filesystem for %q: %w", probe, err)
	}

	var stat unix.Stat_t
	if err := unix.Stat(probe, &stat); err != nil {
		return Usage{}, fmt.Errorf("failed to stat %q: %w", probe, err)
	}

	blockSize := uint64(statfs.Bsize)

	return Usage{
		Path:       probe,
		Device:     uint64(stat.Dev),
		FreeBytes:  statfs.Bavail * blockSize,
		TotalBytes: statfs.Blocks * blockSize,
	}, nil
}

// InsufficientDiskError reports that a filesystem sits below the configured
// free-space floor. It names how much is free, how much is required and which
// knob sets the floor, so the reader never has to guess which limit was hit.
type InsufficientDiskError struct {
	Path          string
	FreeBytes     uint64
	RequiredBytes uint64
	// Knob is the environment variable that sets the floor.
	Knob string
}

func (e *InsufficientDiskError) Error() string {
	return fmt.Sprintf(
		"insufficient free disk space on the build host: %s free on %s, %s required (floor set by %s). "+
			"Reclaim template storage on the build host before retrying.",
		humanize.IBytes(e.FreeBytes),
		e.Path,
		humanize.IBytes(e.RequiredBytes),
		e.Knob,
	)
}

// Check refuses when any of the given paths' filesystems has less than
// requiredBytes available. Paths sharing a filesystem are probed once. A
// requiredBytes of 0 disables the check. A path that cannot be probed is an
// error, not a pass: an unverifiable floor is not a satisfied one.
func Check(requiredBytes uint64, knob string, paths ...string) error {
	if requiredBytes == 0 {
		return nil
	}

	probed := make(map[uint64]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}

		usage, err := Stat(path)
		if err != nil {
			return err
		}

		if _, seen := probed[usage.Device]; seen {
			continue
		}
		probed[usage.Device] = struct{}{}

		if usage.FreeBytes < requiredBytes {
			return &InsufficientDiskError{
				Path:          usage.Path,
				FreeBytes:     usage.FreeBytes,
				RequiredBytes: requiredBytes,
				Knob:          knob,
			}
		}
	}

	return nil
}

func nearestExisting(path string) (string, error) {
	probe, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("failed to resolve %q: %w", path, err)
	}

	for {
		_, err := os.Stat(probe)
		if err == nil {
			return probe, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to stat %q: %w", probe, err)
		}

		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("no existing ancestor of %q to measure free space on", path)
		}
		probe = parent
	}
}
