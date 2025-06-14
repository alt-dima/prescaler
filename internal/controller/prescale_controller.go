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
	if prescaleSpecCpuUtilizationOrphaned != nil {
		//found orphaned prescale need to revert in hpa
		hpa := &autoscalingv2.HorizontalPodAutoscaler{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: prescaler.Spec.TargetHpaName}, hpa); err != nil {
			log.Error(err, "failed to get HPA for orphaned revert")
			r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedGetHPA", err.Error())
			return ctrl.Result{}, nil
		}
		for index, metric := range hpa.Spec.Metrics {
			if metric.Resource.Name == "cpu" && metric.Resource.Target.AverageUtilization != nil {
				hpa.Spec.Metrics[index].Resource.Target.AverageUtilization = prescaleSpecCpuUtilizationOrphaned
				err := r.Update(ctx, hpa)
				if err != nil {
					log.Error(err, "failed to update HPA for orphaned revert")
					r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateHPA", err.Error())
					return ctrl.Result{}, nil
				}
				break
			}
		}
		// Re-fetch Prescale for status update orphanedSpecCpuUtilization to nil
		if err := r.Get(ctx, req.NamespacedName, &prescaler); err != nil {
			log.Error(err, "unable to re-fetch Prescaler for status update orphanedSpecCpuUtilization to nil")
			return ctrl.Result{}, nil
		}
		prescaler.Status.OrphanedSpecCpuUtilization = nil
		if err := r.Status().Update(ctx, &prescaler); err != nil {
			log.Error(err, "Failed to update status to set orphanedSpecCpuUtilization to nil in orphaned revert")
			r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
			return ctrl.Result{}, nil
		}
	}

	if prescaler.Spec.Suspend != nil && *prescaler.Spec.Suspend {
		log.V(1).Info("prescaler suspended, skipping")
		return ctrl.Result{}, nil
	}

	getNextSchedule := func(prescaler *prescalerv1.Prescale, now time.Time) (lastMissed time.Time, next time.Time, err error) {
		sched, err := cron.ParseStandard(prescaler.Spec.Schedule)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("unparseable schedule %q: %w", prescaler.Spec.Schedule, err)
		}

		// for optimization purposes, cheat a bit and start from our last observed run time
		// we could reconstitute this here, but there's not much point, since we've
		// just updated it.
		var earliestTime time.Time
		if prescaler.Status.LastScaledTime != nil {
			earliestTime = prescaler.Status.LastScaledTime.Time
		} else {
			earliestTime = prescaler.CreationTimestamp.Time
		}
		if prescaler.Spec.StartingDeadlineSeconds != nil {
			// controller is not going to schedule anything below this point
			schedulingDeadline := now.Add(-time.Second * time.Duration(*prescaler.Spec.StartingDeadlineSeconds))

			if schedulingDeadline.After(earliestTime) {
				earliestTime = schedulingDeadline
			}
		}
		if earliestTime.After(now) {
			return time.Time{}, sched.Next(now), nil
		}

		starts := 0
		for t := sched.Next(earliestTime); !t.After(now); t = sched.Next(t) {
			lastMissed = t
			// An object might miss several starts. For example, if
			// controller gets wedged on Friday at 5:01pm when everyone has
			// gone home, and someone comes in on Tuesday AM and discovers
			// the problem and restarts the controller, then all the hourly
			// jobs, more than 80 of them for one hourly scheduledJob, should
			// all start running with no further intervention (if the scheduledJob
			// allows concurrency and late starts).
			//
			// However, if there is a bug somewhere, or incorrect clock
			// on controller's server or apiservers (for setting creationTimestamp)
			// then there could be so many missed start times (it could be off
			// by decades or more), that it would eat up all the CPU and memory
			// of this controller. In that case, we want to not try to list
			// all the missed start times.
			starts++
			if starts > 100 {
				// We can't get the most recent times so just return an empty slice
				return time.Time{}, time.Time{}, fmt.Errorf("too many missed start times (> 100). Set or decrease .spec.startingDeadlineSeconds or check clock skew")
			}
		}
		return lastMissed, sched.Next(now), nil
	}

	// figure out the next times that we need to create
	// jobs at (or anything we missed).
	missedRun, nextRun, err := getNextSchedule(&prescaler, time.Now())
	if err != nil {
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedGetNextSchedule", err.Error())
		log.Error(err, "unable to figure out Prescaler schedule")
		// we don't really care about requeuing until we get an update that
		// fixes the schedule, so don't return an error
		return ctrl.Result{}, nil
	}

	scheduledResult := ctrl.Result{RequeueAfter: time.Until(nextRun)} // save this so we can re-use it elsewhere
	log = log.WithValues("now", time.Now(), "next run", nextRun)

	if missedRun.IsZero() {
		log.V(1).Info("no upcoming scheduled times, sleeping until next")
		return scheduledResult, nil
	}

	// make sure we're not too late to start the run
	log = log.WithValues("current run", missedRun)
	tooLate := false
	if prescaler.Spec.StartingDeadlineSeconds != nil {
		tooLate = missedRun.Add(time.Duration(*prescaler.Spec.StartingDeadlineSeconds) * time.Second).Before(time.Now())
	}
	if tooLate {
		r.Recorder.Event(&prescaler, corev1.EventTypeNormal, "MissedStartingDeadline", "missed starting deadline for last run, sleeping till next")
		log.V(1).Info("missed starting deadline for last run, sleeping till next")
		// TODO(directxman12): events
		return scheduledResult, nil
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: prescaler.Spec.TargetHpaName}, hpa); err != nil {
		log.Error(err, "failed to get hpa for prescale")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedGetHPA", err.Error())
		return ctrl.Result{}, nil
	}

	// Find originalSpecCpuUtilization in HPA Spec Metrics for prescale
	var originalSpecCpuUtilization int32
	var originalSpecCpuUtilizationIndex int = -1
	for index, metric := range hpa.Spec.Metrics {
		if metric.Resource.Name == "cpu" && metric.Resource.Target.AverageUtilization != nil {
			originalSpecCpuUtilization = *metric.Resource.Target.AverageUtilization
			originalSpecCpuUtilizationIndex = index
			break
		}
	}
	if originalSpecCpuUtilizationIndex == -1 {
		log.Error(fmt.Errorf("failed to find cpu utilization index in HPA for prescale"), "originalSpecCpuUtilizationIndex is -1")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedFindCPUUtilizationIndex", "originalSpecCpuUtilizationIndex is -1")
		return ctrl.Result{}, nil
	}

	log.Info("originalSpecCpuUtilization", "value", originalSpecCpuUtilization)

	prescaleSpecCpuUtilization := originalSpecCpuUtilization - int32(float64(originalSpecCpuUtilization)*float64(*prescaler.Spec.Percent)/100)

	// Prescale HPA Spec Metrics CPU AverageUtilization to prescaleSpecCpuUtilization
	hpa.Spec.Metrics[originalSpecCpuUtilizationIndex].Resource.Target.AverageUtilization = &prescaleSpecCpuUtilization
	err = r.Update(ctx, hpa)
	if err != nil {
		log.Error(err, "failed to update hpa for prescale")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateHPA", err.Error())
		return ctrl.Result{}, nil
	}

	log.Info("prescaleSpecCpuUtilization", "value", prescaleSpecCpuUtilization)
	r.Recorder.Event(&prescaler, corev1.EventTypeNormal, "Prescaled", fmt.Sprintf("Successfully prescaled HPA to %d%% CPU utilization", prescaleSpecCpuUtilization))

	// Re-fetch Prescaler after prescale
	if err := r.Get(ctx, req.NamespacedName, &prescaler); err != nil {
		log.Error(err, "unable to re-fetch Prescaler after prescale")
		return ctrl.Result{}, nil
	}

	prescaler.Status.LastScaledTime = &metav1.Time{Time: missedRun}
	prescaler.Status.LastPrescaleSpecCpuUtilization = prescaleSpecCpuUtilization
	prescaler.Status.LastOriginalSpecCpuUtilization = originalSpecCpuUtilization
	prescaler.Status.OrphanedSpecCpuUtilization = &originalSpecCpuUtilization

	if err := r.Status().Update(ctx, &prescaler); err != nil {
		log.Error(err, "Failed to update status after prescale")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		return ctrl.Result{}, err
	}

	// Sleep for 10 seconds
	time.Sleep(10 * time.Second)

	// Re-fetch HPA for revert
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: prescaler.Spec.TargetHpaName}, hpa); err != nil {
		log.Error(err, "failed to re-fetch hpa for revert")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedGetHPA", err.Error())
		return ctrl.Result{}, nil
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
	hpa.Spec.Metrics[currentSpecCpuUtilizationIndex].Resource.Target.AverageUtilization = &originalSpecCpuUtilization
	err = r.Update(ctx, hpa)
	if err != nil {
		log.Error(err, "failed to update hpa for revert")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateHPA", err.Error())
		return ctrl.Result{}, nil
	}

	log.Info("revertedSpecCpuUtilization", "value", originalSpecCpuUtilization)
	r.Recorder.Event(&prescaler, corev1.EventTypeNormal, "Reverted", fmt.Sprintf("Successfully reverted HPA to %d%% CPU utilization", originalSpecCpuUtilization))

	// Re-fetch Prescale for status update orphanedSpecCpuUtilization to nil
	if err := r.Get(ctx, req.NamespacedName, &prescaler); err != nil {
		log.Error(err, "unable to re-fetch Prescaler for status update orphanedSpecCpuUtilization to nil")
		return ctrl.Result{}, nil
	}

	// Clear orphanedSpecCpuUtilization
	prescaler.Status.OrphanedSpecCpuUtilization = nil
	if err := r.Status().Update(ctx, &prescaler); err != nil {
		log.Error(err, "Failed to update status for status update orphanedSpecCpuUtilization to nil")
		r.Recorder.Event(&prescaler, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		return ctrl.Result{}, err
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
