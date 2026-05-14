/*
Copyright 2022 The Katalyst Authors.

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

package kcc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic/dynamiclister"
	"k8s.io/client-go/tools/cache"

	"github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	"github.com/kubewharf/katalyst-core/pkg/consts"
	"github.com/kubewharf/katalyst-core/pkg/controller/kcc/target"
	"github.com/kubewharf/katalyst-core/pkg/util"
)

type mockAffectedTargetHandler struct {
	gvrs map[metav1.GroupVersionResource]dynamiclister.Lister
}

func (m *mockAffectedTargetHandler) HasSynced() bool { return true }
func (m *mockAffectedTargetHandler) Run()            {}
func (m *mockAffectedTargetHandler) RegisterTargetHandler(name string, handlerFunc target.KatalystCustomConfigTargetHandlerFunc) {
}
func (m *mockAffectedTargetHandler) GetKCCKeyListByGVR(gvr metav1.GroupVersionResource) []string {
	return nil
}
func (m *mockAffectedTargetHandler) GetTargetAccessorByGVR(gvr metav1.GroupVersionResource) (target.KatalystCustomConfigTargetAccessor, bool) {
	return nil, false
}

func (m *mockAffectedTargetHandler) RangeGVRTargetAccessor(f func(metav1.GroupVersionResource, target.KatalystCustomConfigTargetAccessor) bool) {
	for gvr, lister := range m.gvrs {
		if !f(gvr, &mockAffectedAccessor{Lister: lister}) {
			break
		}
	}
}

func (m *mockAffectedTargetHandler) GetKCCTargetResource(gvr metav1.GroupVersionResource, obj *unstructured.Unstructured) (util.KCCTargetResource, error) {
	return util.ToKCCTargetResource(obj.DeepCopy()), nil
}

type mockAffectedAccessor struct {
	target.DummyKatalystCustomConfigTargetAccessor
	dynamiclister.Lister
}

func (m *mockAffectedAccessor) List(selector labels.Selector) ([]*unstructured.Unstructured, error) {
	return m.Lister.List(selector)
}

func (m *mockAffectedAccessor) Get(namespace, name string) (*unstructured.Unstructured, error) {
	return nil, nil
}

func generateAffectedUnstructuredTarget(name, labelSelector string, canary *intstr.IntOrString) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "config.katalyst.kubewharf.io/v1alpha1",
			"kind":       "AdminQoSConfiguration",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				consts.KCCTargetConfFieldNameLabelSelector: labelSelector,
				consts.KCCTargetConfFieldNamePriority:      int64(0),
			},
		},
	}
	if canary != nil {
		_ = unstructured.SetNestedField(obj.Object, canary.String(), consts.ObjectFieldNameSpec, consts.KCCTargetConfFieldNameUpdateStrategy, consts.KCCTargetConfFieldNameRollingUpdate, consts.KCCTargetConfFieldNameCanary)
	}
	return obj
}

func TestGetAffectedGVRsByLabelChange(t *testing.T) {
	t.Parallel()

	gvr := metav1.GroupVersionResource{Group: "config.katalyst.kubewharf.io", Version: "v1alpha1", Resource: "adminqosconfigurations"}
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	canary10 := intstr.FromString("10%")

	tests := []struct {
		name     string
		oldCNC   *v1alpha1.CustomNodeConfig
		newCNC   *v1alpha1.CustomNodeConfig
		targets  []*unstructured.Unstructured
		affected bool
	}{
		{
			name: "no-change-no-canary",
			oldCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			newCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo", "other": "bar"}},
			},
			targets: []*unstructured.Unstructured{
				generateAffectedUnstructuredTarget("t1", "app=foo", nil),
			},
			affected: false,
		},
		{
			name: "change-target",
			oldCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			newCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "bar"}},
			},
			targets: []*unstructured.Unstructured{
				generateAffectedUnstructuredTarget("t1", "app=foo", nil),
				generateAffectedUnstructuredTarget("t2", "app=bar", nil),
			},
			affected: true,
		},
		{
			name: "no-change-with-partial-canary",
			oldCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			newCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo", "other": "bar"}},
			},
			targets: []*unstructured.Unstructured{
				generateAffectedUnstructuredTarget("t1", "app=foo", &canary10),
			},
			affected: true, // Should be affected due to partial canary
		},
		{
			name: "none-to-match",
			oldCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "bar"}},
			},
			newCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			targets: []*unstructured.Unstructured{
				generateAffectedUnstructuredTarget("t1", "app=foo", nil),
			},
			affected: true,
		},
		{
			name: "match-to-none",
			oldCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			newCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "bar"}},
			},
			targets: []*unstructured.Unstructured{
				generateAffectedUnstructuredTarget("t1", "app=foo", nil),
			},
			affected: true,
		},
		{
			name: "ambiguous-to-match",
			oldCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			newCNC: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo", "extra": "val"}},
			},
			targets: []*unstructured.Unstructured{
				generateAffectedUnstructuredTarget("t1", "app=foo", nil),
				generateAffectedUnstructuredTarget("t2", "app=foo,extra=val", nil),
			},
			affected: true, // t1 matches old, t1 & t2 both match new (ambiguous)
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, obj := range tt.targets {
				_ = indexer.Add(obj)
			}
			lister := dynamiclister.New(indexer, schema.GroupVersionResource{
				Group:    gvr.Group,
				Version:  gvr.Version,
				Resource: gvr.Resource,
			})

			k := &KatalystCustomConfigTargetController{
				targetHandler: &mockAffectedTargetHandler{
					gvrs: map[metav1.GroupVersionResource]dynamiclister.Lister{
						gvr: lister,
					},
				},
			}
			affected := k.getAffectedGVRsByCNCChange(tt.oldCNC, tt.newCNC)
			if tt.affected {
				assert.Contains(t, affected, gvr)
			} else {
				assert.NotContains(t, affected, gvr)
			}
		})
	}
}
