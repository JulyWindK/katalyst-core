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

package control

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	jsonpatch "github.com/evanphx/json-patch"

	"github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	clientset "github.com/kubewharf/katalyst-api/pkg/client/clientset/versioned"
)

// CNCControl is used to update CustomNodeConfig
type CNCControl interface {
	// CreateCNC is used to create new CNC obj
	CreateCNC(ctx context.Context, cnc *v1alpha1.CustomNodeConfig,
		opts metav1.CreateOptions) (*v1alpha1.CustomNodeConfig, error)

	// DeleteCNC is used to delete CNC obj
	DeleteCNC(ctx context.Context, cncName string,
		opts metav1.DeleteOptions) error

	// PatchCNC is used to update the changes for CNC spec and metadata contents
	PatchCNC(ctx context.Context, cncName string, oldCNC,
		newCNC *v1alpha1.CustomNodeConfig) (*v1alpha1.CustomNodeConfig, error)

	// PatchCNCStatus is used to update the changes for CNC status contents
	PatchCNCStatus(ctx context.Context, cncName string, oldCNC,
		newCNC *v1alpha1.CustomNodeConfig) (*v1alpha1.CustomNodeConfig, error)

	// PatchCNCTargetConfig issues a minimal RFC6902 JSON Patch that only mutates the entry
	// in status.katalystCustomConfigList for the given GVR. Compared to PatchCNCStatus
	// (which uses MergePatch and replaces the whole array), this avoids write amplification
	// when many controllers race on different GVRs of the same CNC.
	//
	// If the targetConfig array does not contain the given GVR yet, the patch falls back
	// to MergePatch on the whole status to keep the array sorted (sorting is the
	// responsibility of the original ApplyKCCTargetConfigToCNC helper).
	PatchCNCTargetConfig(ctx context.Context, cncName string, oldCNC,
		newCNC *v1alpha1.CustomNodeConfig) (*v1alpha1.CustomNodeConfig, error)

	// ApplyCNCTargetConfig uses Server-Side Apply (SSA) to update a specific entry in
	// status.katalystCustomConfigList. This avoids conflicts when multiple controllers
	// update different GVRs of the same CNC.
	ApplyCNCTargetConfig(ctx context.Context, cncName string,
		entry *v1alpha1.TargetConfig) (*v1alpha1.CustomNodeConfig, error)
}

type DummyCNCControl struct{}

func (d DummyCNCControl) CreateCNC(_ context.Context, cnc *v1alpha1.CustomNodeConfig,
	_ metav1.CreateOptions,
) (*v1alpha1.CustomNodeConfig, error) {
	return cnc, nil
}

func (d DummyCNCControl) DeleteCNC(_ context.Context, _ string,
	_ metav1.DeleteOptions,
) error {
	return nil
}

func (d DummyCNCControl) PatchCNC(_ context.Context, _ string,
	_, newCNC *v1alpha1.CustomNodeConfig,
) (*v1alpha1.CustomNodeConfig, error) {
	return newCNC, nil
}

func (d DummyCNCControl) PatchCNCStatus(_ context.Context, _ string,
	_, newCNC *v1alpha1.CustomNodeConfig,
) (*v1alpha1.CustomNodeConfig, error) {
	return newCNC, nil
}

func (d DummyCNCControl) PatchCNCTargetConfig(_ context.Context, _ string,
	_, newCNC *v1alpha1.CustomNodeConfig) (*v1alpha1.CustomNodeConfig, error) {
	return newCNC, nil
}

func (d DummyCNCControl) ApplyCNCTargetConfig(_ context.Context, _ string,
	_ *v1alpha1.TargetConfig) (*v1alpha1.CustomNodeConfig, error) {
	return nil, nil
}

type RealCNCControl struct {
	client clientset.Interface
}

func (r *RealCNCControl) CreateCNC(ctx context.Context, cnc *v1alpha1.CustomNodeConfig, opts metav1.CreateOptions) (*v1alpha1.CustomNodeConfig, error) {
	if cnc == nil {
		return nil, fmt.Errorf("can't create a nil cnc")
	}

	return r.client.ConfigV1alpha1().CustomNodeConfigs().Create(ctx, cnc, opts)
}

func (r *RealCNCControl) DeleteCNC(ctx context.Context, cncName string, opts metav1.DeleteOptions) error {
	return r.client.ConfigV1alpha1().CustomNodeConfigs().Delete(ctx, cncName, opts)
}

