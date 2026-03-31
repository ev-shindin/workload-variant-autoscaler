package saturation_v2

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

var _ = Describe("computeReplicaCapacityFallback", func() {
	var (
		analyzer *SaturationAnalyzer
		store    *CapacityKnowledgeStore
		cfg      *config.SaturationScalingConfig
	)

	BeforeEach(func() {
		store = NewCapacityKnowledgeStore()
		analyzer = NewSaturationAnalyzer(store)
		cfg = &config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.1,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.85,
			ScaleDownBoundary:    0.70,
		}
	})

	It("should return nil when capacity store has no record", func() {
		rm := interfaces.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.5,
			TotalKvCapacityTokens: 0, // no cache_config_info
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", 1)
		Expect(result).To(BeNil())
	})

	It("should return nil when capacity store record has zero effective capacity", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			EffectiveCapacity: 0,
			LearnedFrom:       "deployment",
		})

		rm := interfaces.ReplicaMetrics{
			PodName:      "pod-1",
			VariantName:  "variant-a",
			KvCacheUsage: 0.5,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", 1)
		Expect(result).To(BeNil())
	})

	It("should estimate demand from KvCacheUsage percentage when store has capacity", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := interfaces.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.6, // 60% usage
			TotalKvCapacityTokens: 0,   // no cache_config_info
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", 1)
		Expect(result).NotTo(BeNil())
		// demand = 0.6 * 10000 = 6000
		Expect(result.ReplicaDemand).To(Equal(int64(6000)))
		Expect(result.EffectiveCapacity).To(Equal(int64(10000)))
		Expect(result.IsSaturated).To(BeFalse()) // 6000 < 10000
	})

	It("should detect saturation when KvCacheUsage is high", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := interfaces.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          1.0, // 100% usage
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", 1)
		Expect(result).NotTo(BeNil())
		// demand = 1.0 * 10000 = 10000 >= 10000
		Expect(result.ReplicaDemand).To(Equal(int64(10000)))
		Expect(result.IsSaturated).To(BeTrue())
	})

	It("should add queue-based demand when avg input tokens available", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := interfaces.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.5, // 50% usage
			TotalKvCapacityTokens: 0,
			QueueLength:           3,
			AvgInputTokens:        500,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", 1)
		Expect(result).NotTo(BeNil())
		// demand = 0.5 * 10000 + 3 * 500 = 5000 + 1500 = 6500
		Expect(result.ReplicaDemand).To(Equal(int64(6500)))
		Expect(result.IsSaturated).To(BeFalse())
	})

	It("should not add queue demand when avg input tokens is zero", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := interfaces.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.3,
			TotalKvCapacityTokens: 0,
			QueueLength:           10,
			AvgInputTokens:        0, // no avg input tokens
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", 1)
		Expect(result).NotTo(BeNil())
		// demand = 0.3 * 10000 = 3000 (no queue contribution)
		Expect(result.ReplicaDemand).To(Equal(int64(3000)))
	})

	It("should populate all ReplicaCapacity fields correctly", func() {
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			EffectiveCapacity: 8000,
			LearnedFrom:       "deployment",
		})

		rm := interfaces.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			AcceleratorName:       "H100",
			KvCacheUsage:          0.4,
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", 1)
		Expect(result).NotTo(BeNil())
		Expect(result.PodName).To(Equal("pod-1"))
		Expect(result.VariantName).To(Equal("variant-a"))
		Expect(result.AcceleratorName).To(Equal("H100"))
		Expect(result.EffectiveCapacity).To(Equal(int64(8000)))
		Expect(result.MemoryBoundCapacity).To(Equal(int64(8000)))
		Expect(result.ComputeBoundCapacity).To(Equal(int64(8000)))
		Expect(result.TotalKvCapacityTokens).To(Equal(int64(8000))) // synthetic
		Expect(result.TokensInUse).To(Equal(int64(3200)))           // = demand
		Expect(result.ReplicaDemand).To(Equal(int64(3200)))
	})
})

