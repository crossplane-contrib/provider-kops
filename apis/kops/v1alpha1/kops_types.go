/*
Copyright 2022 The Crossplane Authors.

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
	"reflect"
	"time"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kops/pkg/apis/kops"
)

// KopsObservation are the observable fields of a Kops.
type KopsObservation struct {
	ProvisioningState string `json:"provisioningState,omitempty"`
	ID                string `json:"id,omitempty"`
	Name              string `json:"name,omitempty"`
}

// A KopsParameters are the parameters of a Kops.
type KopsParameters struct {
	ClusterSpec                 kops.ClusterSpec         `json:"clusterSpec"`
	InstanceGroupSpec           []kops.InstanceGroupSpec `json:"instanceGroupSpec"`
	Domain                      string                   `json:"domain"`
	StateBucket                 string                   `json:"stateBucket"`
	Region                      string                   `json:"region"`
	KubernetesAPICertificateTTL time.Duration            `json:"kubernetesApiCertificateTTL,omitempty"`
}

// A KopsSpec defines the desired state of a Kops.
type KopsSpec struct {
	xpv1.ResourceSpec `json:",inline"`
	ForProvider       KopsParameters `json:"forProvider"`
}

// A KopsStatus represents the observed state of a Kops.
type KopsStatus struct {
	xpv1.ResourceStatus `json:",inline"`
	AtProvider          KopsObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A Kops is an example API type.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories={crossplane,managed,kops}
type Kops struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KopsSpec   `json:"spec"`
	Status KopsStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KopsList contains a list of Kops
type KopsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Kops `json:"items"`
}

// Kops type metadata.
var (
	KopsKind             = reflect.TypeOf(Kops{}).Name()
	KopsGroupKind        = schema.GroupKind{Group: Group, Kind: KopsKind}.String()
	KopsKindAPIVersion   = KopsKind + "." + SchemeGroupVersion.String()
	KopsGroupVersionKind = SchemeGroupVersion.WithKind(KopsKind)
)

func init() {
	SchemeBuilder.Register(&Kops{}, &KopsList{})
}