func (r *RealCNCControl) PatchCNC(ctx context.Context, cncName string, oldCNC, newCNC *v1alpha1.CustomNodeConfig) (*v1alpha1.CustomNodeConfig, error) {
	patchBytes, err := preparePatchBytesForCNC(cncName, oldCNC, newCNC)
	if err != nil {
		return nil, err
	}

	updatedCNC, err := r.client.ConfigV1alpha1().CustomNodeConfigs().Patch(ctx, cncName, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to patch spec and metadata %q for cnc %q: %v", patchBytes, cncName, err)
	}

	return updatedCNC, nil
}

func (r *RealCNCControl) PatchCNCStatus(ctx context.Context, cncName string, oldCNC, newCNC *v1alpha1.CustomNodeConfig) (*v1alpha1.CustomNodeConfig, error) {
	patchBytes, err := preparePatchBytesForCNCStatus(cncName, oldCNC, newCNC)
	if err != nil {
		return nil, err
	}

	updatedCNC, err := r.client.ConfigV1alpha1().CustomNodeConfigs().Patch(ctx, cncName, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status")
	if err != nil {
		return nil, fmt.Errorf("failed to patch status %q for cnc %q: %v", patchBytes, cncName, err)
	}

	return updatedCNC, nil
}

func (r *RealCNCControl) PatchCNCTargetConfig(ctx context.Context, cncName string, oldCNC, newCNC *v1alpha1.CustomNodeConfig) (*v1alpha1.CustomNodeConfig, error) {
	patchBytes, useJSONPatch, err := prepareJSONPatchForCNCTargetConfig(oldCNC, newCNC)
	if err != nil {
		return nil, err
	}
	// Nothing to update.
	if patchBytes == nil {
		return oldCNC, nil
	}

	if useJSONPatch {
		updatedCNC, err := r.client.ConfigV1alpha1().CustomNodeConfigs().Patch(
			ctx, cncName, types.JSONPatchType, patchBytes, metav1.PatchOptions{}, "status")
		if err != nil {
			return nil, fmt.Errorf("failed to json-patch status for cnc %q: %v", cncName, err)
		}
		return updatedCNC, nil
	}

	// Fallback path: MergePatch on full status (e.g. the entry does not yet exist in the array,
	// or the slice may need re-sort - we let the regular MergePatch handle it).
	updatedCNC, err := r.client.ConfigV1alpha1().CustomNodeConfigs().Patch(
		ctx, cncName, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status")
	if err != nil {
		return nil, fmt.Errorf("failed to merge-patch status for cnc %q: %v", cncName, err)
	}
	return updatedCNC, nil
}

func (r *RealCNCControl) ApplyCNCTargetConfig(ctx context.Context, cncName string, entry *v1alpha1.TargetConfig) (*v1alpha1.CustomNodeConfig, error) {
	if entry == nil {
		return nil, fmt.Errorf("can't apply a nil entry")
	}

	// Construct the partial object for SSA
	patch := map[string]interface{}{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(),
		"kind":       "CustomNodeConfig",
		"status": map[string]interface{}{
			"katalystCustomConfigList": []v1alpha1.TargetConfig{*entry},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal ssa patch for cnc %q: %v", cncName, err)
	}

	// FieldManager should be unique per GVR to avoid conflicts
	fieldManager := fmt.Sprintf("kcct-%s", entry.ConfigType.String())
	force := true

	updatedCNC, err := r.client.ConfigV1alpha1().CustomNodeConfigs().Patch(
		ctx, cncName, types.ApplyPatchType, patchBytes, metav1.PatchOptions{
			FieldManager: fieldManager,
			Force:        &force,
		}, "status")
	if err != nil {
		return nil, fmt.Errorf("failed to apply status for cnc %q: %v", cncName, err)
	}
	return updatedCNC, nil
}

// preparePatchBytesForCNCStatus generate those json patch bytes for comparing new and old CNC status
// while keep the spec remains the same
func preparePatchBytesForCNCStatus(cncName string, oldCNC, newCNC *v1alpha1.CustomNodeConfig) ([]byte, error) {
	if oldCNC == nil || newCNC == nil {
		return nil, fmt.Errorf("neither old nor new object can be nil")
	}

	oldData, err := json.Marshal(oldCNC)
	if err != nil {
		return nil, fmt.Errorf("failed to Marshal oldData for cnc %q: %v", cncName, err)
	}

	diffCNC := oldCNC.DeepCopy()
	diffCNC.Status = newCNC.Status
	newData, err := json.Marshal(diffCNC)
	if err != nil {
		return nil, fmt.Errorf("failed to Marshal newData for cnc %q: %v", cncName, err)
	}

	patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
	if err != nil {
		return nil, fmt.Errorf("failed to CreateTwoWayMergePatch for cnc %q: %v", cncName, err)
	}

	return patchBytes, nil
}

// preparePatchBytesForCNC generate those json patch bytes for comparing new and old CNC spec and metadata
// while keep the status remains the same
func preparePatchBytesForCNC(cncName string, oldCNC, newCNC *v1alpha1.CustomNodeConfig) ([]byte, error) {
	if oldCNC == nil || newCNC == nil {
		return nil, fmt.Errorf("neither old nor new object can be nil")
	}

	oldData, err := json.Marshal(oldCNC)
	if err != nil {
		return nil, fmt.Errorf("failed to Marshal oldData for cnc %q: %v", cncName, err)
	}

	diffCNC := oldCNC.DeepCopy()
	diffCNC.Spec = newCNC.Spec
	diffCNC.ObjectMeta = newCNC.ObjectMeta
	newData, err := json.Marshal(diffCNC)
	if err != nil {
		return nil, fmt.Errorf("failed to Marshal newData for cnc %q: %v", cncName, err)
	}

	patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
	if err != nil {
		return nil, fmt.Errorf("failed to CreateTwoWayMergePatch for cnc %q: %v", cncName, err)
	}

	return patchBytes, nil
}

// jsonPatchOp models a single RFC6902 operation; we only need add/replace/test here.
type jsonPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// prepareJSONPatchForCNCTargetConfig builds a minimal patch for diffs in
// status.katalystCustomConfigList.
//
// Returns (patchBytes, useJSONPatch, err):
//   - useJSONPatch = true  -> patchBytes is an RFC6902 JSON Patch document, and status sub-resource
//     should be patched with types.JSONPatchType.
//   - useJSONPatch = false -> patchBytes is a MergePatch document covering the whole status, used
//     as a fallback when JSON-Patch cannot be applied safely (mostly: array shrinking
//     or new entries that would invalidate sorted order).
//   - patchBytes == nil    -> nothing to patch.
func prepareJSONPatchForCNCTargetConfig(oldCNC, newCNC *v1alpha1.CustomNodeConfig) ([]byte, bool, error) {
	if oldCNC == nil || newCNC == nil {
		return nil, false, fmt.Errorf("neither old nor new object can be nil")
	}
	// Without a resourceVersion we cannot build a safe atomic JSON Patch. This can
	// happen in fake/integration paths where the informer snapshot does not carry
	// the server-assigned resourceVersion, so we fall back to the existing MergePatch path.
	if oldCNC.ResourceVersion == "" {
		return mergePatchFallback(oldCNC, newCNC)
	}

	oldList := oldCNC.Status.KatalystCustomConfigList
	newList := newCNC.Status.KatalystCustomConfigList

	// Index by ConfigType -> position in old list, for O(1) lookup.
	oldIdx := make(map[string]int, len(oldList))
	for i := range oldList {
		oldIdx[oldList[i].ConfigType.String()] = i
	}

	// Detect deletions; if any entry was removed we cannot safely use JSON Patch by index.
	for _, e := range oldList {
		found := false
		for _, n := range newList {
			if n.ConfigType == e.ConfigType {
				found = true
				break
			}
		}
		if !found {
			return mergePatchFallback(oldCNC, newCNC)
		}
	}

	// Start with a 'test' operation on resourceVersion to ensure atomicity.
	// If the object was modified (e.g. array reordered or entries added) by another
	// controller since we read it, this test will fail and the entire patch will be rejected.
	ops := []jsonPatchOp{
		{
			Op:    "test",
			Path:  "/metadata/resourceVersion",
			Value: oldCNC.ResourceVersion,
		},
	}

	for _, n := range newList {
		key := n.ConfigType.String()
		idx, ok := oldIdx[key]
		if !ok {
			// New entry - sorted order may change, fallback to MergePatch on full status.
			return mergePatchFallback(oldCNC, newCNC)
		}
		old := oldList[idx]
		if old == n {
			continue
		}
		ops = append(ops, jsonPatchOp{
			Op:    "replace",
			Path:  fmt.Sprintf("/status/katalystCustomConfigList/%d", idx),
			Value: n,
		})
	}

	if len(ops) == 1 {
		// Only the 'test' op is present, nothing to update.
		return nil, true, nil
	}

	patchBytes, err := json.Marshal(ops)
	if err != nil {
		return nil, false, fmt.Errorf("failed to marshal json patch: %v", err)
	}
	return patchBytes, true, nil
}

func mergePatchFallback(oldCNC, newCNC *v1alpha1.CustomNodeConfig) ([]byte, bool, error) {
	patchBytes, err := preparePatchBytesForCNCStatus(newCNC.GetName(), oldCNC, newCNC)
	if err != nil {
		return nil, false, err
	}
	return patchBytes, false, nil
}

func NewRealCNCControl(client clientset.Interface) *RealCNCControl {
	return &RealCNCControl{
		client: client,
	}
}
