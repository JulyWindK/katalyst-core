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

package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	"github.com/kubewharf/katalyst-core/pkg/consts"
	"github.com/kubewharf/katalyst-core/pkg/util"
)

func generateTestTargetResourceForSnapshot(name, labelSelector string, priority int32, canary *intstr.IntOrString) util.KCCTargetResource {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "config.katalyst.kubewharf.io/v1alpha1",
			"kind":       "AdminQoSConfiguration",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "default",
			},
		},
	}
	_ = unstructured.SetNestedField(obj.Object, labelSelector, consts.ObjectFieldNameSpec, consts.KCCTargetConfFieldNameLabelSelector)
	_ = unstructured.SetNestedField(obj.Object, int64(priority), consts.ObjectFieldNameSpec, consts.KCCTargetConfFieldNamePriority)

	if canary != nil {
		_ = unstructured.SetNestedField(obj.Object, canary.String(), consts.ObjectFieldNameSpec, consts.KCCTargetConfFieldNameUpdateStrategy, consts.KCCTargetConfFieldNameRollingUpdate, consts.KCCTargetConfFieldNameCanary)
	}
	return util.ToKCCTargetResource(obj)
}

func generateTestNodeNamesTargetResourceForSnapshot(name string, nodeNames []string) util.KCCTargetResource {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "config.katalyst.kubewharf.io/v1alpha1",
			"kind":       "AdminQoSConfiguration",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "default",
			},
		},
	}
	// Note: NestedStringSlice expects []string, but unstructured conversion might make it []interface{}.
	// We use the helper from k8s to set it correctly.
	_ = unstructured.SetNestedStringSlice(obj.Object, nodeNames, consts.ObjectFieldNameSpec, consts.KCCTargetConfFieldEphemeralSelector, consts.KCCTargetConfFieldNameNodeNames)
	return util.ToKCCTargetResource(obj)
}

