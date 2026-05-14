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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"

	"github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	"github.com/kubewharf/katalyst-core/pkg/client/control"
	kccconfig "github.com/kubewharf/katalyst-core/pkg/config/controller"
	kcctarget "github.com/kubewharf/katalyst-core/pkg/controller/kcc/target"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	"github.com/kubewharf/katalyst-core/pkg/util"
	"github.com/kubewharf/katalyst-core/pkg/util/native"
)

func toTestUnstructured(obj interface{}) *unstructured.Unstructured {
	ret, err := native.ToUnstructured(obj)
	if err != nil {
		panic(err)
	}
	return ret
}

type captureUnstructuredControl struct {
	control.DummyUnstructuredControl
	updated []*unstructured.Unstructured
}

func (c *captureUnstructuredControl) UpdateUnstructuredStatus(_ context.Context, _ metav1.GroupVersionResource,
	obj *unstructured.Unstructured, _ metav1.UpdateOptions,
) (*unstructured.Unstructured, error) {
	c.updated = append(c.updated, obj.DeepCopy())
	return obj, nil
}

func newTestKCCTController(unstructuredControl control.UnstructuredControl) *KatalystCustomConfigTargetController {
	return &KatalystCustomConfigTargetController{
		kccConfig:           kccconfig.NewKCCConfig(),
		unstructuredControl: unstructuredControl,
		progressCache:       make(map[string]*kcctProgress),
		lastStatusEmit:      make(map[string]time.Time),
		metricsEmitter:      metrics.DummyMetrics{},
	}
}

func testLabelSelector(t *testing.T, labelSelector string) labels.Selector {
	parse, err := labels.Parse(labelSelector)
	if err != nil {
		t.Fatal(err)
	}
	return parse
}

func generateTestLabelSelectorTargetResource(name, labelSelector string, priority int32) util.KCCTargetResource {
	return util.ToKCCTargetResource(toTestUnstructured(&v1alpha1.AdminQoSConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1alpha1.AdminQoSConfigurationSpec{
			GenericConfigSpec: v1alpha1.GenericConfigSpec{
				NodeLabelSelector: labelSelector,
				Priority:          priority,
			},
		},
	}))
}

func generateTestNodeNamesTargetResource(name string, nodeNames []string) util.KCCTargetResource {
	return util.ToKCCTargetResource(toTestUnstructured(&v1alpha1.AdminQoSConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1alpha1.AdminQoSConfigurationSpec{
			GenericConfigSpec: v1alpha1.GenericConfigSpec{
				EphemeralSelector: v1alpha1.EphemeralSelector{
					NodeNames: nodeNames,
				},
			},
		},
	}))
}

