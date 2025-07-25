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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// PrescaleSchedule defines a scheduled prescale action.
type PrescaleSchedule struct {
	// Cron expression for when to apply this prescale (e.g., "55 5 * * *" for 5:55am)
	Cron string `json:"cron"`
	// Percent to scale up by at this schedule
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Percent int32 `json:"percent"`
}

// PrescaleSpec defines the desired state of Prescale.
type PrescaleSpec struct {
	// Name of the resource to scale
	TargetHpaName string `json:"targetHpaName"`

	// List of scheduled prescale actions
	Schedules []PrescaleSchedule `json:"schedules"`

	// Suspend the prescaler
	// +kubebuilder:validation:Type=boolean
	Suspend *bool `json:"suspend"`

	// +kubebuilder:validation:Minimum=0

	// Optional deadline in seconds for starting the job if it misses scheduled
	// time for any reason.  Missed jobs executions will be counted as failed ones.
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

	// RevertWaitSeconds is the number of seconds to wait before reverting the scale
	// +kubebuilder:validation:Minimum=0
	RevertWaitSeconds *int64 `json:"revertWaitSeconds,omitempty"`
}

// PrescaleStatus defines the observed state of Prescale.
type PrescaleStatus struct {
	// LastScaledTime is the last time the resource was scaled
	LastScaledTime *metav1.Time `json:"lastScaledTime,omitempty"`

	// LastPrescaleSpecCpuUtilization is the last prescale spec cpu utilization
	LastPrescaleSpecCpuUtilization int32 `json:"lastPrescaleSpecCpuUtilization,omitempty"`

	// LastOriginalSpecCpuUtilization is the last original spec cpu utilization
	LastOriginalSpecCpuUtilization int32 `json:"lastOriginalSpecCpuUtilization,omitempty"`

	// OrphanedSpecCpuUtilization is the orphaned spec cpu utilization becaof failed hpa reverting
	OrphanedSpecCpuUtilization *int32 `json:"orphanedSpecCpuUtilization,omitempty"`

	// OrphanedScaleUpStabilizationWindowSeconds is the orphaned scale up stabilization window seconds because of failed hpa reverting
	OrphanedScaleUpStabilizationWindowSeconds *int32 `json:"orphanedScaleUpStabilizationWindowSeconds,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Prescale is the Schema for the prescales API.
type Prescale struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PrescaleSpec   `json:"spec,omitempty"`
	Status PrescaleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PrescaleList contains a list of Prescale.
type PrescaleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Prescale `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Prescale{}, &PrescaleList{})
}
