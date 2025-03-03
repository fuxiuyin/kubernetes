/*
Copyright 2014 The Kubernetes Authors.

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

package lifecycle

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
)

func TestResolvePortInt(t *testing.T) {
	expected := 80
	port, err := resolvePort(intstr.FromInt(expected), &v1.Container{})
	if port != expected {
		t.Errorf("expected: %d, saw: %d", expected, port)
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolvePortString(t *testing.T) {
	expected := 80
	name := "foo"
	container := &v1.Container{
		Ports: []v1.ContainerPort{
			{Name: name, ContainerPort: int32(expected)},
		},
	}
	port, err := resolvePort(intstr.FromString(name), container)
	if port != expected {
		t.Errorf("expected: %d, saw: %d", expected, port)
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolvePortStringUnknown(t *testing.T) {
	expected := int32(80)
	name := "foo"
	container := &v1.Container{
		Ports: []v1.ContainerPort{
			{Name: "bar", ContainerPort: expected},
		},
	}
	port, err := resolvePort(intstr.FromString(name), container)
	if port != -1 {
		t.Errorf("expected: -1, saw: %d", port)
	}
	if err == nil {
		t.Error("unexpected non-error")
	}
}

type fakeContainerCommandRunner struct {
	Cmd []string
	ID  kubecontainer.ContainerID
	Err error
	Msg string
}

func (f *fakeContainerCommandRunner) RunInContainer(id kubecontainer.ContainerID, cmd []string, timeout time.Duration) ([]byte, error) {
	f.Cmd = cmd
	f.ID = id
	return []byte(f.Msg), f.Err
}

func stubPodStatusProvider(podIP string) podStatusProvider {
	return podStatusProviderFunc(func(uid types.UID, name, namespace string) (*kubecontainer.PodStatus, error) {
		return &kubecontainer.PodStatus{
			ID:        uid,
			Name:      name,
			Namespace: namespace,
			IPs:       []string{podIP},
		}, nil
	})
}

type podStatusProviderFunc func(uid types.UID, name, namespace string) (*kubecontainer.PodStatus, error)

func (f podStatusProviderFunc) GetPodStatus(uid types.UID, name, namespace string) (*kubecontainer.PodStatus, error) {
	return f(uid, name, namespace)
}

func TestRunHandlerExec(t *testing.T) {
	fakeCommandRunner := fakeContainerCommandRunner{}
	handlerRunner := NewHandlerRunner(&fakeHTTP{}, &fakeCommandRunner, nil)

	containerID := kubecontainer.ContainerID{Type: "test", ID: "abc1234"}
	containerName := "containerFoo"

	container := v1.Container{
		Name: containerName,
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{
				Exec: &v1.ExecAction{
					Command: []string{"ls", "-a"},
				},
			},
		},
	}

	pod := v1.Pod{}
	pod.ObjectMeta.Name = "podFoo"
	pod.ObjectMeta.Namespace = "nsFoo"
	pod.Spec.Containers = []v1.Container{container}
	_, err := handlerRunner.Run(containerID, &pod, &container, container.Lifecycle.PostStart)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if fakeCommandRunner.ID != containerID ||
		!reflect.DeepEqual(container.Lifecycle.PostStart.Exec.Command, fakeCommandRunner.Cmd) {
		t.Errorf("unexpected commands: %v", fakeCommandRunner)
	}
}

type fakeHTTP struct {
	url     string
	headers http.Header
	err     error
	resp    *http.Response
}

func (f *fakeHTTP) Do(req *http.Request) (*http.Response, error) {
	f.url = req.URL.String()
	f.headers = req.Header.Clone()
	return f.resp, f.err
}

func TestRunHandlerHttp(t *testing.T) {
	fakeHTTPGetter := fakeHTTP{}
	fakePodStatusProvider := stubPodStatusProvider("127.0.0.1")
	handlerRunner := NewHandlerRunner(&fakeHTTPGetter, &fakeContainerCommandRunner{}, fakePodStatusProvider)

	containerID := kubecontainer.ContainerID{Type: "test", ID: "abc1234"}
	containerName := "containerFoo"

	container := v1.Container{
		Name: containerName,
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{
				HTTPGet: &v1.HTTPGetAction{
					Host: "foo",
					Port: intstr.FromInt(8080),
					Path: "bar",
				},
			},
		},
	}
	pod := v1.Pod{}
	pod.ObjectMeta.Name = "podFoo"
	pod.ObjectMeta.Namespace = "nsFoo"
	pod.ObjectMeta.UID = "foo-bar-quux"
	pod.Spec.Containers = []v1.Container{container}
	_, err := handlerRunner.Run(containerID, &pod, &container, container.Lifecycle.PostStart)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if fakeHTTPGetter.url != "http://foo:8080/bar" {
		t.Errorf("unexpected url: %s", fakeHTTPGetter.url)
	}
}

func TestRunHandlerHttpWithHeaders(t *testing.T) {
	fakeHTTPDoer := fakeHTTP{}
	fakePodStatusProvider := stubPodStatusProvider("127.0.0.1")

	handlerRunner := NewHandlerRunner(&fakeHTTPDoer, &fakeContainerCommandRunner{}, fakePodStatusProvider)

	containerID := kubecontainer.ContainerID{Type: "test", ID: "abc1234"}
	containerName := "containerFoo"

	container := v1.Container{
		Name: containerName,
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{
				HTTPGet: &v1.HTTPGetAction{
					Host: "foo",
					Port: intstr.FromInt(8080),
					Path: "/bar",
					HTTPHeaders: []v1.HTTPHeader{
						{Name: "Foo", Value: "bar"},
					},
				},
			},
		},
	}
	pod := v1.Pod{}
	pod.ObjectMeta.Name = "podFoo"
	pod.ObjectMeta.Namespace = "nsFoo"
	pod.Spec.Containers = []v1.Container{container}
	_, err := handlerRunner.Run(containerID, &pod, &container, container.Lifecycle.PostStart)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if fakeHTTPDoer.url != "http://foo:8080/bar" {
		t.Errorf("unexpected url: %s", fakeHTTPDoer.url)
	}
	if fakeHTTPDoer.headers["Foo"][0] != "bar" {
		t.Errorf("missing http header: %s", fakeHTTPDoer.headers)
	}
}

func TestRunHandlerHttps(t *testing.T) {
	fakeHTTPDoer := fakeHTTP{}
	fakePodStatusProvider := stubPodStatusProvider("127.0.0.1")
	handlerRunner := NewHandlerRunner(&fakeHTTPDoer, &fakeContainerCommandRunner{}, fakePodStatusProvider)

	containerID := kubecontainer.ContainerID{Type: "test", ID: "abc1234"}
	containerName := "containerFoo"

	container := v1.Container{
		Name: containerName,
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{
				HTTPGet: &v1.HTTPGetAction{
					Scheme: v1.URISchemeHTTPS,
					Host:   "foo",
					Path:   "bar",
				},
			},
		},
	}
	pod := v1.Pod{}
	pod.ObjectMeta.Name = "podFoo"
	pod.ObjectMeta.Namespace = "nsFoo"
	pod.Spec.Containers = []v1.Container{container}

	t.Run("consistent", func(t *testing.T) {
		container.Lifecycle.PostStart.HTTPGet.Port = intstr.FromString("70")
		pod.Spec.Containers = []v1.Container{container}
		_, err := handlerRunner.Run(containerID, &pod, &container, container.Lifecycle.PostStart)

		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if fakeHTTPDoer.url != "https://foo:70/bar" {
			t.Errorf("unexpected url: %s", fakeHTTPDoer.url)
		}
	})

	t.Run("inconsistent", func(t *testing.T) {
		defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentHTTPGetHandlers, false)()
		container.Lifecycle.PostStart.HTTPGet.Port = intstr.FromString("70")
		pod.Spec.Containers = []v1.Container{container}
		_, err := handlerRunner.Run(containerID, &pod, &container, container.Lifecycle.PostStart)

		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if fakeHTTPDoer.url != "http://foo:70/bar" {
			t.Errorf("unexpected url: %q", fakeHTTPDoer.url)
		}
	})
}

func TestRunHandlerHTTPPort(t *testing.T) {
	tests := []struct {
		Name               string
		FeatureGateEnabled bool
		Port               intstr.IntOrString
		ExpectError        bool
		Expected           string
	}{
		{
			Name:               "consistent/with port",
			FeatureGateEnabled: true,
			Port:               intstr.FromString("70"),
			Expected:           "https://foo:70/bar",
		}, {
			Name:               "consistent/without port",
			FeatureGateEnabled: true,
			Port:               intstr.FromString(""),
			ExpectError:        true,
		}, {
			Name:               "inconsistent/with port",
			FeatureGateEnabled: false,
			Port:               intstr.FromString("70"),
			Expected:           "http://foo:70/bar",
		}, {
			Name:               "inconsistent/without port",
			Port:               intstr.FromString(""),
			FeatureGateEnabled: false,
			Expected:           "http://foo:80/bar",
		},
	}

	fakePodStatusProvider := stubPodStatusProvider("127.0.0.1")

	containerID := kubecontainer.ContainerID{Type: "test", ID: "abc1234"}
	containerName := "containerFoo"

	container := v1.Container{
		Name: containerName,
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{
				HTTPGet: &v1.HTTPGetAction{
					Scheme: v1.URISchemeHTTPS,
					Host:   "foo",
					Port:   intstr.FromString("unexpected"),
					Path:   "bar",
				},
			},
		},
	}
	pod := v1.Pod{}
	pod.ObjectMeta.Name = "podFoo"
	pod.ObjectMeta.Namespace = "nsFoo"
	pod.Spec.Containers = []v1.Container{container}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentHTTPGetHandlers, tt.FeatureGateEnabled)()
			fakeHTTPDoer := fakeHTTP{}
			handlerRunner := NewHandlerRunner(&fakeHTTPDoer, &fakeContainerCommandRunner{}, fakePodStatusProvider)

			container.Lifecycle.PostStart.HTTPGet.Port = tt.Port
			pod.Spec.Containers = []v1.Container{container}
			_, err := handlerRunner.Run(containerID, &pod, &container, container.Lifecycle.PostStart)

			if hasError := (err != nil); hasError != tt.ExpectError {
				t.Errorf("unexpected error: %v", err)
			}

			if fakeHTTPDoer.url != tt.Expected {
				t.Errorf("unexpected url: %s", fakeHTTPDoer.url)
			}
		})
	}
}

func TestRunHTTPHandler(t *testing.T) {
	type expected struct {
		OldURL    string
		OldHeader http.Header
		NewURL    string
		NewHeader http.Header
	}

	tests := []struct {
		Name     string
		PodIP    string
		HTTPGet  *v1.HTTPGetAction
		Expected expected
	}{
		{
			Name:  "missing pod IP",
			PodIP: "",
			HTTPGet: &v1.HTTPGetAction{
				Path:        "foo",
				Port:        intstr.FromString("42"),
				Host:        "example.test",
				Scheme:      "http",
				HTTPHeaders: []v1.HTTPHeader{},
			},
			Expected: expected{
				OldURL:    "http://example.test:42/foo",
				OldHeader: http.Header{},
				NewURL:    "http://example.test:42/foo",
				NewHeader: http.Header{
					"Accept":     {"*/*"},
					"User-Agent": {"kube-lifecycle/."},
				},
			},
		}, {
			Name:  "missing host",
			PodIP: "233.252.0.1",
			HTTPGet: &v1.HTTPGetAction{
				Path:        "foo",
				Port:        intstr.FromString("42"),
				Scheme:      "http",
				HTTPHeaders: []v1.HTTPHeader{},
			},
			Expected: expected{
				OldURL:    "http://233.252.0.1:42/foo",
				OldHeader: http.Header{},
				NewURL:    "http://233.252.0.1:42/foo",
				NewHeader: http.Header{
					"Accept":     {"*/*"},
					"User-Agent": {"kube-lifecycle/."},
				},
			},
		}, {
			Name:  "path with leading slash",
			PodIP: "233.252.0.1",
			HTTPGet: &v1.HTTPGetAction{
				Path:        "/foo",
				Port:        intstr.FromString("42"),
				Scheme:      "http",
				HTTPHeaders: []v1.HTTPHeader{},
			},
			Expected: expected{
				OldURL:    "http://233.252.0.1:42//foo",
				OldHeader: http.Header{},
				NewURL:    "http://233.252.0.1:42/foo",
				NewHeader: http.Header{
					"Accept":     {"*/*"},
					"User-Agent": {"kube-lifecycle/."},
				},
			},
		}, {
			Name:  "path without leading slash",
			PodIP: "233.252.0.1",
			HTTPGet: &v1.HTTPGetAction{
				Path:        "foo",
				Port:        intstr.FromString("42"),
				Scheme:      "http",
				HTTPHeaders: []v1.HTTPHeader{},
			},
			Expected: expected{
				OldURL:    "http://233.252.0.1:42/foo",
				OldHeader: http.Header{},
				NewURL:    "http://233.252.0.1:42/foo",
				NewHeader: http.Header{
					"Accept":     {"*/*"},
					"User-Agent": {"kube-lifecycle/."},
				},
			},
		}, {
			Name:  "port resolution",
			PodIP: "233.252.0.1",
			HTTPGet: &v1.HTTPGetAction{
				Path:        "foo",
				Port:        intstr.FromString("quux"),
				Scheme:      "http",
				HTTPHeaders: []v1.HTTPHeader{},
			},
			Expected: expected{
				OldURL:    "http://233.252.0.1:8080/foo",
				OldHeader: http.Header{},
				NewURL:    "http://233.252.0.1:8080/foo",
				NewHeader: http.Header{
					"Accept":     {"*/*"},
					"User-Agent": {"kube-lifecycle/."},
				},
			},
		}, {
			Name:  "https",
			PodIP: "233.252.0.1",
			HTTPGet: &v1.HTTPGetAction{
				Path:        "foo",
				Port:        intstr.FromString("4430"),
				Scheme:      "https",
				HTTPHeaders: []v1.HTTPHeader{},
			},
			Expected: expected{
				OldURL:    "http://233.252.0.1:4430/foo",
				OldHeader: http.Header{},
				NewURL:    "https://233.252.0.1:4430/foo",
				NewHeader: http.Header{
					"Accept":     {"*/*"},
					"User-Agent": {"kube-lifecycle/."},
				},
			},
		}, {
			Name:  "unknown scheme",
			PodIP: "233.252.0.1",
			HTTPGet: &v1.HTTPGetAction{
				Path:        "foo",
				Port:        intstr.FromString("80"),
				Scheme:      "baz",
				HTTPHeaders: []v1.HTTPHeader{},
			},
			Expected: expected{
				OldURL:    "http://233.252.0.1:80/foo",
				OldHeader: http.Header{},
				NewURL:    "baz://233.252.0.1:80/foo",
				NewHeader: http.Header{
					"Accept":     {"*/*"},
					"User-Agent": {"kube-lifecycle/."},
				},
			},
		}, {
			Name:  "query param",
			PodIP: "233.252.0.1",
			HTTPGet: &v1.HTTPGetAction{
				Path:        "foo?k=v",
				Port:        intstr.FromString("80"),
				Scheme:      "http",
				HTTPHeaders: []v1.HTTPHeader{},
			},
			Expected: expected{
				OldURL:    "http://233.252.0.1:80/foo?k=v",
				OldHeader: http.Header{},
				NewURL:    "http://233.252.0.1:80/foo?k=v",
				NewHeader: http.Header{
					"Accept":     {"*/*"},
					"User-Agent": {"kube-lifecycle/."},
				},
			},
		}, {
			Name:  "fragment",
			PodIP: "233.252.0.1",
			HTTPGet: &v1.HTTPGetAction{
				Path:        "foo#frag",
				Port:        intstr.FromString("80"),
				Scheme:      "http",
				HTTPHeaders: []v1.HTTPHeader{},
			},
			Expected: expected{
				OldURL:    "http://233.252.0.1:80/foo#frag",
				OldHeader: http.Header{},
				NewURL:    "http://233.252.0.1:80/foo#frag",
				NewHeader: http.Header{
					"Accept":     {"*/*"},
					"User-Agent": {"kube-lifecycle/."},
				},
			},
		}, {
			Name:  "headers",
			PodIP: "233.252.0.1",
			HTTPGet: &v1.HTTPGetAction{
				Path:   "foo",
				Port:   intstr.FromString("80"),
				Scheme: "http",
				HTTPHeaders: []v1.HTTPHeader{
					{
						Name:  "Foo",
						Value: "bar",
					},
				},
			},
			Expected: expected{
				OldURL:    "http://233.252.0.1:80/foo",
				OldHeader: http.Header{},
				NewURL:    "http://233.252.0.1:80/foo",
				NewHeader: http.Header{
					"Accept":     {"*/*"},
					"Foo":        {"bar"},
					"User-Agent": {"kube-lifecycle/."},
				},
			},
		}, {
			Name:  "host header",
			PodIP: "233.252.0.1",
			HTTPGet: &v1.HTTPGetAction{
				Host:   "example.test",
				Path:   "foo",
				Port:   intstr.FromString("80"),
				Scheme: "http",
				HTTPHeaders: []v1.HTTPHeader{
					{
						Name:  "Host",
						Value: "from.header",
					},
				},
			},
			Expected: expected{
				OldURL:    "http://example.test:80/foo",
				OldHeader: http.Header{},
				NewURL:    "http://example.test:80/foo",
				NewHeader: http.Header{
					"Accept":     {"*/*"},
					"User-Agent": {"kube-lifecycle/."},
					"Host":       {"from.header"},
				},
			},
		},
	}

	containerID := kubecontainer.ContainerID{Type: "test", ID: "abc1234"}
	containerName := "containerFoo"

	container := v1.Container{
		Name: containerName,
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{},
		},
		Ports: []v1.ContainerPort{
			{
				Name:          "quux",
				ContainerPort: 8080,
			},
		},
	}

	pod := v1.Pod{}
	pod.ObjectMeta.Name = "podFoo"
	pod.ObjectMeta.Namespace = "nsFoo"
	pod.Spec.Containers = []v1.Container{container}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			fakePodStatusProvider := stubPodStatusProvider(tt.PodIP)

			container.Lifecycle.PostStart.HTTPGet = tt.HTTPGet
			pod.Spec.Containers = []v1.Container{container}

			verify := func(t *testing.T, expectedHeader http.Header, expectedURL string) {
				fakeHTTPDoer := fakeHTTP{}
				handlerRunner := NewHandlerRunner(&fakeHTTPDoer, &fakeContainerCommandRunner{}, fakePodStatusProvider)

				_, err := handlerRunner.Run(containerID, &pod, &container, container.Lifecycle.PostStart)
				if err != nil {
					t.Fatal(err)
				}

				if diff := cmp.Diff(expectedHeader, fakeHTTPDoer.headers); diff != "" {
					t.Errorf("unexpected header (-want, +got)\n:%s", diff)
				}
				if fakeHTTPDoer.url != expectedURL {
					t.Errorf("url = %v; want %v", fakeHTTPDoer.url, tt.Expected.NewURL)
				}
			}

			t.Run("consistent", func(t *testing.T) {
				defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentHTTPGetHandlers, true)()
				verify(t, tt.Expected.NewHeader, tt.Expected.NewURL)
			})

			t.Run("inconsistent", func(t *testing.T) {
				defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentHTTPGetHandlers, false)()
				verify(t, tt.Expected.OldHeader, tt.Expected.OldURL)
			})
		})
	}
}