func Test_validateLabelSelectorWithOthers(t *testing.T) {
	t.Parallel()

	type args struct {
		priorityAllowedKeyListMap map[int32]sets.String
		targetResource            util.KCCTargetResource
		otherResources            []util.KCCTargetResource
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		{
			name: "test-1",
			args: args{
				priorityAllowedKeyListMap: map[int32]sets.String{
					0: sets.NewString("aa"),
				},
				targetResource: generateTestLabelSelectorTargetResource("1", "aa=bb", 0),
				otherResources: []util.KCCTargetResource{
					generateTestLabelSelectorTargetResource("2", "aa=cc", 0),
				},
			},
			want: true,
		},
		{
			name: "test-2",
			args: args{
				priorityAllowedKeyListMap: map[int32]sets.String{
					0: sets.NewString("aa"),
				},
				targetResource: generateTestLabelSelectorTargetResource("1", "aa=bb", 0),
				otherResources: []util.KCCTargetResource{
					generateTestLabelSelectorTargetResource("2", "aa in (cc,dd)", 0),
				},
			},
			want: true,
		},
		{
			name: "test-3",
			args: args{
				priorityAllowedKeyListMap: map[int32]sets.String{
					0: sets.NewString("aa"),
				},
				targetResource: generateTestLabelSelectorTargetResource("1", "aa=bb", 0),
				otherResources: []util.KCCTargetResource{
					generateTestLabelSelectorTargetResource("2", "aa in (bb,cc)", 0),
				},
			},
			want: false,
		},
		{
			name: "test-4",
			args: args{
				priorityAllowedKeyListMap: map[int32]sets.String{
					0: sets.NewString("aa"),
				},
				targetResource: generateTestLabelSelectorTargetResource("1", "aa=bb", 0),
				otherResources: []util.KCCTargetResource{
					generateTestLabelSelectorTargetResource("2", "aa notin (bb,cc)", 0),
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, _, err := validateLabelSelectorOverlapped(tt.args.priorityAllowedKeyListMap, tt.args.targetResource, tt.args.otherResources)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateLabelSelectorOverlapped() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("validateLabelSelectorOverlapped() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_validateTargetResourceNodeNamesWithOthers(t *testing.T) {
	t.Parallel()

	type args struct {
		targetResource util.KCCTargetResource
		otherResources []util.KCCTargetResource
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		{
			name: "test-1",
			args: args{
				targetResource: generateTestNodeNamesTargetResource("1", []string{"node-1"}),
				otherResources: []util.KCCTargetResource{
					generateTestNodeNamesTargetResource("2", []string{"node-2"}),
				},
			},
			want: true,
		},
		{
			name: "test-2",
			args: args{
				targetResource: generateTestNodeNamesTargetResource("1", []string{"node-1"}),
				otherResources: []util.KCCTargetResource{
					generateTestNodeNamesTargetResource("2", []string{"node-2", "node-3"}),
				},
			},
			want: true,
		},
		{
			name: "test-3",
			args: args{
				targetResource: generateTestNodeNamesTargetResource("1", []string{"node-1"}),
				otherResources: []util.KCCTargetResource{
					generateTestNodeNamesTargetResource("2", []string{"node-1", "node-3"}),
				},
			},
			want: false,
		},
		{
			name: "test-4",
			args: args{
				targetResource: generateTestNodeNamesTargetResource("1", []string{"node-1", "node-2"}),
				otherResources: []util.KCCTargetResource{
					generateTestNodeNamesTargetResource("2", []string{"node-3", "node-4"}),
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, _, err := validateTargetResourceNodeNamesOverlapped(tt.args.targetResource, tt.args.otherResources)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateTargetResourceNodeNamesOverlapped() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("validateTargetResourceNodeNamesOverlapped() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_validateTargetResourceGlobalWithOthers(t *testing.T) {
	t.Parallel()

	type args struct {
		targetResource util.KCCTargetResource
		otherResources []util.KCCTargetResource
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		{
			name: "test-1",
			args: args{
				targetResource: generateTestLabelSelectorTargetResource("1", "", 0),
				otherResources: []util.KCCTargetResource{
					generateTestLabelSelectorTargetResource("2", "", 0),
				},
			},
			want: false,
		},
		{
			name: "test-2",
			args: args{
				targetResource: generateTestLabelSelectorTargetResource("1", "", 0),
				otherResources: []util.KCCTargetResource{
					generateTestLabelSelectorTargetResource("1", "", 0),
					generateTestLabelSelectorTargetResource("2", "aa=bb", 0),
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, _, err := validateTargetResourceGlobalOverlapped(tt.args.targetResource, tt.args.otherResources)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateTargetResourceGlobalOverlapped() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("validateTargetResourceGlobalOverlapped() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func targetResourcesEqual(t1, t2 util.KCCTargetResource) bool {
	status1 := t1.GetGenericStatus()
	status2 := t2.GetGenericStatus()
	if len(status1.Conditions) != len(status2.Conditions) {
		return false
	}

	status1.Conditions[0].LastTransitionTime = metav1.Time{}
	status2.Conditions[0].LastTransitionTime = metav1.Time{}
	t1.SetGenericStatus(status1)
	t2.SetGenericStatus(status2)
	if !apiequality.Semantic.DeepEqual(t1, t2) {
		return false
	}

	return true
}

func Test_updateInvalidTargetResourceStatus(t *testing.T) {
	t.Parallel()

	type args struct {
		targetResource util.KCCTargetResource
		msg            string
		reason         string
	}
	tests := []struct {
		name         string
		args         args
		wantResource util.KCCTargetResource
	}{
		{
			name: "test-1",
			args: args{
				targetResource: util.ToKCCTargetResource(toTestUnstructured(&v1alpha1.AdminQoSConfiguration{
					ObjectMeta: metav1.ObjectMeta{
						Name: "config-1",
					},
					Spec: v1alpha1.AdminQoSConfigurationSpec{
						GenericConfigSpec: v1alpha1.GenericConfigSpec{
							NodeLabelSelector: "aa=bb",
						},
					},
				})),
				msg:    "ssasfr",
				reason: "dasf",
			},
			wantResource: util.ToKCCTargetResource(toTestUnstructured(&v1alpha1.AdminQoSConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "config-1",
				},
				Spec: v1alpha1.AdminQoSConfigurationSpec{
					GenericConfigSpec: v1alpha1.GenericConfigSpec{
						NodeLabelSelector: "aa=bb",
					},
				},
				Status: v1alpha1.GenericConfigStatus{
					Conditions: []v1alpha1.GenericConfigCondition{
						{
							Type:    v1alpha1.ConfigConditionTypeValid,
							Status:  v1.ConditionFalse,
							Reason:  "dasf",
							Message: "ssasfr",
						},
					},
				},
			})),
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if updateInvalidTargetResourceStatus(tt.args.targetResource, tt.args.msg, tt.args.reason); !targetResourcesEqual(tt.args.targetResource, tt.wantResource) {
				t.Errorf("updateInvalidTargetResourceStatus() = %v, want %v", tt.args.targetResource.GetGenericStatus(), tt.wantResource.GetGenericStatus())
			}
		})
	}
}

func Test_updateValidTargetResourceStatus(t *testing.T) {
	t.Parallel()

	type args struct {
		targetResource                                             util.KCCTargetResource
		targetNodes, canaryNodes, updatedTargetNodes, updatedNodes int32
		currentHash                                                string
	}
	tests := []struct {
		name         string
		args         args
		wantResource util.KCCTargetResource
	}{
		{
			name: "test-1",
			args: args{
				targetResource: util.ToKCCTargetResource(toTestUnstructured(&v1alpha1.AdminQoSConfiguration{
					ObjectMeta: metav1.ObjectMeta{
						Name: "config-1",
					},
					Spec: v1alpha1.AdminQoSConfigurationSpec{
						GenericConfigSpec: v1alpha1.GenericConfigSpec{
							NodeLabelSelector: "aa=bb",
						},
					},
				})),
				targetNodes:        10000,
				canaryNodes:        8000,
				updatedTargetNodes: 5000,
				updatedNodes:       6000,
				currentHash:        "hash-1",
			},
			wantResource: util.ToKCCTargetResource(toTestUnstructured(&v1alpha1.AdminQoSConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "config-1",
				},
				Spec: v1alpha1.AdminQoSConfigurationSpec{
					GenericConfigSpec: v1alpha1.GenericConfigSpec{
						NodeLabelSelector: "aa=bb",
					},
				},
				Status: v1alpha1.GenericConfigStatus{
					TargetNodes:        10000,
					CanaryNodes:        8000,
					UpdatedTargetNodes: 5000,
					UpdatedNodes:       6000,
					CurrentHash:        "hash-1",
					Conditions: []v1alpha1.GenericConfigCondition{
						{
							Type:   v1alpha1.ConfigConditionTypeValid,
							Status: v1.ConditionTrue,
							Reason: kccConditionTypeValidReasonNormal,
						},
					},
				},
			})),
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if updateValidTargetResourceStatus(
				tt.args.targetResource,
				tt.args.targetNodes,
				tt.args.canaryNodes,
				tt.args.updatedTargetNodes,
				tt.args.updatedNodes,
				tt.args.currentHash,
			); !targetResourcesEqual(tt.args.targetResource, tt.wantResource) {
				t.Errorf("updateValidTargetResourceStatus() = %v, want %v", tt.args.targetResource.GetGenericStatus(), tt.wantResource.GetGenericStatus())
			}
		})
	}
}

func Test_checkLabelSelectorOverlap(t *testing.T) {
	t.Parallel()

	type args struct {
		selector      labels.Selector
		otherSelector labels.Selector
		keyList       []string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "test-1",
			args: args{
				selector:      testLabelSelector(t, "label1=aa"),
				otherSelector: testLabelSelector(t, "label1=bb"),
				keyList:       []string{"label1"},
			},
			want: false,
		},
		{
			name: "test-2",
			args: args{
				selector:      testLabelSelector(t, "label1=aa"),
				otherSelector: testLabelSelector(t, "label1!=bb"),
				keyList:       []string{"label1"},
			},
			want: true,
		},
		{
			name: "test-3",
			args: args{
				selector:      testLabelSelector(t, "label1=aa"),
				otherSelector: testLabelSelector(t, "label1 in (aa,bb)"),
				keyList:       []string{"label1"},
			},
			want: true,
		},
		{
			name: "test-4",
			args: args{
				selector:      testLabelSelector(t, "label1=aa"),
				otherSelector: testLabelSelector(t, "label1 notin (aa,bb)"),
				keyList:       []string{"label1"},
			},
			want: false,
		},
		{
			name: "test-5",
			args: args{
				selector:      testLabelSelector(t, "label1=aa"),
				otherSelector: testLabelSelector(t, "label1 in (aa,bb),label2=cc"),
				keyList:       []string{"label1", "label2"},
			},
			want: true,
		},
		{
			name: "test-6",
			args: args{
				selector:      testLabelSelector(t, "label1=aa"),
				otherSelector: testLabelSelector(t, "label2=bb"),
				keyList:       []string{"label1", "label2"},
			},
			want: true,
		},
		{
			name: "test-7",
			args: args{
				selector:      testLabelSelector(t, "label1 notin (aa, bb),label2=bb"),
				otherSelector: testLabelSelector(t, "label1 in (aa),label2=bb"),
				keyList:       []string{"label1", "label2"},
			},
			want: false,
		},
		{
			name: "test-8",
			args: args{
				selector:      testLabelSelector(t, "label1 in (aa),label2 notin (bb,cc)"),
				otherSelector: testLabelSelector(t, "label1 notin (cc,dd),label2 notin (cc)"),
				keyList:       []string{"label1", "label2"},
			},
			want: true,
		},
		{
			name: "test-9",
			args: args{
				selector:      testLabelSelector(t, "label1=aa"),
				otherSelector: testLabelSelector(t, "label1 notin (cc,dd),label2 notin (cc)"),
				keyList:       []string{"label1", "label2"},
			},
			want: true,
		},
		{
			name: "test-10",
			args: args{
				selector:      testLabelSelector(t, "label1 notin (aa)"),
				otherSelector: testLabelSelector(t, "label1=cc"),
				keyList:       []string{"label1", "label2"},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equalf(t, tt.want, checkLabelSelectorOverlap(tt.args.selector, tt.args.otherSelector, tt.args.keyList), "checkLabelSelectorOverlap(%v, %v, %v)", tt.args.selector, tt.args.otherSelector, tt.args.keyList)
		})
	}
}

// ============ tests for performance optimization helpers ============

func TestResolveInt(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 7, resolveInt(0, 7))
	assert.Equal(t, 7, resolveInt(-1, 7))
	assert.Equal(t, 9, resolveInt(9, 7))
}

func TestResolveDuration(t *testing.T) {
	t.Parallel()
	assert.Equal(t, defaultCNCEnqueueDelay, resolveDuration(0, defaultCNCEnqueueDelay))
	assert.Equal(t, kcctStatusMinEmitInterval, resolveDuration(kcctStatusMinEmitInterval, defaultCNCEnqueueDelay))
}

func TestResolveBool(t *testing.T) {
	t.Parallel()
	tr := true
	fl := false
	assert.True(t, resolveBool(nil, true))
	assert.False(t, resolveBool(nil, false))
	assert.True(t, resolveBool(&tr, false))
	assert.False(t, resolveBool(&fl, true))
}

func TestClampInt32(t *testing.T) {
	t.Parallel()
	assert.Equal(t, int32(5), clampInt32(5, 0, 10))
	assert.Equal(t, int32(0), clampInt32(-3, 0, 10))
	assert.Equal(t, int32(10), clampInt32(99, 0, 10))
	// when lo>hi the function should still return value clamped to hi
	assert.Equal(t, int32(0), clampInt32(0, 0, 10))
}

func TestProgressKey(t *testing.T) {
	t.Parallel()
	gvr := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}
	got := progressKey(gvr, "ns/name")
	assert.Contains(t, got, "ns/name")
	assert.Equal(t, got, progressKey(gvr, "ns/name"))
	assert.NotEqual(t, got, progressKey(gvr, "ns/other"))
	gvr2 := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "other"}
	assert.NotEqual(t, got, progressKey(gvr2, "ns/name"))
	assert.Equal(t, "g/v, Resource=r|", progressKeyPrefix(gvr))
}

func TestConfigTypesOf(t *testing.T) {
	t.Parallel()
	assert.Nil(t, configTypesOf(nil))

	gvrA := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "a"}
	gvrB := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "b"}
	cnc := &v1alpha1.CustomNodeConfig{
		Status: v1alpha1.CustomNodeConfigStatus{
			KatalystCustomConfigList: []v1alpha1.TargetConfig{
				{ConfigType: gvrA, Hash: "h"},
				{ConfigType: gvrB, Hash: "h2"},
			},
		},
	}
	got := configTypesOf(cnc)
	assert.ElementsMatch(t, []metav1.GroupVersionResource{gvrA, gvrB}, got)
}

func TestDiffTargetConfigGVRs(t *testing.T) {
	t.Parallel()
	gvrA := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "a"}
	gvrB := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "b"}
	gvrC := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "c"}

	tests := []struct {
		name string
		old  []v1alpha1.TargetConfig
		new  []v1alpha1.TargetConfig
		want []metav1.GroupVersionResource
	}{
		{
			name: "identical",
			old:  []v1alpha1.TargetConfig{{ConfigType: gvrA, Hash: "h"}},
			new:  []v1alpha1.TargetConfig{{ConfigType: gvrA, Hash: "h"}},
			want: nil,
		},
		{
			name: "hash flip",
			old:  []v1alpha1.TargetConfig{{ConfigType: gvrA, Hash: "h1"}},
			new:  []v1alpha1.TargetConfig{{ConfigType: gvrA, Hash: "h2"}},
			want: []metav1.GroupVersionResource{gvrA},
		},
		{
			name: "added entry",
			old:  []v1alpha1.TargetConfig{{ConfigType: gvrA, Hash: "h1"}},
			new: []v1alpha1.TargetConfig{
				{ConfigType: gvrA, Hash: "h1"},
				{ConfigType: gvrB, Hash: "h2"},
			},
			want: []metav1.GroupVersionResource{gvrB},
		},
		{
			name: "deleted entry",
			old: []v1alpha1.TargetConfig{
				{ConfigType: gvrA, Hash: "h1"},
				{ConfigType: gvrB, Hash: "h2"},
			},
			new:  []v1alpha1.TargetConfig{{ConfigType: gvrA, Hash: "h1"}},
			want: []metav1.GroupVersionResource{gvrB},
		},
		{
			name: "rename only",
			old:  []v1alpha1.TargetConfig{{ConfigType: gvrC, ConfigName: "old", Hash: "h1"}},
			new:  []v1alpha1.TargetConfig{{ConfigType: gvrC, ConfigName: "new", Hash: "h1"}},
			want: []metav1.GroupVersionResource{gvrC},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := diffTargetConfigGVRs(tt.old, tt.new)
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

func TestObserveHashLocked(t *testing.T) {
	t.Parallel()
	k := &KatalystCustomConfigTargetController{}
	p := &kcctProgress{}
	// First observation -> rollout starts.
	assert.True(t, k.observeHashLocked(p, "h1"))
	assert.Equal(t, "h1", p.hash)
	assert.False(t, p.rolloutStartedAt.IsZero())
	// Same hash -> no transition.
	startedAt := p.rolloutStartedAt
	p.updatedNodes = 5
	assert.False(t, k.observeHashLocked(p, "h1"))
	assert.Equal(t, int32(5), p.updatedNodes)
	assert.Equal(t, startedAt, p.rolloutStartedAt)
	// New hash resets counters.
	assert.True(t, k.observeHashLocked(p, "h2"))
	assert.Equal(t, "h2", p.hash)
	assert.Equal(t, int32(0), p.updatedNodes)
	assert.Equal(t, int32(0), p.updatedTargetNodes)
}

func TestShouldFullReconcileLocked(t *testing.T) {
	t.Parallel()
	// When incremental disabled, always full.
	k := &KatalystCustomConfigTargetController{
		enableIncrementalProgress:      false,
		cncStatusFullReconcileInterval: time.Hour,
	}
	assert.True(t, k.shouldFullReconcileLocked(&kcctProgress{lastFullReconcileAt: time.Now()}))

	// Incremental enabled, never run -> full.
	k2 := &KatalystCustomConfigTargetController{
		enableIncrementalProgress:      true,
		cncStatusFullReconcileInterval: time.Hour,
	}
	assert.True(t, k2.shouldFullReconcileLocked(&kcctProgress{}))

	// Recent full reconcile -> incremental.
	assert.False(t, k2.shouldFullReconcileLocked(&kcctProgress{lastFullReconcileAt: time.Now()}))

	// Long time ago -> full.
	assert.True(t, k2.shouldFullReconcileLocked(&kcctProgress{lastFullReconcileAt: time.Now().Add(-2 * time.Hour)}))
}

func TestShouldEmitStatusLocked(t *testing.T) {
	t.Parallel()
	k := &KatalystCustomConfigTargetController{
		lastStatusEmit: make(map[string]time.Time),
	}

	emit, retry := k.shouldEmitStatusLocked("kA", true)
	assert.True(t, emit)
	assert.False(t, retry)

	emit, retry = k.shouldEmitStatusLocked("kA", false)
	assert.False(t, emit)
	assert.True(t, retry)

	emit, retry = k.shouldEmitStatusLocked("kB", false)
	assert.True(t, emit)
	assert.False(t, retry)

	emit, retry = k.shouldEmitStatusLocked("kA", true)
	assert.True(t, emit)
	assert.False(t, retry)
}

func TestGetOrInitProgressLocked(t *testing.T) {
	t.Parallel()
	k := &KatalystCustomConfigTargetController{progressCache: map[string]*kcctProgress{}}
	p1 := k.getOrInitProgressLocked("k1")
	require.NotNil(t, p1)
	p1.updatedNodes = 7
	p2 := k.getOrInitProgressLocked("k1")
	assert.Same(t, p1, p2)
	assert.Equal(t, int32(7), p2.updatedNodes)
}

func TestComputeUpdatedNodes(t *testing.T) {
	t.Parallel()
	gvr := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}
	target := util.ToKCCTargetResource(toTestUnstructured(&v1alpha1.AdminQoSConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
	}))
	cncs := []*v1alpha1.CustomNodeConfig{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "cfg", Hash: "h1",
			}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "other", Hash: "h1",
			}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-3"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "cfg", Hash: "h1",
			}}},
		},
	}

	k := &KatalystCustomConfigTargetController{}
	assert.Equal(t, int32(2), k.computeUpdatedNodes(gvr, target, "h1", cncs))
}

