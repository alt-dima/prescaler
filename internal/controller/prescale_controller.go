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
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/robfig/cron"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	prescalerv1 "github.com/alt-dima/prescaler/api/v1"
)

const cpuResourceName = "cpu"

// PrescaleReconciler reconciles a Prescale object
type PrescaleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// hpaProcessing tracks which HPA is currently being processed to prevent race conditions
	hpaProcessing sync.Map
}

// acquireHPALock attempts to acquire a lock for processing a specific HPA
// Returns true if lock was acquired, false if HPA is already being processed
func (r *PrescaleReconciler) acquireHPALock(hpaKey string) bool {
	_, loaded := r.hpaProcessing.LoadOrStore(hpaKey, time.Now())
	return !loaded
}

// releaseHPALock releases the lock for a specific HPA
func (r *PrescaleReconciler) releaseHPALock(hpaKey string) {
	r.hpaProcessing.Delete(hpaKey)
}

// cleanupStaleLocks removes locks that have been held for too long (deadlock prevention)
func (r *PrescaleReconciler) cleanupStaleLocks() {
	now := time.Now()
	r.hpaProcessing.Range(func(key, value interface{}) bool {
		if lockTime, ok := value.(time.Time); ok {
			if now.Sub(lockTime) > 5*time.Minute {
				logf.Log.Info("Cleaning up stale HPA lock", "hpaKey", key, "lockDuration", now.Sub(lockTime))
				r.hpaProcessing.Delete(key)
			}
		}
		return true
	})
}