func TestRunHandlerNil(t *testing.T) {
	handlerRunner := NewHandlerRunner(&fakeHTTP{}, &fakeContainerCommandRunner{}, nil)
	containerID := kubecontainer.ContainerID{Type: "test", ID: "abc1234"}
	podName := "podFoo"
	podNamespace := "nsFoo"
	containerName := "containerFoo"

	container := v1.Container{
		Name: containerName,
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{},
		},
	}
	pod := v1.Pod{}
	pod.ObjectMeta.Name = podName
	pod.ObjectMeta.Namespace = podNamespace
	pod.Spec.Containers = []v1.Container{container}
	_, err := handlerRunner.Run(containerID, &pod, &container, container.Lifecycle.PostStart)
	if err == nil {
		t.Errorf("expect error, but got nil")
	}
}

func TestRunHandlerExecFailure(t *testing.T) {
	expectedErr := fmt.Errorf("invalid command")
	fakeCommandRunner := fakeContainerCommandRunner{Err: expectedErr, Msg: expectedErr.Error()}
	handlerRunner := NewHandlerRunner(&fakeHTTP{}, &fakeCommandRunner, nil)

	containerID := kubecontainer.ContainerID{Type: "test", ID: "abc1234"}
	containerName := "containerFoo"
	command := []string{"ls", "--a"}

	container := v1.Container{
		Name: containerName,
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{
				Exec: &v1.ExecAction{
					Command: command,
				},
			},
		},
	}

	pod := v1.Pod{}
	pod.ObjectMeta.Name = "podFoo"
	pod.ObjectMeta.Namespace = "nsFoo"
	pod.Spec.Containers = []v1.Container{container}
	expectedErrMsg := fmt.Sprintf("Exec lifecycle hook (%s) for Container %q in Pod %q failed - error: %v, message: %q", command, containerName, format.Pod(&pod), expectedErr, expectedErr.Error())
	msg, err := handlerRunner.Run(containerID, &pod, &container, container.Lifecycle.PostStart)
	if err == nil {
		t.Errorf("expected error: %v", expectedErr)
	}
	if msg != expectedErrMsg {
		t.Errorf("unexpected error message: %q; expected %q", msg, expectedErrMsg)
	}
}