func TestUpdateTargetStatuses_TargetShrinkDoesNotDriftUpdatedNodes(t *testing.T) {
	t.Parallel()

	gvr := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}
	writer := &captureUnstructuredControl{}
	k := &KatalystCustomConfigTargetController{
		ctx:                            context.Background(),
		unstructuredControl:            writer,
		metricsEmitter:                 metrics.DummyMetrics{},
		enableIncrementalProgress:      true,
		cncStatusFullReconcileInterval: time.Hour,
		progressCache:                  map[string]*kcctProgress{},
		lastStatusEmit:                 map[string]time.Time{},
		pendingRetrySet:                map[string]struct{}{},
	}

	targetObj := &v1alpha1.AdminQoSConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
		Status: v1alpha1.GenericConfigStatus{
			TargetNodes:        3,
			CanaryNodes:        3,
			UpdatedTargetNodes: 3,
			UpdatedNodes:       3,
			CurrentHash:        "h1",
		},
	}
	target := util.ToKCCTargetResource(toTestUnstructured(targetObj))
	kcctName := native.GenerateUniqObjectNameKey(target)
	k.progressCache[progressKey(gvr, kcctName)] = &kcctProgress{
		hash:                "h1",
		updatedTargetNodes:  3,
		updatedNodes:        3,
		targetNodes:         3,
		canaryNodes:         3,
		lastFullReconcileAt: time.Now(),
	}

	allCNCs := []*v1alpha1.CustomNodeConfig{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "cfg", Hash: "h1",
			}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "cfg", Hash: "h1",
			}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-3"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "cfg", Hash: "h1",
			}}},
		},
	}

	errs := k.updateTargetStatuses(
		gvr,
		[]util.KCCTargetResource{target},
		map[string]string{kcctName: "h1"},
		map[string]int{kcctName: 2},
		map[string][]int{kcctName: {0, 1}},
		allCNCs,
	)
	require.Empty(t, errs)
	require.Len(t, writer.updated, 1)

	updated := util.ToKCCTargetResource(writer.updated[0])
	status := updated.GetGenericStatus()
	assert.Equal(t, int32(2), status.TargetNodes)
	assert.Equal(t, int32(2), status.CanaryNodes)
	assert.Equal(t, int32(2), status.UpdatedTargetNodes)
	assert.Equal(t, int32(3), status.UpdatedNodes)

	cache := k.progressCache[progressKey(gvr, kcctName)]
	require.NotNil(t, cache)
	assert.Equal(t, int32(2), cache.updatedTargetNodes)
	assert.Equal(t, int32(3), cache.updatedNodes)
}