// +kubebuilder:rbac:groups=prescaler.altuhov.su,resources=prescales,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=prescaler.altuhov.su,resources=prescales/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=prescaler.altuhov.su,resources=prescales/finalizers,verbs=update
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;update;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Prescale object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *PrescaleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Starting Reconcile")

	// Periodic cleanup of stale locks (every 100 reconciliations to avoid performance impact)
	if req.Name != "" && len(req.Name)%100 == 0 {
		r.cleanupStaleLocks()
	}

	// Fetch the Prescaler instance
	prescaler := prescalerv1.Prescale{}
	if err := r.Get(ctx, req.NamespacedName, &prescaler); err != nil {
		log.Error(err, "unable to fetch Prescaler")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Per-HPA concurrency control: prevent multiple reconciliations from processing the same HPA
	hpaKey := fmt.Sprintf("%s/%s", req.Namespace, prescaler.Spec.TargetHpaName)
	if !r.acquireHPALock(hpaKey) {
		log.V(1).Info("HPA is already being processed by another reconciliation, requeuing", "hpaKey", hpaKey)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	defer r.releaseHPALock(hpaKey)

	// Handle orphaned prescale state
	if err := r.handleOrphanedState(ctx, req, &prescaler); err != nil {
		return ctrl.Result{}, err
	}

	if prescaler.Spec.Suspend {
		log.V(1).Info("prescaler suspended, skipping")
		return ctrl.Result{}, nil
	}

	// Process schedules and find next run
	scheduleResult := r.processSchedules(ctx, &prescaler)

	if scheduleResult.shouldSleep {
		return scheduleResult.result, nil
	}

	// Execute prescale operation
	if err := r.executePrescale(ctx, req, &prescaler, scheduleResult); err != nil {
		return ctrl.Result{}, err
	}

	// Wait for HPA to scale if needed
	if err := r.waitForScale(ctx, req, &prescaler, scheduleResult.currentDesiredReplicas); err != nil {
		return ctrl.Result{}, err
	}

	// Revert HPA changes
	if err := r.revertHPA(ctx, req, &prescaler, scheduleResult); err != nil {
		return ctrl.Result{}, err
	}

	// Clear orphaned fields
	if err := r.clearOrphanedFields(ctx, req, &prescaler); err != nil {
		return ctrl.Result{}, err
	}

	return scheduleResult.result, nil
}

// ScheduleResult holds the result of schedule processing
type ScheduleResult struct {
	shouldSleep                               bool
	result                                    ctrl.Result
	bestMissed                                time.Time
	bestMissedSchedule                        *prescalerv1.PrescaleSchedule
	currentDesiredReplicas                    int32
	originalSpecCpuUtilization                *int32
	originalScaleUpStabilizationWindowSeconds *int32
	originalSpecCpuUtilizationIndex           int
}

// handleOrphanedState handles the case where there are orphaned prescale settings
func (r *PrescaleReconciler) handleOrphanedState(ctx context.Context, req ctrl.Request, prescaler *prescalerv1.Prescale) error {
	log := logf.FromContext(ctx)

	prescaleSpecCpuUtilizationOrphaned := prescaler.Status.OrphanedSpecCpuUtilization
	prescaleSpecScaleUpStabilizationWindowSecondsOrphaned := prescaler.Status.OrphanedScaleUpStabilizationWindowSeconds

	if prescaleSpecCpuUtilizationOrphaned == nil && prescaleSpecScaleUpStabilizationWindowSecondsOrphaned == nil {
		return nil
	}

	// found orphaned prescale need to revert in hpa
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: prescaler.Spec.TargetHpaName}, hpa); err != nil {
		log.Error(err, "failed to get HPA for orphaned revert")
		r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedGetHPA", err.Error())
		return client.IgnoreNotFound(err)
	}

	if prescaleSpecScaleUpStabilizationWindowSecondsOrphaned != nil {
		hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds = prescaleSpecScaleUpStabilizationWindowSecondsOrphaned
	}
	if prescaleSpecCpuUtilizationOrphaned != nil {
		for index, metric := range hpa.Spec.Metrics {
			if metric.Resource.Name == cpuResourceName && metric.Resource.Target.AverageUtilization != nil {
				hpa.Spec.Metrics[index].Resource.Target.AverageUtilization = prescaleSpecCpuUtilizationOrphaned
				break
			}
		}
	}

	if err := r.Update(ctx, hpa); err != nil {
		log.Error(err, "failed to update HPA for orphaned revert")
		r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedUpdateHPA", err.Error())
		return client.IgnoreNotFound(err)
	}

	// Re-fetch Prescale for status update orphanedSpecCpuUtilization to nil
	if err := r.Get(ctx, req.NamespacedName, prescaler); err != nil {
		log.Error(err, "unable to re-fetch Prescaler for status update orphanedSpecCpuUtilization to nil")
		return client.IgnoreNotFound(err)
	}

	prescaler.Status.OrphanedSpecCpuUtilization = nil
	prescaler.Status.OrphanedScaleUpStabilizationWindowSeconds = nil

	if err := r.Status().Update(ctx, prescaler); err != nil {
		log.Error(err, "Failed to update status to set orphanedSpecCpuUtilization to nil in orphaned revert")
		r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		return client.IgnoreNotFound(err)
	}

	return nil
}

