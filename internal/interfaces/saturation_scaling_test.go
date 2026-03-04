package interfaces

import (
	"testing"
)

func float64Ptr(v float64) *float64 { return &v }

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
			name: "V2 valid config with explicit thresholds (old-style analyzerName)",
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
			name: "V2 valid config with analyzers list (new-style)",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
				ScaleUpThreshold:     0.90,
				ScaleDownBoundary:    0.60,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", Score: 1.0},
				},
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
			name: "V2 thresholds ignored when not V2",
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
		{
			name: "V2 valid per-analyzer threshold override",
			config: SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", ScaleUpThreshold: float64Ptr(0.90)},
				},
			},
			wantErr: false,
		},
		{
			name: "V2 invalid per-analyzer scaleUpThreshold > 1",
			config: SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", ScaleUpThreshold: float64Ptr(1.5)},
				},
			},
			wantErr: true,
		},
		{
			name: "V2 invalid per-analyzer scaleDownBoundary > 1",
			config: SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					{Name: "saturation", ScaleDownBoundary: float64Ptr(1.5)},
				},
			},
			wantErr: true,
		},
		{
			name: "V2 invalid per-analyzer effective up <= down",
			config: SaturationScalingConfig{
				ScaleUpThreshold:  0.85,
				ScaleDownBoundary: 0.70,
				Analyzers: []AnalyzerScoreConfig{
					// Override scaleUpThreshold to 0.60, but global down is 0.70 → 0.60 <= 0.70
					{Name: "saturation", ScaleUpThreshold: float64Ptr(0.60)},
				},
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
	t.Run("applies defaults for V2 via analyzerName (backward compat)", func(t *testing.T) {
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
		if len(config.Analyzers) != 1 {
			t.Fatalf("expected 1 analyzer, got %d", len(config.Analyzers))
		}
	})

	t.Run("applies defaults for V2 via analyzers list (new-style)", func(t *testing.T) {
		config := SaturationScalingConfig{
			Analyzers: []AnalyzerScoreConfig{
				{Name: "saturation"},
			},
		}
		config.ApplyDefaults()
		if config.ScaleUpThreshold != DefaultScaleUpThreshold {
			t.Errorf("expected ScaleUpThreshold=%v, got %v", DefaultScaleUpThreshold, config.ScaleUpThreshold)
		}
		if config.ScaleDownBoundary != DefaultScaleDownBoundary {
			t.Errorf("expected ScaleDownBoundary=%v, got %v", DefaultScaleDownBoundary, config.ScaleDownBoundary)
		}
		if config.Analyzers[0].Score != 1.0 {
			t.Errorf("expected default score 1.0, got %v", config.Analyzers[0].Score)
		}
		if config.Analyzers[0].Enabled == nil || !*config.Analyzers[0].Enabled {
			t.Errorf("expected analyzer to be enabled by default")
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

	t.Run("no-op when not V2", func(t *testing.T) {
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
		if len(config.Analyzers) != 0 {
			t.Errorf("expected no analyzers for V1, got %d", len(config.Analyzers))
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

	t.Run("does not overwrite explicit analyzers", func(t *testing.T) {
		disabled := false
		config := SaturationScalingConfig{
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

	t.Run("ApplyDefaults then Validate passes with zero-valued omitempty fields", func(t *testing.T) {
		config := SaturationScalingConfig{
			KvCacheThreshold:     0.80,
			QueueLengthThreshold: 5,
			KvSpareTrigger:       0.10,
			QueueSpareTrigger:    3,
			Analyzers: []AnalyzerScoreConfig{
				{Name: "saturation"},
			},
		}
		config.ApplyDefaults()
		if err := config.Validate(); err != nil {
			t.Errorf("ApplyDefaults + Validate should pass, got: %v", err)
		}
	})
}

func TestIsV2(t *testing.T) {
	t.Run("false when no analyzers and no analyzerName", func(t *testing.T) {
		config := SaturationScalingConfig{}
		if config.IsV2() {
			t.Error("expected IsV2()=false")
		}
	})

	t.Run("true when analyzerName is saturation (backward compat)", func(t *testing.T) {
		config := SaturationScalingConfig{AnalyzerName: "saturation"}
		if !config.IsV2() {
			t.Error("expected IsV2()=true")
		}
	})

	t.Run("true when analyzers list is populated", func(t *testing.T) {
		config := SaturationScalingConfig{
			Analyzers: []AnalyzerScoreConfig{
				{Name: "saturation"},
			},
		}
		if !config.IsV2() {
			t.Error("expected IsV2()=true")
		}
	})

	t.Run("true when both analyzerName and analyzers set", func(t *testing.T) {
		config := SaturationScalingConfig{
			AnalyzerName: "saturation",
			Analyzers: []AnalyzerScoreConfig{
				{Name: "saturation"},
			},
		}
		if !config.IsV2() {
			t.Error("expected IsV2()=true")
		}
	})
}

func TestGetAnalyzerName(t *testing.T) {
	t.Run("returns saturation when analyzers list populated", func(t *testing.T) {
		config := SaturationScalingConfig{
			Analyzers: []AnalyzerScoreConfig{
				{Name: "saturation"},
			},
		}
		if config.GetAnalyzerName() != "saturation" {
			t.Errorf("expected 'saturation', got %q", config.GetAnalyzerName())
		}
	})

	t.Run("returns raw analyzerName when no analyzers list", func(t *testing.T) {
		config := SaturationScalingConfig{AnalyzerName: "saturation"}
		if config.GetAnalyzerName() != "saturation" {
			t.Errorf("expected 'saturation', got %q", config.GetAnalyzerName())
		}
	})

	t.Run("returns empty when no analyzers and no analyzerName", func(t *testing.T) {
		config := SaturationScalingConfig{}
		if config.GetAnalyzerName() != "" {
			t.Errorf("expected empty, got %q", config.GetAnalyzerName())
		}
	})
}

func TestEffectiveThresholds(t *testing.T) {
	t.Run("returns global when per-analyzer not set", func(t *testing.T) {
		a := AnalyzerScoreConfig{Name: "saturation"}
		if a.EffectiveScaleUpThreshold(0.85) != 0.85 {
			t.Errorf("expected 0.85, got %v", a.EffectiveScaleUpThreshold(0.85))
		}
		if a.EffectiveScaleDownBoundary(0.70) != 0.70 {
			t.Errorf("expected 0.70, got %v", a.EffectiveScaleDownBoundary(0.70))
		}
	})

	t.Run("returns per-analyzer when set", func(t *testing.T) {
		a := AnalyzerScoreConfig{
			Name:              "saturation",
			ScaleUpThreshold:  float64Ptr(0.90),
			ScaleDownBoundary: float64Ptr(0.60),
		}
		if a.EffectiveScaleUpThreshold(0.85) != 0.90 {
			t.Errorf("expected 0.90, got %v", a.EffectiveScaleUpThreshold(0.85))
		}
		if a.EffectiveScaleDownBoundary(0.70) != 0.60 {
			t.Errorf("expected 0.60, got %v", a.EffectiveScaleDownBoundary(0.70))
		}
	})

	t.Run("partial override: only scaleUpThreshold", func(t *testing.T) {
		a := AnalyzerScoreConfig{
			Name:             "saturation",
			ScaleUpThreshold: float64Ptr(0.95),
		}
		if a.EffectiveScaleUpThreshold(0.85) != 0.95 {
			t.Errorf("expected 0.95, got %v", a.EffectiveScaleUpThreshold(0.85))
		}
		if a.EffectiveScaleDownBoundary(0.70) != 0.70 {
			t.Errorf("expected 0.70 (global fallback), got %v", a.EffectiveScaleDownBoundary(0.70))
		}
	})
}
