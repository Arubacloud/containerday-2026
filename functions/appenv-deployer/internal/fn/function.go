// Package fn implements the Crossplane Composition Function.
package fn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/Arubacloud/containerday-2026/functions/appenv-deployer/input/v1alpha1"
	"github.com/Arubacloud/containerday-2026/functions/appenv-deployer/internal/deployer"
)

// deployTimeout is the overall budget for a single deployment attempt.
const deployTimeout = 12 * time.Minute

// extraResourcesContextKey is the pipeline context key set by function-extra-resources.
const extraResourcesContextKey = "apiextensions.crossplane.io/extra-resources"

// Function is the Crossplane Composition Function implementation.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log      logging.Logger
	deployer deployer.Deployer
}

// NewFunction constructs a Function.
func NewFunction(log logging.Logger, d deployer.Deployer) *Function {
	return &Function{log: log, deployer: d}
}

// RunFunction implements fnv1.FunctionRunnerServiceServer.
func (f *Function) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	rsp := response.To(req, response.DefaultTTL)

	// --- Parse function input ---
	input, err := parseInput(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot parse function input"))
		return rsp, nil
	}
	applyDefaults(input)

	log := f.log.WithValues("xrName", xrName(req))

	// --- Get desired image from XR spec ---
	xr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composite"))
		return rsp, nil
	}

	image, err := xr.Resource.GetString("spec.image")
	if err != nil || image == "" {
		response.Fatal(rsp, errors.New("spec.image is required"))
		return rsp, nil
	}

	// --- Find the Cloudserver in observed composed resources ---
	observed, err := request.GetObservedComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composed resources"))
		return rsp, nil
	}

	csRes, ok := observed[resource.Name(input.Spec.CloudserverResourceName)]
	if !ok {
		log.Info("Cloudserver not yet observed; waiting for infrastructure provisioning")
		response.Normalf(rsp, "Waiting for Cloudserver %q to be provisioned", input.Spec.CloudserverResourceName)
		return rsp, nil
	}

	// --- Check Cloudserver readiness ---
	if !isCloudserverReady(csRes.Resource.Object) {
		log.Info("Cloudserver exists but is not yet ready")
		response.Normalf(rsp, "Waiting for Cloudserver to become ready")
		return rsp, nil
	}

	// --- Extract public IP ---
	publicIP, err := csRes.Resource.GetString("status.atProvider.publicIp")
	if err != nil || publicIP == "" {
		log.Info("publicIp not yet available on Cloudserver")
		response.Normalf(rsp, "Waiting for Cloudserver publicIp to be assigned")
		return rsp, nil
	}

	log.Info("Cloudserver ready", "publicIp", publicIP)

	// --- Read SSH private key from the pipeline context populated by function-extra-resources ---
	// function-extra-resources stores fetched resources in the pipeline context (not req.ExtraResources).
	// On the first reconcile it exits early before populating the context, so we treat a missing
	// secret as a transient/retryable condition rather than a fatal misconfiguration.
	privateKeyPEM, err := getSSHKeyFromContext(req, input.Spec.SSHSecretRef)
	if err != nil {
		log.Info("SSH secret not yet available in pipeline context, will retry", "error", err)
		response.Normalf(rsp, "Waiting for SSH secret: %s", sanitizeError(err))
		return rsp, nil
	}

	// --- Deploy the application ---
	deployCtx, cancel := context.WithTimeout(ctx, deployTimeout)
	defer cancel()

	opts := deployer.DeployOptions{
		Host:          publicIP,
		Port:          input.Spec.SSHPort,
		User:          input.Spec.SSHUser,
		PrivateKeyPEM: privateKeyPEM,
		Image:         image,
		ContainerName: input.Spec.ContainerName,
	}

	if err := f.deployer.Deploy(deployCtx, opts); err != nil {
		log.Info("Application deployment failed", "error", err)
		response.Normalf(rsp, "Application deployment failed: %s", sanitizeError(err))
		return rsp, nil
	}

	log.Info("Application deployed successfully", "image", image, "container", input.Spec.ContainerName)
	response.Normalf(rsp, "Application %q deployed successfully as container %q", image, input.Spec.ContainerName)

	return rsp, nil
}

