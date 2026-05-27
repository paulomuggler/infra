package env

import (
	"os"
	"strconv"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

var environment = GetEnv("ENVIRONMENT", "prod")

func IsLocal() bool {
	return environment == "local"
}

func IsDevelopment() bool {
	return environment == "dev" || environment == "local"
}

func IsDebug() bool {
	return GetEnv("E2B_DEBUG", "false") == "true"
}

func GetEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if len(value) == 0 {
		return defaultValue
	}

	return value
}

func GetEnvAsInt(key string, defaultValue int) (int, error) {
	if v := os.Getenv(key); v != "" {
		value, err := strconv.Atoi(v)
		if err != nil {
			return defaultValue, err
		}

		return value, nil
	}

	return defaultValue, nil
}

// GetEnvAsDuration returns the env var parsed as a time.Duration, or the default
// if the var is unset or cannot be parsed.
func GetEnvAsDuration(key string, defaultValue time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return defaultValue
		}

		return d
	}

	return defaultValue
}

// GetEnvAsIntDefault returns the env var parsed as an int, or the default
// if the var is unset or cannot be parsed.
func GetEnvAsIntDefault(key string, defaultValue int) int {
	if v := os.Getenv(key); v != "" {
		value, err := strconv.Atoi(v)
		if err != nil {
			return defaultValue
		}

		return value
	}

	return defaultValue
}

func GetNodeID() string {
	return utils.RequiredEnv("NODE_ID", "Node ID of the instance node is required")
}

func LogsCollectorAddress() string {
	return os.Getenv("LOGS_COLLECTOR_ADDRESS")
}
