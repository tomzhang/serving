/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package serverlessservice

import (
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	fakekubeclient "knative.dev/pkg/client/injection/kube/client/fake"
	fakeendpointsinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/endpoints/fake"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	fakedynamicclient "knative.dev/pkg/injection/clients/dynamicclient/fake"
	"knative.dev/serving/pkg/apis/networking"
	fakeservingclient "knative.dev/serving/pkg/client/injection/client/fake"
	"knative.dev/serving/pkg/client/injection/ducks/autoscaling/v1alpha1/podscalable"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

	. "knative.dev/pkg/reconciler/testing"
	. "knative.dev/serving/pkg/reconciler/testing/v1"
	. "knative.dev/serving/pkg/testing"
)

func TestGlobalResyncOnActivatorChange(t *testing.T) {
	const (
		ns1  = "test-ns1"
		ns2  = "test-ns2"
		sks1 = "test-sks-1"
		sks2 = "test-sks-2"
	)
	ctx, cancel, informers := SetupFakeContextWithCancel(t)
	// Replace the fake dynamic client with one containing our objects.
	ctx, _ = fakedynamicclient.With(ctx, runtime.NewScheme(),
		ToUnstructured(t, NewScheme(), []runtime.Object{deploy(ns1, sks1), deploy(ns2, sks2)})...,
	)
	ctx = podscalable.WithDuck(ctx)
	ctrl := NewController(ctx, configmap.NewStaticWatcher())

	grp := errgroup.Group{}

	kubeClnt := fakekubeclient.Get(ctx)

	// Create activator endpoints.
	aEps := activatorEndpoints(WithSubsets)
	if _, err := kubeClnt.CoreV1().Endpoints(aEps.Namespace).Create(aEps); err != nil {
		t.Fatal("Error creating activator endpoints:", err)
	}

	// Private endpoints are supposed to exist, since we're using selector mode for the service.
	privEps := endpointspriv(ns1, sks1)
	if _, err := kubeClnt.CoreV1().Endpoints(privEps.Namespace).Create(privEps); err != nil {
		t.Fatal("Error creating private endpoints:", err)
	}
	// This is passive, so no endpoints.
	privEps = endpointspriv(ns2, sks2, withOtherSubsets)
	if _, err := kubeClnt.CoreV1().Endpoints(privEps.Namespace).Create(privEps); err != nil {
		t.Fatal("Error creating private endpoints:", err)
	}

	waitInformers, err := controller.RunInformers(ctx.Done(), informers...)
	if err != nil {
		t.Fatal("Error starting informers:", err)
	}
	defer func() {
		cancel()
		if err := grp.Wait(); err != nil {
			t.Fatal("Error waiting for contoller to terminate:", err)
		}
		waitInformers()
	}()

	grp.Go(func() error {
		return ctrl.Run(1, ctx.Done())
	})

	// Due to the fact that registering reactors is not guarded by locks in k8s
	// fake clients we need to pre-register those.
	updateHooks := NewHooks()
	updateHooks.OnUpdate(&kubeClnt.Fake, "endpoints", func(obj runtime.Object) HookResult {
		eps := obj.(*corev1.Endpoints)
		if eps.Name == sks1 {
			t.Logf("Registering expected hook update for endpoints %s", eps.Name)
			return HookComplete
		}
		if eps.Name == networking.ActivatorServiceName {
			// Expected, but not the one we're waiting for.
			t.Log("Registering activator endpoint update")
		} else {
			// Something's broken.
			t.Errorf("Unexpected endpoint update for %s", eps.Name)
		}
		return HookIncomplete
	})

	// Inactive, will reconcile.
	sksObj1 := SKS(ns1, sks1, WithPrivateService, WithPubService, WithDeployRef(sks1), WithProxyMode)
	// Active, should not visibly reconcile.
	sksObj2 := SKS(ns2, sks2, WithPrivateService, WithPubService, WithDeployRef(sks2), markHappy)

	if _, err := fakeservingclient.Get(ctx).NetworkingV1alpha1().ServerlessServices(ns1).Create(sksObj1); err != nil {
		t.Fatal("Error creating SKS1:", err)
	}
	if _, err := fakeservingclient.Get(ctx).NetworkingV1alpha1().ServerlessServices(ns2).Create(sksObj2); err != nil {
		t.Fatal("Error creating SKS2:", err)
	}

	eps := fakeendpointsinformer.Get(ctx).Lister()
	if err := wait.PollImmediate(10*time.Millisecond, 5*time.Second, func() (bool, error) {
		l, err := eps.List(labels.Everything())
		return len(l) >= 4, err
	}); err != nil {
		t.Fatal("Failed to see endpoint creation:", err)
	}
	t.Log("Updating the activator endpoints now...")

	// Now that we have established the baseline, update the activator endpoints.
	aEps = activatorEndpoints(withOtherSubsets)
	if _, err := kubeClnt.CoreV1().Endpoints(aEps.Namespace).Update(aEps); err != nil {
		t.Fatal("Error creating activator endpoints:", err)
	}

	if err := updateHooks.WaitForHooks(3 * time.Second); err != nil {
		t.Fatal("Hooks timed out:", err)
	}
}