func TestUpdateTargetStatuses_DeltaZeroStillCatchesExternallyUpdatedTargets(t *testing.T) {
	t.Parallel()

	gvr := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}
	writer := &captureUnstructuredControl{}
	k := &KatalystCustomConfigTargetController{
		ctx:                            context.Background(),
		unstructuredControl:            writer,
		metricsEmitter:                 metrics.DummyMetrics{},
		enableIncrementalProgress:      true,
		cncStatusFullReconcileInterval: time.Hour,
		progressCache:                  map[string]*kcctProgress{},
		lastStatusEmit:                 map[string]time.Time{},
		pendingRetrySet:                map[string]struct{}{},
	}

	target := util.ToKCCTargetResource(toTestUnstructured(&v1alpha1.AdminQoSConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "default"},
		Status:     v1alpha1.GenericConfigStatus{TargetNodes: 2, CanaryNodes: 2, UpdatedTargetNodes: 0, UpdatedNodes: 0, CurrentHash: "h1"},
	}))
	kcctName := native.GenerateUniqObjectNameKey(target)
	k.progressCache[progressKey(gvr, kcctName)] = &kcctProgress{
		hash:                "h1",
		updatedTargetNodes:  0,
		updatedNodes:        0,
		targetNodes:         2,
		canaryNodes:         2,
		lastFullReconcileAt: time.Now(),
	}

	allCNCs := []*v1alpha1.CustomNodeConfig{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "cfg", Hash: "h1",
			}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "cfg", Hash: "h1",
			}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-3"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "other", Hash: "h1",
			}}},
		},
	}

	errs := k.updateTargetStatuses(
		gvr,
		[]util.KCCTargetResource{target},
		map[string]string{kcctName: "h1"},
		map[string]int{kcctName: 2},
		map[string][]int{kcctName: {0, 1}},
		allCNCs,
	)
	require.Empty(t, errs)
	require.Len(t, writer.updated, 1)

	updated := util.ToKCCTargetResource(writer.updated[0])
	status := updated.GetGenericStatus()
	assert.Equal(t, int32(2), status.TargetNodes)
	assert.Equal(t, int32(2), status.UpdatedTargetNodes)
	assert.Equal(t, int32(2), status.UpdatedNodes)

	cache := k.progressCache[progressKey(gvr, kcctName)]
	require.NotNil(t, cache)
	assert.Equal(t, int32(2), cache.updatedTargetNodes)
	assert.Equal(t, int32(2), cache.updatedNodes)
}

