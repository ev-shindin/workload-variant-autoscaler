package utils

import (
	"os"
	"testing"
	"time"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	infernoConfig "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAddServerInfoToSystemData_MinReplicasFromSpec(t *testing.T) {
	minReplicas0 := int32(0)
	minReplicas2 := int32(2)
	minReplicas5 := int32(5)

	tests := []struct {
		name                string
		minReplicasInSpec   *int32
		envVarScaleToZero   string
		expectedMinReplicas int
	}{
		{
			name:                "minReplicas=0 without env var defaults to 1",
			minReplicasInSpec:   &minReplicas0,
			envVarScaleToZero:   "",
			expectedMinReplicas: 1,
		},
		{
			name:                "minReplicas=0 with WVA_SCALE_TO_ZERO=true allows 0",
			minReplicasInSpec:   &minReplicas0,
			envVarScaleToZero:   "true",
			expectedMinReplicas: 0,
		},
		{
			name:                "uses minReplicas from spec when set to 2",
			minReplicasInSpec:   &minReplicas2,
			envVarScaleToZero:   "",
			expectedMinReplicas: 2,
		},
		{
			name:                "uses minReplicas from spec when set to 5",
			minReplicasInSpec:   &minReplicas5,
			envVarScaleToZero:   "",
			expectedMinReplicas: 5,
		},
		{
			name:                "nil minReplicas with WVA_SCALE_TO_ZERO=true defaults to 0",
			minReplicasInSpec:   nil,
			envVarScaleToZero:   "true",
			expectedMinReplicas: 0,
		},
		{
			name:                "nil minReplicas without env var defaults to 1",
			minReplicasInSpec:   nil,
			envVarScaleToZero:   "",
			expectedMinReplicas: 1,
		},
		{
			name:                "positive minReplicas overrides WVA_SCALE_TO_ZERO",
			minReplicasInSpec:   &minReplicas2,
			envVarScaleToZero:   "true",
			expectedMinReplicas: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment variable if needed
			if tt.envVarScaleToZero != "" {
				if err := os.Setenv("WVA_SCALE_TO_ZERO", tt.envVarScaleToZero); err != nil {
					t.Fatalf("Failed to set environment variable: %v", err)
				}
				defer func() {
					if err := os.Unsetenv("WVA_SCALE_TO_ZERO"); err != nil {
						t.Errorf("Failed to unset environment variable: %v", err)
					}
				}()
			}

			// Create test VariantAutoscaling
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: "test-ns",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:     "test-model",
					Accelerator: "test-gpu",
					MinReplicas: tt.minReplicasInSpec,
					VariantCost: "10",
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						MaxBatchSize: 4,
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms: map[string]string{
								"alpha": "5.0",
								"beta":  "2.0",
							},
							PrefillParms: map[string]string{
								"gamma": "10.0",
								"delta": "1.5",
							},
						},
					},
				},
				Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
					CurrentAlloc: llmdVariantAutoscalingV1alpha1.Allocation{
						NumReplicas: 1,
					},
				},
			}

			// Create system data
			sd := &infernoConfig.SystemData{
				Spec: infernoConfig.SystemSpec{
					Servers: infernoConfig.ServerData{
						Spec: []infernoConfig.ServerSpec{},
					},
				},
			}

			// Add server info
			err := AddServerInfoToSystemData(sd, va, "test-class", 60.0, 100.0, 200.0, 50.0, 100.0)
			if err != nil {
				t.Fatalf("AddServerInfoToSystemData failed: %v", err)
			}

			// Verify MinNumReplicas
			if len(sd.Spec.Servers.Spec) != 1 {
				t.Fatalf("Expected 1 server spec, got %d", len(sd.Spec.Servers.Spec))
			}

			if sd.Spec.Servers.Spec[0].MinNumReplicas != tt.expectedMinReplicas {
				t.Errorf("Expected MinNumReplicas=%d, got %d", tt.expectedMinReplicas, sd.Spec.Servers.Spec[0].MinNumReplicas)
			}
		})
	}
}

