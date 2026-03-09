package constants

import (
	"fmt"
	"strconv"
)

// ParseReplicaBounds extracts min and max replica bounds from annotations.
// Returns nil for unset annotations. Returns error for invalid values
// (non-numeric, negative, min > max when both set).
func ParseReplicaBounds(annotations map[string]string) (minReplicas *int, maxReplicas *int, err error) {
	if annotations == nil {
		return nil, nil, nil
	}

	if v, ok := annotations[MinReplicasAnnotationKey]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid %s annotation value %q: %w", MinReplicasAnnotationKey, v, err)
		}
		if n < 0 {
			return nil, nil, fmt.Errorf("invalid %s annotation value %d: must be non-negative", MinReplicasAnnotationKey, n)
		}
		minReplicas = &n
	}

	if v, ok := annotations[MaxReplicasAnnotationKey]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid %s annotation value %q: %w", MaxReplicasAnnotationKey, v, err)
		}
		if n < 0 {
			return nil, nil, fmt.Errorf("invalid %s annotation value %d: must be non-negative", MaxReplicasAnnotationKey, n)
		}
		maxReplicas = &n
	}

	if minReplicas != nil && maxReplicas != nil && *maxReplicas > 0 && *minReplicas > *maxReplicas {
		return nil, nil, fmt.Errorf("min-replicas (%d) cannot exceed max-replicas (%d)", *minReplicas, *maxReplicas)
	}

	return minReplicas, maxReplicas, nil
}
