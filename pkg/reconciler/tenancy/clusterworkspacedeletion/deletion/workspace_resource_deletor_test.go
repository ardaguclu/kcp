/*
Copyright 2022 The KCP Authors.

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

package deletion

import (
	"context"
	"fmt"
	"testing"

	"github.com/kcp-dev/logicalcluster"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"

	tenancyv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/tenancy/v1alpha1"
	conditionsv1alpha1 "github.com/kcp-dev/kcp/third_party/conditions/apis/conditions/v1alpha1"
	"github.com/kcp-dev/kcp/third_party/conditions/util/conditions"
)

var scheme *runtime.Scheme

func init() {
	scheme = runtime.NewScheme()
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
}

func TestWorkspaceTerminating(t *testing.T) {
	now := metav1.Now()
	ws := &tenancyv1alpha1.ClusterWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test",
			DeletionTimestamp: &now,
			Finalizers:        []string{WorkspaceFinalizer},
		},
	}
	resources := testResources()

	tests := []struct {
		name                    string
		existingObject          []runtime.Object
		metadataClientActionSet metaActionSet
		gvrError                error
		expectErrorOnDelete     error
		expectConditions        conditionsv1alpha1.Conditions
	}{
		{
			name:           "discovery client error",
			existingObject: []runtime.Object{},
			metadataClientActionSet: []metaAction{
				{"secrets", "list"},
				{"secrets", "list"},
				{"customresourcedefinitions", "list"},
				{"customresourcedefinitions", "list"},
			},
			gvrError:            fmt.Errorf("test error"),
			expectErrorOnDelete: fmt.Errorf("test error"),
			expectConditions: conditionsv1alpha1.Conditions{
				{
					Type:   tenancyv1alpha1.WorkspaceDeletionContentSuccess,
					Status: v1.ConditionFalse,
				},
				{
					Type:   tenancyv1alpha1.WorkspaceContentDeleted,
					Status: v1.ConditionTrue,
				},
			},
		},
		{
			name: "delete ns scoped resource",
			existingObject: []runtime.Object{
				newPartialObject("v1", "Secret", "s1", "ns1"),
				newPartialObject("v1", "Secret", "s2", "ns2"),
			},
			metadataClientActionSet: []metaAction{
				{"secrets", "list"},
				{"secrets", "delete-collection"},
				{"secrets", "delete-collection"},
				{"secrets", "list"},
				{"customresourcedefinitions", "list"},
				{"customresourcedefinitions", "list"},
			},
			expectErrorOnDelete: &ResourcesRemainingError{5},
			expectConditions: conditionsv1alpha1.Conditions{
				{
					Type:   tenancyv1alpha1.WorkspaceDeletionContentSuccess,
					Status: v1.ConditionTrue,
				},
				{
					Type:   tenancyv1alpha1.WorkspaceContentDeleted,
					Status: v1.ConditionFalse,
				},
			},
		},
		{
			name: "delete cluster scoped resource",
			existingObject: []runtime.Object{
				newPartialObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "crd1", ""),
				newPartialObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "crd2", ""),
			},
			metadataClientActionSet: []metaAction{
				{"secrets", "list"},
				{"secrets", "list"},
				{"customresourcedefinitions", "list"},
				{"customresourcedefinitions", "delete-collection"},
				{"customresourcedefinitions", "list"},
			},
			expectErrorOnDelete: &ResourcesRemainingError{5},
			expectConditions: conditionsv1alpha1.Conditions{
				{
					Type:   tenancyv1alpha1.WorkspaceDeletionContentSuccess,
					Status: v1.ConditionTrue,
				},
				{
					Type:   tenancyv1alpha1.WorkspaceContentDeleted,
					Status: v1.ConditionFalse,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fn := func(clusterName logicalcluster.Name) ([]*metav1.APIResourceList, error) {
				return resources, tt.gvrError
			}
			mockMetadataClient := metadatafake.NewSimpleMetadataClient(scheme, tt.existingObject...)
			d := NewWorkspacedResourcesDeleter(mockMetadataClient, fn)

			err := d.Delete(context.TODO(), ws)
			if !matchErrors(err, tt.expectErrorOnDelete) {
				t.Errorf("expected error %q when syncing namespace, got %q", tt.expectErrorOnDelete, err)
			}
			for _, expCondition := range tt.expectConditions {
				cond := conditions.Get(ws, expCondition.Type)
				if cond == nil {
					t.Fatalf("Missing status condition %v", expCondition.Type)
				}

				if cond.Status != expCondition.Status {
					t.Errorf("expect condition status %q, got %q for type %s", expCondition.Status, cond.Status, cond.Type)
				}
			}

			if len(mockMetadataClient.Actions()) != len(tt.metadataClientActionSet) {
				t.Fatalf("mismatched actions, expect %d actions, got %d actions", len(tt.metadataClientActionSet), len(mockMetadataClient.Actions()))
			}

			for index, action := range mockMetadataClient.Actions() {
				if !tt.metadataClientActionSet.match(action) {
					t.Errorf("expect action for resource %q for verb %q but got %v", tt.metadataClientActionSet[index].resource, tt.metadataClientActionSet[index].verb, action)
				}
			}
		})
	}
}

type metaAction struct {
	resource string
	verb     string
}

type metaActionSet []metaAction

func (m metaActionSet) match(action clienttesting.Action) bool {
	for _, a := range m {
		if action.Matches(a.verb, a.resource) {
			return true
		}
	}

	return false
}

func newPartialObject(apiversion, kind, name, namespace string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiversion,
			Kind:       kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

// testResources returns a mocked up set of resources across different api groups for testing namespace controller.
func testResources() []*metav1.APIResourceList {
	results := []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{
					Name:       "secrets",
					Namespaced: true,
					Kind:       "Secret",
					Verbs:      []string{"get", "list", "delete", "deletecollection", "create", "update"},
				},
				{
					Name:       "nodelete",
					Namespaced: true,
					Kind:       "NoDelete",
					Verbs:      []string{"get", "list", "create", "update"},
				},
			},
		},
		{
			GroupVersion: "apiextensions.k8s.io/v1",
			APIResources: []metav1.APIResource{
				{
					Name:       "customresourcedefinitions",
					Namespaced: false,
					Kind:       "CustomResourceDefinition",
					Verbs:      []string{"get", "list", "delete", "deletecollection", "create", "update"},
				},
			},
		},
	}
	return results
}

// matchError returns true if errors match, false if they don't, compares by error message only for convenience which should be sufficient for these tests
func matchErrors(e1, e2 error) bool {
	if e1 == nil && e2 == nil {
		return true
	}
	if e1 != nil && e2 != nil {
		return e1.Error() == e2.Error()
	}
	return false
}