func TestUpdateTargetStatuses_SwitchBetweenKCCTsKeepsBothSidesCorrect(t *testing.T) {
	t.Parallel()

	gvr := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}
	writer := &captureUnstructuredControl{}
	k := &KatalystCustomConfigTargetController{
		ctx:                            context.Background(),
		unstructuredControl:            writer,
		metricsEmitter:                 metrics.DummyMetrics{},
		enableIncrementalProgress:      true,
		cncStatusFullReconcileInterval: time.Hour,
		progressCache:                  map[string]*kcctProgress{},
		lastStatusEmit:                 map[string]time.Time{},
		pendingRetrySet:                map[string]struct{}{},
	}

	targetA := util.ToKCCTargetResource(toTestUnstructured(&v1alpha1.AdminQoSConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg-a", Namespace: "default"},
		Status:     v1alpha1.GenericConfigStatus{TargetNodes: 2, CanaryNodes: 2, UpdatedTargetNodes: 2, UpdatedNodes: 2, CurrentHash: "ha"},
	}))
	targetB := util.ToKCCTargetResource(toTestUnstructured(&v1alpha1.AdminQoSConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg-b", Namespace: "default"},
		Status:     v1alpha1.GenericConfigStatus{TargetNodes: 1, CanaryNodes: 1, UpdatedTargetNodes: 1, UpdatedNodes: 1, CurrentHash: "hb"},
	}))
	nameA := native.GenerateUniqObjectNameKey(targetA)
	nameB := native.GenerateUniqObjectNameKey(targetB)
	k.progressCache[progressKey(gvr, nameA)] = &kcctProgress{
		hash:                "ha",
		updatedTargetNodes:  2,
		updatedNodes:        2,
		targetNodes:         2,
		canaryNodes:         2,
		lastFullReconcileAt: time.Now(),
	}
	k.progressCache[progressKey(gvr, nameB)] = &kcctProgress{
		hash:                "hb",
		updatedTargetNodes:  1,
		updatedNodes:        1,
		targetNodes:         1,
		canaryNodes:         1,
		lastFullReconcileAt: time.Now(),
	}

	allCNCs := []*v1alpha1.CustomNodeConfig{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "cfg-a", Hash: "ha",
			}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "cfg-b", Hash: "hb",
			}}},
		},
	}

	errs := k.updateTargetStatuses(
		gvr,
		[]util.KCCTargetResource{targetA, targetB},
		map[string]string{nameA: "ha", nameB: "hb"},
		map[string]int{nameA: 1, nameB: 1},
		map[string][]int{nameA: {0}, nameB: {1}},
		allCNCs,
	)
	require.Empty(t, errs)
	require.Len(t, writer.updated, 2)

	statuses := make(map[string]v1alpha1.GenericConfigStatus, len(writer.updated))
	for _, obj := range writer.updated {
		updated := util.ToKCCTargetResource(obj)
		statuses[native.GenerateUniqObjectNameKey(updated)] = updated.GetGenericStatus()
	}

	statusA, ok := statuses[nameA]
	require.True(t, ok)
	assert.Equal(t, int32(1), statusA.TargetNodes)
	assert.Equal(t, int32(1), statusA.UpdatedTargetNodes)
	assert.Equal(t, int32(1), statusA.UpdatedNodes)

	statusB, ok := statuses[nameB]
	require.True(t, ok)
	assert.Equal(t, int32(1), statusB.TargetNodes)
	assert.Equal(t, int32(1), statusB.UpdatedTargetNodes)
	assert.Equal(t, int32(1), statusB.UpdatedNodes)

	cacheA := k.progressCache[progressKey(gvr, nameA)]
	require.NotNil(t, cacheA)
	assert.Equal(t, int32(1), cacheA.updatedTargetNodes)
	assert.Equal(t, int32(1), cacheA.updatedNodes)

	cacheB := k.progressCache[progressKey(gvr, nameB)]
	require.NotNil(t, cacheB)
	assert.Equal(t, int32(1), cacheB.updatedTargetNodes)
	assert.Equal(t, int32(1), cacheB.updatedNodes)
}

