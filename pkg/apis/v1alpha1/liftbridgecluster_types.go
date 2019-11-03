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

// LiftbridgeClusterSpec defines the desired state of LiftbridgeCluster
type LiftbridgeClusterSpec struct {
	// Config contains the Liftbridge config as YAML.
	Config string `json:"config,omitempty" protobuf:"bytes,1,opt,name=config,proto3"`

	// Image is the Liftbridge Docker image to use when building the cluster.
	Image string `json:"image,omitempty" protobuf:"bytes,2,opt,name=config,proto3"`

	// VolumeClaimTemplates is a list of claims that pods are allowed to reference.
	// The StatefulSet controller is responsible for mapping network identities to
	// claims in a way that maintains the identity of a pod. Every claim in
	// this list must have at least one matching (by name) volumeMount in one
	// container in the template. A claim in this list takes precedence over
	// any volumes in the template, with the same name.
	// +optional
	VolumeClaimTemplates []v1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty" protobuf:"bytes,3,rep,name=volumeClaimTemplates"`

	// Replicas is the desired number of replicas of the given Template.
	// These are replicas in the sense that they are instantiations of the
	// same Template, but individual replicas also have a consistent identity.
	// If unspecified, defaults to 1.
	// +optional
	Replicas *int32 `json:"replicas,omitempty" protobuf:"varint,4,opt,name=replicas"`
}

type ClusterState string

const (
	ClusterStateCreating   ClusterState = "CREATING"
	ClusterStateUpdating   ClusterState = "UPDATING"
	ClusterStateStable     ClusterState = "STABLE"
	ClusterStateDestroying ClusterState = "DESTROYING"
	ClusterStateUnknown    ClusterState = "UNKNOWN"
)

// LiftbridgeClusterStatus defines the observed state of LiftbridgeCluster
type LiftbridgeClusterStatus struct {
	ClusterState ClusterState `json:"clusterState" protobuf:"bytes,1,opt,name=clusterState,proto3"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster state",type=string,JSONPath=`.status.clusterState`

// LiftbridgeCluster is the Schema for the liftbridgeclusters API
type LiftbridgeCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LiftbridgeClusterSpec   `json:"spec,omitempty"`
	Status LiftbridgeClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiftbridgeClusterList contains a list of LiftbridgeCluster
type LiftbridgeClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiftbridgeCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiftbridgeCluster{}, &LiftbridgeClusterList{})
}
