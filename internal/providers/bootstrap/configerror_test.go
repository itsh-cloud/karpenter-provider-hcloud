package bootstrap

import (
	"context"
	"errors"
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestAsConfigError is the highest-stakes classification in this package.
//
// cluster-info is read from the apiserver on every NodeClass reconcile. Getting
// a rolling control-plane upgrade classified as configuration publishes
// BootstrapDiscoveryReady=False on every HCloudNodeClass at once, which core
// copies onto every NodePool as NodeClassReady=False, which stops scheduling
// and makes it delete in-flight NodeClaims. Getting it wrong the other way
// costs only a slower diagnosis.
func TestAsConfigError(t *testing.T) {
	gr := schema.GroupResource{Resource: "configmaps"}

	for _, tc := range []struct {
		name     string
		err      error
		isConfig bool
	}{
		// Permanent. Someone has to change something.
		{"notFound", apierrors.NewNotFound(gr, "cluster-info"), true},
		{"forbidden", apierrors.NewForbidden(gr, "cluster-info", errors.New("no")), true},
		{"unauthorized", apierrors.NewUnauthorized("no token"), true},
		{"badRequest", apierrors.NewBadRequest("malformed"), true},

		// Transient. These all happen during a normal control-plane upgrade.
		{"timeout", apierrors.NewTimeoutError("timed out", 1), false},
		{"serverTimeout", apierrors.NewServerTimeout(gr, "get", 1), false},
		{"tooManyRequests", apierrors.NewTooManyRequests("slow down", 1), false},
		{"internalError", apierrors.NewInternalError(errors.New("boom")), false},
		{"serviceUnavailable", apierrors.NewServiceUnavailable("no endpoints"), false},
		{"connectionRefused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, false},
		{"contextCanceled", context.Canceled, false},
		{"deadlineExceeded", context.DeadlineExceeded, false},
		{"unwrapped", errors.New("etcdserver: request timed out"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var configErr *ConfigError
			got := errors.As(asConfigError(tc.err), &configErr)
			if got != tc.isConfig {
				t.Errorf("asConfigError(%v) classified as config=%v, want %v", tc.err, got, tc.isConfig)
			}
		})
	}
}

// TestAsConfigErrorPreservesWrapping: the caller renders the underlying error
// into the condition message, so the cause has to survive classification.
func TestAsConfigErrorPreservesWrapping(t *testing.T) {
	underlying := apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "cluster-info")
	wrapped := asConfigError(underlying)

	if !apierrors.IsNotFound(wrapped) {
		t.Error("wrapping lost the apierrors identity of the cause")
	}
	if !errors.Is(wrapped, underlying) {
		t.Error("errors.Is cannot reach the cause through ConfigError")
	}
}

// TestRefreshClassifiesContentFailuresAsConfig: a ConfigMap that was read
// successfully but cannot be used is a configuration problem, not a blip, and
// retrying it forever would leave the operator with nothing on the object.
func TestRefreshClassifiesContentFailuresAsConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string]string
	}{
		{"noKubeconfigKey", map[string]string{"other": "x"}},
		{"unparseableKubeconfig", map[string]string{"kubeconfig": "{{not yaml"}},
		{"noServer", map[string]string{"kubeconfig": "apiVersion: v1\nkind: Config\nclusters:\n- name: \"\"\n  cluster: {}\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDiscovery(&stubReader{data: tc.data})
			err := d.Refresh(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Errorf("Refresh returned %v, want it classified as configuration", err)
			}
		})
	}
}

// TestRefreshClassifiesReadFailuresByCause proves the split is made on the Get
// error rather than on the fact that Refresh failed at all.
func TestRefreshClassifiesReadFailuresByCause(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		isConfig bool
	}{
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "cluster-info", errors.New("no")), true},
		{"serviceUnavailable", apierrors.NewServiceUnavailable("apiserver restarting"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDiscovery(&stubReader{err: tc.err})
			err := d.Refresh(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			var configErr *ConfigError
			if got := errors.As(err, &configErr); got != tc.isConfig {
				t.Errorf("Refresh classified config=%v, want %v (err %v)", got, tc.isConfig, err)
			}
		})
	}
}

// TestRefreshKeepsLastGoodValuesOnFailure: a failed refresh must not blank the
// cached endpoint and pins, because the caller resolves against them and a
// half-populated join is a server that boots and never joins.
func TestRefreshKeepsLastGoodValuesOnFailure(t *testing.T) {
	caPEM, _ := makeCA(t, false)
	r := &stubReader{data: map[string]string{"kubeconfig": kubeconfig("https://10.1.0.2:6443", caPEM)}}
	d := NewDiscovery(r)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	endpoint, hashes, ok := d.Get()
	if !ok {
		t.Fatal("first refresh produced nothing")
	}

	r.err = apierrors.NewServiceUnavailable("apiserver restarting")
	if err := d.Refresh(context.Background()); err == nil {
		t.Fatal("expected the second refresh to fail")
	}

	gotEndpoint, gotHashes, gotOK := d.Get()
	if !gotOK || gotEndpoint != endpoint || len(gotHashes) != len(hashes) {
		t.Errorf("Get() = %q, %v, %v after a failed refresh; want the last good values preserved",
			gotEndpoint, gotHashes, gotOK)
	}
}

type stubReader struct {
	data map[string]string
	err  error
}

func (s *stubReader) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if s.err != nil {
		return s.err
	}
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return errors.New("unexpected type")
	}
	cm.ObjectMeta = metav1.ObjectMeta{Namespace: ClusterInfoNamespace, Name: ClusterInfoName}
	cm.Data = s.data
	return nil
}

func (s *stubReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("List must not be called: this reader must be uncached, and the RBAC for it is get on one named ConfigMap")
}
