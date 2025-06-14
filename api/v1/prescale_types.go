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

// PrescaleSpec defines the desired state of Prescale.
type PrescaleSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Name of the resource to scale
	TargetHpaName string `json:"targetHpaName"`

	// Percentage to scale up by
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:validation:ExclusiveMaximum=false
	Percent *int32 `json:"percent"`

	// Suspend the prescaler
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html
	// +kubebuilder:validation:Type=boolean
	Suspend *bool `json:"suspend"`

	// The schedule in Cron format, see https://en.wikipedia.org/wiki/Cron.
	Schedule string `json:"schedule"`

	// +kubebuilder:validation:Minimum=0

	// Optional deadline in seconds for starting the job if it misses scheduled
	// time for any reason.  Missed jobs executions will be counted as failed ones.
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`
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
