package fn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Arubacloud/containerday-2026/functions/appenv-deployer/internal/deployer"
)

// noopDeployer satisfies deployer.Deployer without doing anything.
type noopDeployer struct{ err error }

func (n *noopDeployer) Deploy(_ context.Context, _ deployer.DeployOptions) error { return n.err }

// mustStruct converts an arbitrary value to *structpb.Struct via JSON.
func mustStruct(t *testing.T, v interface{}) *structpb.Struct {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	s := &structpb.Struct{}
	if err := protojson.Unmarshal(b, s); err != nil {
		t.Fatalf("protojson.Unmarshal: %v", err)
	}
	return s
}

func defaultXR() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "platform.example.com/v1alpha1",
		"kind":       "ApplicationEnvironment",
		"metadata":   map[string]interface{}{"name": "test"},
		"spec":       map[string]interface{}{"image": "nginx:latest"},
	}
}

func defaultInput() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "appenv.platform.example.com/v1alpha1",
		"kind":       "DeployerInput",
		"spec": map[string]interface{}{
			"cloudserverResourceName": "cloudserver",
			"sshSecretRef": map[string]interface{}{
				"name":      "app-ssh-privkey",
				"namespace": "crossplane-system",
				"key":       "privateKey",
			},
			"sshUser":       "ubuntu",
			"sshPort":       22,
			"containerName": "application",
		},
	}
}

func readyCloudserver(publicIP string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "arubacloud.crossplane.io/v1alpha1",
		"kind":       "Cloudserver",
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True"},
			},
			"atProvider": map[string]interface{}{
				"publicIp": publicIP,
			},
		},
	}
}

func buildRequest(t *testing.T, xr map[string]interface{}, composed map[string]map[string]interface{}, input map[string]interface{}) *fnv1.RunFunctionRequest {
	t.Helper()

	req := &fnv1.RunFunctionRequest{
		Observed: &fnv1.State{
			Composite: &fnv1.Resource{
				Resource: mustStruct(t, xr),
			},
			Resources: map[string]*fnv1.Resource{},
		},
		Input: mustStruct(t, input),
	}

	for name, obj := range composed {
		req.Observed.Resources[name] = &fnv1.Resource{
			Resource: mustStruct(t, obj),
		}
	}

	return req
}

func addSSHSecret(t *testing.T, req *fnv1.RunFunctionRequest, pemKey string) {
	t.Helper()

	// function-extra-resources stores resources in the pipeline context, not req.ExtraResources.
	// The secret data value must be base64-encoded (as it is in the Kubernetes API).
	encoded := base64.StdEncoding.EncodeToString([]byte(pemKey))
	secret := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      "app-ssh-privkey",
			"namespace": "default",
		},
		"data": map[string]interface{}{
			"privateKey": encoded,
		},
	}

	extraResources := map[string]interface{}{
		"ssh-secret": []interface{}{secret},
	}

	ctx := mustStruct(t, map[string]interface{}{
		"apiextensions.crossplane.io/extra-resources": extraResources,
	})
	req.Context = ctx
}


// Test 1: Cloudserver not yet observed
func TestRunFunction_CloudserverNotObserved(t *testing.T) {
	f := NewFunction(logging.NewNopLogger(), &noopDeployer{})
	req := buildRequest(t, defaultXR(), nil, defaultInput())

	rsp, err := f.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rsp.Results) == 0 {
		t.Fatal("expected at least one result")
	}
	for _, r := range rsp.Results {
		if r.Severity == fnv1.Severity_SEVERITY_FATAL {
			t.Errorf("should not be fatal when Cloudserver not yet observed: %s", r.Message)
		}
	}
}

// Test 2: Cloudserver exists but publicIp is missing
func TestRunFunction_CloudserverNoPublicIP(t *testing.T) {
	f := NewFunction(logging.NewNopLogger(), &noopDeployer{})

	cs := map[string]interface{}{
		"apiVersion": "arubacloud.crossplane.io/v1alpha1",
		"kind":       "Cloudserver",
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True"},
			},
			"atProvider": map[string]interface{}{},
		},
	}

	req := buildRequest(t, defaultXR(), map[string]map[string]interface{}{"cloudserver": cs}, defaultInput())
	rsp, err := f.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, r := range rsp.Results {
		if r.Severity == fnv1.Severity_SEVERITY_FATAL {
			t.Errorf("unexpected fatal result: %s", r.Message)
		}
	}
}

// Test 3: Cloudserver ready + publicIp present → deployer is called
func TestRunFunction_CloudserverReadyTriesDeploy(t *testing.T) {
	f := NewFunction(logging.NewNopLogger(), &noopDeployer{err: context.DeadlineExceeded})

	req := buildRequest(t, defaultXR(), map[string]map[string]interface{}{
		"cloudserver": readyCloudserver("1.2.3.4"),
	}, defaultInput())
	addSSHSecret(t, req, "fake-pem-key")

	rsp, err := f.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// SSH failure should produce non-fatal result (retryable)
	for _, r := range rsp.Results {
		if r.Severity == fnv1.Severity_SEVERITY_FATAL {
			t.Errorf("SSH failure should not be fatal (should be retryable): %s", r.Message)
		}
	}
}

// Test: Successful deployment
func TestRunFunction_SuccessfulDeployment(t *testing.T) {
	f := NewFunction(logging.NewNopLogger(), &noopDeployer{err: nil})

	req := buildRequest(t, defaultXR(), map[string]map[string]interface{}{
		"cloudserver": readyCloudserver("1.2.3.4"),
	}, defaultInput())
	addSSHSecret(t, req, "fake-pem-key")

	rsp, err := f.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have a Normal result with success message
	var found bool
	for _, r := range rsp.Results {
		if r.Severity == fnv1.Severity_SEVERITY_NORMAL {
			found = true
		}
	}
	if !found {
		t.Error("expected a Normal result indicating success")
	}
}

// Test: Missing ssh-secret in context (first reconcile before function-extra-resources populates it)
func TestRunFunction_MissingSSHSecret(t *testing.T) {
	f := NewFunction(logging.NewNopLogger(), &noopDeployer{})

	req := buildRequest(t, defaultXR(), map[string]map[string]interface{}{
		"cloudserver": readyCloudserver("1.2.3.4"),
	}, defaultInput())
	// No context set — simulates first reconcile where function-extra-resources hasn't run yet.

	rsp, err := f.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should be non-fatal (retryable) — transient condition, not misconfiguration.
	for _, r := range rsp.Results {
		if r.Severity == fnv1.Severity_SEVERITY_FATAL {
			t.Errorf("missing SSH secret should produce a retryable (non-fatal) result, got fatal: %s", r.Message)
		}
	}
}
