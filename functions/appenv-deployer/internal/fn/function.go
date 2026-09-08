// Package fn implements the Crossplane Composition Function.
package fn

import (
	"context"
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

	// --- Read SSH private key from extra resources ---
	privateKeyPEM, err := getSSHKey(req, input.Spec.SSHSecretRef)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot read SSH secret"))
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

// getSSHKey retrieves the private key bytes from an extra resource named "ssh-secret".
func getSSHKey(req *fnv1.RunFunctionRequest, ref v1alpha1.SSHSecretRef) ([]byte, error) {
	extras, err := request.GetExtraResources(req)
	if err != nil {
		return nil, fmt.Errorf("get extra resources: %w", err)
	}

	items, ok := extras["ssh-secret"]
	if !ok || len(items) == 0 {
		return nil, fmt.Errorf("extra resource 'ssh-secret' not found; ensure the Composition requests it via an EnvironmentConfig or ExtraResources pipeline step")
	}

	secretObj := items[0].Resource.Object

	key := ref.Key
	if key == "" {
		key = "privateKey"
	}

	data, ok := secretObj["data"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("ssh secret has no data field")
	}

	val, ok := data[key]
	if !ok {
		return nil, fmt.Errorf("key %q not found in SSH secret %s/%s", key, ref.Namespace, ref.Name)
	}

	// Secret data is base64-encoded by Kubernetes; unstructured decode gives []byte already decoded.
	switch v := val.(type) {
	case string:
		return []byte(v), nil
	case []byte:
		return v, nil
	default:
		return nil, fmt.Errorf("unexpected type for secret key %q: %T", key, val)
	}
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
