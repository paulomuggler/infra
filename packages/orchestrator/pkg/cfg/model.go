package cfg

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/willscott/go-nfs"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/snapshotpolicy"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

const DefaultBusyboxVersion = "1.36.1"

type BuilderConfig struct {
	AllowSandboxInternet   bool          `env:"ALLOW_SANDBOX_INTERNET"   envDefault:"true"`
	DomainName             string        `env:"DOMAIN_NAME"              envDefault:""`
	EnvdTimeout            time.Duration `env:"ENVD_TIMEOUT"             envDefault:"10s"`
	FirecrackerVersionsDir string        `env:"FIRECRACKER_VERSIONS_DIR" envDefault:"/fc-versions"`
	BusyboxVersion         string        `env:"BUSYBOX_VERSION"          envDefault:"1.36.1"`
	HostBusyboxDir         string        `env:"HOST_BUSYBOX_DIR"         envDefault:"/fc-busybox"`
	HostEnvdPath           string        `env:"HOST_ENVD_PATH"           envDefault:"/fc-envd/envd"`
	HostKernelsDir         string        `env:"HOST_KERNELS_DIR"         envDefault:"/fc-kernels"`
	OrchestratorBaseDir    string        `env:"ORCHESTRATOR_BASE_PATH"   envDefault:"/orchestrator"`
	SandboxDir             string        `env:"SANDBOX_DIR"              envDefault:"/fc-vm"`
	SharedChunkCacheDir    string        `env:"SHARED_CHUNK_CACHE_PATH"`
	TemplatesDir           string        `env:"TEMPLATES_DIR,expand"     envDefault:"${ORCHESTRATOR_BASE_PATH}/build-templates"`

	DefaultCacheDir string `env:"DEFAULT_CACHE_DIR,expand" envDefault:"${ORCHESTRATOR_BASE_PATH}/build"`

	// TemplateBuildMinFreeDiskGB is the free-space floor, in GiB, that the
	// filesystems a template build writes to must be above for the build to be
	// admitted. Set to 0 to disable the guard. See pkg/diskguard.
	TemplateBuildMinFreeDiskGB int64 `env:"TEMPLATE_BUILD_MIN_FREE_DISK_GB" envDefault:"50"`

	// TemplateGCMinAge protects build directories modified more recently than
	// this from storage GC. It covers intermediate layer directories minted by
	// a build the orchestrator has since forgotten — a crash or a restart
	// mid-build — which no root references and no in-flight build claims. See
	// pkg/template/gc.
	TemplateGCMinAge time.Duration `env:"TEMPLATE_GC_MIN_AGE" envDefault:"2h"`

	// TemplateSnapshotPolicy is the host-wide default for which of a build's
	// layers persist their VM RAM image: "leaf-only" or "all-layers". A build
	// may override it per request. See pkg/template/snapshotpolicy.
	TemplateSnapshotPolicy string `env:"TEMPLATE_SNAPSHOT_POLICY" envDefault:"leaf-only"`

	Provider string `env:"PROVIDER" envDefault:"gcp"`

	StorageConfig storage.Config
	NetworkConfig network.Config
}

// SnapshotPolicy is the validated host-wide default snapshot policy.
func (c BuilderConfig) SnapshotPolicy() (snapshotpolicy.Policy, error) {
	p, err := snapshotpolicy.Parse(c.TemplateSnapshotPolicy)
	if err != nil {
		return "", fmt.Errorf("invalid TEMPLATE_SNAPSHOT_POLICY: %w", err)
	}

	if p == "" {
		return snapshotpolicy.Default, nil
	}

	return p, nil
}

// TemplateBuildMinFreeDiskBytes is the free-space floor in bytes, 0 when the
// guard is disabled.
func (c BuilderConfig) TemplateBuildMinFreeDiskBytes() uint64 {
	if c.TemplateBuildMinFreeDiskGB <= 0 {
		return 0
	}

	return uint64(c.TemplateBuildMinFreeDiskGB) << 30
}

func makePathsAbsolute(c *BuilderConfig) error {
	for _, item := range []*string{
		&c.DefaultCacheDir,
		&c.FirecrackerVersionsDir,
		&c.HostBusyboxDir,
		&c.HostEnvdPath,
		&c.HostKernelsDir,
		&c.OrchestratorBaseDir,
		&c.StorageConfig.SandboxCacheDir,
		&c.SandboxDir,
		&c.SharedChunkCacheDir,
		&c.StorageConfig.SnapshotCacheDir,
		&c.StorageConfig.TemplateCacheDir,
		&c.TemplatesDir,
	} {
		dir := *item

		if dir == "" {
			continue
		}

		if filepath.IsAbs(dir) {
			continue
		}

		dir, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("failed to resolve %q to absolute path: %w", *item, err)
		}

		*item = dir
	}

	return nil
}

