package interfaces

import (
	"testing"
)

func TestSaturationScalingConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  SaturationScalingConfig
		wantErr bool
	}{
		{
			name: "valid default config",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
			},
			wantErr: false,
		},
		{
			name: "valid custom config",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.75,
				QueueLengthThreshold: 10,
				KvSpareTrigger:       0.15,
				QueueSpareTrigger:    5,
			},
			wantErr: false,
		},
		{
			name: "invalid KvCacheThreshold too high",
			config: SaturationScalingConfig{
				KvCacheThreshold:     1.5,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "invalid KvCacheThreshold negative",
			config: SaturationScalingConfig{
				KvCacheThreshold:     -0.1,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "invalid QueueLengthThreshold negative",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: -1,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "invalid KvSpareTrigger too high",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       1.5,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "invalid KvSpareTrigger negative",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       -0.1,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "invalid QueueSpareTrigger negative",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    -1,
			},
			wantErr: true,
		},
		{
			name: "invalid KvCacheThreshold less than KvSpareTrigger",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.5,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.6,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "edge case: zero values are valid",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.0,
				QueueLengthThreshold: 0,
				KvSpareTrigger:       0.0,
				QueueSpareTrigger:    0,
			},
			wantErr: false,
		},
		{
			name: "edge case: max values are valid",
			config: SaturationScalingConfig{
				KvCacheThreshold:     1.0,
				QueueLengthThreshold: 1000,
				KvSpareTrigger:       1.0,
				QueueSpareTrigger:    1000,
			},
			wantErr: false,
		},
		{
			name: "V2 valid config with explicit thresholds",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.90,
				ScaleDownBoundary:    0.60,
			},
			wantErr: false,
		},
		{
			name: "V2 invalid: scaleUpThreshold > 1",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     1.5,
				ScaleDownBoundary:    0.70,
			},
			wantErr: true,
		},
		{
			name: "V2 invalid: scaleUpThreshold <= scaleDownBoundary",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.60,
				ScaleDownBoundary:    0.70,
			},
			wantErr: true,
		},
		{
			name: "V2 thresholds ignored when analyzerName is not saturation",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				AnalyzerName:         "",
				ScaleUpThreshold:     0,
				ScaleDownBoundary:    0,
			},
			wantErr: false,
		},
		{
			name: "valid priority",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				Priority:             5.0,
			},
			wantErr: false,
		},
		{
			name: "invalid negative priority",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				Priority:             -1.0,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSaturationScalingConfigApplyDefaults(t *testing.T) {
	t.Run("applies defaults for V2 analyzer when thresholds are zero", func(t *testing.T) {
		config := SaturationScalingConfig{
			AnalyzerName: "saturation",
		}
		config.ApplyDefaults()
		if config.ScaleUpThreshold != DefaultScaleUpThreshold {
			t.Errorf("expected ScaleUpThreshold=%v, got %v", DefaultScaleUpThreshold, config.ScaleUpThreshold)
		}
		if config.ScaleDownBoundary != DefaultScaleDownBoundary {
			t.Errorf("expected ScaleDownBoundary=%v, got %v", DefaultScaleDownBoundary, config.ScaleDownBoundary)
		}
	})

	t.Run("does not overwrite explicit values", func(t *testing.T) {
		config := SaturationScalingConfig{
			AnalyzerName:      "saturation",
			ScaleUpThreshold:  0.90,
			ScaleDownBoundary: 0.60,
		}
		config.ApplyDefaults()
		if config.ScaleUpThreshold != 0.90 {
			t.Errorf("expected ScaleUpThreshold=0.90, got %v", config.ScaleUpThreshold)
		}
		if config.ScaleDownBoundary != 0.60 {
			t.Errorf("expected ScaleDownBoundary=0.60, got %v", config.ScaleDownBoundary)
		}
	})

	t.Run("no-op when analyzerName is not saturation", func(t *testing.T) {
		config := SaturationScalingConfig{
			AnalyzerName: "",
		}
		config.ApplyDefaults()
		if config.ScaleUpThreshold != 0 {
			t.Errorf("expected ScaleUpThreshold=0 for V1, got %v", config.ScaleUpThreshold)
		}
		if config.ScaleDownBoundary != 0 {
			t.Errorf("expected ScaleDownBoundary=0 for V1, got %v", config.ScaleDownBoundary)
		}
	})

	t.Run("applies default priority when zero", func(t *testing.T) {
		config := SaturationScalingConfig{}
		config.ApplyDefaults()
		if config.Priority != DefaultPriority {
			t.Errorf("expected Priority=%v, got %v", DefaultPriority, config.Priority)
		}
	})

	t.Run("does not overwrite explicit priority", func(t *testing.T) {
		config := SaturationScalingConfig{
			Priority: 5.0,
		}
		config.ApplyDefaults()
		if config.Priority != 5.0 {
			t.Errorf("expected Priority=5.0, got %v", config.Priority)
		}
	})

	t.Run("applies default analyzers list for V2", func(t *testing.T) {
		config := SaturationScalingConfig{
			AnalyzerName: "saturation",
		}
		config.ApplyDefaults()
		if len(config.Analyzers) != 1 {
			t.Fatalf("expected 1 analyzer, got %d", len(config.Analyzers))
		}
		if config.Analyzers[0].Name != "saturation" {
			t.Errorf("expected analyzer name 'saturation', got %q", config.Analyzers[0].Name)
		}
		if config.Analyzers[0].Score != 1.0 {
			t.Errorf("expected score 1.0, got %v", config.Analyzers[0].Score)
		}
		if config.Analyzers[0].Enabled == nil || !*config.Analyzers[0].Enabled {
			t.Errorf("expected analyzer to be enabled")
		}
	})

	t.Run("does not overwrite explicit analyzers", func(t *testing.T) {
		disabled := false
		config := SaturationScalingConfig{
			AnalyzerName: "saturation",
			Analyzers: []AnalyzerScoreConfig{
				{Name: "saturation", Score: 0.5, Enabled: &disabled},
			},
		}
		config.ApplyDefaults()
		if config.Analyzers[0].Score != 0.5 {
			t.Errorf("expected score 0.5, got %v", config.Analyzers[0].Score)
		}
		if *config.Analyzers[0].Enabled != false {
			t.Errorf("expected analyzer to remain disabled")
		}
	})

	t.Run("applies per-entry defaults for zero score", func(t *testing.T) {
		config := SaturationScalingConfig{
			AnalyzerName: "saturation",
			Analyzers: []AnalyzerScoreConfig{
				{Name: "saturation"},
			},
		}
		config.ApplyDefaults()
		if config.Analyzers[0].Score != 1.0 {
			t.Errorf("expected default score 1.0, got %v", config.Analyzers[0].Score)
		}
		if config.Analyzers[0].Enabled == nil || !*config.Analyzers[0].Enabled {
			t.Errorf("expected analyzer to be enabled by default")
		}
	})

	t.Run("does not add analyzers for V1", func(t *testing.T) {
		config := SaturationScalingConfig{
			AnalyzerName: "",
		}
		config.ApplyDefaults()
		if len(config.Analyzers) != 0 {
			t.Errorf("expected no analyzers for V1, got %d", len(config.Analyzers))
		}
	})

	t.Run("ApplyDefaults then Validate passes with zero-valued omitempty fields", func(t *testing.T) {
		config := SaturationScalingConfig{
			KvCacheThreshold:     0.80,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.10,
			QueueSpareTrigger:    3,
			AnalyzerName:         "saturation",
			// ScaleUpThreshold and ScaleDownBoundary omitted (zero)
		}
		config.ApplyDefaults()
		if err := config.Validate(); err != nil {
			t.Errorf("ApplyDefaults + Validate should pass, got: %v", err)
		}
	})
}