// processSchedules processes all schedules and determines the next run
func (r *PrescaleReconciler) processSchedules(ctx context.Context, prescaler *prescalerv1.Prescale) *ScheduleResult {
	log := logf.FromContext(ctx)

	now := time.Now()
	var bestNext time.Time
	var bestMissed time.Time
	var bestMissedSchedule *prescalerv1.PrescaleSchedule

	for i := range prescaler.Spec.Schedules {
		sched := &prescaler.Spec.Schedules[i]
		cronSched, err := cron.ParseStandard(sched.Cron)
		if err != nil {
			// skip invalid cron
			continue
		}

		var earliestTime time.Time
		if prescaler.Status.LastScaledTime != nil {
			earliestTime = prescaler.Status.LastScaledTime.Time
		} else {
			earliestTime = prescaler.CreationTimestamp.Time
		}

		if prescaler.Spec.StartingDeadlineSeconds != nil {
			schedulingDeadline := now.Add(-time.Second * time.Duration(*prescaler.Spec.StartingDeadlineSeconds))
			if schedulingDeadline.After(earliestTime) {
				earliestTime = schedulingDeadline
			}
		}

		if earliestTime.After(now) {
			next := cronSched.Next(now)
			if bestNext.IsZero() || next.Before(bestNext) {
				bestNext = next
			}
			continue
		}

		starts := 0
		var lastMissed time.Time
		for t := cronSched.Next(earliestTime); !t.After(now); t = cronSched.Next(t) {
			lastMissed = t
			starts++
			if starts > 100 {
				break
			}
		}

		next := cronSched.Next(now)
		if !lastMissed.IsZero() {
			if bestMissed.IsZero() || lastMissed.After(bestMissed) {
				bestMissed = lastMissed
				bestMissedSchedule = sched
			}
		}

		if bestNext.IsZero() || next.Before(bestNext) {
			bestNext = next
		}
	}

	scheduledResult := ctrl.Result{RequeueAfter: time.Until(bestNext)}
	log = log.WithValues("now", now, "next run", bestNext)

	if bestMissed.IsZero() {
		log.V(1).Info("no upcoming scheduled times, sleeping until next")
		return &ScheduleResult{shouldSleep: true, result: scheduledResult}
	}

	// make sure we're not too late to start the run
	log = log.WithValues("current run", bestMissed)
	tooLate := false
	if prescaler.Spec.StartingDeadlineSeconds != nil {
		tooLate = bestMissed.Add(time.Duration(*prescaler.Spec.StartingDeadlineSeconds) * time.Second).Before(now)
	}
	if tooLate {
		r.Recorder.Event(prescaler, corev1.EventTypeNormal, "MissedStartingDeadline", "missed starting deadline for last run, sleeping till next")
		log.V(1).Info("missed starting deadline for last run, sleeping till next")
		return &ScheduleResult{shouldSleep: true, result: scheduledResult}
	}

	return &ScheduleResult{
		shouldSleep:        false,
		result:             scheduledResult,
		bestMissed:         bestMissed,
		bestMissedSchedule: bestMissedSchedule,
	}
}

