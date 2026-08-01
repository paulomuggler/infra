// Command template-gc runs one template-storage collection pass: it reads the
// live root set from the registry and asks a builder to reclaim every build
// directory those roots cannot reach.
//
// It is the periodic driver — a systemd timer runs it — and the operator's
// manual trigger. It shares the root-set query with the API service's reactive
// trigger and the closure with the builder, so a sweep and a post-build
// collection can never disagree about what is live.
//
//	template-gc --dry-run     # report only, deletes nothing
//	template-gc               # collect
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	templatemanager "github.com/e2b-dev/infra/packages/api/internal/template-manager"
	sqlcdb "github.com/e2b-dev/infra/packages/db/client"
	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	templatemanagergrpc "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
)

const defaultTimeout = 30 * time.Minute

// unsetMinAge is the sentinel for "let the builder decide", which is the normal
// case. A literal 0 is meaningful and different: it drops the age floor
// entirely, which is how a proof run collects something minted minutes ago.
const unsetMinAge = -1 * time.Second

// safeMinAge is the smallest age floor that still closes the pause window on
// its own. Pausing a sandbox writes its snapshot directory outside the build
// lock, so for the interleaving "roots are read -> a pause commits and writes
// its directory -> the pass scans", the age floor is the ONLY thing protecting
// that directory: the snapshot build was not in the root set the pass is
// holding, and the sandbox's own map entry names the build it booted from, not
// the snapshot. That window is the root read plus one snapshot write —
// measured at one to two seconds on this host — so a minute is a ~30x margin.
// Below it the operator has to say so.
const safeMinAge = time.Minute

// acknowledgeFlag is named in the refusal so the operator reading it knows
// exactly what to add and, from the message above it, what they are taking on.
const acknowledgeFlag = "unsafe-no-age-floor"

func minAgeOverride(d time.Duration) *uint64 {
	if d < 0 {
		return nil
	}

	seconds := uint64(d.Seconds())

	return &seconds
}

// checkMinAge refuses an age floor low enough to expose a concurrently pausing
// sandbox unless the operator has said they mean it.
//
// This is not an "are you sure" prompt. Deleting a paused sandbox's snapshot
// fails silently — the sandbox resumes into Input/output error on whatever page
// it happens to touch — and that is the failure class this whole collector
// exists to end. The author of this tool used --min-age=0 during its own proof
// runs without realising the exposure, which is the plainest evidence that
// documenting it is not enough.
func checkMinAge(minAge time.Duration, acknowledged bool) error {
	if minAge < 0 || minAge >= safeMinAge || acknowledged {
		return nil
	}

	return fmt.Errorf(
		"--min-age=%s is below %s, which leaves a sandbox that is pausing right now "+
			"unprotected: its snapshot directory is written outside the build lock and is "+
			"not in the root set this pass is holding, so only the age floor keeps it. "+
			"Deleting it is silent — the sandbox resumes into I/O errors. "+
			"Pass --%s if that is what you mean.",
		minAge, safeMinAge, acknowledgeFlag)
}

func main() {
	dryRun := flag.Bool("dry-run", false, "report what would be collected without deleting anything")
	builder := flag.String("builder", env.GetEnv("TEMPLATE_GC_BUILDER_ADDR", "localhost:5008"),
		"address of the builder's template-manager gRPC service")
	reason := flag.String("reason", "periodic", "recorded in the ledger and the log line")
	timeout := flag.Duration("timeout", defaultTimeout, "overall deadline for the pass")
	minAge := flag.Duration("min-age", unsetMinAge,
		"protect directories modified more recently than this; unset uses the builder's TEMPLATE_GC_MIN_AGE")
	acknowledged := flag.Bool(acknowledgeFlag, false,
		"permit a --min-age below "+safeMinAge.String()+", which exposes a concurrently pausing sandbox")
	flag.Parse()

	if err := checkMinAge(*minAge, *acknowledged); err != nil {
		fmt.Fprintf(os.Stderr, "template-gc: %s\n", err)
		os.Exit(2)
	}

	if err := run(*builder, *reason, *dryRun, *timeout, *minAge); err != nil {
		fmt.Fprintf(os.Stderr, "template-gc: %s\n", err)
		os.Exit(1)
	}
}

func run(builderAddr, reason string, dryRun bool, timeout, minAge time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	databaseURL := os.Getenv("POSTGRES_CONNECTION_STRING")
	if databaseURL == "" {
		return fmt.Errorf("POSTGRES_CONNECTION_STRING is not set")
	}

	db, err := sqlcdb.NewClient(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to the registry database: %w", err)
	}
	defer db.Close()

	conn, err := grpc.NewClient(builderAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to dial builder at %s: %w", builderAddr, err)
	}
	defer conn.Close()

	client := templatemanagergrpc.NewTemplateServiceClient(conn)

	res, err := templatemanager.CollectStorageWithClient(ctx, db, client, reason, dryRun, minAgeOverride(minAge))
	if err != nil {
		return err
	}

	report(res, dryRun)

	return nil
}

func report(res *templatemanagergrpc.TemplateStorageCollectResponse, dryRun bool) {
	verb := "collected"
	if dryRun {
		verb = "would collect"
	}

	fmt.Printf("run %s\n", res.GetRunID())
	fmt.Printf("  scanned            %d dirs\n", res.GetScannedDirs())
	fmt.Printf("  roots              %d builds\n", res.GetRootBuilds())
	fmt.Printf("  kept (closure)     %d dirs\n", res.GetKeptDirs())
	fmt.Printf("  %-18s %d dirs, %s\n", verb, res.GetCollectedDirs(), humanize.IBytes(res.GetFreedBytes()))
	fmt.Printf("  skipped (too new)  %d dirs\n", res.GetSkippedRecentDirs())
	fmt.Printf("  index blobs pruned %d\n", res.GetPrunedIndexBlobs())
	fmt.Printf("  dangling refs      %d\n", res.GetDanglingRefs())
	fmt.Printf("  broken roots       %d\n", len(res.GetBrokenRoots()))
	fmt.Printf("  missing roots      %d\n", len(res.GetMissingRoots()))
	fmt.Printf("  ledger             %s\n", res.GetLedgerPath())
}