type Config struct {
	BuilderConfig

	ClickhouseConnectionString string            `env:"CLICKHOUSE_CONNECTION_STRING"`
	ForceStop                  bool              `env:"FORCE_STOP"`
	GRPCPort                   uint16            `env:"GRPC_PORT"                     envDefault:"5008"`
	LaunchDarklyAPIKey         string            `env:"LAUNCH_DARKLY_API_KEY"`
	LocalUploadBaseURL         string            `env:"LOCAL_UPLOAD_BASE_URL"`
	NodeIP                     string            `env:"NODE_IP"                       envDefault:"localhost"`
	NodeLabels                 []string          `env:"NODE_LABELS"                   envSeparator:","`
	OrchestratorLockPath       string            `env:"ORCHESTRATOR_LOCK_PATH"        envDefault:"/orchestrator.lock"`
	NFSProxyLogging            bool              `env:"NFS_PROXY_LOGGING"             envDefault:"false"`
	NFSProxyTracing            bool              `env:"NFS_PROXY_TRACING"             envDefault:"false"`
	NFSProxyMetrics            bool              `env:"NFS_PROXY_METRICS"             envDefault:"true"`
	NFSProxyRecordHandleCalls  bool              `env:"NFS_PROXY_RECORD_HANDLE_CALLS" envDefault:"false"`
	NFSProxyRecordStatCalls    bool              `env:"NFS_PROXY_RECORD_STAT_CALLS"   envDefault:"false"`
	NFSProxyLogLevel           nfs.LogLevel      `env:"NFS_PROXY_LOG_LEVEL"           envDefault:"info"`
	ProxyPort                  uint16            `env:"PROXY_PORT"                    envDefault:"5007"`
	RedisClusterURL            string            `env:"REDIS_CLUSTER_URL"`
	RedisTLSCABase64           string            `env:"REDIS_TLS_CA_BASE64"`
	RedisURL                   string            `env:"REDIS_URL"`
	RedisPoolSize              int               `env:"REDIS_POOL_SIZE"               envDefault:"5"`
	RedisMinIdleConns          int               `env:"REDIS_MIN_IDLE_CONNS"          envDefault:"2"`
	NBDPoolSize                int               `env:"NBD_POOL_SIZE"                 envDefault:"64"`
	Services                   []string          `env:"ORCHESTRATOR_SERVICES"         envDefault:"orchestrator"`
	PersistentVolumeMounts     map[string]string `env:"PERSISTENT_VOLUME_MOUNTS"`
}

func (c Config) NodeAddress() *string {
	if c.NodeIP == "localhost" {
		return nil
	}

	addr := fmt.Sprintf("%s:%d", c.NodeIP, c.GRPCPort)

	return &addr
}

func Parse() (Config, error) {
	config, err := env.ParseAsWithOptions[Config](env.Options{
		FuncMap: map[reflect.Type]env.ParserFunc{
			reflect.TypeFor[nfs.LogLevel](): func(s string) (any, error) {
				s = strings.ToLower(s)

				return nfs.Log.ParseLevel(s)
			},
		},
	})
	if err != nil {
		return config, err
	}

	bc := config.BuilderConfig
	if err = makePathsAbsolute(&bc); err != nil {
		return config, err
	}

	config.BuilderConfig = bc

	if _, err = bc.SnapshotPolicy(); err != nil {
		return config, err
	}

	if config.PersistentVolumeMounts != nil {
		for name, path := range config.PersistentVolumeMounts {
			path = filepath.Clean(path)
			path, err = filepath.Abs(path)
			if err != nil {
				return config, fmt.Errorf("failed to make persistent volume mount %q an absolute path: %w", name, err)
			}

			if _, err := os.Stat(path); err != nil {
				return config, fmt.Errorf("failed to access persistent volume mount %q (%q): %w", name, path, err)
			}

			config.PersistentVolumeMounts[name] = path // store the cleaned path
		}
	}

	return config, nil
}

func ParseBuilder() (BuilderConfig, error) {
	model, err := env.ParseAs[BuilderConfig]()
	if err != nil {
		return BuilderConfig{}, err
	}

	if err = makePathsAbsolute(&model); err != nil {
		return BuilderConfig{}, err
	}

	if _, err = model.SnapshotPolicy(); err != nil {
		return BuilderConfig{}, err
	}

	return model, nil
}
