/*
Copyright 2026.

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

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openstack-k8s-operators/lib-common/modules/common/clusterdns"
	"github.com/openstack-k8s-operators/lib-common/modules/common/service"
	"github.com/openstack-k8s-operators/lib-common/modules/common/util"
	swiftv1beta1 "github.com/openstack-k8s-operators/swift-operator/api/v1beta1"
)

func TestSwiftProxyServicesUseBarePortNamesAndEndpointDNSAliases(t *testing.T) {
	ctx := context.Background()
	const namespace = "swiftproxy-reuse"

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := swiftv1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	customURLs := map[service.Endpoint]string{
		service.EndpointPublic:   "https://swift-public.custom.example.test",
		service.EndpointInternal: "https://swift-internal.custom.example.test",
	}
	proxy := &swiftv1beta1.SwiftProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "swiftproxy-reuse", Namespace: namespace},
		Spec: swiftv1beta1.SwiftProxySpec{
			SwiftProxySpecCore: swiftv1beta1.SwiftProxySpecCore{
				Replicas:          ptr.To[int32](1),
				Secret:            "osp-secret",
				MemcachedInstance: "memcached",
				Override: swiftv1beta1.ProxyOverrideSpec{
					Service: map[service.Endpoint]service.RoutedOverrideSpec{
						service.EndpointPublic:   loadBalancerOverride(customURLs[service.EndpointPublic]),
						service.EndpointInternal: loadBalancerOverride(customURLs[service.EndpointInternal]),
					},
				},
			},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&swiftv1beta1.SwiftProxy{}, &corev1.Service{}).
		WithObjects(proxy).
		Build()
	reconciler := &SwiftProxyReconciler{Client: k8sClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: proxy.Name, Namespace: namespace}}
	reconcile := func() error {
		_, err := reconciler.Reconcile(ctx, request)
		return err
	}

	if err := reconcile(); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	expectLoadBalancerPending(t, reconcile())

	internalService := getService(t, k8sClient, namespace, "swift-internal")
	internalService.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}
	if err := k8sClient.Status().Update(ctx, internalService); err != nil {
		t.Fatalf("mark internal Service ready: %v", err)
	}
	expectLoadBalancerPending(t, reconcile())

	endpoints := []service.Endpoint{service.EndpointPublic, service.EndpointInternal}
	for _, endpoint := range endpoints {
		serviceName := "swift-" + string(endpoint)
		defaultName := fmt.Sprintf("%s.%s.svc", serviceName, namespace)
		fqdn := defaultName + "." + clusterdns.GetDNSClusterDomain()
		assertServiceEndpoint(t, k8sClient, namespace, serviceName, string(endpoint), customURLs[endpoint][len("https://"):], []string{defaultName, fqdn})
	}

	updateEndpointURLs(t, k8sClient, ctx, namespace, nil, nil)
	expectLoadBalancerPending(t, reconcile())
	for _, endpoint := range endpoints {
		serviceName := "swift-" + string(endpoint)
		defaultName := fmt.Sprintf("%s.%s.svc", serviceName, namespace)
		fqdn := defaultName + "." + clusterdns.GetDNSClusterDomain()
		assertServiceEndpoint(t, k8sClient, namespace, serviceName, string(endpoint), defaultName, []string{fqdn})
	}

	publicName := "swift-public." + namespace + ".svc"
	internalName := "swift-internal." + namespace + ".svc"
	internalFQDN := internalName + "." + clusterdns.GetDNSClusterDomain()
	updateEndpointURLs(
		t,
		k8sClient,
		ctx,
		namespace,
		ptr.To("https://"+publicName),
		ptr.To("https://"+internalFQDN),
	)
	expectLoadBalancerPending(t, reconcile())
	assertServiceEndpoint(
		t, k8sClient, namespace, "swift-public", string(service.EndpointPublic),
		publicName, []string{publicName + "." + clusterdns.GetDNSClusterDomain()},
	)
	assertServiceEndpoint(
		t, k8sClient, namespace, "swift-internal", string(service.EndpointInternal),
		internalFQDN, []string{internalName},
	)
}

func updateEndpointURLs(
	t *testing.T,
	k8sClient client.Client,
	ctx context.Context,
	namespace string,
	publicURL *string,
	internalURL *string,
) {
	t.Helper()

	proxy := &swiftv1beta1.SwiftProxy{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "swiftproxy-reuse", Namespace: namespace}, proxy); err != nil {
		t.Fatal(err)
	}
	publicOverride := proxy.Spec.Override.Service[service.EndpointPublic]
	publicOverride.EndpointURL = publicURL
	proxy.Spec.Override.Service[service.EndpointPublic] = publicOverride
	internalOverride := proxy.Spec.Override.Service[service.EndpointInternal]
	internalOverride.EndpointURL = internalURL
	proxy.Spec.Override.Service[service.EndpointInternal] = internalOverride
	if err := k8sClient.Update(ctx, proxy); err != nil {
		t.Fatalf("update endpoint URLs: %v", err)
	}
}

func loadBalancerOverride(endpointURL string) service.RoutedOverrideSpec {
	return service.RoutedOverrideSpec{
		OverrideSpec: service.OverrideSpec{
			EmbeddedLabelsAnnotations: &service.EmbeddedLabelsAnnotations{
				Annotations: map[string]string{
					service.AnnotationHostnameKey: "stale.example.test",
					service.AnnotationCNAMEsKey:   "stale.example.test",
				},
			},
			Spec: &service.OverrideServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		},
		EndpointURL: ptr.To(endpointURL),
	}
}

func expectLoadBalancerPending(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, util.ErrResourceIsNotReady) {
		t.Fatalf("expected a pending LoadBalancer, got %v", err)
	}
}

func getService(t *testing.T, k8sClient client.Client, namespace, name string) *corev1.Service {
	t.Helper()

	svc := &corev1.Service{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, svc); err != nil {
		t.Fatal(err)
	}
	return svc
}

func assertServiceEndpoint(
	t *testing.T,
	k8sClient client.Client,
	namespace string,
	name string,
	portName string,
	hostname string,
	aliases []string,
) {
	t.Helper()

	svc := getService(t, k8sClient, namespace, name)
	if svc.Name != name {
		t.Errorf("Service name = %q, want %q", svc.Name, name)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("Service %s has %d ports, want one", name, len(svc.Spec.Ports))
	}
	if svc.Spec.Ports[0].Name != portName {
		t.Errorf("Service %s port name = %q, want %q", name, svc.Spec.Ports[0].Name, portName)
	}
	if got := svc.Annotations[service.AnnotationHostnameKey]; got != hostname {
		t.Errorf("Service %s hostname annotation = %q, want %q", name, got, hostname)
	}
	if got, want := svc.Annotations[service.AnnotationCNAMEsKey], strings.Join(aliases, ","); got != want {
		t.Errorf("Service %s CNAME annotation = %q, want %q", name, got, want)
	}
}
