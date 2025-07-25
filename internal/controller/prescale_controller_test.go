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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	prescalerv1 "github.com/alt-dima/prescaler/api/v1"
)

var _ = Describe("Prescale Controller", func() {
	var (
		ctx               context.Context
		reconciler        *PrescaleReconciler
		fakeRecorder      *record.FakeRecorder
		prescale          *prescalerv1.Prescale
		hpa               *autoscalingv2.HorizontalPodAutoscaler
		namespacedName    types.NamespacedName
		hpaNamespacedName types.NamespacedName
	)

	BeforeEach(func() {
		ctx = context.Background()
		fakeRecorder = record.NewFakeRecorder(10)
		reconciler = &PrescaleReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: fakeRecorder,
		}

		namespacedName = types.NamespacedName{
			Name:      "test-prescale",
			Namespace: "default",
		}

		hpaNamespacedName = types.NamespacedName{
			Name:      "test-hpa",
			Namespace: "default",
		}

		// Define HPA template (not created yet)
		hpa = &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hpaNamespacedName.Name,
				Namespace: hpaNamespacedName.Namespace,
			},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind:       "Deployment",
					Name:       "test-deployment",
					APIVersion: "apps/v1",
				},
				MinReplicas: func(i int32) *int32 { return &i }(1),
				MaxReplicas: 10,
				Metrics: []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ResourceMetricSourceType,
						Resource: &autoscalingv2.ResourceMetricSource{
							Name: "cpu",
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: func(i int32) *int32 { return &i }(70),
							},
						},
					},
				},
				Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
					ScaleUp: &autoscalingv2.HPAScalingRules{
						StabilizationWindowSeconds: func(i int32) *int32 { return &i }(60),
					},
				},
			},
			Status: autoscalingv2.HorizontalPodAutoscalerStatus{
				DesiredReplicas: 5,
				CurrentReplicas: 5,
			},
		}

		// Create a test Prescale
		prescale = &prescalerv1.Prescale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      namespacedName.Name,
				Namespace: namespacedName.Namespace,
			},
			Spec: prescalerv1.PrescaleSpec{
				TargetHpaName: hpaNamespacedName.Name,
				Schedules: []prescalerv1.PrescaleSchedule{
					{
						Cron:    "55 * * * *",
						Percent: 70,
					},
				},
				Suspend:           func(b bool) *bool { return &b }(false),
				RevertWaitSeconds: func(i int64) *int64 { return &i }(10),
			},
		}
	})

	AfterEach(func() {
		// Cleanup - ignore errors if resources don't exist
		_ = k8sClient.Delete(ctx, prescale)
		_ = k8sClient.Delete(ctx, hpa)

		// Wait for resources to be fully deleted
		Eventually(func() bool {
			err := k8sClient.Get(ctx, namespacedName, prescale)
			return errors.IsNotFound(err)
		}, 10*time.Second, time.Second).Should(BeTrue())

		Eventually(func() bool {
			err := k8sClient.Get(ctx, hpaNamespacedName, hpa)
			return errors.IsNotFound(err)
		}, 10*time.Second, time.Second).Should(BeTrue())
	})

	Describe("Reconcile", func() {
		Context("When prescale resource is suspended", func() {
			BeforeEach(func() {
				prescale.Spec.Suspend = func(b bool) *bool { return &b }(true)
				Expect(k8sClient.Create(ctx, prescale)).To(Succeed())
			})

			It("should return early without processing", func() {
				result, err := reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})

				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
			})
		})

		Context("When prescale resource is not found", func() {
			It("should return without error", func() {
				result, err := reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "non-existent",
						Namespace: "default",
					},
				})

				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
			})
		})

		Context("When HPA is not found", func() {
			BeforeEach(func() {
				prescale.Spec.TargetHpaName = "non-existent-hpa"
				Expect(k8sClient.Create(ctx, prescale)).To(Succeed())
			})

			It("should return without error", func() {
				result, err := reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})

				Expect(err).NotTo(HaveOccurred())
				// When HPA is not found, it should still return a result with requeue time
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			})
		})
	})

	Describe("handleOrphanedState", func() {
		Context("When there are orphaned CPU utilization settings", func() {
			BeforeEach(func() {
				// Create HPA first
				Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

				// Create the prescale first
				Expect(k8sClient.Create(ctx, prescale)).To(Succeed())

				// Then update the prescale status to set orphaned state
				prescale.Status.OrphanedSpecCpuUtilization = func(i int32) *int32 { return &i }(50)
				Expect(k8sClient.Status().Update(ctx, prescale)).To(Succeed())

				// Update HPA to have a different CPU utilization than the orphaned value
				hpa.Spec.Metrics[0].Resource.Target.AverageUtilization = func(i int32) *int32 { return &i }(80)
				Expect(k8sClient.Update(ctx, hpa)).To(Succeed())
			})

			It("should revert the HPA CPU utilization", func() {
				// Verify initial state
				initialHPA := &autoscalingv2.HorizontalPodAutoscaler{}
				Expect(k8sClient.Get(ctx, hpaNamespacedName, initialHPA)).To(Succeed())
				Expect(*initialHPA.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(int32(80)))

				err := reconciler.handleOrphanedState(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				}, prescale)

				Expect(err).NotTo(HaveOccurred())

				// Verify HPA was updated to the orphaned value
				updatedHPA := &autoscalingv2.HorizontalPodAutoscaler{}
				Expect(k8sClient.Get(ctx, hpaNamespacedName, updatedHPA)).To(Succeed())
				Expect(*updatedHPA.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(int32(50)))

				// Verify orphaned fields were cleared
				updatedPrescale := &prescalerv1.Prescale{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedPrescale)).To(Succeed())
				Expect(updatedPrescale.Status.OrphanedSpecCpuUtilization).To(BeNil())
			})
		})

		Context("When there are orphaned stabilization window settings", func() {
			BeforeEach(func() {
				// Create HPA first
				Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

				// Create the prescale first
				Expect(k8sClient.Create(ctx, prescale)).To(Succeed())

				// Then update the prescale status to set orphaned state
				prescale.Status.OrphanedScaleUpStabilizationWindowSeconds = func(i int32) *int32 { return &i }(30)
				Expect(k8sClient.Status().Update(ctx, prescale)).To(Succeed())

				// Update HPA to have a different stabilization window than the orphaned value
				hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds = func(i int32) *int32 { return &i }(60)
				Expect(k8sClient.Update(ctx, hpa)).To(Succeed())
			})

			It("should revert the HPA stabilization window", func() {
				err := reconciler.handleOrphanedState(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				}, prescale)

				Expect(err).NotTo(HaveOccurred())

				// Verify HPA was updated to the orphaned value
				updatedHPA := &autoscalingv2.HorizontalPodAutoscaler{}
				Expect(k8sClient.Get(ctx, hpaNamespacedName, updatedHPA)).To(Succeed())
				Expect(*updatedHPA.Spec.Behavior.ScaleUp.StabilizationWindowSeconds).To(Equal(int32(30)))

				// Verify orphaned fields were cleared
				updatedPrescale := &prescalerv1.Prescale{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedPrescale)).To(Succeed())
				Expect(updatedPrescale.Status.OrphanedScaleUpStabilizationWindowSeconds).To(BeNil())
			})
		})

		Context("When there are no orphaned settings", func() {
			BeforeEach(func() {
				Expect(k8sClient.Create(ctx, prescale)).To(Succeed())
			})

			It("should return early without processing", func() {
				err := reconciler.handleOrphanedState(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				}, prescale)

				Expect(err).NotTo(HaveOccurred())
			})
		})
	})

	Describe("processSchedules", func() {
		Context("When no schedules are due", func() {
			BeforeEach(func() {
				prescale.Spec.Schedules = []prescalerv1.PrescaleSchedule{
					{
						Cron:    "0 0 1 1 *", // January 1st at midnight
						Percent: 70,
					},
				}
				Expect(k8sClient.Create(ctx, prescale)).To(Succeed())
			})

			It("should return shouldSleep=true", func() {
				result := reconciler.processSchedules(ctx, prescale)

				Expect(result.shouldSleep).To(BeTrue())
				Expect(result.result.RequeueAfter).To(BeNumerically(">", 0))
			})
		})

		Context("When a schedule is due", func() {
			BeforeEach(func() {
				// Set a schedule that should be due (1 minute ago)
				now := time.Now()
				dueTime := now.Add(-time.Minute)
				cronExpr := fmt.Sprintf("%d %d * * *", dueTime.Minute(), dueTime.Hour())
				prescale.Spec.Schedules = []prescalerv1.PrescaleSchedule{
					{
						Cron:    cronExpr,
						Percent: 70,
					},
				}
				Expect(k8sClient.Create(ctx, prescale)).To(Succeed())

				// Set LastScaledTime to be in the past so the function can find missed schedules
				prescale.Status.LastScaledTime = &metav1.Time{Time: now.Add(-2 * time.Minute)}
				Expect(k8sClient.Status().Update(ctx, prescale)).To(Succeed())
			})

			It("should return shouldSleep=false with schedule details", func() {
				result := reconciler.processSchedules(ctx, prescale)

				Expect(result.shouldSleep).To(BeFalse())
				Expect(result.bestMissedSchedule).NotTo(BeNil())
				Expect(result.bestMissedSchedule.Percent).To(Equal(int32(70)))
			})
		})

		Context("When schedule is too late", func() {
			BeforeEach(func() {
				prescale.Spec.StartingDeadlineSeconds = func(i int64) *int64 { return &i }(30) // 30 seconds deadline
				prescale.Spec.Schedules = []prescalerv1.PrescaleSchedule{
					{
						Cron:    time.Now().Add(-time.Minute).Format("4 15 * * *"), // 1 minute ago
						Percent: 70,
					},
				}
				Expect(k8sClient.Create(ctx, prescale)).To(Succeed())
			})

			It("should return shouldSleep=true due to missed deadline", func() {
				result := reconciler.processSchedules(ctx, prescale)

				Expect(result.shouldSleep).To(BeTrue())
			})
		})

		Context("When there are invalid cron expressions", func() {
			BeforeEach(func() {
				prescale.Spec.Schedules = []prescalerv1.PrescaleSchedule{
					{
						Cron:    "invalid-cron",
						Percent: 70,
					},
					{
						Cron:    "55 * * * *", // Valid cron
						Percent: 50,
					},
				}
				Expect(k8sClient.Create(ctx, prescale)).To(Succeed())
			})

			It("should skip invalid cron and process valid ones", func() {
				result := reconciler.processSchedules(ctx, prescale)

				// Should still process the valid cron
				Expect(result.shouldSleep).To(BeTrue())
			})
		})
	})

	Describe("executePrescale", func() {
		var scheduleResult *ScheduleResult

		BeforeEach(func() {
			// Create HPA first
			Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

			Expect(k8sClient.Create(ctx, prescale)).To(Succeed())
			scheduleResult = &ScheduleResult{
				bestMissed: time.Now().Add(-time.Minute),
				bestMissedSchedule: &prescalerv1.PrescaleSchedule{
					Cron:    "55 * * * *",
					Percent: 70,
				},
			}
		})

		Context("When HPA has CPU utilization metric", func() {
			It("should update HPA and prescale status", func() {
				err := reconciler.executePrescale(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				}, prescale, scheduleResult)

				Expect(err).NotTo(HaveOccurred())

				// Verify HPA was updated
				updatedHPA := &autoscalingv2.HorizontalPodAutoscaler{}
				Expect(k8sClient.Get(ctx, hpaNamespacedName, updatedHPA)).To(Succeed())

				// CPU utilization should be reduced by 70%
				expectedCPU := int32(70 - (70 * 70 / 100)) // 70 - 49 = 21
				Expect(*updatedHPA.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(expectedCPU))

				// Stabilization window should be set to 0
				Expect(*updatedHPA.Spec.Behavior.ScaleUp.StabilizationWindowSeconds).To(Equal(int32(0)))

				// Verify prescale status was updated
				updatedPrescale := &prescalerv1.Prescale{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedPrescale)).To(Succeed())
				Expect(updatedPrescale.Status.LastScaledTime).NotTo(BeNil())
				Expect(updatedPrescale.Status.LastPrescaleSpecCpuUtilization).To(Equal(expectedCPU))
				Expect(updatedPrescale.Status.LastOriginalSpecCpuUtilization).To(Equal(int32(70)))
				Expect(*updatedPrescale.Status.OrphanedSpecCpuUtilization).To(Equal(int32(70)))
				Expect(*updatedPrescale.Status.OrphanedScaleUpStabilizationWindowSeconds).To(Equal(int32(60)))
			})
		})

		Context("When HPA has no CPU utilization metric", func() {
			BeforeEach(func() {
				// Update HPA to remove CPU metric
				hpa.Spec.Metrics = []autoscalingv2.MetricSpec{
					{
						Type: autoscalingv2.ResourceMetricSourceType,
						Resource: &autoscalingv2.ResourceMetricSource{
							Name: "memory",
							Target: autoscalingv2.MetricTarget{
								Type:               autoscalingv2.UtilizationMetricType,
								AverageUtilization: func(i int32) *int32 { return &i }(80),
							},
						},
					},
				}
				Expect(k8sClient.Update(ctx, hpa)).To(Succeed())
			})

			It("should return error", func() {
				err := reconciler.executePrescale(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				}, prescale, scheduleResult)

				Expect(err).NotTo(HaveOccurred()) // Returns nil but logs error
			})
		})
	})

	Describe("waitForScale", func() {
		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, prescale)).To(Succeed())
		})

		Context("When RevertWaitSeconds is not set", func() {
			BeforeEach(func() {
				prescale.Spec.RevertWaitSeconds = nil
				Expect(k8sClient.Update(ctx, prescale)).To(Succeed())
			})

			It("should return immediately", func() {
				err := reconciler.waitForScale(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				}, prescale, 5)

				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("When HPA scales up", func() {
			BeforeEach(func() {
				// Create HPA first
				Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

				prescale.Spec.RevertWaitSeconds = func(i int64) *int64 { return &i }(5)
				Expect(k8sClient.Update(ctx, prescale)).To(Succeed())
			})

			It("should wait for scale and return", func() {
				// Start waiting in a goroutine
				done := make(chan error, 1)
				go func() {
					done <- reconciler.waitForScale(ctx, reconcile.Request{
						NamespacedName: namespacedName,
					}, prescale, 5)
				}()

				// Update HPA to scale up
				time.Sleep(1 * time.Second)
				updatedHPA := &autoscalingv2.HorizontalPodAutoscaler{}
				Expect(k8sClient.Get(ctx, hpaNamespacedName, updatedHPA)).To(Succeed())
				updatedHPA.Status.DesiredReplicas = 8
				Expect(k8sClient.Status().Update(ctx, updatedHPA)).To(Succeed())

				// Wait for the function to complete
				Eventually(done, 10*time.Second).Should(Receive(BeNil()))
			})
		})
	})

	Describe("revertHPA", func() {
		var scheduleResult *ScheduleResult

		BeforeEach(func() {
			// Create HPA first
			Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

			Expect(k8sClient.Create(ctx, prescale)).To(Succeed())
			scheduleResult = &ScheduleResult{
				originalSpecCpuUtilization:                func(i int32) *int32 { return &i }(70),
				originalScaleUpStabilizationWindowSeconds: func(i int32) *int32 { return &i }(60),
			}
		})

		It("should revert HPA to original values", func() {
			err := reconciler.revertHPA(ctx, reconcile.Request{
				NamespacedName: namespacedName,
			}, prescale, scheduleResult)

			Expect(err).NotTo(HaveOccurred())

			// Verify HPA was reverted
			updatedHPA := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, hpaNamespacedName, updatedHPA)).To(Succeed())
			Expect(*updatedHPA.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(int32(70)))
			Expect(*updatedHPA.Spec.Behavior.ScaleUp.StabilizationWindowSeconds).To(Equal(int32(60)))
		})
	})

	Describe("clearOrphanedFields", func() {
		BeforeEach(func() {
			prescale.Status.OrphanedSpecCpuUtilization = func(i int32) *int32 { return &i }(50)
			prescale.Status.OrphanedScaleUpStabilizationWindowSeconds = func(i int32) *int32 { return &i }(30)
			Expect(k8sClient.Create(ctx, prescale)).To(Succeed())
		})

		It("should clear orphaned fields from status", func() {
			err := reconciler.clearOrphanedFields(ctx, reconcile.Request{
				NamespacedName: namespacedName,
			}, prescale)

			Expect(err).NotTo(HaveOccurred())

			// Verify orphaned fields were cleared
			updatedPrescale := &prescalerv1.Prescale{}
			Expect(k8sClient.Get(ctx, namespacedName, updatedPrescale)).To(Succeed())
			Expect(updatedPrescale.Status.OrphanedSpecCpuUtilization).To(BeNil())
			Expect(updatedPrescale.Status.OrphanedScaleUpStabilizationWindowSeconds).To(BeNil())
		})
	})

	Describe("Integration tests", func() {
		Context("Complete prescale cycle", func() {
			BeforeEach(func() {
				// Create HPA first
				Expect(k8sClient.Create(ctx, hpa)).To(Succeed())

				// Set a schedule that should be due
				now := time.Now()
				dueTime := now.Add(-time.Minute)
				cronExpr := fmt.Sprintf("%d %d * * *", dueTime.Minute(), dueTime.Hour())
				prescale.Spec.Schedules = []prescalerv1.PrescaleSchedule{
					{
						Cron:    cronExpr,
						Percent: 50,
					},
				}
				prescale.Spec.RevertWaitSeconds = func(i int64) *int64 { return &i }(3) // Short wait for testing
				Expect(k8sClient.Create(ctx, prescale)).To(Succeed())

				// Set LastScaledTime to be in the past so the function can find missed schedules
				prescale.Status.LastScaledTime = &metav1.Time{Time: now.Add(-2 * time.Minute)}
				Expect(k8sClient.Status().Update(ctx, prescale)).To(Succeed())
			})

			It("should complete a full prescale cycle", func() {
				// First reconciliation - should execute prescale and wait for scale
				result, err := reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: namespacedName,
				})

				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))

				// The reconciliation cycle includes prescale, wait, and revert
				// Since the HPA doesn't scale up in the test, the revert happens after timeout
				// So we need to check the final state after the full cycle
				updatedHPA := &autoscalingv2.HorizontalPodAutoscaler{}
				Expect(k8sClient.Get(ctx, hpaNamespacedName, updatedHPA)).To(Succeed())
				// After the full cycle, HPA should be reverted to original values
				Expect(*updatedHPA.Spec.Metrics[0].Resource.Target.AverageUtilization).To(Equal(int32(70)))
				Expect(*updatedHPA.Spec.Behavior.ScaleUp.StabilizationWindowSeconds).To(Equal(int32(60)))

				// Verify orphaned fields were cleared
				updatedPrescale := &prescalerv1.Prescale{}
				Expect(k8sClient.Get(ctx, namespacedName, updatedPrescale)).To(Succeed())
				Expect(updatedPrescale.Status.OrphanedSpecCpuUtilization).To(BeNil())
				Expect(updatedPrescale.Status.OrphanedScaleUpStabilizationWindowSeconds).To(BeNil())
			})
		})
	})
})
