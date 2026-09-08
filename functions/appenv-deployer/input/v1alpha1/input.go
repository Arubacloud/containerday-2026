// Package v1alpha1 contains the input type for the appenv-deployer function.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeployerInput configures the appenv-deployer function.
// This is implementation-level configuration set by the platform team in the
// Composition and is never exposed to ApplicationEnvironment users.
//
// +kubebuilder:object:root=true
type DeployerInput struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DeployerInputSpec `json:"spec"`
}

// DeployerInputSpec holds configuration for the deployer function.
type DeployerInputSpec struct {
	// CloudserverResourceName is the name of the Cloudserver composed resource
	// within this Composition (as set in the resources[].name field).
	// +kubebuilder:default=cloudserver
	CloudserverResourceName string `json:"cloudserverResourceName"`

	// SSHSecretRef identifies the Kubernetes Secret holding the SSH private key.
	SSHSecretRef SSHSecretRef `json:"sshSecretRef"`

	// SSHUser is the username to authenticate as. Defaults to "ubuntu".
	// +kubebuilder:default=ubuntu
	SSHUser string `json:"sshUser,omitempty"`

	// SSHPort is the TCP port for SSH. Defaults to 22.
	// +kubebuilder:default=22
	SSHPort int `json:"sshPort,omitempty"`

	// ContainerName is the Docker container name used on the VM.
	// +kubebuilder:default=application
	ContainerName string `json:"containerName,omitempty"`
}

// SSHSecretRef identifies a Kubernetes Secret and key within it.
type SSHSecretRef struct {
	// Name of the Secret.
	Name string `json:"name"`
	// Namespace of the Secret.
	Namespace string `json:"namespace"`
	// Key within the Secret data that holds the PEM-encoded private key.
	// +kubebuilder:default=privateKey
	Key string `json:"key,omitempty"`
}
