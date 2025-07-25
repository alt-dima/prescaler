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

	"github.com/robfig/cron"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	prescalerv1 "github.com/alt-dima/prescaler/api/v1"
)

// PrescaleReconciler reconciles a Prescale object
type PrescaleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
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

	// Fetch the Prescaler instance
	prescaler := prescalerv1.Prescale{}
	if err := r.Get(ctx, req.NamespacedName, &prescaler); err != nil {
		log.Error(err, "unable to fetch Prescaler")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	prescaleSpecCpuUtilizationOrphaned := prescaler.Status.OrphanedSpecCpuUtilization
	prescaleSpecScaleUpStabilizationWindowSecondsOrphaned := prescaler.Status.OrphanedScaleUpStabilizationWindowSeconds
	if prescaleSpecCpuUtilizationOrphaned != nil || prescaleSpecScaleUpStabilizationWindowSecondsOrphaned != nil {
		//found orphaned prescale need to revert in hpa
		hpa := &autoscalingv2.HorizontalPodAutoscaler{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: prescaler.Spec.TargetHpaName}, hpa); err != nil {
			log.Error(err, "failed to get HPA for orphaned revert")
			r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedGetHPA", err.Error())
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}

		if prescaleSpecScaleUpStabilizationWindowSecondsOrphaned != nil {
			hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds = prescaleSpecScaleUpStabilizationWindowSecondsOrphaned
		}
		if prescaleSpecCpuUtilizationOrphaned != nil {
			for index, metric := range hpa.Spec.Metrics {
				if metric.Resource.Name == "cpu" && metric.Resource.Target.AverageUtilization != nil {
					hpa.Spec.Metrics[index].Resource.Target.AverageUtilization = prescaleSpecCpuUtilizationOrphaned
					break
				}
			}
		}

		err := r.Update(ctx, hpa)
		if err != nil {
			log.Error(err, "failed to update HPA for orphaned revert")
			r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateHPA", err.Error())
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		// Re-fetch Prescale for status update orphanedSpecCpuUtilization to nil
		if err := r.Get(ctx, req.NamespacedName, &prescaler); err != nil {
			log.Error(err, "unable to re-fetch Prescaler for status update orphanedSpecCpuUtilization to nil")
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		prescaler.Status.OrphanedSpecCpuUtilization = nil
		prescaler.Status.OrphanedScaleUpStabilizationWindowSeconds = nil
		if err := r.Status().Update(ctx, &prescaler); err != nil {
			log.Error(err, "Failed to update status to set orphanedSpecCpuUtilization to nil in orphaned revert")
			r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	if prescaler.Spec.Suspend != nil && *prescaler.Spec.Suspend {
		log.V(1).Info("prescaler suspended, skipping")
		return ctrl.Result{}, nil
	}

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
		return scheduledResult, nil
	}

	// make sure we're not too late to start the run
	log = log.WithValues("current run", bestMissed)
	tooLate := false
	if prescaler.Spec.StartingDeadlineSeconds != nil {
		tooLate = bestMissed.Add(time.Duration(*prescaler.Spec.StartingDeadlineSeconds) * time.Second).Before(now)
	}
	if tooLate {
		r.Recorder.Event(&prescaler, corev1.EventTypeNormal, "MissedStartingDeadline", "missed starting deadline for last run, sleeping till next")
		log.V(1).Info("missed starting deadline for last run, sleeping till next")
		return scheduledResult, nil
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: prescaler.Spec.TargetHpaName}, hpa); err != nil {
		log.Error(err, "failed to get hpa for prescale")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedGetHPA", err.Error())
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Find originalSpecCpuUtilization in HPA Spec Metrics for prescale
	var originalSpecCpuUtilization *int32
	var originalScaleUpStabilizationWindowSeconds *int32
	var originalSpecCpuUtilizationIndex int = -1
	for index, metric := range hpa.Spec.Metrics {
		if metric.Resource.Name == "cpu" && metric.Resource.Target.AverageUtilization != nil {
			originalSpecCpuUtilization = metric.Resource.Target.AverageUtilization
			originalSpecCpuUtilizationIndex = index
			break
		}
	}

	if hpa.Spec.Behavior.ScaleUp != nil && hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds != nil {
		originalScaleUpStabilizationWindowSeconds = hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds
	}

	if originalSpecCpuUtilizationIndex == -1 {
		log.Error(fmt.Errorf("failed to find cpu utilization index in HPA for prescale"), "originalSpecCpuUtilizationIndex is -1")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedFindCPUUtilizationIndex", "originalSpecCpuUtilizationIndex is -1")
		return ctrl.Result{}, nil
	}

	currentStatusDesiredReplicas := hpa.Status.DesiredReplicas
	log.Info("currentStatus", "originalSpecCpuUtilization", originalSpecCpuUtilization, "currentStatusDesiredReplicas", currentStatusDesiredReplicas, "originalScaleUpStabilizationWindowSeconds", originalScaleUpStabilizationWindowSeconds)

	// Use the percent from the selected schedule
	percent := bestMissedSchedule.Percent
	prescaleSpecCpuUtilization := *originalSpecCpuUtilization - int32(float64(*originalSpecCpuUtilization)*float64(percent)/100)

	// Re-fetch Prescaler for status update
	if err := r.Get(ctx, req.NamespacedName, &prescaler); err != nil {
		log.Error(err, "unable to re-fetch Prescaler after prescale")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	prescaler.Status.LastScaledTime = &metav1.Time{Time: bestMissed}
	prescaler.Status.LastPrescaleSpecCpuUtilization = prescaleSpecCpuUtilization
	prescaler.Status.LastOriginalSpecCpuUtilization = *originalSpecCpuUtilization
	prescaler.Status.OrphanedSpecCpuUtilization = originalSpecCpuUtilization
	prescaler.Status.OrphanedScaleUpStabilizationWindowSeconds = originalScaleUpStabilizationWindowSeconds

	if err := r.Status().Update(ctx, &prescaler); err != nil {
		log.Error(err, "Failed to update status for prescale status update")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Prescale HPA Spec Metrics CPU AverageUtilization to prescaleSpecCpuUtilization
	hpa.Spec.Metrics[originalSpecCpuUtilizationIndex].Resource.Target.AverageUtilization = &prescaleSpecCpuUtilization
	if originalScaleUpStabilizationWindowSeconds != nil {
		zero := int32(0)
		hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds = &zero
	}
	if err := r.Update(ctx, hpa); err != nil {
		log.Error(err, "failed to update hpa for prescale")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateHPA", err.Error())
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("prescaleSpecCpuUtilization", "value", prescaleSpecCpuUtilization)
	r.Recorder.Event(&prescaler, corev1.EventTypeNormal, "Prescaled", fmt.Sprintf("Successfully prescaled HPA to %d%% CPU utilization", prescaleSpecCpuUtilization))

	// Sleep for revertWaitSeconds seconds
	if prescaler.Spec.RevertWaitSeconds != nil {
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
			if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: prescaler.Spec.TargetHpaName}, hpa); err != nil {
				log.Error(err, "failed to re-fetch hpa while waiting for scale")
				r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedGetHPA", err.Error())
				return ctrl.Result{}, err
			}

			// Check if desired replicas has changed
			if hpa.Status.DesiredReplicas > currentStatusDesiredReplicas {
				log.Info("HPA has scaled, proceeding with revert", "desiredReplicas", hpa.Status.DesiredReplicas)
				break
			}

			// Sleep for a short duration before next check
			time.Sleep(3 * time.Second)
		}
	}

	// Re-fetch HPA for revert
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: prescaler.Spec.TargetHpaName}, hpa); err != nil {
		log.Error(err, "failed to re-fetch hpa for revert")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedGetHPA", err.Error())
		return ctrl.Result{}, err
	}

	// Find cpu utilization index in HPA Spec Metrics for revert
	var currentSpecCpuUtilizationIndex int = -1
	for index, metric := range hpa.Spec.Metrics {
		if metric.Resource.Name == "cpu" && metric.Resource.Target.AverageUtilization != nil {
			currentSpecCpuUtilizationIndex = index
			break
		}
	}
	if currentSpecCpuUtilizationIndex == -1 {
		log.Error(fmt.Errorf("failed to find cpu utilization index in HPA for revert"), "currentSpecCpuUtilizationIndex is -1")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedFindCPUUtilizationIndex", "currentSpecCpuUtilizationIndex is -1")
		return ctrl.Result{}, nil
	}

	// Revert HPA Spec Metrics to originalSpecCpuUtilization
	hpa.Spec.Metrics[currentSpecCpuUtilizationIndex].Resource.Target.AverageUtilization = originalSpecCpuUtilization
	if originalScaleUpStabilizationWindowSeconds != nil {
		hpa.Spec.Behavior.ScaleUp.StabilizationWindowSeconds = originalScaleUpStabilizationWindowSeconds
	}
	if err := r.Update(ctx, hpa); err != nil {
		log.Error(err, "failed to update hpa for revert")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateHPA", err.Error())
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("RevertedStatus", "revertedSpecCpuUtilization", originalSpecCpuUtilization, "currentStatusDesiredReplicas", hpa.Status.DesiredReplicas, "originalScaleUpStabilizationWindowSeconds", originalScaleUpStabilizationWindowSeconds)
	r.Recorder.Event(&prescaler, corev1.EventTypeNormal, "Reverted", fmt.Sprintf("Successfully reverted HPA to %d%% CPU utilization and %d replicas", *originalSpecCpuUtilization, hpa.Status.DesiredReplicas))

	// Re-fetch Prescale for status update orphaned fields to nil
	if err := r.Get(ctx, req.NamespacedName, &prescaler); err != nil {
		log.Error(err, "unable to re-fetch Prescaler for status update orphaned fields to nil")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Clear orphaned fields
	prescaler.Status.OrphanedSpecCpuUtilization = nil
	prescaler.Status.OrphanedScaleUpStabilizationWindowSeconds = nil
	if err := r.Status().Update(ctx, &prescaler); err != nil {
		log.Error(err, "Failed to update status for status update orphaned fields to nil")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// we'll requeue once we see the running job, and update our status
	return scheduledResult, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PrescaleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&prescalerv1.Prescale{}).
		Named("prescale").
		Complete(r)
}