func TestAddServerInfoToSystemData_MaxReplicasFromSpec(t *testing.T) {
	maxReplicas0 := int32(0)
	maxReplicas3 := int32(3)
	maxReplicas10 := int32(10)

	tests := []struct {
		name                string
		maxReplicasInSpec   *int32
		expectedMaxReplicas *int
	}{
		{
			name:                "uses maxReplicas from spec when set to 0",
			maxReplicasInSpec:   &maxReplicas0,
			expectedMaxReplicas: func() *int { v := 0; return &v }(),
		},
		{
			name:                "uses maxReplicas from spec when set to 3",
			maxReplicasInSpec:   &maxReplicas3,
			expectedMaxReplicas: func() *int { v := 3; return &v }(),
		},
		{
			name:                "uses maxReplicas from spec when set to 10",
			maxReplicasInSpec:   &maxReplicas10,
			expectedMaxReplicas: func() *int { v := 10; return &v }(),
		},
		{
			name:                "nil maxReplicas means unlimited",
			maxReplicasInSpec:   nil,
			expectedMaxReplicas: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test VariantAutoscaling
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: "test-ns",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:     "test-model",
					Accelerator: "test-gpu",
					MaxReplicas: tt.maxReplicasInSpec,
					VariantCost: "10",
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						MaxBatchSize: 4,
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms: map[string]string{
								"alpha": "5.0",
								"beta":  "2.0",
							},
							PrefillParms: map[string]string{
								"gamma": "10.0",
								"delta": "1.5",
							},
						},
					},
				},
				Status: llmdVariantAutoscalingV1alpha1.VariantAutoscalingStatus{
					CurrentAlloc: llmdVariantAutoscalingV1alpha1.Allocation{
						NumReplicas: 1,
					},
				},
			}

			// Create system data
			sd := &infernoConfig.SystemData{
				Spec: infernoConfig.SystemSpec{
					Servers: infernoConfig.ServerData{
						Spec: []infernoConfig.ServerSpec{},
					},
				},
			}

			// Add server info
			err := AddServerInfoToSystemData(sd, va, "test-class", 60.0, 100.0, 200.0, 50.0, 100.0)
			if err != nil {
				t.Fatalf("AddServerInfoToSystemData failed: %v", err)
			}

			// Verify MaxNumReplicas
			if len(sd.Spec.Servers.Spec) != 1 {
				t.Fatalf("Expected 1 server spec, got %d", len(sd.Spec.Servers.Spec))
			}

			serverSpec := sd.Spec.Servers.Spec[0]
			if tt.expectedMaxReplicas == nil {
				if serverSpec.MaxNumReplicas != nil {
					t.Errorf("Expected MaxNumReplicas=nil (unlimited), got %v", *serverSpec.MaxNumReplicas)
				}
			} else {
				if serverSpec.MaxNumReplicas == nil {
					t.Errorf("Expected MaxNumReplicas=%d, got nil", *tt.expectedMaxReplicas)
				} else if *serverSpec.MaxNumReplicas != *tt.expectedMaxReplicas {
					t.Errorf("Expected MaxNumReplicas=%d, got %d", *tt.expectedMaxReplicas, *serverSpec.MaxNumReplicas)
				}
			}
		})
	}
}

