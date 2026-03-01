package pipeline

import (
	"context"
	"math"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// GreedyByScoreOptimizer is a multi-model optimizer for GPU-constrained
// environments. It uses iterative mean-based fair-sharing to distribute scarce
// GPUs across competing models, ordered by composite score
// (priority * sum(requiredCapacity_i * analyzerScore_i)).
//
// Key differences from CostAwareOptimizer:
//   - Respects ResourceConstraints (GPU budgets per accelerator type)
//   - Fair-shares GPUs across models (highest-score model gets GPUs first)
//   - Supports per-role work units for P/D disaggregated models
//   - Scale-down is identical to CostAwareOptimizer (reuses costAwareScaleDown)
type GreedyByScoreOptimizer struct{}

// NewGreedyByScoreOptimizer creates a new GreedyByScoreOptimizer.
func NewGreedyByScoreOptimizer() *GreedyByScoreOptimizer {
	return &GreedyByScoreOptimizer{}
}

// Name returns the optimizer identifier.
func (o *GreedyByScoreOptimizer) Name() string {
	return "greedy-by-score"
}

// modelWork tracks per-model (or per-model-role) allocation state during fair-share iteration.
type modelWork struct {
	req       ModelScalingRequest
	remaining float64        // remaining Score (negative = fully satisfied)
	targets   map[string]int // variant name → target replicas
	role      string         // P/D role for this work unit ("prefill", "decode", "both")
}

// Optimize produces VariantDecisions for all models, fair-sharing GPUs across
// models that need to scale up. Scale-down models are handled independently.
func (o *GreedyByScoreOptimizer) Optimize(
	ctx context.Context,
	requests []ModelScalingRequest,
	constraints []*ResourceConstraints,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx).WithName(o.Name())
	available := mergeConstraints(constraints)

	// Separate scale-up and scale-down/steady models
	var scaleUpWork []*modelWork
	var otherRequests []ModelScalingRequest

	for _, req := range requests {
		if req.Result == nil {
			continue
		}

		if req.Result.RequiredCapacity > 0 || req.Result.Score > 0 {
			work := o.buildScaleUpWork(req)
			scaleUpWork = append(scaleUpWork, work...)
		} else {
			otherRequests = append(otherRequests, req)
		}
	}

	// Scale-up: iterative mean-based fair sharing
	o.fairShareScaleUp(ctx, scaleUpWork, available)

	// P/D proportional balancing
	o.proportionalPDBalance(ctx, scaleUpWork)

	// Build all decisions
	var allDecisions []interfaces.VariantDecision

	// Merge work unit targets back per-model before building decisions
	modelTargets := o.mergeWorkTargets(scaleUpWork)
	for modelKey, targets := range modelTargets {
		// Find the request for this model
		var req *ModelScalingRequest
		for i := range requests {
			key := requests[i].ModelID + "#" + requests[i].Namespace
			if key == modelKey {
				req = &requests[i]
				break
			}
		}
		if req == nil {
			continue
		}

		stateMap := buildStateMap(req.VariantStates)
		vcMap := buildCapacityMap(req.Result.VariantCapacities)
		decisions := buildDecisionsWithOptimizer(*req, stateMap, vcMap, targets, "greedy-by-score")
		logger.V(logging.DEBUG).Info("Greedy-by-score optimizer decisions (scale-up)",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	for _, req := range otherRequests {
		stateMap := buildStateMap(req.VariantStates)
		vcMap := buildCapacityMap(req.Result.VariantCapacities)
		targets := initTargets(req.VariantStates)

		if req.Result.SpareCapacity > 0 {
			costAwareScaleDown(ctx, req.Result, targets)
		}

		decisions := buildDecisionsWithOptimizer(req, stateMap, vcMap, targets, "greedy-by-score")
		logger.V(logging.DEBUG).Info("Greedy-by-score optimizer decisions (other)",
			"modelID", req.ModelID,
			"decisions", len(decisions))
		allDecisions = append(allDecisions, decisions...)
	}

	return allDecisions
}

// buildScaleUpWork creates work units for a scale-up request.
// For disaggregated models, creates separate work units per role.
func (o *GreedyByScoreOptimizer) buildScaleUpWork(req ModelScalingRequest) []*modelWork {
	if req.Disaggregated && req.Result.RoleCapacities != nil {
		var work []*modelWork
		for role, rc := range req.Result.RoleCapacities {
			if rc.RequiredCapacity > 0 {
				// Per-role score: priority * role's requiredCapacity
				// (analyzer score already factored into Result.Score at model level,
				// here we distribute proportionally by role)
				score := req.Priority * rc.RequiredCapacity
				work = append(work, &modelWork{
					req:       req,
					remaining: score,
					targets:   initTargetsForRole(req.VariantStates, role),
					role:      role,
				})
			}
		}
		if len(work) > 0 {
			return work
		}
	}

	// Non-disaggregated or no role capacities: use Score
	remaining := req.Result.Score
	if remaining <= 0 {
		remaining = req.Result.RequiredCapacity
	}
	if remaining <= 0 {
		return nil
	}

	return []*modelWork{
		{
			req:       req,
			remaining: remaining,
			targets:   initTargets(req.VariantStates),
			role:      "both",
		},
	}
}

// initTargetsForRole creates initial targets for variants matching the specified role.
func initTargetsForRole(states []interfaces.VariantReplicaState, role string) map[string]int {
	targets := make(map[string]int)
	for _, s := range states {
		if s.Role == role {
			targets[s.VariantName] = s.CurrentReplicas
		}
	}
	return targets
}

// mergeWorkTargets combines targets from multiple work units back to per-model maps.
func (o *GreedyByScoreOptimizer) mergeWorkTargets(work []*modelWork) map[string]map[string]int {
	result := make(map[string]map[string]int)
	for _, w := range work {
		key := w.req.ModelID + "#" + w.req.Namespace
		if _, ok := result[key]; !ok {
			// Start with current replicas for ALL variants (including those not in any work unit)
			result[key] = initTargets(w.req.VariantStates)
		}
		// Override with work unit targets
		for name, target := range w.targets {
			result[key][name] = target
		}
	}
	return result
}

// fairShareScaleUp implements the iterative mean-based fair-sharing algorithm.
// Each iteration picks the most starved model and allocates enough replicas to
// bring its remaining score below the current mean.
func (o *GreedyByScoreOptimizer) fairShareScaleUp(
	ctx context.Context,
	work []*modelWork,
	available map[string]int,
) {
	logger := ctrl.LoggerFrom(ctx)

	for {
		// Filter active models (remaining > 0)
		active := filterActive(work)
		if len(active) == 0 {
			break
		}

		// Check if any GPUs remain
		totalGPUs := 0
		for _, v := range available {
			totalGPUs += v
		}
		if totalGPUs == 0 {
			logger.V(logging.DEBUG).Info("GreedyByScore: no GPUs remaining, stopping fair-share")
			break
		}

		// Compute mean remaining score
		mean := computeMean(active)
		logger.V(logging.DEBUG).Info("GreedyByScore: iteration",
			"activeModels", len(active), "meanRemaining", mean)

		// Sort by remaining DESC (most starved first)
		sortByRemainingDesc(active)

		// Pick the most starved model
		w := active[0]

		// Compute allocation target
		allocationMean := mean
		if len(active) == 1 {
			allocationMean = 0
		} else if w.remaining <= mean {
			allocationMean = mean - (w.remaining / float64(len(active)))
		}

		// Allocate replicas
		allocated := o.allocateForModel(ctx, w, allocationMean, available)

		if !allocated {
			w.remaining = -1
			logger.V(logging.DEBUG).Info("GreedyByScore: no GPUs available for model, removing",
				"model", w.req.ModelID, "role", w.role)
			continue
		}

		if w.remaining > mean {
			logger.V(logging.DEBUG).Info("GreedyByScore: model still above mean, removing",
				"model", w.req.ModelID, "role", w.role, "remaining", w.remaining, "mean", mean)
			w.remaining = -1
		}
	}
}

// allocateForModel allocates replicas from the cheapest available variants
// to bring the model's remaining score below the mean.
func (o *GreedyByScoreOptimizer) allocateForModel(
	ctx context.Context,
	w *modelWork,
	mean float64,
	available map[string]int,
) bool {
	logger := ctrl.LoggerFrom(ctx)

	target := w.remaining - mean
	if target <= 0 {
		return false
	}

	// Filter variant capacities to those matching this work unit's role
	variantCaps := filterVariantCapacitiesByRole(w.req.Result.VariantCapacities, w.role, w.targets)
	sorted := sortByCostEfficiencyAsc(variantCaps)
	stateMap := buildStateMap(w.req.VariantStates)

	allocated := false
	for _, vc := range sorted {
		if target <= 0 {
			break
		}

		state := stateMap[vc.VariantName]

		if vc.PerReplicaCapacity <= 0 {
			continue
		}

		gpusPerReplica := state.GPUsPerReplica
		if gpusPerReplica <= 0 {
			gpusPerReplica = 1
		}
		gpusAvail := available[vc.AcceleratorName]
		if gpusAvail < gpusPerReplica {
			continue
		}

		n := int(math.Ceil(target / vc.PerReplicaCapacity))

		maxByGPU := gpusAvail / gpusPerReplica
		if n > maxByGPU {
			n = maxByGPU
		}
		if n <= 0 {
			continue
		}

		w.targets[vc.VariantName] += n
		capacityAdded := float64(n) * vc.PerReplicaCapacity
		w.remaining -= capacityAdded
		target -= capacityAdded
		available[vc.AcceleratorName] -= n * gpusPerReplica

		logger.V(logging.DEBUG).Info("GreedyByScore: allocated replicas",
			"model", w.req.ModelID,
			"role", w.role,
			"variant", vc.VariantName,
			"replicas", n,
			"gpusUsed", n*gpusPerReplica,
			"remainingScore", w.remaining)
		allocated = true
	}
	return allocated
}

// filterVariantCapacitiesByRole returns variant capacities that are in the work
// unit's target map. For role "both", returns all capacities. For specific roles,
// only returns capacities for variants matching that role.
func filterVariantCapacitiesByRole(capacities []interfaces.VariantCapacity, role string, targets map[string]int) []interfaces.VariantCapacity {
	if role == "both" || role == "" {
		return capacities
	}
	var filtered []interfaces.VariantCapacity
	for _, vc := range capacities {
		if _, ok := targets[vc.VariantName]; ok {
			filtered = append(filtered, vc)
		}
	}
	return filtered
}

// proportionalPDBalance adjusts work unit targets to maintain the initial
// P:D ratio when multiple roles are present for the same model.
func (o *GreedyByScoreOptimizer) proportionalPDBalance(
	ctx context.Context,
	work []*modelWork,
) {
	logger := ctrl.LoggerFrom(ctx)

	// Group work units by model
	type modelRoles struct {
		units []*modelWork
	}
	byModel := make(map[string]*modelRoles)
	for _, w := range work {
		if w.role == "both" || w.role == "" {
			continue
		}
		key := w.req.ModelID + "#" + w.req.Namespace
		mr, ok := byModel[key]
		if !ok {
			mr = &modelRoles{}
			byModel[key] = mr
		}
		mr.units = append(mr.units, w)
	}

	for modelKey, mr := range byModel {
		if len(mr.units) < 2 {
			continue // Need at least 2 roles for balancing
		}

		// Compute initial replicas per role and new total
		type roleInfo struct {
			role         string
			initialTotal int
			newTotal     int
			work         *modelWork
		}
		roles := make([]roleInfo, 0, len(mr.units))
		grandInitial := 0
		grandNew := 0
		for _, w := range mr.units {
			initial := 0
			newT := 0
			stateMap := buildStateMap(w.req.VariantStates)
			for name, target := range w.targets {
				newT += target
				if s, ok := stateMap[name]; ok {
					initial += s.CurrentReplicas
				}
			}
			grandInitial += initial
			grandNew += newT
			roles = append(roles, roleInfo{
				role:         w.role,
				initialTotal: initial,
				newTotal:     newT,
				work:         w,
			})
		}

		if grandInitial == 0 || grandNew <= grandInitial {
			continue // No initial ratio or no growth to redistribute
		}

		// Compute proportional targets
		adjustments := false
		for i := range roles {
			ratio := float64(roles[i].initialTotal) / float64(grandInitial)
			ideal := int(math.Round(float64(grandNew) * ratio))
			// Never go below initial
			if ideal < roles[i].initialTotal {
				ideal = roles[i].initialTotal
			}
			if ideal != roles[i].newTotal {
				adjustments = true
			}
			roles[i].newTotal = ideal
		}

		if !adjustments {
			continue
		}

		// Adjust work unit targets proportionally
		for _, ri := range roles {
			currentTotal := 0
			for _, t := range ri.work.targets {
				currentTotal += t
			}
			diff := ri.newTotal - currentTotal
			if diff == 0 {
				continue
			}

			stateMap := buildStateMap(ri.work.req.VariantStates)
			if diff > 0 {
				// Need to add replicas — add to cheapest variant in this role
				variantCaps := filterVariantCapacitiesByRole(ri.work.req.Result.VariantCapacities, ri.role, ri.work.targets)
				sorted := sortByCostEfficiencyAsc(variantCaps)
				for _, vc := range sorted {
					if diff <= 0 {
						break
					}
					ri.work.targets[vc.VariantName] += diff
					diff = 0
				}
			} else {
				// Need to remove replicas — remove from most expensive variant in this role
				variantCaps := filterVariantCapacitiesByRole(ri.work.req.Result.VariantCapacities, ri.role, ri.work.targets)
				sorted := sortByCostDesc(variantCaps)
				for _, vc := range sorted {
					if diff >= 0 {
						break
					}
					current := ri.work.targets[vc.VariantName]
					state := stateMap[vc.VariantName]
					removable := current - state.CurrentReplicas
					if removable <= 0 {
						continue
					}
					toRemove := -diff
					if toRemove > removable {
						toRemove = removable
					}
					ri.work.targets[vc.VariantName] -= toRemove
					diff += toRemove
				}
			}

			logger.V(logging.DEBUG).Info("GreedyByScore: P/D balance adjustment",
				"model", modelKey,
				"role", ri.role,
				"adjustedTotal", ri.newTotal)
		}
	}
}

// filterActive returns modelWork entries that still have remaining > 0.
func filterActive(work []*modelWork) []*modelWork {
	var active []*modelWork
	for _, w := range work {
		if w.remaining > 0 {
			active = append(active, w)
		}
	}
	return active
}

// computeMean returns the average remaining across active models.
func computeMean(active []*modelWork) float64 {
	if len(active) == 0 {
		return 0
	}
	total := 0.0
	for _, w := range active {
		total += w.remaining
	}
	return total / float64(len(active))
}

// sortByRemainingDesc sorts active models by remaining descending.
func sortByRemainingDesc(active []*modelWork) {
	sort.Slice(active, func(i, j int) bool {
		return active[i].remaining > active[j].remaining
	})
}

// Ensure GreedyByScoreOptimizer implements ScalingOptimizer
var _ ScalingOptimizer = (*GreedyByScoreOptimizer)(nil)