// executePrescale executes the prescale operation
func (r *PrescaleReconciler) executePrescale(ctx context.Context, req ctrl.Request, prescaler *prescalerv1.Prescale, scheduleResult *ScheduleResult) error {
	log := logf.FromContext(ctx)

	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: prescaler.Spec.TargetHpaName}, hpa); err != nil {
		log.Error(err, "failed to get hpa for prescale")
		r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedGetHPA", err.Error())
		return client.IgnoreNotFound(err)
	}

	// Find originalSpecCpuUtilization in HPA Spec Metrics for prescale
	var originalSpecCpuUtilization *int32
	var originalSpecCpuUtilizationIndex = -1
	for index, metric := range hpa.Spec.Metrics {
		if metric.Resource.Name == cpuResourceName && metric.Resource.Target.AverageUtilization != nil {
			originalSpecCpuUtilization = metric.Resource.Target.AverageUtilization
			originalSpecCpuUtilizationIndex = index
			break
		}
	}
	if originalSpecCpuUtilizationIndex == -1 || originalSpecCpuUtilization == nil {
		log.Error(fmt.Errorf("failed to find cpu utilization index in HPA for prescale"), "originalSpecCpuUtilizationIndex is -1 or originalSpecCpuUtilization is nil")
		r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedFindCPUUtilizationIndex", "originalSpecCpuUtilizationIndex is -1 or originalSpecCpuUtilization is nil")
		return nil
	}

	var selectedCpuUtilization *int32
	if scheduleResult.bestMissedSchedule.UseCurrentCpuUtilization {
		for _, metric := range hpa.Status.CurrentMetrics {
			if metric.Resource.Name == cpuResourceName && metric.Resource.Current.AverageUtilization != nil {
				selectedCpuUtilization = metric.Resource.Current.AverageUtilization
				break
			}
		}
	}
	if selectedCpuUtilization == nil || *selectedCpuUtilization > *originalSpecCpuUtilization {
		selectedCpuUtilization = originalSpecCpuUtilization
	}

	var originalScaleUpStabilizationWindowSeconds *int32
	if hpa.Spec.Behavior.ScaleUp != nil && hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds != nil {
		originalScaleUpStabilizationWindowSeconds = hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds
	}

	currentStatusDesiredReplicas := hpa.Status.DesiredReplicas
	log.Info("currentStatus", "originalSpecCpuUtilization", originalSpecCpuUtilization, "selectedCpuUtilization", selectedCpuUtilization, "currentStatusDesiredReplicas", currentStatusDesiredReplicas, "originalScaleUpStabilizationWindowSeconds", originalScaleUpStabilizationWindowSeconds)

	// Store values in scheduleResult for later use
	scheduleResult.currentDesiredReplicas = currentStatusDesiredReplicas
	scheduleResult.originalSpecCpuUtilization = originalSpecCpuUtilization
	scheduleResult.originalScaleUpStabilizationWindowSeconds = originalScaleUpStabilizationWindowSeconds
	scheduleResult.originalSpecCpuUtilizationIndex = originalSpecCpuUtilizationIndex

	// Use the percent from the selected schedule
	percent := scheduleResult.bestMissedSchedule.Percent
	prescaleSpecCpuUtilization := int32(float64(*selectedCpuUtilization) * 100 / float64(percent))

	// Re-fetch Prescaler for status update
	if err := r.Get(ctx, req.NamespacedName, prescaler); err != nil {
		log.Error(err, "unable to re-fetch Prescaler after prescale")
		return client.IgnoreNotFound(err)
	}

	prescaler.Status.LastScaledTime = &metav1.Time{Time: scheduleResult.bestMissed}
	prescaler.Status.LastPrescaleSpecCpuUtilization = prescaleSpecCpuUtilization
	prescaler.Status.LastOriginalSpecCpuUtilization = *originalSpecCpuUtilization
	prescaler.Status.OrphanedSpecCpuUtilization = originalSpecCpuUtilization
	prescaler.Status.OrphanedScaleUpStabilizationWindowSeconds = originalScaleUpStabilizationWindowSeconds

	if err := r.Status().Update(ctx, prescaler); err != nil {
		log.Error(err, "Failed to update status for prescale status update")
		r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		return client.IgnoreNotFound(err)
	}

	// Prescale HPA Spec Metrics CPU AverageUtilization to prescaleSpecCpuUtilization
	hpa.Spec.Metrics[originalSpecCpuUtilizationIndex].Resource.Target.AverageUtilization = &prescaleSpecCpuUtilization
	if originalScaleUpStabilizationWindowSeconds != nil {
		zero := int32(0)
		hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds = &zero
	}
	if err := r.Update(ctx, hpa); err != nil {
		log.Error(err, "failed to update hpa for prescale")
		r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedUpdateHPA", err.Error())
		return client.IgnoreNotFound(err)
	}

	log.Info("prescaleSpecCpuUtilization", "value", prescaleSpecCpuUtilization)
	r.Recorder.Event(prescaler, corev1.EventTypeNormal, "Prescaled", fmt.Sprintf("Successfully prescaled HPA to %d%% CPU utilization, currently %d replicas, %d%%", prescaleSpecCpuUtilization, currentStatusDesiredReplicas, percent))

	return nil
}

// waitForScale waits for the HPA to scale up
func (r *PrescaleReconciler) waitForScale(ctx context.Context, req ctrl.Request, prescaler *prescalerv1.Prescale, currentStatusDesiredReplicas int32) error {
	log := logf.FromContext(ctx)

	if prescaler.Spec.RevertWaitSeconds == nil {
		return nil
	}

	log.Info("waiting for HPA to scale up to revertWaitSeconds", "value", *prescaler.Spec.RevertWaitSeconds)

	startTime := time.Now()
	timeout := time.Duration(*prescaler.Spec.RevertWaitSeconds) * time.Second

	for {
		// Check if we've exceeded the timeout
		if time.Since(startTime) > timeout {
			log.Info("timeout reached waiting for HPA to scale")
			break
		}

		// Re-fetch HPA to check current status
		hpa := &autoscalingv2.HorizontalPodAutoscaler{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: prescaler.Spec.TargetHpaName}, hpa); err != nil {
			log.Error(err, "failed to re-fetch hpa while waiting for scale")
			r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedGetHPA", err.Error())
			return err
		}

		// Check if desired replicas has changed
		if hpa.Status.DesiredReplicas > currentStatusDesiredReplicas {
			log.Info("HPA has scaled, proceeding with revert", "desiredReplicas", hpa.Status.DesiredReplicas)
			break
		}

		// Sleep for a short duration before next check
		time.Sleep(3 * time.Second)
	}

	return nil
}

