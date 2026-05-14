package target

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newTestTarget(spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "config.katalyst.kubewharf.io/v1alpha1",
		"kind":       "AdminQoSConfiguration",
		"metadata": map[string]interface{}{
			"name":      "cfg",
			"namespace": "default",
		},
		"spec": spec,
	}}
}

func TestTargetNeedsReconcile(t *testing.T) {
	t.Parallel()

	base := newTestTarget(map[string]interface{}{"foo": "bar"})

	t.Run("ignore status change", func(t *testing.T) {
		oldObj := base.DeepCopy()
		newObj := base.DeepCopy()
		newObj.Object["status"] = map[string]interface{}{"phase": "ok"}
		assert.False(t, targetNeedsReconcile(oldObj, newObj))
	})

	t.Run("ignore metadata annotation change", func(t *testing.T) {
		oldObj := base.DeepCopy()
		newObj := base.DeepCopy()
		_ = unstructured.SetNestedStringMap(newObj.Object, map[string]string{"k": "v"}, "metadata", "annotations")
		assert.False(t, targetNeedsReconcile(oldObj, newObj))
	})

	t.Run("reconcile on spec change", func(t *testing.T) {
		oldObj := base.DeepCopy()
		newObj := newTestTarget(map[string]interface{}{"foo": "baz"})
		assert.True(t, targetNeedsReconcile(oldObj, newObj))
	})

	t.Run("reconcile on deletion timestamp change", func(t *testing.T) {
		oldObj := base.DeepCopy()
		newObj := base.DeepCopy()
		ts := metav1.NewTime(time.Unix(123, 0))
		newObj.SetDeletionTimestamp(&ts)
		assert.True(t, targetNeedsReconcile(oldObj, newObj))
	})

	t.Run("reconcile on nil target", func(t *testing.T) {
		assert.True(t, targetNeedsReconcile(nil, base.DeepCopy()))
		assert.True(t, targetNeedsReconcile(base.DeepCopy(), nil))
	})
}