func TestRunHandlerHttpFailure(t *testing.T) {
	expectedErr := fmt.Errorf("fake http error")
	expectedResp := http.Response{
		Body: io.NopCloser(strings.NewReader(expectedErr.Error())),
	}
	fakeHTTPGetter := fakeHTTP{err: expectedErr, resp: &expectedResp}

	fakePodStatusProvider := stubPodStatusProvider("127.0.0.1")

	handlerRunner := NewHandlerRunner(&fakeHTTPGetter, &fakeContainerCommandRunner{}, fakePodStatusProvider)

	containerName := "containerFoo"
	containerID := kubecontainer.ContainerID{Type: "test", ID: "abc1234"}
	container := v1.Container{
		Name: containerName,
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{
				HTTPGet: &v1.HTTPGetAction{
					Host: "foo",
					Port: intstr.FromInt(8080),
					Path: "bar",
				},
			},
		},
	}
	pod := v1.Pod{}
	pod.ObjectMeta.Name = "podFoo"
	pod.ObjectMeta.Namespace = "nsFoo"
	pod.Spec.Containers = []v1.Container{container}
	expectedErrMsg := fmt.Sprintf("HTTP lifecycle hook (%s) for Container %q in Pod %q failed - error: %v", "bar", containerName, format.Pod(&pod), expectedErr)
	msg, err := handlerRunner.Run(containerID, &pod, &container, container.Lifecycle.PostStart)
	if err == nil {
		t.Errorf("expected error: %v", expectedErr)
	}
	if msg != expectedErrMsg {
		t.Errorf("unexpected error message: %q; expected %q", msg, expectedErrMsg)
	}
	if fakeHTTPGetter.url != "http://foo:8080/bar" {
		t.Errorf("unexpected url: %s", fakeHTTPGetter.url)
	}
}

