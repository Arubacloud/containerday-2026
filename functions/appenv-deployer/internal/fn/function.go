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

	// If the XR is being deleted, skip SSH deployment entirely.
	if dt, _, _ := unstructuredString(xr.Resource.Object, "metadata", "deletionTimestamp"); dt != "" {
		log.Info("XR is being deleted, skipping deployment")
		return rsp, nil
	}

	image, err := xr.Resource.GetString("spec.image")
	if err != nil || image == "" {
		response.Fatal(rsp, errors.New("spec.image is required"))
		return rsp, nil
	}

	appPort, err := xr.Resource.GetInteger("spec.port")
	if err != nil || appPort == 0 {
		appPort = 9898
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

	// --- Read SSH private key from pipeline context ---
	privateKeyPEM, err := getSSHKeyFromContext(req, input.Spec.SSHSecretRef)
	if err != nil {
		log.Info("SSH secret not yet available in pipeline context, will retry", "error", err)
		response.Normalf(rsp, "Waiting for SSH secret: %s", sanitizeError(err))
		return rsp, nil
	}

	// --- Build deploy options ---
	opts := deployer.DeployOptions{
		Host:          publicIP,
		Port:          input.Spec.SSHPort,
		User:          input.Spec.SSHUser,
		PrivateKeyPEM: privateKeyPEM,
		Image:         image,
		ContainerName: input.Spec.ContainerName,
	}

	// --- If database is configured, wait for DBaaS EIP and inject env vars ---
	if input.Spec.Database != nil {
		envVars, ready := buildDatabaseEnvVars(req, input.Spec.Database, observed, rsp, log)
		if !ready {
			return rsp, nil
		}
		opts.EnvVars = envVars
	}

	// --- Deploy the application ---
	deployCtx, cancel := context.WithTimeout(ctx, deployTimeout)
	defer cancel()

	if err := f.deployer.Deploy(deployCtx, opts); err != nil {
		log.Info("Application deployment failed", "error", err)
		response.Normalf(rsp, "Application deployment failed: %s", sanitizeError(err))
		return rsp, nil
	}

	log.Info("Application deployed successfully", "image", image, "container", input.Spec.ContainerName)
	response.Normalf(rsp, "Application %q deployed successfully as container %q", image, input.Spec.ContainerName)

	// Write runtime values into XR status.
	dxr, err := request.GetDesiredCompositeResource(req)
	if err == nil {
		endpoint := fmt.Sprintf("http://%s:%d", publicIP, appPort)
		_ = dxr.Resource.SetString("status.endpoint", endpoint)

		if opts.EnvVars != nil {
			_ = dxr.Resource.SetString("status.databaseHost", opts.EnvVars["MYSQL_HOST"])
			_ = dxr.Resource.SetString("status.databasePort", opts.EnvVars["MYSQL_PORT"])
			_ = dxr.Resource.SetString("status.databaseName", opts.EnvVars["MYSQL_DATABASE"])
			_ = dxr.Resource.SetString("status.databaseUser", opts.EnvVars["MYSQL_USER"])
			_ = dxr.Resource.SetString("status.databasePassword", opts.EnvVars["MYSQL_PASSWORD"])
		}

		_ = response.SetDesiredCompositeResource(rsp, dxr)
	}

	return rsp, nil
}

