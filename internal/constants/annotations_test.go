package constants

import (
	"fmt"
	"testing"
)

func intPtr(n int) *int {
	return &n
}

func TestParseReplicaBounds(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantMin     *int
		wantMax     *int
		wantErr     bool
	}{
		{
			name:        "nil annotations",
			annotations: nil,
			wantMin:     nil,
			wantMax:     nil,
			wantErr:     false,
		},
		{
			name:        "empty annotations",
			annotations: map[string]string{},
			wantMin:     nil,
			wantMax:     nil,
			wantErr:     false,
		},
		{
			name:        "only min set",
			annotations: map[string]string{MinReplicasAnnotationKey: "2"},
			wantMin:     intPtr(2),
			wantMax:     nil,
			wantErr:     false,
		},
		{
			name:        "only max set",
			annotations: map[string]string{MaxReplicasAnnotationKey: "5"},
			wantMin:     nil,
			wantMax:     intPtr(5),
			wantErr:     false,
		},
		{
			name: "both min and max set",
			annotations: map[string]string{
				MinReplicasAnnotationKey: "1",
				MaxReplicasAnnotationKey: "10",
			},
			wantMin: intPtr(1),
			wantMax: intPtr(10),
			wantErr: false,
		},
		{
			name: "min equals max",
			annotations: map[string]string{
				MinReplicasAnnotationKey: "3",
				MaxReplicasAnnotationKey: "3",
			},
			wantMin: intPtr(3),
			wantMax: intPtr(3),
			wantErr: false,
		},
		{
			name:        "min zero (allows scale to zero)",
			annotations: map[string]string{MinReplicasAnnotationKey: "0"},
			wantMin:     intPtr(0),
			wantMax:     nil,
			wantErr:     false,
		},
		{
			name:        "max zero (no cap)",
			annotations: map[string]string{MaxReplicasAnnotationKey: "0"},
			wantMin:     nil,
			wantMax:     intPtr(0),
			wantErr:     false,
		},
		{
			name: "min greater than max",
			annotations: map[string]string{
				MinReplicasAnnotationKey: "5",
				MaxReplicasAnnotationKey: "3",
			},
			wantErr: true,
		},
		{
			name:        "invalid min (non-numeric)",
			annotations: map[string]string{MinReplicasAnnotationKey: "abc"},
			wantErr:     true,
		},
		{
			name:        "invalid max (non-numeric)",
			annotations: map[string]string{MaxReplicasAnnotationKey: "xyz"},
			wantErr:     true,
		},
		{
			name:        "negative min",
			annotations: map[string]string{MinReplicasAnnotationKey: "-1"},
			wantErr:     true,
		},
		{
			name:        "negative max",
			annotations: map[string]string{MaxReplicasAnnotationKey: "-2"},
			wantErr:     true,
		},
		{
			name:        "invalid min (float)",
			annotations: map[string]string{MinReplicasAnnotationKey: "1.5"},
			wantErr:     true,
		},
		{
			name: "min > max but max is 0 (no cap) - allowed",
			annotations: map[string]string{
				MinReplicasAnnotationKey: "5",
				MaxReplicasAnnotationKey: "0",
			},
			wantMin: intPtr(5),
			wantMax: intPtr(0),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMin, gotMax, err := ParseReplicaBounds(tt.annotations)

			if (err != nil) != tt.wantErr {
				t.Errorf("ParseReplicaBounds() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if !ptrEqual(gotMin, tt.wantMin) {
				t.Errorf("ParseReplicaBounds() minReplicas = %v, want %v", ptrStr(gotMin), ptrStr(tt.wantMin))
			}
			if !ptrEqual(gotMax, tt.wantMax) {
				t.Errorf("ParseReplicaBounds() maxReplicas = %v, want %v", ptrStr(gotMax), ptrStr(tt.wantMax))
			}
		})
	}
}

func ptrEqual(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func ptrStr(p *int) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *p)
}
