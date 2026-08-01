package server

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/diskguard"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/builderrors"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/buildlogger"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/config"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/build/core/oci/auth"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/snapshotpolicy"
	templatemanager "github.com/e2b-dev/infra/packages/shared/pkg/grpc/template-manager"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
	"github.com/e2b-dev/infra/packages/shared/pkg/templates"
)

// minFreeDiskKnob is named in the refusal message so an operator reading it
// three layers up knows exactly which setting to change.
const minFreeDiskKnob = "TEMPLATE_BUILD_MIN_FREE_DISK_GB"

func (s *ServerStore) TemplateCreate(ctx context.Context, templateRequest *templatemanager.TemplateCreateRequest) (*emptypb.Empty, error) {
	ctx, childSpan := tracer.Start(ctx, "template-create")
	defer childSpan.End()

	cfg := templateRequest.GetTemplate()

	// Refuse before the build writes anything. Local template storage has no
	// GC, so builds are this host's disk growth vector (~1.7 GB each); letting
	// them run the disk to zero takes E2B's own Postgres down with it and the
	// platform then cannot even report why. A refusal here is the last line —
	// storage reclaim is the maintenance that should keep us far away from it.
	if err := s.checkBuildDiskSpace(); err != nil {
		var insufficient *diskguard.InsufficientDiskError
		if errors.As(err, &insufficient) {
			s.logger.Error(ctx, "refusing template build: build host is below the free-space floor",
				zap.Error(err),
				zap.String("templateID", cfg.GetTemplateID()),
				zap.String("buildID", cfg.GetBuildID()),
			)

			return nil, status.Error(codes.ResourceExhausted, insufficient.Error())
		}

		s.logger.Error(ctx, "cannot verify build host disk space", zap.Error(err))

		return nil, status.Errorf(codes.Internal, "cannot verify free disk space on the build host: %s", err)
	}

	childSpan.SetAttributes(
		telemetry.WithTemplateID(cfg.GetTemplateID()),
		telemetry.WithBuildID(cfg.GetBuildID()),
		attribute.String("env.kernel.version", cfg.GetKernelVersion()),
		attribute.String("env.firecracker.version", cfg.GetFirecrackerVersion()),
		attribute.String("env.start_cmd", cfg.GetStartCommand()),
		attribute.Int64("env.memory_mb", int64(cfg.GetMemoryMB())),
		attribute.Int64("env.vcpu_count", int64(cfg.GetVCpuCount())),
		attribute.Bool("env.huge_pages", cfg.GetHugePages()),
	)

	metadata := storage.Paths{
		BuildID: cfg.GetBuildID(),
	}

	// default to scope by template ID
	cacheScope := cfg.GetTemplateID()
	if templateRequest.CacheScope != nil {
		cacheScope = templateRequest.GetCacheScope()
	}

	// Snapshot policy: the build may override the host-wide default, e.g. a
	// template-iteration workflow asking for all-layers so cache resumes stay
	// warm. An unparseable value is refused rather than silently defaulted.
	snapshotPolicy, err := s.resolveSnapshotPolicy(templateRequest.GetSnapshotPolicy())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Create the auth provider using the factory
	authProvider := auth.NewAuthProvider(cfg.GetFromImageRegistry())

	// TODO: Remove, temporary handling when version is not sent from the API
	version := templateRequest.GetVersion()
	if version == "" {
		if cfg.GetFromImage() == "" && cfg.GetFromTemplate() == nil {
			version = templates.TemplateV1Version
		} else {
			version = templates.TemplateV2BetaVersion
		}
	}

	template := config.TemplateConfig{
		Version:              version,
		TeamID:               cfg.GetTeamID(),
		TemplateID:           cfg.GetTemplateID(),
		CacheScope:           cacheScope,
		SnapshotPolicy:       snapshotPolicy,
		VCpuCount:            int64(cfg.GetVCpuCount()),
		MemoryMB:             int64(cfg.GetMemoryMB()),
		StartCmd:             cfg.GetStartCommand(),
		ReadyCmd:             cfg.GetReadyCommand(),
		DiskSizeMB:           int64(cfg.GetDiskSizeMB()),
		HugePages:            cfg.GetHugePages(),
		FromImage:            cfg.GetFromImage(),
		FromTemplate:         cfg.GetFromTemplate(),
		RegistryAuthProvider: authProvider,
		Force:                cfg.Force,
		Steps:                cfg.GetSteps(),
		KernelVersion:        cfg.GetKernelVersion(),
		FirecrackerVersion:   cfg.GetFirecrackerVersion(),
	}

	logs := buildlogger.NewLogEntryLogger()
	buildInfo, err := s.buildCache.Create(template.TeamID, metadata.BuildID, logs)
	if err != nil {
		return nil, fmt.Errorf("error while creating build cache: %w", err)
	}

	// Add new core that will log all messages using logger (zap.Logger) to the logs buffer too
	encoder := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	bufferCore := zapcore.NewCore(encoder, logs, zapcore.DebugLevel)
	core := zapcore.NewTee(bufferCore, s.buildLogger.Detach(ctx).Core().
		With([]zap.Field{
			{Type: zapcore.StringType, Key: "envID", String: cfg.GetTemplateID()},
			{Type: zapcore.StringType, Key: "buildID", String: metadata.BuildID},
		}),
	)

	s.wg.Add(1)
	s.activeBuilds.Add(1)
	go func(ctx context.Context) {
		defer s.wg.Done()
		defer s.activeBuilds.Add(-1)

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		ctx, buildSpan := tracer.Start(ctx, "template-background-build", trace.WithAttributes(
			telemetry.WithTemplateID(template.TemplateID),
			telemetry.WithBuildID(metadata.BuildID),
			telemetry.WithTeamID(template.TeamID),
		))
		defer buildSpan.End()

		defer func() {
			if r := recover(); r != nil {
				telemetry.ReportCriticalError(ctx, "recovered from panic in template build handler", nil, attribute.String("panic", fmt.Sprintf("%v", r)), telemetry.WithTemplateID(cfg.GetTemplateID()), telemetry.WithBuildID(cfg.GetBuildID()))
				buildInfo.SetFail(builderrors.UnwrapUserError(nil))
			}
		}()

		// Watch for build cancellation requests
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-buildInfo.Result.Done:
				res, _ := buildInfo.Result.Result()
				if res.Status == templatemanager.TemplateBuildState_Failed {
					cancel()
				}

				return
			}
		}()

		res, err := s.builder.Build(ctx, metadata, template, core)
		_ = core.Sync()
		if err != nil {
			userError := builderrors.UnwrapUserError(err)

			attrs := []attribute.KeyValue{
				telemetry.WithTemplateID(cfg.GetTemplateID()),
				telemetry.WithBuildID(cfg.GetBuildID()),
			}
			if userError.GetMessage() == builderrors.InternalErrorMessage {
				telemetry.ReportCriticalError(ctx, "error while building template", err, attrs...)
			} else {
				telemetry.ReportError(ctx, "error while building template", err, attrs...)
			}

			buildInfo.SetFail(userError)
		} else {
			buildInfo.SetSuccess(&templatemanager.TemplateBuildMetadata{
				RootfsSizeKey:  int32(res.RootfsSizeMB),
				EnvdVersionKey: res.EnvdVersion,
			})
			telemetry.ReportEvent(ctx, "Environment built")
		}
	}(context.WithoutCancel(ctx))

	return nil, nil
}