func TestCreateOptimizedAlloc_BoundsEnforcement(t *testing.T) {
	minReplicas2 := int32(2)
	minReplicas5 := int32(5)
	maxReplicas3 := int32(3)
	maxReplicas10 := int32(10)

	tests := []struct {
		name              string
		minReplicas       *int32
		maxReplicas       *int32
		optimizerReplicas int
		expectedReplicas  int32
		expectWarning     bool
		description       string
	}{
		{
			name:              "optimizer respects bounds - no clamping needed",
			minReplicas:       &minReplicas2,
			maxReplicas:       &maxReplicas10,
			optimizerReplicas: 5,
			expectedReplicas:  5,
			expectWarning:     false,
			description:       "Optimizer value within bounds",
		},
		{
			name:              "optimizer below minReplicas - clamp to min",
			minReplicas:       &minReplicas5,
			maxReplicas:       &maxReplicas10,
			optimizerReplicas: 2,
			expectedReplicas:  5,
			expectWarning:     true,
			description:       "Should clamp to minReplicas=5",
		},
		{
			name:              "optimizer above maxReplicas - clamp to max",
			minReplicas:       &minReplicas2,
			maxReplicas:       &maxReplicas3,
			optimizerReplicas: 10,
			expectedReplicas:  3,
			expectWarning:     true,
			description:       "Should clamp to maxReplicas=3",
		},
		{
			name:              "no bounds set - no clamping",
			minReplicas:       nil,
			maxReplicas:       nil,
			optimizerReplicas: 100,
			expectedReplicas:  100,
			expectWarning:     false,
			description:       "No bounds, should use optimizer value",
		},
		{
			name:              "only minReplicas set - enforce min only",
			minReplicas:       &minReplicas5,
			maxReplicas:       nil,
			optimizerReplicas: 2,
			expectedReplicas:  5,
			expectWarning:     true,
			description:       "Should enforce minReplicas only",
		},
		{
			name:              "only maxReplicas set - enforce max only",
			minReplicas:       nil,
			maxReplicas:       &maxReplicas3,
			optimizerReplicas: 10,
			expectedReplicas:  3,
			expectWarning:     true,
			description:       "Should enforce maxReplicas only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test VA
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: "test-ns",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					MinReplicas: tt.minReplicas,
					MaxReplicas: tt.maxReplicas,
				},
			}

			// Create allocation solution
			allocationSolution := &infernoConfig.AllocationSolution{
				Spec: map[string]infernoConfig.AllocationData{
					"test-va:test-ns": {
						NumReplicas: tt.optimizerReplicas,
						Accelerator: "test-gpu",
					},
				},
			}

			// Call CreateOptimizedAlloc
			optimizedAlloc, err := CreateOptimizedAlloc("test-va", "test-ns", allocationSolution, va)
			if err != nil {
				t.Fatalf("CreateOptimizedAlloc failed: %v", err)
			}

			// Verify bounds enforcement
			if optimizedAlloc.NumReplicas != tt.expectedReplicas {
				t.Errorf("Expected %d replicas (clamped), got %d", tt.expectedReplicas, optimizedAlloc.NumReplicas)
			}

			// Verify LastRunTime is set
			if optimizedAlloc.LastRunTime.IsZero() {
				t.Error("Expected LastRunTime to be set")
			}
		})
	}
}

func TestCreateOptimizedAlloc_ServerNotFound(t *testing.T) {
	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-va",
			Namespace: "test-ns",
		},
	}

	allocationSolution := &infernoConfig.AllocationSolution{
		Spec: map[string]infernoConfig.AllocationData{
			"other-va:other-ns": {
				NumReplicas: 5,
			},
		},
	}

	_, err := CreateOptimizedAlloc("test-va", "test-ns", allocationSolution, va)
	if err == nil {
		t.Error("Expected error for server not found, got nil")
	}
}

func TestCreateOptimizedAlloc_LastRunTimeSet(t *testing.T) {
	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-va",
			Namespace: "test-ns",
		},
	}

	allocationSolution := &infernoConfig.AllocationSolution{
		Spec: map[string]infernoConfig.AllocationData{
			"test-va:test-ns": {
				NumReplicas: 3,
				Accelerator: "test-gpu",
			},
		},
	}

	before := time.Now()
	optimizedAlloc, err := CreateOptimizedAlloc("test-va", "test-ns", allocationSolution, va)
	after := time.Now()

	if err != nil {
		t.Fatalf("CreateOptimizedAlloc failed: %v", err)
	}

	// Verify LastRunTime is within expected range
	lastRunTime := optimizedAlloc.LastRunTime.Time
	if lastRunTime.Before(before) || lastRunTime.After(after) {
		t.Errorf("LastRunTime %v is not within expected range [%v, %v]", lastRunTime, before, after)
	}
}
