package utils

import (
	"testing"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSuggestResourceNameFromVariantID(t *testing.T) {
	tests := []struct {
		name      string
		variantID string
		expected  string
	}{
		{
			name:      "variant with slashes and dots",
			variantID: "meta/llama-3.1-8b-A100-1",
			expected:  "meta-llama-3-1-8b-a100-1",
		},
		{
			name:      "variant with uppercase",
			variantID: "Meta/Llama-3.1-8B-A100-1",
			expected:  "meta-llama-3-1-8b-a100-1",
		},
		{
			name:      "variant with special characters",
			variantID: "model@name/variant_1",
			expected:  "modelname-variant1",
		},
		{
			name:      "variant with leading/trailing hyphens",
			variantID: "-model/variant-",
			expected:  "model-variant",
		},
		{
			name:      "simple variant",
			variantID: "vllm-deployment",
			expected:  "vllm-deployment",
		},
		{
			name:      "variant with multiple slashes",
			variantID: "org/team/model-A100-1",
			expected:  "org-team-model-a100-1",
		},
		{
			name:      "variant with underscores",
			variantID: "model_name_variant_1",
			expected:  "modelnamevariant1",
		},
		{
			name:      "variant with spaces",
			variantID: "model name variant 1",
			expected:  "modelnamevariant1",
		},
		{
			name:      "empty string",
			variantID: "",
			expected:  "",
		},
		{
			name:      "only invalid characters",
			variantID: "@#$%",
			expected:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SuggestResourceNameFromVariantID(tt.variantID)
			if result != tt.expected {
				t.Errorf("SuggestResourceNameFromVariantID(%q) = %q, expected %q",
					tt.variantID, result, tt.expected)
			}
		})
	}
}

func TestValidateVariantAutoscalingName(t *testing.T) {
	tests := []struct {
		name          string
		vaName        string
		variantID     string
		shouldLogDiff bool
		description   string
	}{
		{
			name:          "matching normalized name",
			vaName:        "meta-llama-3-1-8b-a100-1",
			variantID:     "meta/llama-3.1-8b-A100-1",
			shouldLogDiff: false,
			description:   "VA name matches the normalized variant_id",
		},
		{
			name:          "different but valid names",
			vaName:        "vllm-deployment",
			variantID:     "meta/llama-3.1-8b-A100-1",
			shouldLogDiff: true,
			description:   "VA name differs from normalized variant_id (normal case)",
		},
		{
			name:          "identical strings",
			vaName:        "simple-variant",
			variantID:     "simple-variant",
			shouldLogDiff: false,
			description:   "Both names are identical",
		},
		{
			name:          "deployment-style name with complex variant_id",
			vaName:        "llm-inference",
			variantID:     "organization/model-name/variant-1-A100-8",
			shouldLogDiff: true,
			description:   "Deployment name style vs hierarchical variant_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.vaName,
					Namespace: "test-namespace",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					VariantID: tt.variantID,
				},
			}

			// This function logs internally, we're just testing it doesn't panic
			// and completes successfully
			ValidateVariantAutoscalingName(va)

			// Verify the suggested name matches what we expect
			suggested := SuggestResourceNameFromVariantID(tt.variantID)
			if tt.shouldLogDiff {
				if va.Name == suggested {
					t.Errorf("Expected VA name %q to differ from suggested %q, but they match",
						va.Name, suggested)
				}
			} else {
				if va.Name != suggested {
					t.Errorf("Expected VA name %q to match suggested %q, but they differ",
						va.Name, suggested)
				}
			}
		})
	}
}

// TestDNS1123Compliance verifies that suggested names are valid Kubernetes resource names
func TestDNS1123Compliance(t *testing.T) {
	tests := []struct {
		name      string
		variantID string
	}{
		{
			name:      "complex variant",
			variantID: "Meta/Llama-3.1-8B-A100-1",
		},
		{
			name:      "special characters",
			variantID: "model@name/variant_1!test",
		},
		{
			name:      "unicode characters",
			variantID: "modèl/variánt",
		},
	}

	// DNS-1123 pattern: ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ (or empty)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SuggestResourceNameFromVariantID(tt.variantID)

			// Check that result only contains valid characters
			for _, c := range result {
				if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
					t.Errorf("SuggestResourceNameFromVariantID(%q) = %q contains invalid character %q",
						tt.variantID, result, string(c))
				}
			}

			// Check it doesn't start or end with hyphen
			if len(result) > 0 {
				if result[0] == '-' || result[len(result)-1] == '-' {
					t.Errorf("SuggestResourceNameFromVariantID(%q) = %q starts or ends with hyphen",
						tt.variantID, result)
				}
			}

			t.Logf("Input: %q -> Output: %q (valid: %t)", tt.variantID, result, result == "" || len(result) > 0)
		})
	}
}

// TestRealWorldExamples tests with actual variant_id patterns from the codebase
func TestRealWorldExamples(t *testing.T) {
	tests := []struct {
		name      string
		variantID string
		vaName    string
	}{
		{
			name:      "e2e test pattern",
			variantID: "test-model-A100-1",
			vaName:    "vllm-deployment",
		},
		{
			name:      "openshift test pattern",
			variantID: "test-model/variant-1-A100-1",
			vaName:    "vllm-deployment",
		},
		{
			name:      "huggingface model pattern",
			variantID: "meta-llama/Llama-3.1-8B-Instruct",
			vaName:    "llama-deployment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suggested := SuggestResourceNameFromVariantID(tt.variantID)
			t.Logf("variant_id: %q -> suggested: %q (actual va.Name: %q)",
				tt.variantID, suggested, tt.vaName)

			// Verify suggested name is valid
			if suggested != "" {
				// Must be lowercase alphanumeric with hyphens
				for _, c := range suggested {
					if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
						t.Errorf("Suggested name %q contains invalid character %q", suggested, string(c))
					}
				}
			}
		})
	}
}
