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

func main() {
	dryRun := flag.Bool("dry-run", false, "report what would be collected without deleting anything")
	builder := flag.String("builder", env.GetEnv("TEMPLATE_GC_BUILDER_ADDR", "localhost:5008"),
		"address of the builder's template-manager gRPC service")
	reason := flag.String("reason", "periodic", "recorded in the ledger and the log line")
	timeout := flag.Duration("timeout", defaultTimeout, "overall deadline for the pass")
	flag.Parse()

	if err := run(*builder, *reason, *dryRun, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "template-gc: %s\n", err)
		os.Exit(1)
	}
}

func run(builderAddr, reason string, dryRun bool, timeout time.Duration) error {
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

	res, err := templatemanager.CollectStorageWithClient(ctx, db, client, reason, dryRun)
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
