// Scenario: scale-up-latency
//
// Measures how quickly WVA scales from min to target replicas under sudden load.
//
//	phases:
//	  - baseline (2m):  no load, establish baseline metrics, verify stable at 1 replica
//	  - spike (5m):     burst load via parallel curl workers targeting /v1/completions,
//	                    measure time to first scale-up (VA → HPA → deployment)
//	  - sustained (3m): load continues, collect replica stability (stddev),
//	                    avg KV cache usage and queue depth from Prometheus
//	  - cooldown (5m):  remove load, measure time to scale back down to 1 replica
//
//	metrics:
//	  - scale_up_time_seconds:   time from load start to VA recommending >1 replicas
//	  - scale_down_time_seconds: time from load removal to deployment returning to 1 replica
//	  - max_replicas_reached:    peak replica count during spike+sustained
//	  - replica_oscillation:     stddev of replica samples during sustained phase
//	  - avg_kv_cache_usage:      mean KV cache utilization during sustained phase
//	  - avg_queue_depth:         mean queue depth during sustained phase
package benchmark

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Benchmark load generation constants.
// Match e2e's maxSingleReplicaWorkers=1 — single-replica deployments need only 1 worker
// to avoid overwhelming the simulator's max-num-seqs queue and causing request failures.
const (
	benchLoadWorkers       = 1
	benchRequestsPerWorker = 1100
	benchMaxTokens         = 400
)