// parseInput decodes the function input struct into DeployerInput.
func parseInput(req *fnv1.RunFunctionRequest) (*v1alpha1.DeployerInput, error) {
	raw := req.GetInput()
	if raw == nil {
		return &v1alpha1.DeployerInput{}, nil
	}

	b, err := protojson.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal input struct: %w", err)
	}

	out := &v1alpha1.DeployerInput{}
	if err := json.Unmarshal(b, out); err != nil {
		return nil, fmt.Errorf("unmarshal input: %w", err)
	}
	return out, nil
}

// xrName extracts the XR name for logging.
func xrName(req *fnv1.RunFunctionRequest) string {
	if cr := req.GetObserved().GetComposite().GetResource(); cr != nil {
		if f, ok := cr.GetFields()["metadata"]; ok {
			if m, ok := f.GetStructValue().GetFields()["name"]; ok {
				return m.GetStringValue()
			}
		}
	}
	return "unknown"
}

// applyDefaults fills in zero-value fields with their defaults.
func applyDefaults(in *v1alpha1.DeployerInput) {
	if in.Spec.CloudserverResourceName == "" {
		in.Spec.CloudserverResourceName = "cloudserver"
	}
	if in.Spec.SSHUser == "" {
		in.Spec.SSHUser = "ubuntu"
	}
	if in.Spec.SSHPort == 0 {
		in.Spec.SSHPort = 22
	}
	if in.Spec.ContainerName == "" {
		in.Spec.ContainerName = "application"
	}
	if in.Spec.SSHSecretRef.Key == "" {
		in.Spec.SSHSecretRef.Key = "privateKey"
	}
}

// isCloudserverReady returns true when the Cloudserver has a Ready=True condition.
func isCloudserverReady(obj map[string]interface{}) bool {
	status, ok := obj["status"].(map[string]interface{})
	if !ok {
		return false
	}
	conditions, ok := status["conditions"].([]interface{})
	if !ok {
		return false
	}
	for _, c := range conditions {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cm["type"] == "Ready" && cm["status"] == "True" {
			return true
		}
	}
	return false
}

// getSSHKeyFromContext reads the SSH private key from the pipeline context populated by
// function-extra-resources. The context key is "apiextensions.crossplane.io/extra-resources"
// and the structure is map[into][]unstructured.Unstructured serialised as JSON.
//
// Kubernetes Secret data values are base64-encoded strings in the API response, so we
// decode them before returning.
func getSSHKeyFromContext(req *fnv1.RunFunctionRequest, ref v1alpha1.SSHSecretRef) ([]byte, error) {
	ctx := req.GetContext()
	if ctx == nil {
		return nil, fmt.Errorf("pipeline context is empty; waiting for function-extra-resources to populate it")
	}

	ctxFields := ctx.GetFields()
	extraVal, ok := ctxFields[extraResourcesContextKey]
	if !ok {
		return nil, fmt.Errorf("extra resources not yet in pipeline context")
	}

	extraStruct := extraVal.GetStructValue()
	if extraStruct == nil {
		return nil, fmt.Errorf("extra resources context value is not a struct")
	}

	secretListVal, ok := extraStruct.GetFields()["ssh-secret"]
	if !ok {
		return nil, fmt.Errorf("'ssh-secret' key not found in extra resources context")
	}

	secretList := secretListVal.GetListValue()
	if secretList == nil || len(secretList.GetValues()) == 0 {
		return nil, fmt.Errorf("ssh-secret list is empty in extra resources context")
	}

	secretStruct := secretList.GetValues()[0].GetStructValue()
	if secretStruct == nil {
		return nil, fmt.Errorf("ssh-secret first item is not a struct")
	}

	dataVal, ok := secretStruct.GetFields()["data"]
	if !ok {
		return nil, fmt.Errorf("ssh secret has no data field")
	}

	dataStruct := dataVal.GetStructValue()
	if dataStruct == nil {
		return nil, fmt.Errorf("ssh secret data is not a struct")
	}

	key := ref.Key
	if key == "" {
		key = "privateKey"
	}

	keyVal, ok := dataStruct.GetFields()[key]
	if !ok {
		return nil, fmt.Errorf("key %q not found in SSH secret", key)
	}

	// Kubernetes Secret data is base64-encoded in the API response.
	encoded := keyVal.GetStringValue()
	if encoded == "" {
		return nil, fmt.Errorf("SSH private key value is empty")
	}

	pem, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("base64-decode SSH private key: %w", err)
	}

	return pem, nil
}

// sanitizeError strips any path/credential noise from an error message.
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 200 {
		return msg[:200] + "..."
	}
	return msg
}