// buildDatabaseEnvVars reads the DBaaS Elastic IP address and password from the
// pipeline context, then returns the MySQL env vars to inject into the container.
// Returns (nil, false) and sets the response when the DBaaS is not yet ready.
func buildDatabaseEnvVars(
	req *fnv1.RunFunctionRequest,
	db *v1alpha1.DatabaseConfig,
	observed map[resource.Name]resource.ObservedComposed,
	rsp *fnv1.RunFunctionResponse,
	log logging.Logger,
) (map[string]string, bool) {
	eipName := db.DbaasEIPResourceName
	if eipName == "" {
		eipName = "dbaas-eip"
	}

	eipRes, ok := observed[resource.Name(eipName)]
	if !ok {
		log.Info("DBaaS Elastic IP not yet observed", "resource", eipName)
		response.Normalf(rsp, "Waiting for DBaaS Elastic IP %q to be provisioned", eipName)
		return nil, false
	}

	dbaasHost, err := eipRes.Resource.GetString("status.atProvider.address")
	if err != nil || dbaasHost == "" {
		log.Info("DBaaS Elastic IP address not yet available")
		response.Normalf(rsp, "Waiting for DBaaS Elastic IP address to be assigned")
		return nil, false
	}

	contextKey := db.PasswordContextKey
	if contextKey == "" {
		contextKey = "db-password-secret"
	}
	secretKey := db.PasswordSecretKey
	if secretKey == "" {
		secretKey = "password"
	}

	password, err := getDatabasePassword(req, contextKey, secretKey)
	if err != nil {
		log.Info("DB password not yet available in pipeline context", "error", err)
		response.Normalf(rsp, "Waiting for DB password: %s", sanitizeError(err))
		return nil, false
	}

	port := db.Port
	if port == 0 {
		port = 3306
	}
	portStr := fmt.Sprintf("%d", port)

	log.Info("DBaaS ready", "host", dbaasHost, "port", portStr, "database", db.DatabaseName)

	// Inject a standard set of MySQL env vars plus well-known aliases so that
	// most MySQL-aware images (adminer, phpmyadmin, wordpress, custom apps) work
	// without per-image configuration in the Composition.
	return map[string]string{
		"MYSQL_HOST":              dbaasHost,
		"MYSQL_PORT":              portStr,
		"MYSQL_DATABASE":          db.DatabaseName,
		"MYSQL_USER":              db.Username,
		"MYSQL_PASSWORD":          password,
		"DB_HOST":                 dbaasHost,
		"DB_PORT":                 portStr,
		"DB_NAME":                 db.DatabaseName,
		"DB_USER":                 db.Username,
		"DB_PASSWORD":             password,
		"ADMINER_DEFAULT_SERVER":  dbaasHost,
	}, true
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

// getSecretDataValue reads a base64-encoded value from a Secret stored in the
// pipeline context by function-extra-resources.
// contextKey is the "into" label used in the function-extra-resources input.
// dataKey is the key within the Secret's data map.
func getSecretDataValue(req *fnv1.RunFunctionRequest, contextKey, dataKey string) (string, error) {
	ctx := req.GetContext()
	if ctx == nil {
		return "", fmt.Errorf("pipeline context is empty; waiting for function-extra-resources")
	}

	ctxFields := ctx.GetFields()
	extraVal, ok := ctxFields[extraResourcesContextKey]
	if !ok {
		return "", fmt.Errorf("extra resources not yet in pipeline context")
	}

	extraStruct := extraVal.GetStructValue()
	if extraStruct == nil {
		return "", fmt.Errorf("extra resources context value is not a struct")
	}

	secretListVal, ok := extraStruct.GetFields()[contextKey]
	if !ok {
		return "", fmt.Errorf("key %q not found in extra resources context", contextKey)
	}

	secretList := secretListVal.GetListValue()
	if secretList == nil || len(secretList.GetValues()) == 0 {
		return "", fmt.Errorf("secret list for key %q is empty in extra resources context", contextKey)
	}

	secretStruct := secretList.GetValues()[0].GetStructValue()
	if secretStruct == nil {
		return "", fmt.Errorf("first item for key %q is not a struct", contextKey)
	}

	dataVal, ok := secretStruct.GetFields()["data"]
	if !ok {
		return "", fmt.Errorf("secret for key %q has no data field", contextKey)
	}

	dataStruct := dataVal.GetStructValue()
	if dataStruct == nil {
		return "", fmt.Errorf("secret data for key %q is not a struct", contextKey)
	}

	valField, ok := dataStruct.GetFields()[dataKey]
	if !ok {
		return "", fmt.Errorf("data key %q not found in secret %q", dataKey, contextKey)
	}

	encoded := valField.GetStringValue()
	if encoded == "" {
		return "", fmt.Errorf("data key %q in secret %q is empty", dataKey, contextKey)
	}
	return encoded, nil
}

// getSSHKeyFromContext reads the SSH private key from the pipeline context.
func getSSHKeyFromContext(req *fnv1.RunFunctionRequest, ref v1alpha1.SSHSecretRef) ([]byte, error) {
	key := ref.Key
	if key == "" {
		key = "privateKey"
	}
	encoded, err := getSecretDataValue(req, "ssh-secret", key)
	if err != nil {
		return nil, err
	}
	pem, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("base64-decode SSH private key: %w", err)
	}
	return pem, nil
}

// getDatabasePassword reads and base64-decodes the DB password from the pipeline context.
// The password value is never logged.
func getDatabasePassword(req *fnv1.RunFunctionRequest, contextKey, secretKey string) (string, error) {
	encoded, err := getSecretDataValue(req, contextKey, secretKey)
	if err != nil {
		return "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("base64-decode DB password: %w", err)
	}
	return string(decoded), nil
}

// unstructuredString walks a path in an unstructured map and returns the string value.
func unstructuredString(obj map[string]interface{}, path ...string) (string, bool, error) {
	cur := obj
	for _, key := range path[:len(path)-1] {
		next, ok := cur[key]
		if !ok {
			return "", false, nil
		}
		m, ok := next.(map[string]interface{})
		if !ok {
			return "", false, nil
		}
		cur = m
	}
	last := path[len(path)-1]
	val, ok := cur[last]
	if !ok {
		return "", false, nil
	}
	s, ok := val.(string)
	if !ok {
		return "", false, nil
	}
	return s, true, nil
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