func TestRunHandlerHttpsFailureFallback(t *testing.T) {
	var actualHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		actualHeaders = r.Header.Clone()
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	fakePodStatusProvider := stubPodStatusProvider("127.0.0.1")

	handlerRunner := NewHandlerRunner(srv.Client(), &fakeContainerCommandRunner{}, fakePodStatusProvider).(*handlerRunner)

	containerName := "containerFoo"
	containerID := kubecontainer.ContainerID{Type: "test", ID: "abc1234"}
	container := v1.Container{
		Name: containerName,
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{
				HTTPGet: &v1.HTTPGetAction{
					// set the scheme to https to ensure it falls back to HTTP.
					Scheme: "https",
					Host:   "127.0.0.1",
					Port:   intstr.FromString(port),
					Path:   "bar",
					HTTPHeaders: []v1.HTTPHeader{
						{
							Name:  "Authorization",
							Value: "secret",
						},
					},
				},
			},
		},
	}
	pod := v1.Pod{}
	pod.ObjectMeta.Name = "podFoo"
	pod.ObjectMeta.Namespace = "nsFoo"
	pod.Spec.Containers = []v1.Container{container}
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentHTTPGetHandlers, true)()
	msg, err := handlerRunner.Run(containerID, &pod, &container, container.Lifecycle.PostStart)

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if msg != "" {
		t.Errorf("unexpected error message: %q", msg)
	}
	if actualHeaders.Get("Authorization") != "" {
		t.Error("unexpected Authorization header")
	}
}

func TestIsHTTPResponseError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer s.Close()
	req, err := http.NewRequest("GET", s.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.Scheme = "https"
	_, err = http.DefaultClient.Do(req)
	if !isHTTPResponseError(err) {
		t.Errorf("unexpected http response error: %v", err)
	}
}