// revertHPA reverts the HPA changes back to original values
func (r *PrescaleReconciler) revertHPA(ctx context.Context, req ctrl.Request, prescaler *prescalerv1.Prescale, scheduleResult *ScheduleResult) error {
	log := logf.FromContext(ctx)

	// Re-fetch HPA for revert
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: prescaler.Spec.TargetHpaName}, hpa); err != nil {
		log.Error(err, "failed to re-fetch hpa for revert")
		r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedGetHPA", err.Error())
		return err
	}

	// Find cpu utilization index in HPA Spec Metrics for revert
	var currentSpecCpuUtilizationIndex = -1
	for index, metric := range hpa.Spec.Metrics {
		if metric.Resource.Name == cpuResourceName && metric.Resource.Target.AverageUtilization != nil {
			currentSpecCpuUtilizationIndex = index
			break
		}
	}
	if currentSpecCpuUtilizationIndex == -1 {
		log.Error(fmt.Errorf("failed to find cpu utilization index in HPA for revert"), "currentSpecCpuUtilizationIndex is -1")
		r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedFindCPUUtilizationIndex", "currentSpecCpuUtilizationIndex is -1")
		return nil
	}

	// Revert HPA Spec Metrics to originalSpecCpuUtilization
	hpa.Spec.Metrics[currentSpecCpuUtilizationIndex].Resource.Target.AverageUtilization = scheduleResult.originalSpecCpuUtilization
	if scheduleResult.originalScaleUpStabilizationWindowSeconds != nil {
		hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds = scheduleResult.originalScaleUpStabilizationWindowSeconds
	}
	if err := r.Update(ctx, hpa); err != nil {
		log.Error(err, "failed to update hpa for revert")
		r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedUpdateHPA", err.Error())
		return client.IgnoreNotFound(err)
	}

	log.Info("RevertedStatus", "revertedSpecCpuUtilization", scheduleResult.originalSpecCpuUtilization, "currentStatusDesiredReplicas", hpa.Status.DesiredReplicas, "originalScaleUpStabilizationWindowSeconds", scheduleResult.originalScaleUpStabilizationWindowSeconds)
	r.Recorder.Event(prescaler, corev1.EventTypeNormal, "Reverted", fmt.Sprintf("Successfully reverted HPA to %d%% CPU utilization, currently %d replicas", *scheduleResult.originalSpecCpuUtilization, hpa.Status.DesiredReplicas))

	return nil
}

// clearOrphanedFields clears the orphaned fields from the prescaler status
func (r *PrescaleReconciler) clearOrphanedFields(ctx context.Context, req ctrl.Request, prescaler *prescalerv1.Prescale) error {
	log := logf.FromContext(ctx)

	// Re-fetch Prescale for status update orphaned fields to nil
	if err := r.Get(ctx, req.NamespacedName, prescaler); err != nil {
		log.Error(err, "unable to re-fetch Prescaler for status update orphaned fields to nil")
		return client.IgnoreNotFound(err)
	}

	// Clear orphaned fields
	prescaler.Status.OrphanedSpecCpuUtilization = nil
	prescaler.Status.OrphanedScaleUpStabilizationWindowSeconds = nil
	if err := r.Status().Update(ctx, prescaler); err != nil {
		log.Error(err, "Failed to update status for status update orphaned fields to nil")
		r.Recorder.Event(prescaler, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		return client.IgnoreNotFound(err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PrescaleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	maxConcurrentReconciles := 5
	if val, ok := os.LookupEnv("MAX_CONCURRENT_RECONCILES"); ok {
		if num, err := strconv.Atoi(val); err == nil {
			maxConcurrentReconciles = num
		}
	}

	// Only reconcile when the generation changes to ignore status updates
	pred := predicate.GenerationChangedPredicate{}

	return ctrl.NewControllerManagedBy(mgr).
		For(&prescalerv1.Prescale{}).
		WithEventFilter(pred).
		Named("prescale").
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxConcurrentReconciles,
		}).
		Complete(r)
}
