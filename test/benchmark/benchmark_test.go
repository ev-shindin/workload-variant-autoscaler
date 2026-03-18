package benchmark

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// BenchmarkResults holds the collected metrics from the benchmark scenario.
type BenchmarkResults struct {
	ScaleUpTimeSec     float64 `json:"scaleUpTimeSec"`
	ScaleDownTimeSec   float64 `json:"scaleDownTimeSec"`
	MaxReplicas        int32   `json:"maxReplicas"`
	AvgKVCacheUsage    float64 `json:"avgKVCacheUsage"`
	AvgQueueDepth      float64 `json:"avgQueueDepth"`
	ReplicaOscillation float64 `json:"replicaOscillation"`
	TotalDurationSec   float64 `json:"totalDurationSec"`
	GrafanaSnapshotURL string  `json:"grafanaSnapshotUrl,omitempty"`
}

// Benchmark load generation constants
const (
	benchLoadWorkers       = 4
	benchRequestsPerWorker = 1100
	benchMaxTokens         = 400
)

var _ = Describe("Scale-Up Latency Benchmark", Label("benchmark"), Ordered, func() {
	var (
		poolName         = "bench-pool"
		modelServiceName = "bench-ms"
		vaName           = "bench-va"
		hpaName          = "bench-hpa"
		deploymentName   string
		serviceName      string
		jobBaseName      string

		results       BenchmarkResults
		scenarioStart time.Time
	)

	BeforeAll(func() {
		deploymentName = modelServiceName + "-decode"
		serviceName = modelServiceName + "-service"
		jobBaseName = "bench-ms"

		By("Creating model service deployment")
		err := fixtures.EnsureModelService(ctx, k8sClient, benchCfg.LLMDNamespace, modelServiceName, poolName, benchCfg.ModelID, benchCfg.UseSimulator, benchCfg.MaxNumSeqs)
		Expect(err).NotTo(HaveOccurred(), "Failed to create model service")

		DeferCleanup(func() {
			_ = k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
		})

		By("Creating service to expose model server")
		err = fixtures.EnsureService(ctx, k8sClient, benchCfg.LLMDNamespace, modelServiceName, deploymentName, 8000)
		Expect(err).NotTo(HaveOccurred(), "Failed to create service")

		DeferCleanup(func() {
			_ = k8sClient.CoreV1().Services(benchCfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		})

		By("Creating ServiceMonitor for metrics scraping")
		err = fixtures.EnsureServiceMonitor(ctx, crClient, benchCfg.MonitoringNS, benchCfg.LLMDNamespace, modelServiceName, deploymentName)
		Expect(err).NotTo(HaveOccurred(), "Failed to create ServiceMonitor")

		DeferCleanup(func() {
			serviceMonitorName := modelServiceName + "-monitor"
			_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceMonitorName,
					Namespace: benchCfg.MonitoringNS,
				},
			})
		})

		By("Creating VariantAutoscaling resource")
		err = fixtures.EnsureVariantAutoscalingWithDefaults(
			ctx, crClient, benchCfg.LLMDNamespace, vaName,
			deploymentName, benchCfg.ModelID, benchCfg.AcceleratorType,
			benchCfg.ControllerInstance,
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to create VariantAutoscaling")

		DeferCleanup(func() {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      vaName,
					Namespace: benchCfg.LLMDNamespace,
				},
			}
			_ = crClient.Delete(ctx, va)
		})

		By("Creating HPA for the deployment")
		err = fixtures.EnsureHPA(ctx, k8sClient, benchCfg.LLMDNamespace, hpaName, deploymentName, vaName, 1, 10)
		Expect(err).NotTo(HaveOccurred(), "Failed to create HPA")

		DeferCleanup(func() {
			hpaNameFull := hpaName + "-hpa"
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Delete(ctx, hpaNameFull, metav1.DeleteOptions{})
		})

		By("Waiting for deployment to be ready at 1 replica")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1), "Deployment should have at least 1 ready replica")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("Waiting for VA to stabilize")
		Eventually(func(g Gomega) {
			currentVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: benchCfg.LLMDNamespace,
				Name:      vaName,
			}, currentVA)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(currentVA.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil(), "NumReplicas should be set")
			g.Expect(*currentVA.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 1), "VA should have optimized >= 1")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("Verifying external metrics API serves wva_desired_replicas")
		Eventually(func(g Gomega) {
			result, err := k8sClient.RESTClient().
				Get().
				AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + benchCfg.LLMDNamespace + "/wva_desired_replicas").
				DoRaw(ctx)
			g.Expect(err).NotTo(HaveOccurred(), "External metrics API should be accessible")
			g.Expect(string(result)).To(ContainSubstring("wva_desired_replicas"), "Metric should be available")
			g.Expect(string(result)).To(ContainSubstring(vaName), "Metric should reference the benchmark VA")
			GinkgoWriter.Printf("External metrics API confirmed: wva_desired_replicas available for %s\n", vaName)
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("Waiting for Prometheus to scrape simulator metrics")
		Eventually(func(g Gomega) {
			_, err := promClient.QueryWithRetry(ctx, `vllm:kv_cache_usage_perc`)
			g.Expect(err).NotTo(HaveOccurred(), "Prometheus should have KV cache metrics from simulator")
			GinkgoWriter.Println("Prometheus confirmed: vllm:kv_cache_usage_perc is available")
		}, 5*time.Minute, 15*time.Second).Should(Succeed())

		scenarioStart = time.Now()
		GinkgoWriter.Println("BeforeAll completed — metrics pipeline verified, benchmark scenario starting")
	})

	AfterAll(func() {
		results.TotalDurationSec = time.Since(scenarioStart).Seconds()

		if grafanaClient != nil && benchCfg.GrafanaEnabled {
			By("Capturing Grafana snapshot of benchmark dashboard")
			snapResult, snapErr := grafanaClient.CreateSnapshot(scenarioStart)
			if snapErr != nil {
				GinkgoWriter.Printf("Warning: failed to create Grafana snapshot: %v\n", snapErr)
			} else {
				results.GrafanaSnapshotURL = snapResult.URL
				GinkgoWriter.Printf("Grafana snapshot: %s\n", snapResult.URL)

				if benchCfg.GrafanaSnapshotFile != "" {
					if writeErr := os.WriteFile(benchCfg.GrafanaSnapshotFile, []byte(snapResult.URL+"\n"), 0644); writeErr != nil {
						GinkgoWriter.Printf("Warning: failed to write snapshot URL file: %v\n", writeErr)
					}
				}

				// Export full snapshot JSON for offline re-import
				if benchCfg.GrafanaSnapshotJSONFile != "" {
					By("Exporting Grafana snapshot JSON")
					if exportErr := grafanaClient.ExportSnapshotJSON(snapResult.Key, benchCfg.GrafanaSnapshotJSONFile); exportErr != nil {
						GinkgoWriter.Printf("Warning: failed to export snapshot JSON: %v\n", exportErr)
					} else {
						GinkgoWriter.Printf("Snapshot JSON exported to %s\n", benchCfg.GrafanaSnapshotJSONFile)
					}
				}
			}

			// Render all panels to PNG
			if benchCfg.GrafanaPanelDir != "" {
				By("Rendering dashboard panels to PNG")
				panelFiles, renderErr := grafanaClient.RenderAllPanels(scenarioStart, time.Now(), benchCfg.GrafanaPanelDir)
				if renderErr != nil {
					GinkgoWriter.Printf("Warning: panel rendering failed: %v\n", renderErr)
				} else {
					GinkgoWriter.Printf("Rendered %d panels to %s\n", len(panelFiles), benchCfg.GrafanaPanelDir)
				}
			}
		}

		By("Writing benchmark results to file")
		data, err := json.MarshalIndent(results, "", "  ")
		Expect(err).NotTo(HaveOccurred(), "Failed to marshal results")

		err = os.WriteFile(benchCfg.BenchmarkResultsFile, data, 0644)
		Expect(err).NotTo(HaveOccurred(), "Failed to write results file")

		GinkgoWriter.Printf("Benchmark results written to %s\n", benchCfg.BenchmarkResultsFile)
		GinkgoWriter.Printf("Results: %s\n", string(data))
	})

	It("Phase 1: Baseline — verify stable at 1 replica", func() {
		baselineDuration := time.Duration(benchCfg.BaselineDurationSec) * time.Second
		GinkgoWriter.Printf("Running baseline phase for %v\n", baselineDuration)

		deadline := time.Now().Add(baselineDuration)
		for time.Now().Before(deadline) {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			replicas := deployment.Status.ReadyReplicas
			GinkgoWriter.Printf("Baseline: ready replicas = %d\n", replicas)
			Expect(replicas).To(BeNumerically("<=", 1), "Should be stable at 1 replica during baseline")

			time.Sleep(10 * time.Second)
		}

		GinkgoWriter.Println("Baseline phase complete — stable at 1 replica")
	})

	It("Phase 2: Spike — launch load and measure scale-up time", func() {
		spikeDuration := time.Duration(benchCfg.SpikeDurationSec) * time.Second
		GinkgoWriter.Printf("Running spike phase for %v\n", spikeDuration)

		spikeStart := time.Now()
		targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:8000/v1/completions", serviceName, benchCfg.LLMDNamespace)

		By("Cleaning up any existing load jobs")
		fixtures.DeleteParallelLoadJobs(ctx, k8sClient, jobBaseName, benchCfg.LLMDNamespace, benchLoadWorkers)
		time.Sleep(2 * time.Second)

		By("Waiting for service endpoints to exist")
		Eventually(func(g Gomega) {
			endpoints, err := k8sClient.CoreV1().Endpoints(benchCfg.LLMDNamespace).Get(ctx, serviceName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(endpoints.Subsets).NotTo(BeEmpty())
			readyCount := 0
			for _, subset := range endpoints.Subsets {
				readyCount += len(subset.Addresses)
			}
			GinkgoWriter.Printf("Service %s has %d ready endpoints\n", serviceName, readyCount)
			g.Expect(readyCount).To(BeNumerically(">", 0))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("Launching parallel load generation jobs")
		loadCfg := fixtures.LoadConfig{
			Strategy:     benchCfg.LoadStrategy,
			NumPrompts:   benchRequestsPerWorker,
			InputTokens:  benchCfg.InputTokens,
			OutputTokens: benchMaxTokens,
			ModelID:      benchCfg.ModelID,
		}
		err := fixtures.EnsureParallelLoadJobs(ctx, k8sClient, jobBaseName, benchCfg.LLMDNamespace, targetURL, benchLoadWorkers, loadCfg)
		Expect(err).NotTo(HaveOccurred(), "Failed to create load generation jobs")

		DeferCleanup(func() {
			fixtures.DeleteParallelLoadJobs(ctx, k8sClient, jobBaseName, benchCfg.LLMDNamespace, benchLoadWorkers)
		})

		By("Waiting for load job pods to be running")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("experiment=%s", jobBaseName),
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(len(podList.Items)).To(BeNumerically(">=", benchLoadWorkers), "All job pods should exist")

			runningCount := 0
			for _, pod := range podList.Items {
				if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodSucceeded {
					runningCount++
				}
			}
			g.Expect(runningCount).To(BeNumerically(">=", benchLoadWorkers),
				fmt.Sprintf("At least %d job pods should be running, got %d", benchLoadWorkers, runningCount))
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
		GinkgoWriter.Printf("All %d load generation jobs are running\n", benchLoadWorkers)

		By("Waiting for load generation to ramp up (30 seconds)")
		time.Sleep(30 * time.Second)
		GinkgoWriter.Println("Load ramp-up complete, monitoring VA for scale-up")

		// Phase 2a: Monitor VA status for scale-up intent (matches e2e flow)
		By("Monitoring VariantAutoscaling for scale-up recommendation")
		vaScaleUpDetected := false
		vaCheckStart := time.Now()
		vaAttempt := 0

		Eventually(func(g Gomega) {
			vaAttempt++
			elapsed := time.Since(vaCheckStart)

			currentVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Namespace: benchCfg.LLMDNamespace, Name: vaName}, currentVA)
			g.Expect(err).NotTo(HaveOccurred())

			var optimized int32
			if currentVA.Status.DesiredOptimizedAlloc.NumReplicas != nil {
				optimized = *currentVA.Status.DesiredOptimizedAlloc.NumReplicas
			}
			GinkgoWriter.Printf("VA check #%d (%v elapsed): optimized=%d\n",
				vaAttempt, elapsed.Round(time.Second), optimized)

			// Dump diagnostics every 30s
			if vaAttempt%3 == 1 {
				// HPA status
				hpaNameFull := hpaName + "-hpa"
				hpa, hpaErr := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Get(ctx, hpaNameFull, metav1.GetOptions{})
				if hpaErr == nil {
					GinkgoWriter.Printf("  HPA: desired=%d, current=%d\n", hpa.Status.DesiredReplicas, hpa.Status.CurrentReplicas)
					for _, cond := range hpa.Status.Conditions {
						GinkgoWriter.Printf("  HPA condition: %s=%s (%s)\n", cond.Type, cond.Status, cond.Message)
					}
					for _, metric := range hpa.Status.CurrentMetrics {
						if metric.External != nil {
							GinkgoWriter.Printf("  HPA metric: %s = %s\n", metric.External.Metric.Name, metric.External.Current.Value.String())
						}
					}
				}

				// Load pod status
				loadPods, podErr := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("experiment=%s", jobBaseName),
				})
				if podErr == nil {
					for _, pod := range loadPods.Items {
						GinkgoWriter.Printf("  Load pod %s: phase=%s\n", pod.Name, pod.Status.Phase)
					}
				}

				// Prometheus metrics
				if promClient != nil {
					kvVal, kvErr := promClient.QueryWithRetry(ctx, `avg(vllm:kv_cache_usage_perc)`)
					qdVal, qdErr := promClient.QueryWithRetry(ctx, `avg(vllm:num_requests_waiting)`)
					GinkgoWriter.Printf("  Prometheus: kv_cache=%v (err=%v), queue_depth=%v (err=%v)\n", kvVal, kvErr, qdVal, qdErr)
				}
			}

			if optimized > 1 {
				vaScaleUpDetected = true
			}
			g.Expect(optimized).To(BeNumerically(">", 1),
				fmt.Sprintf("VA should recommend >1 replicas under load (current: %d)", optimized))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		if vaScaleUpDetected {
			results.ScaleUpTimeSec = time.Since(spikeStart).Seconds()
			GinkgoWriter.Printf("VA scale-up detected at %.1fs\n", results.ScaleUpTimeSec)
		}

		// Phase 2b: Monitor HPA for scale-up (matches e2e flow)
		By("Monitoring HPA for scale-up")
		Eventually(func(g Gomega) {
			hpaNameFull := hpaName + "-hpa"
			hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Get(ctx, hpaNameFull, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("HPA: desired=%d, current=%d\n", hpa.Status.DesiredReplicas, hpa.Status.CurrentReplicas)
			g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">", 1),
				fmt.Sprintf("HPA should desire >1 replicas (current: %d)", hpa.Status.DesiredReplicas))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		// Phase 2c: Monitor deployment for actual scale-up (matches e2e flow)
		By("Monitoring deployment for actual scale-up")
		var maxReplicas int32 = 1
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			replicas := *deployment.Spec.Replicas
			readyReplicas := deployment.Status.ReadyReplicas
			GinkgoWriter.Printf("Deployment: spec=%d, ready=%d\n", replicas, readyReplicas)
			if replicas > maxReplicas {
				maxReplicas = replicas
			}
			g.Expect(deployment.Status.Replicas).To(BeNumerically(">", 1),
				fmt.Sprintf("Deployment should have >1 replicas under load (current: %d)", deployment.Status.Replicas))
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		results.MaxReplicas = maxReplicas
		GinkgoWriter.Printf("Scale-up complete: maxReplicas=%d, scaleUpTime=%.1fs\n", maxReplicas, results.ScaleUpTimeSec)
	})

	It("Phase 3: Sustained — collect stability metrics", func() {
		sustainedDuration := time.Duration(benchCfg.SustainedDurationSec) * time.Second
		GinkgoWriter.Printf("Running sustained phase for %v\n", sustainedDuration)

		sustainedStart := time.Now()

		// Collect replica samples for oscillation (stddev)
		var replicaSamples []float64
		deadline := time.Now().Add(sustainedDuration)

		for time.Now().Before(deadline) {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			replicas := float64(*deployment.Spec.Replicas)
			replicaSamples = append(replicaSamples, replicas)

			if replicas > float64(results.MaxReplicas) {
				results.MaxReplicas = int32(replicas)
			}

			GinkgoWriter.Printf("Sustained: replicas=%.0f\n", replicas)
			time.Sleep(15 * time.Second)
		}

		// Compute replica oscillation (stddev)
		if len(replicaSamples) > 1 {
			results.ReplicaOscillation = stddev(replicaSamples)
		}

		// Query Prometheus for average KV cache usage during sustained phase
		sustainedEnd := time.Now()
		kvAvg, err := QueryRangeAvg(
			promClient.API(),
			`avg(vllm:kv_cache_usage_perc)`,
			sustainedStart, sustainedEnd,
			30*time.Second,
		)
		if err != nil {
			GinkgoWriter.Printf("Warning: failed to query KV cache avg: %v\n", err)
		} else {
			results.AvgKVCacheUsage = kvAvg
		}

		// Query Prometheus for average queue depth during sustained phase
		qdAvg, err := QueryRangeAvg(
			promClient.API(),
			`avg(vllm:num_requests_waiting)`,
			sustainedStart, sustainedEnd,
			30*time.Second,
		)
		if err != nil {
			GinkgoWriter.Printf("Warning: failed to query queue depth avg: %v\n", err)
		} else {
			results.AvgQueueDepth = qdAvg
		}

		GinkgoWriter.Printf("Sustained phase complete: oscillation=%.2f, kvCache=%.3f, queueDepth=%.2f\n",
			results.ReplicaOscillation, results.AvgKVCacheUsage, results.AvgQueueDepth)
	})

	It("Phase 4: Cooldown — delete load and measure scale-down time", func() {
		cooldownDuration := time.Duration(benchCfg.CooldownDurationSec) * time.Second
		GinkgoWriter.Printf("Running cooldown phase for %v\n", cooldownDuration)

		By("Deleting load generation jobs")
		fixtures.DeleteParallelLoadJobs(ctx, k8sClient, jobBaseName, benchCfg.LLMDNamespace, benchLoadWorkers)

		cooldownStart := time.Now()
		scaleDownDetected := false
		deadline := time.Now().Add(cooldownDuration)

		for time.Now().Before(deadline) {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, deploymentName, metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					break
				}
				Expect(err).NotTo(HaveOccurred())
			}

			replicas := *deployment.Spec.Replicas
			GinkgoWriter.Printf("Cooldown: spec replicas=%d (elapsed %v)\n",
				replicas, time.Since(cooldownStart).Round(time.Second))

			if replicas <= 1 && !scaleDownDetected {
				results.ScaleDownTimeSec = time.Since(cooldownStart).Seconds()
				scaleDownDetected = true
				GinkgoWriter.Printf("Scale-down detected at %.1fs\n", results.ScaleDownTimeSec)
				break
			}

			time.Sleep(15 * time.Second)
		}

		if !scaleDownDetected {
			GinkgoWriter.Println("WARNING: Scale-down was NOT detected during cooldown phase")
			results.ScaleDownTimeSec = -1
		}
	})
})

// stddev computes the standard deviation of a float64 slice.
func stddev(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))

	var variance float64
	for _, v := range values {
		diff := v - mean
		variance += diff * diff
	}
	variance /= float64(len(values))

	return math.Sqrt(variance)
}
