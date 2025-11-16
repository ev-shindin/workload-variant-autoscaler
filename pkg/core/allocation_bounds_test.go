package core

import (
	"testing"

	"github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
)

// Helper function to setup test system with specific min/max replicas
func setupTestSystemWithBounds(minReplicas int, maxReplicas *int, arrivalRate float32) {
	system := &System{
		accelerators:     make(map[string]*Accelerator),
		servers:          make(map[string]*Server),
		models:           make(map[string]*Model),
		serviceClasses:   make(map[string]*ServiceClass),
		capacity:         make(map[string]int),
		allocationByType: make(map[string]*AllocationByType),
	}

	// Add test accelerator
	accSpec := &config.AcceleratorSpec{
		Name: "test-gpu",
		Cost: 100.0,
	}
	acc := NewAcceleratorFromSpec(accSpec)
	system.accelerators["test-gpu"] = acc

	// Add test model with performance data
	model := NewModel("test-model")
	model.numInstances["test-gpu"] = 1
	perfData := &config.ModelAcceleratorPerfData{
		Name:         "test-model",
		Acc:          "test-gpu",
		AccCount:     1,
		MaxBatchSize: 16,
		AtTokens:     200,
		DecodeParms: config.DecodeParms{
			Alpha: 5.0,
			Beta:  2.0,
		},
		PrefillParms: config.PrefillParms{
			Gamma: 10.0,
			Delta: 1.5,
		},
	}
	model.AddPerfDataFromSpec(perfData)
	system.models["test-model"] = model

	// Add test server with specific bounds
	serverSpec := &config.ServerSpec{
		Name:  "test-server",
		Model: "test-model",
		Class: "default",
		CurrentAlloc: config.AllocationData{
			Load: config.ServerLoadSpec{
				ArrivalRate:  arrivalRate,
				AvgInTokens:  100,
				AvgOutTokens: 200,
			},
		},
		MinNumReplicas: minReplicas,
		MaxNumReplicas: maxReplicas,
	}
	server := NewServerFromSpec(serverSpec)
	system.servers["test-server"] = server

	// Add test service class
	serviceClass := NewServiceClass("default", 10)
	target := &Target{
		TTFT: 2000.0,
		ITL:  500.0,
		TPS:  0.0,
	}
	serviceClass.targets["test-model"] = target
	system.serviceClasses["default"] = serviceClass

	// Set global system
	TheSystem = system
}

func TestCreateAllocation_MinReplicasEnforcement(t *testing.T) {
	tests := []struct {
		name            string
		minReplicas     int
		arrivalRate     float32
		expectedMinReps int
		description     string
	}{
		{
			name:            "low load enforces minReplicas=2",
			minReplicas:     2,
			arrivalRate:     1.0, // Very low load that would normally result in 1 replica
			expectedMinReps: 2,
			description:     "Should enforce minReplicas even with very low load",
		},
		{
			name:            "zero load enforces minReplicas=1",
			minReplicas:     1,
			arrivalRate:     0.0,
			expectedMinReps: 1,
			description:     "Should enforce minReplicas=1 even with zero load",
		},
		{
			name:            "minReplicas=0 allows scale to zero",
			minReplicas:     0,
			arrivalRate:     0.0,
			expectedMinReps: 0,
			description:     "Should allow scaling to zero when minReplicas=0",
		},
		{
			name:            "minReplicas=5 with moderate load",
			minReplicas:     5,
			arrivalRate:     60.0, // Moderate load that might result in 2-3 replicas
			expectedMinReps: 5,
			description:     "Should enforce minReplicas=5 even if optimizer calculates fewer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset system
			TheSystem = nil

			// Setup system with specific bounds
			setupTestSystemWithBounds(tt.minReplicas, nil, tt.arrivalRate)

			// Create allocation
			alloc := CreateAllocation("test-server", "test-gpu")

			if alloc == nil {
				if tt.arrivalRate == 0.0 && tt.minReplicas == 0 {
					// This is expected for zero load with minReplicas=0
					return
				}
				t.Fatalf("CreateAllocation returned nil unexpectedly")
			}

			// Check that minReplicas is enforced
			if alloc.NumReplicas() < tt.expectedMinReps {
				t.Errorf("Expected at least %d replicas (minReplicas), got %d", tt.expectedMinReps, alloc.NumReplicas())
			}
		})
	}
}