var _ = Describe("Scale-Up Latency Benchmark", Label("benchmark"), Ordered, func() {
	var (
		res = ScenarioResources{
			PoolName:       "bench-pool",
			ModelService:   "bench-ms",
			DeploymentName: "bench-ms-decode",
			ServiceName:    "bench-ms-service",
			VAName:         "bench-va",
			HPAName:        "bench-hpa",
			JobBaseName:    "bench-ms",
		}

		results       BenchmarkResults
		scenarioStart time.Time
	)

	BeforeAll(func() {
		SetupBenchmarkScenario(res)
		scenarioStart = time.Now()
		GinkgoWriter.Println("Benchmark scenario starting")
	})

	AfterAll(func() {
		CaptureResultsAndGrafana(&results, scenarioStart)
	})

	It("Phase 1: Baseline — verify stable at 1 replica", func() {
		baselineDuration := time.Duration(benchCfg.BaselineDurationSec) * time.Second
		GinkgoWriter.Printf("Running baseline phase for %v\n", baselineDuration)

		deadline := time.Now().Add(baselineDuration)
		for time.Now().Before(deadline) {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
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

		targetURL := GatewayTargetURL()
		GinkgoWriter.Printf("Load target URL (via Gateway): %s\n", targetURL)

		By("Cleaning up any existing load jobs")
		fixtures.DeleteParallelLoadJobs(ctx, k8sClient, res.JobBaseName, benchCfg.LLMDNamespace, benchLoadWorkers)
		time.Sleep(2 * time.Second)

		By("Waiting for model service endpoints to exist")
		Eventually(func(g Gomega) {
			endpoints, err := k8sClient.CoreV1().Endpoints(benchCfg.LLMDNamespace).Get(ctx, res.ServiceName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(endpoints.Subsets).NotTo(BeEmpty())
			readyCount := 0
			for _, subset := range endpoints.Subsets {
				readyCount += len(subset.Addresses)
			}
			GinkgoWriter.Printf("Service %s has %d ready endpoints\n", res.ServiceName, readyCount)
			g.Expect(readyCount).To(BeNumerically(">", 0))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("Running in-cluster connectivity probe via Gateway")
		gwHost := fmt.Sprintf("%s.%s.svc.cluster.local", benchCfg.GatewayServiceName, benchCfg.LLMDNamespace)
		probePodName := "bench-connectivity-probe"
		probeScript := fmt.Sprintf(`#!/bin/sh
echo "=== Gateway DNS Resolution ==="
nslookup %s 2>&1 || echo "nslookup failed (tool may not exist)"
echo ""
echo "=== Direct model service check ==="
HTTP_CODE=$(curl -s -o /dev/null -w "%%{http_code}" --max-time 10 "http://%s.%s.svc.cluster.local:8000/v1/completions" 2>/dev/null)
echo "Direct model service HTTP status: $HTTP_CODE"
echo ""
echo "=== Gateway POST (full stack path) ==="
curl -v --max-time 15 -X POST "%s" -H "Content-Type: application/json" -d '{"model":"%s","prompt":"hello","max_tokens":1}' 2>&1
echo ""
echo "=== Gateway HTTP status code ==="
HTTP_CODE=$(curl -s -o /dev/null -w "%%{http_code}" --max-time 15 -X POST "%s" -H "Content-Type: application/json" -d '{"model":"%s","prompt":"test","max_tokens":1}' 2>/dev/null)
echo "Gateway HTTP status code: $HTTP_CODE"
`, gwHost, res.ServiceName, benchCfg.LLMDNamespace, targetURL, benchCfg.ModelID, targetURL, benchCfg.ModelID)

		probePod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      probePodName,
				Namespace: benchCfg.LLMDNamespace,
			},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{
					{
						Name:    "probe",
						Image:   "quay.io/curl/curl:8.11.1",
						Command: []string{"/bin/sh", "-c"},
						Args:    []string{probeScript},
					},
				},
			},
		}
		_ = k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).Delete(ctx, probePodName, metav1.DeleteOptions{})
		time.Sleep(2 * time.Second)
		_, probeErr := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).Create(ctx, probePod, metav1.CreateOptions{})
		if probeErr != nil {
			GinkgoWriter.Printf("Warning: could not create connectivity probe pod: %v\n", probeErr)
		} else {
			Eventually(func(g Gomega) {
				p, err := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).Get(ctx, probePodName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(p.Status.Phase).To(SatisfyAny(Equal(corev1.PodSucceeded), Equal(corev1.PodFailed)),
					fmt.Sprintf("Probe pod phase: %s", p.Status.Phase))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			logReq := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).GetLogs(probePodName, &corev1.PodLogOptions{})
			logBytes, logErr := logReq.DoRaw(ctx)
			if logErr == nil {
				GinkgoWriter.Printf("=== CONNECTIVITY PROBE OUTPUT ===\n%s\n=== END PROBE ===\n", string(logBytes))
			}

			_ = k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).Delete(ctx, probePodName, metav1.DeleteOptions{})
		}

		By("Launching parallel load generation jobs")
		loadCfg := fixtures.LoadConfig{
			Strategy:     benchCfg.LoadStrategy,
			NumPrompts:   benchRequestsPerWorker,
			InputTokens:  benchCfg.InputTokens,
			OutputTokens: benchMaxTokens,
			ModelID:      benchCfg.ModelID,
		}
		err := fixtures.EnsureParallelLoadJobs(ctx, k8sClient, res.JobBaseName, benchCfg.LLMDNamespace, targetURL, benchLoadWorkers, loadCfg)
		Expect(err).NotTo(HaveOccurred(), "Failed to create load generation jobs")

		DeferCleanup(func() {
			fixtures.DeleteParallelLoadJobs(ctx, k8sClient, res.JobBaseName, benchCfg.LLMDNamespace, benchLoadWorkers)
		})

		By("Waiting for load job pods to be running")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("experiment=%s", res.JobBaseName),
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

		By("Monitoring VariantAutoscaling for scale-up recommendation")
		vaScaleUpDetected := false
		vaCheckStart := time.Now()
		vaAttempt := 0

		Eventually(func(g Gomega) {
			vaAttempt++
			elapsed := time.Since(vaCheckStart)

			currentVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Namespace: benchCfg.LLMDNamespace, Name: res.VAName}, currentVA)
			g.Expect(err).NotTo(HaveOccurred())

			var optimized int32
			if currentVA.Status.DesiredOptimizedAlloc.NumReplicas != nil {
				optimized = *currentVA.Status.DesiredOptimizedAlloc.NumReplicas
			}
			GinkgoWriter.Printf("VA check #%d (%v elapsed): optimized=%d\n",
				vaAttempt, elapsed.Round(time.Second), optimized)

			// Dump diagnostics every 30s
			if vaAttempt % 3 == 1 {
				hpaNameFull := res.HPAName + "-hpa"
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

				loadPods, podErr := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("experiment=%s", res.JobBaseName),
				})
				if podErr == nil {
					for _, pod := range loadPods.Items {
						GinkgoWriter.Printf("  Load pod %s: phase=%s\n", pod.Name, pod.Status.Phase)
						if pod.Status.Phase == corev1.PodFailed {
							tailLines := int64(50)
							logReq := k8sClient.CoreV1().Pods(benchCfg.LLMDNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: &tailLines})
							logBytes, logErr := logReq.DoRaw(ctx)
							if logErr == nil {
								GinkgoWriter.Printf("  === FAILED POD LOGS (%s) ===\n%s\n  === END LOGS ===\n", pod.Name, string(logBytes))
							}
						}
					}
				}

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

		By("Monitoring HPA for scale-up")
		Eventually(func(g Gomega) {
			hpaNameFull := res.HPAName + "-hpa"
			hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(benchCfg.LLMDNamespace).Get(ctx, hpaNameFull, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("HPA: desired=%d, current=%d\n", hpa.Status.DesiredReplicas, hpa.Status.CurrentReplicas)
			g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">", 1),
				fmt.Sprintf("HPA should desire >1 replicas (current: %d)", hpa.Status.DesiredReplicas))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("Monitoring deployment for actual scale-up")
		var maxReplicas int32 = 1
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
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

		var replicaSamples []float64
		deadline := time.Now().Add(sustainedDuration)

		for time.Now().Before(deadline) {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			replicas := float64(*deployment.Spec.Replicas)
			replicaSamples = append(replicaSamples, replicas)

			if replicas > float64(results.MaxReplicas) {
				results.MaxReplicas = int32(replicas)
			}

			GinkgoWriter.Printf("Sustained: replicas=%.0f\n", replicas)
			time.Sleep(15 * time.Second)
		}

		if len(replicaSamples) > 1 {
			results.ReplicaOscillation = stddev(replicaSamples)
		}

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
		fixtures.DeleteParallelLoadJobs(ctx, k8sClient, res.JobBaseName, benchCfg.LLMDNamespace, benchLoadWorkers)

		cooldownStart := time.Now()
		scaleDownDetected := false
		deadline := time.Now().Add(cooldownDuration)

		for time.Now().Before(deadline) {
			deployment, err := k8sClient.AppsV1().Deployments(benchCfg.LLMDNamespace).Get(ctx, res.DeploymentName, metav1.GetOptions{})
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
