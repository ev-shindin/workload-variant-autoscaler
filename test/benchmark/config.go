package benchmark

import (
	"os"
	"strconv"
	"strings"
)

// BenchmarkConfig holds configuration for benchmark tests loaded from environment variables.
// Fields are duplicated from e2e config to avoid importing the e2e package (which pulls in all tests).
type BenchmarkConfig struct {
	// Cluster info
	Environment string
	Kubeconfig  string

	// Namespaces
	WVANamespace  string
	LLMDNamespace string
	MonitoringNS  string

	// Infrastructure mode
	UseSimulator bool
	GPUType      string

	// Scaler backend
	ScalerBackend string
	KEDANamespace string

	// EPP configuration
	EPPMode          string
	PoolName         string
	EndpointSelector map[string]string
	EPPServiceName   string

	// Model configuration
	ModelID         string
	AcceleratorType string
	MaxNumSeqs      int

	// Load generation
	LoadStrategy string
	RequestRate  int
	NumPrompts   int
	InputTokens  int
	OutputTokens int

	// Controller isolation
	ControllerInstance string

	// Benchmark-specific
	BenchmarkResultsFile string

	// Grafana
	GrafanaEnabled          bool   // Deploy ephemeral Grafana and capture snapshot
	GrafanaSnapshotFile     string // Path to write snapshot URL
	GrafanaSnapshotJSONFile string // Path to export full snapshot JSON (re-importable)
	GrafanaPanelDir         string // Directory to write rendered panel PNGs

	// Phase durations (seconds, overridable via env for tuning)
	BaselineDurationSec  int
	SpikeDurationSec     int
	SustainedDurationSec int
	CooldownDurationSec  int
}

// LoadConfigFromEnv reads benchmark configuration from environment variables.
func LoadConfigFromEnv() BenchmarkConfig {
	env := getEnv("ENVIRONMENT", "kind-emulator")
	eppServiceDefault := "gaie-inference-scheduling-epp"
	if env == "kind-emulator" {
		eppServiceDefault = "gaie-sim-epp"
	}

	return BenchmarkConfig{
		Environment: env,
		Kubeconfig:  getEnv("KUBECONFIG", os.Getenv("HOME")+"/.kube/config"),

		WVANamespace:  getEnv("WVA_NAMESPACE", "workload-variant-autoscaler-system"),
		LLMDNamespace: getEnv("LLMD_NAMESPACE", "llm-d-sim"),
		MonitoringNS:  getEnv("MONITORING_NAMESPACE", "workload-variant-autoscaler-monitoring"),

		UseSimulator: getEnvBool("USE_SIMULATOR", true),
		GPUType:      getEnv("GPU_TYPE", "nvidia-mix"),

		ScalerBackend: getEnv("SCALER_BACKEND", "prometheus-adapter"),
		KEDANamespace: getEnv("KEDA_NAMESPACE", "keda-system"),

		EPPMode:          getEnv("EPP_MODE", "poolName"),
		PoolName:         getEnv("POOL_NAME", ""),
		EndpointSelector: parseEndpointSelector(getEnv("ENDPOINT_SELECTOR", "")),
		EPPServiceName:   getEnv("EPP_SERVICE_NAME", eppServiceDefault),

		ModelID:         getEnv("MODEL_ID", "unsloth/Meta-Llama-3.1-8B"),
		AcceleratorType: getEnv("ACCELERATOR_TYPE", "H100"),
		MaxNumSeqs:      getEnvInt("MAX_NUM_SEQS", 5),

		LoadStrategy: getEnv("LOAD_STRATEGY", "synthetic"),
		RequestRate:  getEnvInt("REQUEST_RATE", 8),
		NumPrompts:   getEnvInt("NUM_PROMPTS", 1000),
		InputTokens:  getEnvInt("INPUT_TOKENS", 100),
		OutputTokens: getEnvInt("OUTPUT_TOKENS", 50),

		ControllerInstance: getEnv("CONTROLLER_INSTANCE", ""),

		BenchmarkResultsFile: getEnv("BENCHMARK_RESULTS_FILE", "/tmp/benchmark-results.json"),

		GrafanaEnabled:          getEnvBool("BENCHMARK_GRAFANA_ENABLED", true),
		GrafanaSnapshotFile:     getEnv("BENCHMARK_GRAFANA_SNAPSHOT_FILE", "/tmp/benchmark-grafana-snapshot.txt"),
		GrafanaSnapshotJSONFile: getEnv("BENCHMARK_GRAFANA_SNAPSHOT_JSON", "/tmp/benchmark-grafana-snapshot.json"),
		GrafanaPanelDir:         getEnv("BENCHMARK_GRAFANA_PANEL_DIR", "/tmp/benchmark-panels"),

		BaselineDurationSec:  getEnvInt("BENCHMARK_BASELINE_DURATION", 120),
		SpikeDurationSec:     getEnvInt("BENCHMARK_SPIKE_DURATION", 300),
		SustainedDurationSec: getEnvInt("BENCHMARK_SUSTAINED_DURATION", 180),
		CooldownDurationSec:  getEnvInt("BENCHMARK_COOLDOWN_DURATION", 300),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err == nil {
			return parsed
		}
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		parsed, err := strconv.Atoi(value)
		if err == nil {
			return parsed
		}
	}
	return defaultValue
}

func parseEndpointSelector(value string) map[string]string {
	if value == "" {
		return nil
	}

	selector := make(map[string]string)
	pairs := strings.Split(value, ",")
	for _, pair := range pairs {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) == 2 {
			selector[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return selector
}
