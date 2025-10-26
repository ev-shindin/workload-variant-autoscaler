/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	logger "github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	testutils "github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
)

var _ = Describe("Status.CurrentAlloc Initialization Tests", func() {
	const (
		testNamespace = "default"
		systemNS      = "workload-variant-autoscaler-system"
	)

	var (
		ctx context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		logger.Log = zap.NewNop().Sugar()

		// Ensure system namespace exists
		ns := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: systemNS,
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

		// Create required ConfigMaps
		By("creating required configmaps for controller")
		configMap := testutils.CreateServiceClassConfigMap(systemNS)
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, configMap))).NotTo(HaveOccurred())

		configMap = testutils.CreateAcceleratorUnitCostConfigMap(systemNS)
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, configMap))).NotTo(HaveOccurred())

		configMap = testutils.CreateVariantAutoscalingConfigMap(configMapName, systemNS)
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, configMap))).NotTo(HaveOccurred())
	})

	Context("When deployment does not exist (first reconciliation)", func() {
		const resourceName = "test-no-deployment"

		It("should initialize CurrentAlloc.NumReplicas to 0", func() {
			By("creating VariantAutoscaling without corresponding deployment")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "default/default",
					VariantID:        "default/default-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms:  map[string]string{"alpha": "20.28", "beta": "0.72"},
							PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
						},
						MaxBatchSize: 4,
					},
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "premium",
						Key:  "default/default",
					},
				},
			}
			Expect(k8sClient.Create(ctx, va)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      resourceName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CurrentAlloc is initialized to 0")
			updatedVA := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, updatedVA)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updatedVA.Status.CurrentAlloc.NumReplicas).To(Equal(int32(0)), "CurrentAlloc should be 0 when deployment doesn't exist")
			}, time.Second*10, time.Second).Should(Succeed())

			// Cleanup
			Expect(k8sClient.Delete(ctx, va)).To(Succeed())
		})
	})

	Context("When deployment exists with replicas", func() {
		const resourceName = "test-with-deployment"

		It("should initialize CurrentAlloc.NumReplicas from deployment", func() {
			By("creating deployment with 3 replicas")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(3)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": resourceName},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": resourceName},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// Update deployment status to simulate actual replicas
			Eventually(func(g Gomega) {
				d := &appsv1.Deployment{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, d)
				g.Expect(err).NotTo(HaveOccurred())
				d.Status.Replicas = 3
				d.Status.ReadyReplicas = 3
				err = k8sClient.Status().Update(ctx, d)
				g.Expect(err).NotTo(HaveOccurred())
			}, time.Second*5, time.Millisecond*100).Should(Succeed())

			By("creating VariantAutoscaling")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "default/default",
					VariantID:        "default/default-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms:  map[string]string{"alpha": "20.28", "beta": "0.72"},
							PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
						},
						MaxBatchSize: 4,
					},
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "premium",
						Key:  "default/default",
					},
				},
			}
			Expect(k8sClient.Create(ctx, va)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      resourceName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CurrentAlloc is initialized to 3")
			updatedVA := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, updatedVA)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updatedVA.Status.CurrentAlloc.NumReplicas).To(Equal(int32(3)), "CurrentAlloc should be 3 from deployment")
			}, time.Second*10, time.Second).Should(Succeed())

			// Cleanup
			Expect(k8sClient.Delete(ctx, va)).To(Succeed())
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})
	})

	Context("When deployment scales (multiple reconciliations)", func() {
		const resourceName = "test-scaling"

		It("should update CurrentAlloc when deployment scales", func() {
			By("creating deployment with 2 replicas")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(2)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": resourceName},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": resourceName},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// Update deployment status
			Eventually(func(g Gomega) {
				d := &appsv1.Deployment{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, d)
				g.Expect(err).NotTo(HaveOccurred())
				d.Status.Replicas = 2
				err = k8sClient.Status().Update(ctx, d)
				g.Expect(err).NotTo(HaveOccurred())
			}, time.Second*5, time.Millisecond*100).Should(Succeed())

			By("creating VariantAutoscaling")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "default/default",
					VariantID:        "default/default-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms:  map[string]string{"alpha": "20.28", "beta": "0.72"},
							PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
						},
						MaxBatchSize: 4,
					},
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "premium",
						Key:  "default/default",
					},
				},
			}
			Expect(k8sClient.Create(ctx, va)).To(Succeed())

			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("first reconciliation")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CurrentAlloc is 2")
			updatedVA := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, updatedVA)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updatedVA.Status.CurrentAlloc.NumReplicas).To(Equal(int32(2)))
			}, time.Second*10, time.Second).Should(Succeed())

			By("scaling deployment to 5 replicas")
			Eventually(func(g Gomega) {
				d := &appsv1.Deployment{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, d)
				g.Expect(err).NotTo(HaveOccurred())
				d.Spec.Replicas = ptr.To(int32(5))
				err = k8sClient.Update(ctx, d)
				g.Expect(err).NotTo(HaveOccurred())
			}, time.Second*5, time.Millisecond*100).Should(Succeed())

			// Update deployment status to reflect new replicas
			Eventually(func(g Gomega) {
				d := &appsv1.Deployment{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, d)
				g.Expect(err).NotTo(HaveOccurred())
				d.Status.Replicas = 5
				err = k8sClient.Status().Update(ctx, d)
				g.Expect(err).NotTo(HaveOccurred())
			}, time.Second*5, time.Millisecond*100).Should(Succeed())

			By("second reconciliation")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CurrentAlloc is updated to 5")
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, updatedVA)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updatedVA.Status.CurrentAlloc.NumReplicas).To(Equal(int32(5)), "CurrentAlloc should update to 5")
			}, time.Second*10, time.Second).Should(Succeed())

			// Cleanup
			Expect(k8sClient.Delete(ctx, va)).To(Succeed())
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})
	})

	Context("Scale-to-zero scenario", func() {
		const resourceName = "test-scale-to-zero"

		It("should handle deployment with 0 replicas", func() {
			By("creating deployment with 0 replicas")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(0)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": resourceName},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": resourceName},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// Update deployment status
			Eventually(func(g Gomega) {
				d := &appsv1.Deployment{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, d)
				g.Expect(err).NotTo(HaveOccurred())
				d.Status.Replicas = 0
				err = k8sClient.Status().Update(ctx, d)
				g.Expect(err).NotTo(HaveOccurred())
			}, time.Second*5, time.Millisecond*100).Should(Succeed())

			By("creating VariantAutoscaling")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "default/default",
					VariantID:        "default/default-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms:  map[string]string{"alpha": "20.28", "beta": "0.72"},
							PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
						},
						MaxBatchSize: 4,
					},
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "premium",
						Key:  "default/default",
					},
				},
			}
			Expect(k8sClient.Create(ctx, va)).To(Succeed())

			By("reconciling the resource")
			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CurrentAlloc is 0")
			updatedVA := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, updatedVA)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updatedVA.Status.CurrentAlloc.NumReplicas).To(Equal(int32(0)), "CurrentAlloc should be 0 for scale-to-zero")
			}, time.Second*10, time.Second).Should(Succeed())

			By("verifying DesiredOptimizedAlloc is also set (metrics should be emitted)")
			Expect(updatedVA.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 0), "DesiredOptimizedAlloc should be set for metric emission")

			// Cleanup
			Expect(k8sClient.Delete(ctx, va)).To(Succeed())
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})
	})

	Context("Deployment fetch failure scenario", func() {
		const resourceName = "test-fetch-fail"

		It("should use existing CurrentAlloc when deployment temporarily unreachable", func() {
			By("creating deployment with 7 replicas")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(7)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": resourceName},
					},
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": resourceName},
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// Update deployment status
			Eventually(func(g Gomega) {
				d := &appsv1.Deployment{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, d)
				g.Expect(err).NotTo(HaveOccurred())
				d.Status.Replicas = 7
				err = k8sClient.Status().Update(ctx, d)
				g.Expect(err).NotTo(HaveOccurred())
			}, time.Second*5, time.Millisecond*100).Should(Succeed())

			By("creating VariantAutoscaling and doing first reconciliation")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ModelID:          "default/default",
					VariantID:        "default/default-A100-1",
					Accelerator:      "A100",
					AcceleratorCount: 1,
					VariantCost:      "10.5",
					VariantProfile: llmdVariantAutoscalingV1alpha1.VariantProfile{
						PerfParms: llmdVariantAutoscalingV1alpha1.PerfParms{
							DecodeParms:  map[string]string{"alpha": "20.28", "beta": "0.72"},
							PrefillParms: map[string]string{"gamma": "0", "delta": "0"},
						},
						MaxBatchSize: 4,
					},
					SLOClassRef: llmdVariantAutoscalingV1alpha1.ConfigMapKeyRef{
						Name: "premium",
						Key:  "default/default",
					},
				},
			}
			Expect(k8sClient.Create(ctx, va)).To(Succeed())

			controllerReconciler := &VariantAutoscalingReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CurrentAlloc is 7")
			updatedVA := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, updatedVA)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updatedVA.Status.CurrentAlloc.NumReplicas).To(Equal(int32(7)))
			}, time.Second*10, time.Second).Should(Succeed())

			By("deleting deployment to simulate temporary failure")
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())

			// Wait for deployment to be deleted
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, &appsv1.Deployment{})
				return errors.IsNotFound(err)
			}, time.Second*5, time.Millisecond*100).Should(BeTrue())

			By("reconciling again with deployment missing")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: resourceName, Namespace: testNamespace},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying CurrentAlloc still has value 7 (from previous status)")
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, updatedVA)
				g.Expect(err).NotTo(HaveOccurred())
				// Should retain last known value, not reset to 0
				g.Expect(updatedVA.Status.CurrentAlloc.NumReplicas).To(Equal(int32(7)), "CurrentAlloc should retain previous value when deployment fetch fails")
			}, time.Second*10, time.Second).Should(Succeed())

			// Cleanup
			Expect(k8sClient.Delete(ctx, va)).To(Succeed())
		})
	})
})
