package stabilization

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/resource"
)

// fakeClock is a manually advanceable time source for deterministic windows.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func ptr[T any](v T) *T { return &v }

// behaviorFor builds a single-direction behavior for tests. Pass nil for the
// direction that should keep its defaults.
func behaviorFor(scaleUp, scaleDown *autoscalingv2.HPAScalingRules) *autoscalingv2.HorizontalPodAutoscalerBehavior {
	return &autoscalingv2.HorizontalPodAutoscalerBehavior{ScaleUp: scaleUp, ScaleDown: scaleDown}
}

func rulesFor(window int32, sel autoscalingv2.ScalingPolicySelect, policies ...autoscalingv2.HPAScalingPolicy) *autoscalingv2.HPAScalingRules {
	return &autoscalingv2.HPAScalingRules{
		StabilizationWindowSeconds: ptr(window),
		SelectPolicy:               ptr(sel),
		Policies:                   policies,
	}
}

// testPeriod is the rate-policy period used across the specs; clock advances of
// 61s cross it to exercise per-period budgeting.
const testPeriod int32 = 60

func podsPolicy(value int32) autoscalingv2.HPAScalingPolicy {
	return autoscalingv2.HPAScalingPolicy{Type: autoscalingv2.PodsScalingPolicy, Value: value, PeriodSeconds: testPeriod}
}

func percentPolicy(value int32) autoscalingv2.HPAScalingPolicy {
	return autoscalingv2.HPAScalingPolicy{Type: autoscalingv2.PercentScalingPolicy, Value: value, PeriodSeconds: testPeriod}
}