func TestScheduleRetryCoalescesByKCCT(t *testing.T) {
	t.Parallel()

	k := &KatalystCustomConfigTargetController{pendingRetrySet: map[string]struct{}{}}
	gvr := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}
	key := progressKey(gvr, "default/cfg")

	_, exists := k.pendingRetrySet[key]
	assert.False(t, exists)
	if _, ok := k.pendingRetrySet[key]; !ok {
		k.pendingRetrySet[key] = struct{}{}
	}
	_, exists = k.pendingRetrySet[key]
	assert.True(t, exists)
	if _, ok := k.pendingRetrySet[key]; !ok {
		k.pendingRetrySet[key] = struct{}{}
	}
	assert.Len(t, k.pendingRetrySet, 1)
	delete(k.pendingRetrySet, key)
	assert.Empty(t, k.pendingRetrySet)
}

func TestClearProgressForTarget(t *testing.T) {
	t.Parallel()
	gvr := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}
	key := progressKey(gvr, "default/cfg")
	otherKey := progressKey(gvr, "default/other")
	k := &KatalystCustomConfigTargetController{
		progressCache:   map[string]*kcctProgress{key: {}, otherKey: {}},
		lastStatusEmit:  map[string]time.Time{key: time.Now(), otherKey: time.Now()},
		pendingRetrySet: map[string]struct{}{key: {}, otherKey: {}},
	}

	k.clearProgressForTarget(gvr, "default/cfg")
	_, ok := k.progressCache[key]
	assert.False(t, ok)
	_, ok = k.lastStatusEmit[key]
	assert.False(t, ok)
	_, ok = k.pendingRetrySet[key]
	assert.False(t, ok)
	_, ok = k.progressCache[otherKey]
	assert.True(t, ok)
	_, ok = k.pendingRetrySet[otherKey]
	assert.True(t, ok)
}

