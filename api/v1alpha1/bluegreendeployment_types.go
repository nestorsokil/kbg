/*


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

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// BlueGreenDeploymentSpec defines the desired state of BlueGreenDeployment
type BlueGreenDeploymentSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	Template               v1.PodTemplateSpec `json:"template"`
	Service                v1.ServiceSpec     `json:"service"`
	Replicas               *int32             `json:"replicas"`
	BackupScaleDownPercent *int32             `json:"backupScaleDownPercent"`
}

type StatusName string

const (
	StatusNameActive = "Active"
	// todo other
)

const (
	ColorBlue  = "blue"
	ColorGreen = "green"
)

// BlueGreenDeploymentStatus defines the observed state of BlueGreenDeployment
type BlueGreenDeploymentStatus struct {
	// Important: Run "make" to regenerate code after modifying this file
	StatusName  int32  `json:"status"`
	Replicas    int32  `json:"replicas"`
	ActiveColor string `json:"activeColor"`
}

// +kubebuilder:object:root=true

// BlueGreenDeployment is the Schema for the bluegreendeployments API
// +kubebuilder:resource:shortName=bgd
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
// +kubebuilder:printcolumn:name="Color",type=string,JSONPath=`.status.activeColor`
type BlueGreenDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BlueGreenDeploymentSpec   `json:"spec,omitempty"`
	Status BlueGreenDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BlueGreenDeploymentList contains a list of BlueGreenDeployment
type BlueGreenDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BlueGreenDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BlueGreenDeployment{}, &BlueGreenDeploymentList{})
}