var _ = Describe("Stabilizer", func() {
	var (
		clock *fakeClock
		s     *Stabilizer
	)

	BeforeEach(func() {
		clock = newFakeClock()
		s = New(WithClock(clock.Now))
	})

	Describe("stabilization window", func() {
		It("scales up immediately under the default behavior", func() {
			res := s.Stabilize(Args{Key: "ns/v", CurrentReplicas: 2, DesiredReplicas: 4, MinReplicas: 1, MaxReplicas: 10})
			Expect(res.Replicas).To(Equal(int32(4)))
		})

		It("holds scale-down for the default 300s window, then releases", func() {
			args := Args{Key: "ns/v", MinReplicas: 1, MaxReplicas: 10}

			args.CurrentReplicas, args.DesiredReplicas = 4, 4
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(4)))

			clock.Advance(30 * time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 4, 2
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(4)), "within the down window the recent max is held")

			clock.Advance(DefaultDownscaleStabilizationWindow + time.Second)
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(2)), "after the window decays the low recommendation wins")
		})

		It("delays scale-up while the up window still holds a recent low", func() {
			up := rulesFor(60, autoscalingv2.MaxChangePolicySelect, podsPolicy(1000))
			args := Args{Key: "ns/v", Behavior: behaviorFor(up, nil), MinReplicas: 1, MaxReplicas: 100}

			args.CurrentReplicas, args.DesiredReplicas = 2, 2
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(2)))

			clock.Advance(10 * time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 2, 5
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(2)), "the recent low (2) gates the spike")

			clock.Advance(61 * time.Second)
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(5)), "after the window passes, scale-up proceeds")
		})

		It("freezes the current count when it sits inside the historical band", func() {
			// Both directions windowed at 60s with generous rate policies, so only
			// the trailing band governs.
			up := rulesFor(60, autoscalingv2.MaxChangePolicySelect, podsPolicy(1000))
			down := rulesFor(60, autoscalingv2.MaxChangePolicySelect, podsPolicy(1000))
			args := Args{Key: "ns/v", Behavior: behaviorFor(up, down), MinReplicas: 0, MaxReplicas: 0}

			// Seed the window with 5 then 10, so the band becomes [upRec=5, downRec=10].
			args.CurrentReplicas, args.DesiredReplicas = 5, 5
			s.Stabilize(args)
			clock.Advance(20 * time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 5, 10
			s.Stabilize(args)

			// current=8 sits inside [5,10]; desired=7 is below current but still in
			// the band, so neither the floor nor the ceiling clamp fires.
			clock.Advance(20 * time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 8, 7
			res := s.Stabilize(args)
			Expect(res.Stabilized).To(Equal(int32(8)), "current inside the band is held, not the desired 7")
			Expect(res.Replicas).To(Equal(int32(8)))
			Expect(res.Reason).To(ContainSubstring("stabilization window applied"))
		})
	})

	Describe("rate policies", func() {
		It("caps scale-up by a pods-per-period budget", func() {
			up := rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(2))
			args := Args{Key: "ns/v", Behavior: behaviorFor(up, nil), MinReplicas: 1, MaxReplicas: 100}

			args.CurrentReplicas, args.DesiredReplicas = 2, 10
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(4)), "first cycle adds at most 2")

			clock.Advance(10 * time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 4, 10
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(4)), "budget already spent this period")

			clock.Advance(61 * time.Second)
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(6)), "a new period grants a fresh +2")
		})

		It("caps scale-down by a percent-per-period budget", func() {
			down := rulesFor(0, autoscalingv2.MaxChangePolicySelect, percentPolicy(50))
			res := s.Stabilize(Args{Key: "ns/v", Behavior: behaviorFor(nil, down), CurrentReplicas: 10, DesiredReplicas: 2, MinReplicas: 1, MaxReplicas: 100})
			Expect(res.Replicas).To(Equal(int32(5)), "at most -50% in one period")
		})

		DescribeTable("selectPolicy combines competing policies",
			func(sel autoscalingv2.ScalingPolicySelect, want int32) {
				// Pods(+4) proposes 14; Percent(+100%) proposes 20.
				up := rulesFor(0, sel, podsPolicy(4), percentPolicy(100))
				res := s.Stabilize(Args{Key: "ns/v", Behavior: behaviorFor(up, nil), CurrentReplicas: 10, DesiredReplicas: 100, MinReplicas: 1, MaxReplicas: 1000})
				Expect(res.Replicas).To(Equal(want))
			},
			Entry("Max picks the most permissive", autoscalingv2.MaxChangePolicySelect, int32(20)),
			Entry("Min picks the most conservative", autoscalingv2.MinChangePolicySelect, int32(14)),
			Entry("Disabled holds current", autoscalingv2.DisabledPolicySelect, int32(10)),
		)

		DescribeTable("selectPolicy combines competing scale-down policies",
			func(sel autoscalingv2.ScalingPolicySelect, want int32) {
				// From 10: Pods(-4) proposes 6; Percent(-50%) proposes 5.
				down := rulesFor(0, sel, podsPolicy(4), percentPolicy(50))
				res := s.Stabilize(Args{Key: "ns/v", Behavior: behaviorFor(nil, down), CurrentReplicas: 10, DesiredReplicas: 1, MinReplicas: 0, MaxReplicas: 100})
				Expect(res.Replicas).To(Equal(want))
			},
			Entry("Max picks the most permissive (lowest)", autoscalingv2.MaxChangePolicySelect, int32(5)),
			Entry("Min picks the most conservative (highest)", autoscalingv2.MinChangePolicySelect, int32(6)),
			Entry("Disabled holds current", autoscalingv2.DisabledPolicySelect, int32(10)),
		)

		It("grants a fresh percent budget each period for scale-down", func() {
			down := rulesFor(0, autoscalingv2.MaxChangePolicySelect, percentPolicy(50))
			args := Args{Key: "ns/v", Behavior: behaviorFor(nil, down), MinReplicas: 0, MaxReplicas: 100}

			args.CurrentReplicas, args.DesiredReplicas = 10, 0
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(5)), "first cycle: at most -50% -> 5")

			clock.Advance(10 * time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 5, 0
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(5)), "budget already spent this period")

			clock.Advance(61 * time.Second)
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(2)), "new period: -50% of current 5 -> 2")
		})

		It("reconstructs the period start from both directions (cross-direction budgeting)", func() {
			// Scale-up +4 pods/60s; scale-down unlimited and un-stabilized so the
			// intervening down actuates immediately and records a down event.
			up := rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(4))
			down := rulesFor(0, autoscalingv2.MaxChangePolicySelect, percentPolicy(100))
			b := behaviorFor(up, down)
			args := Args{Key: "ns/v", Behavior: b, MinReplicas: 0, MaxReplicas: 100}

			// t0: 2 -> 6 (records +4 up event).
			args.CurrentReplicas, args.DesiredReplicas = 2, 6
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(6)))

			// t20: load drops, 6 -> 2 (records -4 down event).
			clock.Advance(20 * time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 6, 2
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(2)))

			// t40: load spikes again, 2 -> 6. Both events are still in the 60s
			// window: periodStart = 2 - 4(added) + 4(removed) = 2, so +4 reaches 6.
			// (Same-direction-only accounting would wrongly pin this at 2.)
			clock.Advance(20 * time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 2, 6
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(6)), "cross-direction term must restore the legitimate scale-up")
		})

		It("reconstructs the period start from both directions for scale-down", func() {
			// Symmetric to the scale-up case: an intervening up event must not
			// inflate the down period-start baseline and block a legitimate down.
			down := rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(4))
			up := rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(1000))
			b := behaviorFor(up, down)
			args := Args{Key: "ns/v", Behavior: b, MinReplicas: 0, MaxReplicas: 100}

			// t0: 10 -> 6 (records -4 down event).
			args.CurrentReplicas, args.DesiredReplicas = 10, 6
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(6)))

			// t20: load recovers, 6 -> 10 (records +4 up event).
			clock.Advance(20 * time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 6, 10
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(10)))

			// t40: load drops again, 10 -> 6. periodStart = 10 - 4(added) + 4(removed)
			// = 10, so -4 reaches 6. (Same-direction-only would inflate to 14, block at 10.)
			clock.Advance(20 * time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 10, 6
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(6)), "cross-direction up event must not inflate the down baseline")
		})

		It("floors the scale-down limit to 0 when a policy would reach negative replicas", func() {
			// Pods policy removes more than the period-start count: 3 - 10 = -7 -> 0.
			down := rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(10))
			res := s.Stabilize(Args{Key: "ns/v", Behavior: behaviorFor(nil, down), CurrentReplicas: 3, DesiredReplicas: 0, MinReplicas: 0, MaxReplicas: 20})
			Expect(res.Replicas).To(Equal(int32(0)), "a negative scale-down limit is floored to 0")
		})
	})

	Describe("tolerance deadband", func() {
		It("suppresses changes within tolerance and allows larger ones", func() {
			up := &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr[int32](0),
				SelectPolicy:               ptr(autoscalingv2.MaxChangePolicySelect),
				Policies:                   []autoscalingv2.HPAScalingPolicy{podsPolicy(1000)},
				Tolerance:                  ptr(resource.MustParse("0.5")),
			}
			args := Args{Key: "ns/v", Behavior: behaviorFor(up, nil), MinReplicas: 1, MaxReplicas: 1000}

			args.CurrentReplicas, args.DesiredReplicas = 10, 12
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(10)), "+20% is within the 50% deadband")

			clock.Advance(time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 10, 20
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(20)), "+100% exceeds the deadband")
		})

		It("suppresses a scale-down within the down-direction tolerance", func() {
			down := &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr[int32](0),
				SelectPolicy:               ptr(autoscalingv2.MaxChangePolicySelect),
				Policies:                   []autoscalingv2.HPAScalingPolicy{podsPolicy(1000)},
				Tolerance:                  ptr(resource.MustParse("0.5")),
			}
			args := Args{Key: "ns/v", Behavior: behaviorFor(nil, down), MinReplicas: 0, MaxReplicas: 20}

			args.CurrentReplicas, args.DesiredReplicas = 10, 9
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(10)), "-10% is within the 50% down deadband")

			clock.Advance(time.Second)
			args.CurrentReplicas, args.DesiredReplicas = 10, 4
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(4)), "-60% exceeds the down deadband")
		})

		It("does not suppress scale-from-zero even when a tolerance is set", func() {
			up := &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr[int32](0),
				SelectPolicy:               ptr(autoscalingv2.MaxChangePolicySelect),
				Policies:                   []autoscalingv2.HPAScalingPolicy{podsPolicy(1000)},
				Tolerance:                  ptr(resource.MustParse("0.9")),
			}
			res := s.Stabilize(Args{Key: "ns/v", Behavior: behaviorFor(up, nil), CurrentReplicas: 0, DesiredReplicas: 2, MinReplicas: 0, MaxReplicas: 100})
			Expect(res.Replicas).To(Equal(int32(2)), "current=0 must not divide-by-zero into a suppressed change")
		})
	})

	Describe("min/max clamping", func() {
		var b *autoscalingv2.HorizontalPodAutoscalerBehavior

		BeforeEach(func() {
			// Generous policies so clamping, not rate, bounds the result.
			up := rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(1000))
			down := rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(1000))
			b = behaviorFor(up, down)
		})

		It("clamps to maxReplicas", func() {
			res := s.Stabilize(Args{Key: "ns/a", Behavior: b, CurrentReplicas: 1, DesiredReplicas: 100, MinReplicas: 1, MaxReplicas: 5})
			Expect(res.Replicas).To(Equal(int32(5)))
		})

		It("clamps to minReplicas", func() {
			res := s.Stabilize(Args{Key: "ns/b", Behavior: b, CurrentReplicas: 10, DesiredReplicas: 0, MinReplicas: 2, MaxReplicas: 20})
			Expect(res.Replicas).To(Equal(int32(2)))
		})

		It("ignores maxReplicas when non-positive", func() {
			up := rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(1000))
			res := s.Stabilize(Args{Key: "ns/v", Behavior: behaviorFor(up, nil), CurrentReplicas: 3, DesiredReplicas: 9, MinReplicas: 1, MaxReplicas: 0})
			Expect(res.Replicas).To(Equal(int32(9)))
		})
	})

	Describe("per-key isolation", func() {
		It("keeps each scale target's history independent", func() {
			s.Stabilize(Args{Key: "ns/a", CurrentReplicas: 8, DesiredReplicas: 8, MinReplicas: 1, MaxReplicas: 20})
			clock.Advance(10 * time.Second)

			byB := s.Stabilize(Args{Key: "ns/b", CurrentReplicas: 8, DesiredReplicas: 2, MinReplicas: 1, MaxReplicas: 20})
			Expect(byB.Replicas).To(Equal(int32(2)), "key b is not held by key a's window")

			byA := s.Stabilize(Args{Key: "ns/a", CurrentReplicas: 8, DesiredReplicas: 2, MinReplicas: 1, MaxReplicas: 20})
			Expect(byA.Replicas).To(Equal(int32(8)), "key a is still held by its own window")
		})
	})

	Describe("Retain", func() {
		It("drops history for keys not in the active set", func() {
			// Build a high down-window history for key a.
			s.Stabilize(Args{Key: "ns/a", CurrentReplicas: 8, DesiredReplicas: 8, MinReplicas: 0, MaxReplicas: 20})
			clock.Advance(10 * time.Second)

			// Retain only key b this cycle.
			s.Retain(map[string]struct{}{"ns/b": {}})

			// With its window gone, key a's scale-down is no longer held at 8.
			got := s.Stabilize(Args{Key: "ns/a", CurrentReplicas: 8, DesiredReplicas: 2, MinReplicas: 0, MaxReplicas: 20}).Replicas
			Expect(got).To(Equal(int32(2)), "dropped history must not damp the next decision")
		})

		It("keeps history for keys still in the active set", func() {
			s.Stabilize(Args{Key: "ns/a", CurrentReplicas: 8, DesiredReplicas: 8, MinReplicas: 0, MaxReplicas: 20})
			clock.Advance(10 * time.Second)
			s.Retain(map[string]struct{}{"ns/a": {}})
			got := s.Stabilize(Args{Key: "ns/a", CurrentReplicas: 8, DesiredReplicas: 2, MinReplicas: 0, MaxReplicas: 20}).Replicas
			Expect(got).To(Equal(int32(8)), "retained window still holds the recent max")
		})

		It("drops all history when the active set is empty", func() {
			s.Stabilize(Args{Key: "ns/a", CurrentReplicas: 8, DesiredReplicas: 8, MinReplicas: 0, MaxReplicas: 20})
			clock.Advance(10 * time.Second)
			s.Retain(map[string]struct{}{})
			got := s.Stabilize(Args{Key: "ns/a", CurrentReplicas: 8, DesiredReplicas: 2, MinReplicas: 0, MaxReplicas: 20}).Replicas
			Expect(got).To(Equal(int32(2)), "an empty active set evicts every key")
		})

		It("clears the rate-event budget so the next cycle starts fresh", func() {
			// First cycle records an up-event: 2->6 hits the +4-pods cap.
			up := rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(4))
			args := Args{Key: "ns/a", Behavior: behaviorFor(up, nil), CurrentReplicas: 2, DesiredReplicas: 10, MinReplicas: 0, MaxReplicas: 100}
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(6)), "rate-limited on the first cycle")

			// Evict the key, then the same period must grant a fresh +4 budget.
			s.Retain(map[string]struct{}{"ns/b": {}})
			args.CurrentReplicas, args.DesiredReplicas = 6, 100
			Expect(s.Stabilize(args).Replicas).To(Equal(int32(10)), "after Retain drops the event, +4 from 6 = 10")
		})
	})

	Describe("Result observability fields", func() {
		It("reports the pre-rate recommendation in Stabilized", func() {
			up := rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(1))
			res := s.Stabilize(Args{Key: "ns/v", Behavior: behaviorFor(up, nil), CurrentReplicas: 2, DesiredReplicas: 9, MinReplicas: 1, MaxReplicas: 100})
			Expect(res.Stabilized).To(Equal(int32(9)), "Stabilized is the window output, before rate limiting")
			Expect(res.Replicas).To(Equal(int32(3)), "Replicas is rate-limited to +1")
		})

		DescribeTable("Reason names the stage responsible for the final value",
			func(args Args, want string) {
				Expect(s.Stabilize(args).Reason).To(ContainSubstring(want))
			},
			Entry("unchanged target",
				Args{Key: "ns/v", CurrentReplicas: 4, DesiredReplicas: 4, MinReplicas: 1, MaxReplicas: 10},
				"no stabilization applied"),
			Entry("rate-limited",
				Args{Key: "ns/v", Behavior: behaviorFor(rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(1)), nil),
					CurrentReplicas: 2, DesiredReplicas: 9, MinReplicas: 1, MaxReplicas: 100},
				"rate-limited"),
			Entry("clamped to max",
				Args{Key: "ns/c", Behavior: behaviorFor(rulesFor(0, autoscalingv2.MaxChangePolicySelect, podsPolicy(1000)), nil),
					CurrentReplicas: 1, DesiredReplicas: 100, MinReplicas: 1, MaxReplicas: 5},
				"clamped to bounds"),
		)
	})
})

var _ = Describe("DefaultBehavior", func() {
	It("sets immediate scale-up and a 300s scale-down window", func() {
		b := DefaultBehavior()
		Expect(b.ScaleUp).NotTo(BeNil())
		Expect(b.ScaleDown).NotTo(BeNil())
		Expect(*b.ScaleUp.StabilizationWindowSeconds).To(Equal(int32(0)))
		Expect(*b.ScaleDown.StabilizationWindowSeconds).To(Equal(int32(DefaultDownscaleStabilizationWindow / time.Second)))
	})
})