func TestClearProgressForGVR(t *testing.T) {
	t.Parallel()
	gvr := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}
	otherGVR := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "other"}
	key := progressKey(gvr, "default/cfg")
	otherKey := progressKey(otherGVR, "default/cfg")
	otherRetryKey := progressKey(otherGVR, "default/cfg")
	k := &KatalystCustomConfigTargetController{
		progressCache:   map[string]*kcctProgress{key: {}, otherKey: {}},
		lastStatusEmit:  map[string]time.Time{key: time.Now(), otherKey: time.Now()},
		pendingRetrySet: map[string]struct{}{key: {}, otherRetryKey: {}},
	}

	k.clearProgressForGVR(gvr)
	_, ok := k.progressCache[key]
	assert.False(t, ok)
	_, ok = k.lastStatusEmit[key]
	assert.False(t, ok)
	_, ok = k.pendingRetrySet[key]
	assert.False(t, ok)
	_, ok = k.progressCache[otherKey]
	assert.True(t, ok)
	_, ok = k.pendingRetrySet[otherRetryKey]
	assert.True(t, ok)
}

func TestProcessNextWorkItem_AccessorNotFoundDoesNotRetry(t *testing.T) {
	t.Parallel()
	gvr := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}
	key := progressKey(gvr, "default/cfg")
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "test-kcct")
	defer queue.ShutDown()
	queue.Add(gvr)

	k := &KatalystCustomConfigTargetController{
		queue:           queue,
		targetHandler:   &kcctarget.KatalystCustomConfigTargetHandler{},
		metricsEmitter:  metrics.DummyMetrics{},
		progressCache:   map[string]*kcctProgress{key: {}},
		lastStatusEmit:  map[string]time.Time{key: time.Now()},
		pendingRetrySet: map[string]struct{}{key: {}},
	}

	processed := k.processNextWorkItem()
	require.True(t, processed)
	assert.Equal(t, 0, queue.Len())
	assert.Equal(t, 0, queue.NumRequeues(gvr))
	_, ok := k.progressCache[key]
	assert.False(t, ok)
	_, ok = k.lastStatusEmit[key]
	assert.False(t, ok)
	_, ok = k.pendingRetrySet[key]
	assert.False(t, ok)
}

