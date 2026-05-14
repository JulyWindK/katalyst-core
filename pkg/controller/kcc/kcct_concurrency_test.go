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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	"github.com/kubewharf/katalyst-core/pkg/client/control"
	"github.com/kubewharf/katalyst-core/pkg/controller/kcc/target"
	"github.com/kubewharf/katalyst-core/pkg/util"
)

// mockTargetAccessor mocks the target.KatalystCustomConfigTargetAccessor interface
type mockTargetAccessor struct {
	target.DummyKatalystCustomConfigTargetAccessor
	getFunc func(namespace, name string) (*unstructured.Unstructured, error)
}

func (m *mockTargetAccessor) Get(namespace, name string) (*unstructured.Unstructured, error) {
	return m.getFunc(namespace, name)
}

// mockUnstructuredControl mocks the control.UnstructuredControl interface
type mockUnstructuredControl struct {
	control.DummyUnstructuredControl
	updateStatusFunc func(obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

func (m *mockUnstructuredControl) UpdateUnstructuredStatus(_ context.Context, _ metav1.GroupVersionResource, obj *unstructured.Unstructured, _ metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	return m.updateStatusFunc(obj)
}

// mockConcurrencyTargetHandler mocks the target handler to return our mock accessor
type mockConcurrencyTargetHandler struct {
	KatalystCustomConfigTargetHandlerInterface
	accessor target.KatalystCustomConfigTargetAccessor
}

func (m *mockConcurrencyTargetHandler) GetTargetAccessorByGVR(gvr metav1.GroupVersionResource) (target.KatalystCustomConfigTargetAccessor, bool) {
	return m.accessor, m.accessor != nil
}

func TestUpdateTargetStatuses_ConcurrencyAndDrift(t *testing.T) {
	t.Parallel()

	gvr := metav1.GroupVersionResource{Group: "config.katalyst.kubewharf.io", Version: "v1alpha1", Resource: "adminqosconfigurations"}
	kcctName := "default/test-config"
	pkey := progressKey(gvr, kcctName)

	t.Run("Status rollback protection on version drift", func(t *testing.T) {
		targetObj := &v1alpha1.AdminQoSConfiguration{
			TypeMeta: metav1.TypeMeta{Kind: "AdminQoSConfiguration", APIVersion: "config.katalyst.kubewharf.io/v1alpha1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:            "test-config",
				Namespace:       "default",
				ResourceVersion: "100",
			},
		}
		targetRes := util.ToKCCTargetResource(toTestUnstructured(targetObj))

		latestObj := targetObj.DeepCopy()
		latestObj.ResourceVersion = "101"
		mockAccessor := &mockTargetAccessor{
			getFunc: func(ns, name string) (*unstructured.Unstructured, error) {
				return toTestUnstructured(latestObj), nil
			},
		}

		writer := &captureUnstructuredControl{}
		k := newTestKCCTController(writer)
		k.progressCache[pkey] = &kcctProgress{hash: "h1", rolloutStartedAt: time.Now()}
		k.targetHandler = &mockConcurrencyTargetHandler{accessor: mockAccessor}

		errs := k.updateTargetStatuses(
			gvr,
			[]util.KCCTargetResource{targetRes},
			map[string]string{kcctName: "h1"},
			map[string]int{kcctName: 0},
			map[string][]int{kcctName: {}},
			nil,
		)

		assert.Empty(t, errs)
		assert.Len(t, writer.updated, 0)
	})

	t.Run("Memory progress safety on API failure", func(t *testing.T) {
		targetObj := &v1alpha1.AdminQoSConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default", ResourceVersion: "100"},
		}
		targetRes := util.ToKCCTargetResource(toTestUnstructured(targetObj))

		initialEmitTime := time.Now().Add(-10 * time.Minute)
		writer := &mockUnstructuredControl{
			updateStatusFunc: func(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
				return nil, apierrors.NewConflict(schema.GroupResource{}, "test", fmt.Errorf("conflict"))
			},
		}
		k := newTestKCCTController(writer)
		k.progressCache[pkey] = &kcctProgress{
			hash:                "h1",
			updatedNodes:        0,
			lastFullReconcileAt: time.Time{},
		}
		k.lastStatusEmit[pkey] = initialEmitTime
		k.targetHandler = &mockConcurrencyTargetHandler{}

		errs := k.updateTargetStatuses(
			gvr,
			[]util.KCCTargetResource{targetRes},
			map[string]string{kcctName: "h1"},
			map[string]int{kcctName: 0},
			map[string][]int{kcctName: {}},
			nil,
		)

		assert.Empty(t, errs)
		k.progressMu.Lock()
		progress := k.progressCache[pkey]
		assert.Equal(t, int32(0), progress.updatedNodes)
		assert.True(t, progress.lastFullReconcileAt.IsZero())
		assert.Equal(t, initialEmitTime, k.lastStatusEmit[pkey])
		k.progressMu.Unlock()
	})

	t.Run("Lock contention and parallel execution", func(t *testing.T) {
		k := newTestKCCTController(&control.DummyUnstructuredControl{})
		k.targetHandler = &mockConcurrencyTargetHandler{}

		gvr1 := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r1"}
		gvr2 := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "r2"}
		k.progressCache[progressKey(gvr1, "c1")] = &kcctProgress{hash: "h1"}
		k.progressCache[progressKey(gvr2, "c2")] = &kcctProgress{hash: "h2"}

		var wg sync.WaitGroup
		wg.Add(2)

		start := time.Now()
		go func() {
			defer wg.Done()
			k.updateTargetStatuses(gvr1, []util.KCCTargetResource{generateTestLabelSelectorTargetResource("c1", "", 0)}, map[string]string{"c1": "h1"}, nil, nil, nil)
		}()
		go func() {
			defer wg.Done()
			k.updateTargetStatuses(gvr2, []util.KCCTargetResource{generateTestLabelSelectorTargetResource("c2", "", 0)}, map[string]string{"c2": "h2"}, nil, nil, nil)
		}()

		wg.Wait()
		duration := time.Since(start)
		assert.Less(t, duration, 1*time.Second)
	})
}