func TestGetCNCMatchSnapshot(t *testing.T) {
	t.Parallel()

	canary10 := intstr.FromString("10%")
	canary100 := intstr.FromString("100%")

	tests := []struct {
		name          string
		cnc           *v1alpha1.CustomNodeConfig
		kccTargetList []util.KCCTargetResource
		want          CNCMatchSnapshot
	}{
		{
			name: "match-selector-no-canary",
			cnc: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			kccTargetList: []util.KCCTargetResource{
				generateTestTargetResourceForSnapshot("t1", "app=foo", 0, nil),
			},
			want: CNCMatchSnapshot{
				TargetName:      "t1",
				TargetNamespace: "default",
				MatchKind:       MatchKindSelector,
				Priority:        0,
				PartialCanary:   false,
				ConfigHash:      "44136fa355b3",
			},
		},
		{
			name: "match-selector-partial-canary",
			cnc: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			kccTargetList: []util.KCCTargetResource{
				generateTestTargetResourceForSnapshot("t1", "app=foo", 0, &canary10),
			},
			want: CNCMatchSnapshot{
				TargetName:      "t1",
				TargetNamespace: "default",
				MatchKind:       MatchKindSelector,
				Priority:        0,
				PartialCanary:   true,
				ConfigHash:      "44136fa355b3",
			},
		},
		{
			name: "match-selector-100-percent-canary",
			cnc: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			kccTargetList: []util.KCCTargetResource{
				generateTestTargetResourceForSnapshot("t1", "app=foo", 0, &canary100),
			},
			want: CNCMatchSnapshot{
				TargetName:      "t1",
				TargetNamespace: "default",
				MatchKind:       MatchKindSelector,
				Priority:        0,
				PartialCanary:   false,
				ConfigHash:      "44136fa355b3",
			},
		},
		{
			name: "match-node-names",
			cnc: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			},
			kccTargetList: []util.KCCTargetResource{
				generateTestNodeNamesTargetResourceForSnapshot("t1", []string{"node1"}),
			},
			want: CNCMatchSnapshot{
				TargetName:      "t1",
				TargetNamespace: "default",
				MatchKind:       MatchKindNodeNames,
				Priority:        0,
				PartialCanary:   false,
				ConfigHash:      "44136fa355b3",
			},
		},
		{
			name: "match-global",
			cnc: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			},
			kccTargetList: []util.KCCTargetResource{
				generateTestTargetResourceForSnapshot("t1", "", 0, nil),
			},
			want: CNCMatchSnapshot{
				TargetName:      "t1",
				TargetNamespace: "default",
				MatchKind:       MatchKindGlobal,
				Priority:        0,
				PartialCanary:   false,
				ConfigHash:      "44136fa355b3",
			},
		},
		{
			name: "match-priority-higher",
			cnc: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			kccTargetList: []util.KCCTargetResource{
				generateTestTargetResourceForSnapshot("t1", "app=foo", 0, nil),
				generateTestTargetResourceForSnapshot("t2", "app=foo", 10, nil),
			},
			want: CNCMatchSnapshot{
				TargetName:      "t2",
				TargetNamespace: "default",
				MatchKind:       MatchKindSelector,
				Priority:        10,
				PartialCanary:   false,
				ConfigHash:      "44136fa355b3",
			},
		},
		{
			name: "match-ambiguous",
			cnc: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			kccTargetList: []util.KCCTargetResource{
				generateTestTargetResourceForSnapshot("t1", "app=foo", 10, nil),
				generateTestTargetResourceForSnapshot("t2", "app=foo", 10, nil),
			},
			want: CNCMatchSnapshot{
				MatchKind: MatchKindAmbiguous,
			},
		},
		{
			name: "match-none",
			cnc: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "bar"}},
			},
			kccTargetList: []util.KCCTargetResource{
				generateTestTargetResourceForSnapshot("t1", "app=foo", 0, nil),
			},
			want: CNCMatchSnapshot{
				MatchKind: MatchKindNone,
			},
		},
		{
			name: "precedence-node-names-over-selector",
			cnc: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			kccTargetList: []util.KCCTargetResource{
				generateTestTargetResourceForSnapshot("t1", "app=foo", 100, nil), // Higher priority but selector
				generateTestNodeNamesTargetResourceForSnapshot("t2", []string{"node1"}), // Lower priority (default 0) but nodeNames
			},
			want: CNCMatchSnapshot{
				TargetName:      "t2",
				TargetNamespace: "default",
				MatchKind:       MatchKindNodeNames,
				Priority:        0,
				PartialCanary:   false,
				ConfigHash:      "44136fa355b3",
			},
		},
		{
			name: "precedence-selector-over-global",
			cnc: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			kccTargetList: []util.KCCTargetResource{
				generateTestTargetResourceForSnapshot("t1", "", 100, nil),       // Global with high priority
				generateTestTargetResourceForSnapshot("t2", "app=foo", 0, nil), // Selector with low priority
			},
			want: CNCMatchSnapshot{
				TargetName:      "t2",
				TargetNamespace: "default",
				MatchKind:       MatchKindSelector,
				Priority:        0,
				PartialCanary:   false,
				ConfigHash:      "44136fa355b3",
			},
		},
		{
			name: "invalid-selector-error",
			cnc: &v1alpha1.CustomNodeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
			},
			kccTargetList: []util.KCCTargetResource{
				generateTestTargetResourceForSnapshot("t1", "invalid!!selector", 0, nil),
			},
			want: CNCMatchSnapshot{
				MatchKind: MatchKindError,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := GetCNCMatchSnapshot(tt.cnc, tt.kccTargetList)
			if got.TargetName != "" && tt.want.TargetName == got.TargetName {
				assert.NotEmpty(t, got.TargetHash)
				tt.want.TargetHash = got.TargetHash
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTargetHashChange(t *testing.T) {
	t.Parallel()

	cnc := &v1alpha1.CustomNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "foo"}},
	}

	// Case 1: Base target
	t1 := generateTestTargetResourceForSnapshot("t1", "app=foo", 0, nil)
	snap1 := GetCNCMatchSnapshot(cnc, []util.KCCTargetResource{t1})

	// Case 2: Change priority
	t2 := generateTestTargetResourceForSnapshot("t1", "app=foo", 10, nil)
	snap2 := GetCNCMatchSnapshot(cnc, []util.KCCTargetResource{t2})

	assert.NotEqual(t, snap1.TargetHash, snap2.TargetHash)
	assert.NotEqual(t, snap1.Priority, snap2.Priority)

	// Case 3: Change selector
	t3 := generateTestTargetResourceForSnapshot("t1", "app=bar", 0, nil)
	// We need a CNC that matches app=bar to get a snapshot for t1
	cncBar := &v1alpha1.CustomNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"app": "bar"}},
	}
	snap3 := GetCNCMatchSnapshot(cncBar, []util.KCCTargetResource{t3})

	assert.NotEqual(t, snap1.TargetHash, snap3.TargetHash)

	// Case 4: Change canary
	canary10 := intstr.FromString("10%")
	t4 := generateTestTargetResourceForSnapshot("t1", "app=foo", 0, &canary10)
	snap4 := GetCNCMatchSnapshot(cnc, []util.KCCTargetResource{t4})

	assert.NotEqual(t, snap1.TargetHash, snap4.TargetHash)
	assert.True(t, snap4.PartialCanary)
}