func TestUpdateTargetStatuses_CanaryZeroStillMarksDoneAndEmits(t *testing.T) {
	t.Parallel()

	gvr := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}
	writer := &captureUnstructuredControl{}
	targetObj := &v1alpha1.AdminQoSConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg-zero", Namespace: "default"},
		Status: v1alpha1.GenericConfigStatus{
			TargetNodes:        2,
			CanaryNodes:        0,
			UpdatedTargetNodes: 0,
			UpdatedNodes:       2,
			CurrentHash:        "h1",
		},
	}
	target := util.ToKCCTargetResource(toTestUnstructured(targetObj))
	kcctName := native.GenerateUniqObjectNameKey(target)
	k := &KatalystCustomConfigTargetController{
		ctx:                            context.Background(),
		unstructuredControl:            writer,
		metricsEmitter:                 metrics.DummyMetrics{},
		enableIncrementalProgress:      true,
		cncStatusFullReconcileInterval: time.Hour,
		progressCache: map[string]*kcctProgress{
			progressKey(gvr, kcctName): {
				hash:                "h1",
				updatedTargetNodes:  2,
				updatedNodes:        2,
				targetNodes:         2,
				canaryNodes:         0,
				rolloutStartedAt:    time.Now().Add(-time.Minute),
				rolloutDone:         false,
				lastFullReconcileAt: time.Now(),
			},
		},
		lastStatusEmit: map[string]time.Time{
			progressKey(gvr, kcctName): time.Now(),
		},
	}

	allCNCs := []*v1alpha1.CustomNodeConfig{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "cfg-zero", Hash: "h1",
			}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
			Status: v1alpha1.CustomNodeConfigStatus{KatalystCustomConfigList: []v1alpha1.TargetConfig{{
				ConfigType: gvr, ConfigNamespace: "default", ConfigName: "cfg-zero", Hash: "h1",
			}}},
		},
	}

	errs := k.updateTargetStatuses(
		gvr,
		[]util.KCCTargetResource{target},
		map[string]string{kcctName: "h1"},
		map[string]int{kcctName: 0},
		map[string][]int{kcctName: {0, 1}},
		allCNCs,
	)
	require.Empty(t, errs)
	require.Len(t, writer.updated, 1)

	updated := util.ToKCCTargetResource(writer.updated[0])
	status := updated.GetGenericStatus()
	assert.Equal(t, int32(2), status.TargetNodes)
	assert.Equal(t, int32(0), status.CanaryNodes)
	assert.Equal(t, int32(2), status.UpdatedTargetNodes)
	assert.Equal(t, int32(2), status.UpdatedNodes)

	cache := k.progressCache[progressKey(gvr, kcctName)]
	require.NotNil(t, cache)
	assert.True(t, cache.rolloutDone)
}

func TestGroupCacheUsesPerGVRStateVersion(t *testing.T) {
	t.Parallel()

	gvr1 := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r1"}
	gvr2 := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r2"}
	target := generateTestLabelSelectorTargetResource("cfg", "app=foo", 0)
	combinedRV := combinedKCCTRV([]util.KCCTargetResource{target})
	expected := map[string][]int{"default/cfg": []int{0, 2}}

	k := &KatalystCustomConfigTargetController{}
	k.setCachedGroupCNCIndexes(gvr1, combinedRV, k.getGVRStateVersion(gvr1), expected)

	cached, ok := k.getCachedGroupCNCIndexes(gvr1, combinedRV, k.getGVRStateVersion(gvr1))
	require.True(t, ok)
	assert.Equal(t, expected, cached)

	k.bumpGVRStateVersion(gvr2)
	cached, ok = k.getCachedGroupCNCIndexes(gvr1, combinedRV, k.getGVRStateVersion(gvr1))
	require.True(t, ok)
	assert.Equal(t, expected, cached)

	k.bumpGVRStateVersion(gvr1)
	_, ok = k.getCachedGroupCNCIndexes(gvr1, combinedRV, 0)
	assert.False(t, ok)
	_, ok = k.getCachedGroupCNCIndexes(gvr1, combinedRV, k.getGVRStateVersion(gvr1))
	assert.False(t, ok)
}

func TestClearProgressForGVRAlsoClearsGroupCacheAndVersion(t *testing.T) {
	t.Parallel()

	gvr := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r"}
	otherGVR := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "other"}
	key := progressKey(gvr, "default/cfg")
	otherKey := progressKey(otherGVR, "default/cfg")
	k := &KatalystCustomConfigTargetController{
		progressCache:   map[string]*kcctProgress{key: {}, otherKey: {}},
		lastStatusEmit:  map[string]time.Time{key: time.Now(), otherKey: time.Now()},
		pendingRetrySet: map[string]struct{}{key: {}, otherKey: {}},
	}
	k.setCachedGroupCNCIndexes(gvr, "rv-1", 1, map[string][]int{"default/cfg": []int{0}})
	k.setCachedGroupCNCIndexes(otherGVR, "rv-2", 2, map[string][]int{"default/other": []int{1}})
	k.gvrCNCStateVersions.Store(gvr, func() *int64 { v := int64(1); return &v }())
	k.gvrCNCStateVersions.Store(otherGVR, func() *int64 { v := int64(2); return &v }())

	k.clearProgressForGVR(gvr)

	_, ok := k.groupCache.Load(gvr)
	assert.False(t, ok)
	_, ok = k.groupCache.Load(otherGVR)
	assert.True(t, ok)
	_, ok = k.gvrCNCStateVersions.Load(gvr)
	assert.False(t, ok)
	_, ok = k.gvrCNCStateVersions.Load(otherGVR)
	assert.True(t, ok)
}