var _ = Describe("Analyze with fallback (no cache_config_info)", func() {
	var (
		analyzer *SaturationAnalyzer
		store    *CapacityKnowledgeStore
		ctx      context.Context
	)

	BeforeEach(func() {
		store = NewCapacityKnowledgeStore()
		analyzer = NewSaturationAnalyzer(store)
		ctx = context.Background()
	})

	It("should produce a valid AnalyzerResult using fallback when cache_config_info is absent", func() {
		// Pre-populate store with deployment-derived capacity
		store.Update("test-ns", "test-model", "variant-a", CapacityRecord{
			AcceleratorName:   "H100",
			GpuCount:          1,
			EffectiveCapacity: 8192,
			LearnedFrom:       "deployment",
		})

		input := interfaces.AnalyzerInput{
			ModelID:   "test-model",
			Namespace: "test-ns",
			ReplicaMetrics: []interfaces.ReplicaMetrics{
				{
					PodName:               "pod-1",
					VariantName:           "variant-a",
					AcceleratorName:       "H100",
					ModelID:               "test-model",
					Namespace:             "test-ns",
					Cost:                  10.0,
					KvCacheUsage:          0.9,  // 90% — high load
					TotalKvCapacityTokens: 0,    // no cache_config_info
					TokensInUse:           0,    // unavailable without cache_config_info
					QueueLength:           3,
					AvgInputTokens:        100,
					AvgOutputTokens:       50,
				},
			},
			VariantStates: []interfaces.VariantReplicaState{
				{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
			Config: &config.SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.85,
				ScaleDownBoundary:    0.70,
			},
		}

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())
		Expect(result.VariantCapacities).To(HaveLen(1))

		vc := result.VariantCapacities[0]
		Expect(vc.VariantName).To(Equal("variant-a"))
		Expect(vc.PerReplicaCapacity).To(BeNumerically(">", 0))
		Expect(vc.TotalDemand).To(BeNumerically(">", 0))

		// With 90% KV usage + queue demand, should signal scale-up
		Expect(result.TotalDemand).To(BeNumerically(">", 0))
		Expect(result.RequiredCapacity).To(BeNumerically(">", 0))
	})

	It("should skip replicas with no store data and no cache_config_info", func() {
		// No store pre-population — fallback returns nil, replica skipped
		input := interfaces.AnalyzerInput{
			ModelID:   "test-model",
			Namespace: "test-ns",
			ReplicaMetrics: []interfaces.ReplicaMetrics{
				{
					PodName:               "pod-1",
					VariantName:           "variant-a",
					AcceleratorName:       "H100",
					ModelID:               "test-model",
					Namespace:             "test-ns",
					Cost:                  10.0,
					KvCacheUsage:          0.5,
					TotalKvCapacityTokens: 0,
				},
			},
			VariantStates: []interfaces.VariantReplicaState{
				{VariantName: "variant-a", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
			Config: &config.SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.85,
				ScaleDownBoundary:    0.70,
			},
		}

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())
		// Variant should still appear but with zero capacity (no data)
		// or be empty if all replicas skipped
	})
})

var _ = Describe("parseSaturationConfig ApplyDefaults before Validate", func() {
	// This tests the fix in configmap_helpers.go indirectly via the config package.
	// The actual parseSaturationConfig is in the controller package (requires envtest),
	// so we test the underlying behavior: ApplyDefaults then Validate should pass
	// for configs that rely on default values.

	It("should pass validation after ApplyDefaults for V2 config with omitted thresholds", func() {
		cfg := config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.1,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
			// ScaleUpThreshold and ScaleDownBoundary intentionally omitted (zero value)
		}

		// Without ApplyDefaults, Validate would fail because ScaleUpThreshold == 0
		cfg.ApplyDefaults()
		err := cfg.Validate()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.ScaleUpThreshold).To(BeNumerically(">", 0))
		Expect(cfg.ScaleDownBoundary).To(BeNumerically(">", 0))
	})

	It("should fail validation without ApplyDefaults for V2 config with omitted thresholds", func() {
		cfg := config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.1,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
			// ScaleUpThreshold and ScaleDownBoundary intentionally omitted
		}

		// Validate without defaults should fail
		err := cfg.Validate()
		Expect(err).To(HaveOccurred())
	})

	It("should preserve explicitly set values after ApplyDefaults", func() {
		cfg := config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.1,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.9,
			ScaleDownBoundary:    0.6,
			Priority:             2.0,
		}

		cfg.ApplyDefaults()
		err := cfg.Validate()
		Expect(err).NotTo(HaveOccurred())
		// Explicitly set values should not be overwritten
		Expect(cfg.ScaleUpThreshold).To(Equal(0.9))
		Expect(cfg.ScaleDownBoundary).To(Equal(0.6))
		Expect(cfg.Priority).To(Equal(2.0))
	})

	It("should apply default priority when omitted", func() {
		cfg := config.SaturationScalingConfig{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.1,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
		}

		cfg.ApplyDefaults()
		Expect(cfg.Priority).To(BeNumerically(">", 0))
	})
})
