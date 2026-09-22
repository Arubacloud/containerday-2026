// Package v1alpha1 contains the input type for the appenv-deployer function.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeployerInput configures the appenv-deployer function.
// This is implementation-level configuration set by the platform team in the
// Composition and is never exposed to ApplicationEnvironment or Microservice users.
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

	// Database configures optional managed MySQL connectivity.
	// When set, the function waits for the DBaaS Elastic IP to be ready and
	// injects MySQL connection env vars into the container.
	Database *DatabaseConfig `json:"database,omitempty"`
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

// DatabaseConfig configures managed MySQL connectivity for the deployed container.
// The function injects standard MySQL env vars (MYSQL_HOST, MYSQL_PORT,
// MYSQL_DATABASE, MYSQL_USER, MYSQL_PASSWORD) plus common aliases so that
// most MySQL-aware images work without per-image configuration.
type DatabaseConfig struct {
	// DbaasResourceName is the composition resource name of the Dbaas cluster.
	// The function waits for this resource to be Ready before deploying the
	// container, ensuring MySQL is accessible when the application starts.
	// +kubebuilder:default=dbaas
	DbaasResourceName string `json:"dbaasResourceName,omitempty"`

	// DbaasEIPResourceName is the composition resource name of the Elasticip
	// attached to the DBaaS cluster. The function reads status.atProvider.address
	// from this resource to obtain the MySQL host.
	// +kubebuilder:default=dbaas-eip
	DbaasEIPResourceName string `json:"dbaasEIPResourceName,omitempty"`

	// Port is the MySQL TCP port. Defaults to 3306.
	// +kubebuilder:default=3306
	Port int `json:"port,omitempty"`

	// DatabaseName is the logical database name passed as MYSQL_DATABASE.
	DatabaseName string `json:"databaseName"`

	// Username is the MySQL username passed as MYSQL_USER.
	Username string `json:"username"`

	// PasswordContextKey is the key in the function-extra-resources pipeline
	// context that holds the password Secret. Defaults to "db-password-secret".
	// +kubebuilder:default=db-password-secret
	PasswordContextKey string `json:"passwordContextKey,omitempty"`

	// PasswordSecretKey is the key within the Secret data holding the password.
	// +kubebuilder:default=password
	PasswordSecretKey string `json:"passwordSecretKey,omitempty"`
}