// resolveSnapshotPolicy takes the build's requested policy when it names one and
// the orchestrator's configured default otherwise.
func (s *ServerStore) resolveSnapshotPolicy(requested string) (snapshotpolicy.Policy, error) {
	p, err := snapshotpolicy.Parse(requested)
	if err != nil {
		return "", err
	}

	if p != "" {
		return p, nil
	}

	return s.builderConfig.SnapshotPolicy()
}

// checkBuildDiskSpace refuses when any filesystem a template build writes to is
// below the configured free-space floor.
func (s *ServerStore) checkBuildDiskSpace() error {
	return diskguard.Check(
		s.builderConfig.TemplateBuildMinFreeDiskBytes(),
		minFreeDiskKnob,
		s.buildStoragePaths()...,
	)
}

// buildStoragePaths are the host paths a template build writes to. The build
// directory is where layers are assembled; with STORAGE_PROVIDER=Local the
// finished artifacts and the layer index also land on this host, and those are
// what accumulate. Paths that share a filesystem are probed once.
func (s *ServerStore) buildStoragePaths() []string {
	paths := []string{s.builderConfig.TemplatesDir}

	if storage.GetProviderType() == storage.LocalStorageProvider {
		paths = append(paths,
			storage.TemplateStorageConfig.GetLocalBasePath(),
			storage.BuildCacheStorageConfig.GetLocalBasePath(),
		)
	}

	return paths
}
