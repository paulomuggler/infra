// Package snapshotpolicy decides which of a template build's layers get their
// VM RAM image (the memfile) persisted to template storage.
//
// Every layer of a build pauses its VM and takes a snapshot; the rootfs half of
// that snapshot is the layer's content and is always persisted. The memfile
// half is a full RAM image — 150-300 MiB regardless of what the step did, so a
// metadata-only step such as setEnvs writes 0.2 MiB of rootfs and would
// otherwise cost ~190 MiB of memfile.
//
// Only the final layer's memfile is load-bearing at runtime: sandboxes boot
// from the newest uploaded build's final snapshot, and that snapshot is always
// taken from a cold-booted VM, so its header maps no pages from any ancestor's
// memfile. Intermediate memfiles exist solely to speed a build-cache resume —
// and the builder does not use them for that either: a layer whose source came
// from the cache is cold-created (see phases/steps/builder.go), never resumed.
package snapshotpolicy

import "fmt"

// Policy names which layers persist their memfile.
type Policy string

const (
	// LeafOnly persists only the final layer's memfile. Intermediate layers keep
	// their rootfs diff and their metadata but carry no RAM image.
	LeafOnly Policy = "leaf-only"

	// AllLayers persists every layer's memfile, as upstream does.
	AllLayers Policy = "all-layers"
)

// Default is the policy used when neither the orchestrator nor the build says
// otherwise.
const Default = LeafOnly

// Parse validates a policy name. An empty name yields the zero Policy so
// callers can distinguish "not specified" from "specified as something".
func Parse(s string) (Policy, error) {
	switch Policy(s) {
	case "":
		return "", nil
	case LeafOnly:
		return LeafOnly, nil
	case AllLayers:
		return AllLayers, nil
	default:
		return "", fmt.Errorf("unknown snapshot policy %q, expected %q or %q", s, LeafOnly, AllLayers)
	}
}

// PersistMemfile reports whether a layer's memfile should be written to
// template storage. The final layer always persists: it is what sandboxes boot
// from.
func (p Policy) PersistMemfile(isFinalLayer bool) bool {
	if isFinalLayer {
		return true
	}

	return p != LeafOnly
}