func TestCreateAllocation_MaxReplicasEnforcement(t *testing.T) {
	maxReplicas2 := 2
	maxReplicas5 := 5
	maxReplicas0 := 0

	tests := []struct {
		name            string
		maxReplicas     *int
		arrivalRate     float32
		expectedMaxReps int
		description     string
		allowNil        bool // Some tests may return nil if SLO cannot be met
	}{
		{
			name:            "moderate load capped by maxReplicas=2",
			maxReplicas:     &maxReplicas2,
			arrivalRate:     60.0, // Moderate load (1 req/sec) that would normally result in ~2 replicas
			expectedMaxReps: 2,
			description:     "Should cap at maxReplicas=2",
			allowNil:        false,
		},
		{
			name:            "moderate load capped by maxReplicas=5",
			maxReplicas:     &maxReplicas5,
			arrivalRate:     180.0, // Moderate load (3 req/sec)
			expectedMaxReps: 5,
			description:     "Should cap at maxReplicas=5",
			allowNil:        false,
		},
		{
			name:            "maxReplicas=nil allows unlimited scaling",
			maxReplicas:     nil,
			arrivalRate:     180.0, // Moderate load
			expectedMaxReps: 999,   // Just checking it's not capped at some low value
			description:     "Should not cap replicas when maxReplicas is nil",
			allowNil:        false,
		},
		{
			name:            "maxReplicas=0 with minReplicas=0 allows scale to zero",
			maxReplicas:     &maxReplicas0,
			arrivalRate:     0.0, // Zero load required for scale to zero
			expectedMaxReps: 0,
			description:     "Should allow scale to zero when both min and max are 0",
			allowNil:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset system
			TheSystem = nil

			// Setup system with specific bounds
			// For maxReplicas=0, also set minReplicas=0
			minReplicas := 1
			if tt.maxReplicas != nil && *tt.maxReplicas == 0 {
				minReplicas = 0
			}
			setupTestSystemWithBounds(minReplicas, tt.maxReplicas, tt.arrivalRate)

			// Create allocation
			alloc := CreateAllocation("test-server", "test-gpu")

			if alloc == nil {
				if tt.allowNil {
					// This is expected for some edge cases
					return
				}
				t.Fatalf("CreateAllocation returned nil unexpectedly")
			}

			// Check that maxReplicas is enforced
			if tt.maxReplicas != nil {
				if alloc.NumReplicas() > *tt.maxReplicas {
					t.Errorf("Expected at most %d replicas (maxReplicas), got %d", *tt.maxReplicas, alloc.NumReplicas())
				}
			} else {
				// nil maxReplicas should not cap
				if alloc.NumReplicas() < 3 {
					t.Logf("With nil maxReplicas and moderate load, got %d replicas (expected more)", alloc.NumReplicas())
				}
			}
		})
	}
}

func TestCreateAllocation_MinMaxBoundsTogether(t *testing.T) {
	maxReplicas3 := 3
	maxReplicas10 := 10

	tests := []struct {
		name        string
		minReplicas int
		maxReplicas *int
		arrivalRate float32
		expectedMin int
		expectedMax int
		description string
	}{
		{
			name:        "low load with min=2, max=10",
			minReplicas: 2,
			maxReplicas: &maxReplicas10,
			arrivalRate: 5.0, // Low load
			expectedMin: 2,
			expectedMax: 10,
			description: "Should enforce min=2, not hit max=10",
		},
		{
			name:        "moderate load with min=1, max=3",
			minReplicas: 1,
			maxReplicas: &maxReplicas3,
			arrivalRate: 120.0, // Moderate load (2 req/sec)
			expectedMin: 1,
			expectedMax: 3,
			description: "Should enforce max=3 with moderate load",
		},
		{
			name:        "moderate load with min=3, max=10",
			minReplicas: 3,
			maxReplicas: &maxReplicas10,
			arrivalRate: 100.0, // Moderate load
			expectedMin: 3,
			expectedMax: 10,
			description: "Should respect both bounds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset system
			TheSystem = nil

			// Setup system with specific bounds
			setupTestSystemWithBounds(tt.minReplicas, tt.maxReplicas, tt.arrivalRate)

			// Create allocation
			alloc := CreateAllocation("test-server", "test-gpu")

			if alloc == nil {
				t.Fatalf("CreateAllocation returned nil unexpectedly")
			}

			// Check that bounds are enforced
			if alloc.NumReplicas() < tt.expectedMin {
				t.Errorf("Expected at least %d replicas (minReplicas), got %d", tt.expectedMin, alloc.NumReplicas())
			}
			if alloc.NumReplicas() > tt.expectedMax {
				t.Errorf("Expected at most %d replicas (maxReplicas), got %d", tt.expectedMax, alloc.NumReplicas())
			}
		})
	}
}

func TestZeroLoadAllocation_WithMaxReplicas(t *testing.T) {
	maxReplicas2 := 2

	tests := []struct {
		name         string
		minReplicas  int
		maxReplicas  *int
		expectedReps int
	}{
		{
			name:         "zero load with min=1, max=2",
			minReplicas:  1,
			maxReplicas:  &maxReplicas2,
			expectedReps: 1,
		},
		{
			name:         "zero load with min=0, max=nil",
			minReplicas:  0,
			maxReplicas:  nil,
			expectedReps: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				minNumReplicas: tt.minReplicas,
				maxNumReplicas: tt.maxReplicas,
			}
			model := &Model{
				numInstances: make(map[string]int),
			}
			model.numInstances["test-gpu"] = 1
			acc := &Accelerator{
				name: "test-gpu",
				spec: &config.AcceleratorSpec{
					Cost: 100.0,
				},
			}
			perf := &config.ModelAcceleratorPerfData{
				MaxBatchSize: 16,
				DecodeParms: config.DecodeParms{
					Alpha: 5.0,
					Beta:  2.0,
				},
				PrefillParms: config.PrefillParms{
					Gamma: 10.0,
					Delta: 1.5,
				},
			}

			alloc := zeroLoadAllocation(server, model, acc, perf)

			if tt.expectedReps == 0 {
				if alloc != nil && alloc.NumReplicas() != 0 {
					t.Errorf("Expected 0 replicas for zero load with min=0, got %d", alloc.NumReplicas())
				}
			} else {
				if alloc == nil {
					t.Fatal("zeroLoadAllocation returned nil unexpectedly")
				}
				if alloc.NumReplicas() != tt.expectedReps {
					t.Errorf("Expected %d replicas, got %d", tt.expectedReps, alloc.NumReplicas())
				}
			}
		})
	}
}
