/*
Copyright 2023 The Kubernetes Authors.

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

package framework

import (
	"context"
	"fmt"
	"reflect"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterctlcluster "sigs.k8s.io/cluster-api/cmd/clusterctl/client/cluster"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	addonsv1 "sigs.k8s.io/cluster-api/exp/addons/api/v1beta1"
	infrav1 "sigs.k8s.io/cluster-api/test/infrastructure/docker/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/patch"
)

// finalizerAssertion contains a list of expected Finalizers corresponding to a resource Kind.
var finalizerAssertion = map[string][]string{
	"Cluster":             {clusterv1.ClusterFinalizer},
	"Machine":             {clusterv1.MachineFinalizer},
	"MachineSet":          {clusterv1.MachineSetTopologyFinalizer},
	"MachineDeployment":   {clusterv1.MachineDeploymentTopologyFinalizer},
	"ClusterResourceSet":  {addonsv1.ClusterResourceSetFinalizer},
	"DockerMachine":       {infrav1.MachineFinalizer},
	"DockerCluster":       {infrav1.ClusterFinalizer},
	"KubeadmControlPlane": {controlplanev1.KubeadmControlPlaneFinalizer},
}

// ValidateFinalizersResilience checks that expected finalizers are in place, deletes them, and verifies that expected finalizers are properly added again.
func ValidateFinalizersResilience(ctx context.Context, proxy ClusterProxy, namespace, clusterName string) {
	clusterKey := client.ObjectKey{Namespace: namespace, Name: clusterName}

	// Collect all objects where finalizers were initially set
	objectsWithFinalizers := getObjectsWithFinalizers(ctx, proxy, namespace)

	// Setting the paused property on the Cluster resource will pause reconciliations, thereby having no effect on Finalizers.
	// This also makes debugging easier.
	setClusterPause(ctx, proxy.GetClient(), clusterKey, true)

	// We are testing the worst-case scenario, i.e. all finalizers are deleted.
	// Once all Clusters are paused remove all the Finalizers from all objects in the graph.
	// The reconciliation loop should be able to recover from this, by adding the required Finalizers back.
	removeFinalizers(ctx, proxy, namespace)

	// Unpause the cluster.
	setClusterPause(ctx, proxy.GetClient(), clusterKey, false)

	// Check that the Finalizers are as expected after further reconciliations.
	assertFinalizersExist(ctx, proxy, namespace, objectsWithFinalizers)
}

// removeFinalizers removes all Finalizers from objects in the owner graph.
func removeFinalizers(ctx context.Context, proxy ClusterProxy, namespace string) {
	graph, err := clusterctlcluster.GetOwnerGraph(ctx, namespace, proxy.GetKubeconfigPath())
	Expect(err).ToNot(HaveOccurred())
	for _, object := range graph {
		ref := object.Object
		obj := new(unstructured.Unstructured)
		obj.SetAPIVersion(ref.APIVersion)
		obj.SetKind(ref.Kind)
		obj.SetName(ref.Name)

		Expect(proxy.GetClient().Get(ctx, client.ObjectKey{Namespace: namespace, Name: object.Object.Name}, obj)).To(Succeed())
		helper, err := patch.NewHelper(obj, proxy.GetClient())
		Expect(err).ToNot(HaveOccurred())
		obj.SetFinalizers([]string{})
		Expect(helper.Patch(ctx, obj)).To(Succeed())
	}
}

func getObjectsWithFinalizers(ctx context.Context, proxy ClusterProxy, namespace string) map[string]*unstructured.Unstructured {
	graph, err := clusterctlcluster.GetOwnerGraph(ctx, namespace, proxy.GetKubeconfigPath())
	Expect(err).ToNot(HaveOccurred())

	objsWithFinalizers := map[string]*unstructured.Unstructured{}

	for _, node := range graph {
		nodeNamespacedName := client.ObjectKey{Namespace: node.Object.Namespace, Name: node.Object.Name}
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(node.Object.APIVersion)
		obj.SetKind(node.Object.Kind)
		err = proxy.GetClient().Get(ctx, nodeNamespacedName, obj)
		Expect(err).ToNot(HaveOccurred())

		setFinalizers := obj.GetFinalizers()

		if len(setFinalizers) > 0 {
			objsWithFinalizers[fmt.Sprintf("%s/%s/%s", node.Object.Kind, node.Object.Namespace, node.Object.Name)] = obj
		}
	}

	return objsWithFinalizers
}

// assertFinalizersExist ensures that current Finalizers match those in the initialObjectsWithFinalizers.
func assertFinalizersExist(ctx context.Context, proxy ClusterProxy, namespace string, initialObjsWithFinalizers map[string]*unstructured.Unstructured) {
	Eventually(func() error {
		var allErrs []error
		finalObjsWithFinalizers := getObjectsWithFinalizers(ctx, proxy, namespace)

		for objKindNamespacedName, obj := range initialObjsWithFinalizers {
			// verify if finalizers for this resource were set on reconcile
			if _, valid := finalObjsWithFinalizers[objKindNamespacedName]; !valid {
				allErrs = append(allErrs, fmt.Errorf("no finalizers set for %s",
					objKindNamespacedName))
				continue
			}

			// verify if this resource has the appropriate Finalizers set
			expectedFinalizers, assert := finalizerAssertion[obj.GetKind()]
			if !assert {
				continue
			}

			setFinalizers := finalObjsWithFinalizers[objKindNamespacedName].GetFinalizers()
			if !reflect.DeepEqual(expectedFinalizers, setFinalizers) {
				allErrs = append(allErrs, fmt.Errorf("expected finalizers do not exist for %s: expected: %v, found: %v",
					objKindNamespacedName, expectedFinalizers, setFinalizers))
			}
		}

		return kerrors.NewAggregate(allErrs)
	}).WithTimeout(1 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
}
