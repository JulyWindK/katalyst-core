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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configapis "github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
)

func makeCNCWithStatus(name string, list []configapis.TargetConfig) *configapis.CustomNodeConfig {
	return &configapis.CustomNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: configapis.CustomNodeConfigStatus{
			KatalystCustomConfigList: list,
		},
	}
}

func TestPrepareJSONPatchForCNCTargetConfig_Replace(t *testing.T) {
	t.Parallel()

	gvrA := metav1.GroupVersionResource{Group: "config.katalyst.kubewharf.io", Version: "v1alpha1", Resource: "a"}
	gvrB := metav1.GroupVersionResource{Group: "config.katalyst.kubewharf.io", Version: "v1alpha1", Resource: "b"}

	old := makeCNCWithStatus("n1", []configapis.TargetConfig{
		{ConfigType: gvrA, ConfigName: "ca", Hash: "h1"},
		{ConfigType: gvrB, ConfigName: "cb", Hash: "h2"},
	})
	newC := makeCNCWithStatus("n1", []configapis.TargetConfig{
		{ConfigType: gvrA, ConfigName: "ca", Hash: "h1"},
		{ConfigType: gvrB, ConfigName: "cb", Hash: "h2-new"},
	})

	patch, useJSON, err := prepareJSONPatchForCNCTargetConfig(old, newC)
	require.NoError(t, err)
	assert.True(t, useJSON)
	require.NotNil(t, patch)

	var ops []jsonPatchOp
	require.NoError(t, json.Unmarshal(patch, &ops))
	require.Len(t, ops, 1)
	assert.Equal(t, "replace", ops[0].Op)
	assert.Equal(t, "/status/katalystCustomConfigList/1", ops[0].Path)
}

func TestPrepareJSONPatchForCNCTargetConfig_NoOp(t *testing.T) {
	t.Parallel()

	gvrA := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "a"}
	old := makeCNCWithStatus("n1", []configapis.TargetConfig{
		{ConfigType: gvrA, ConfigName: "ca", Hash: "h1"},
	})
	newC := old.DeepCopy()

	patch, useJSON, err := prepareJSONPatchForCNCTargetConfig(old, newC)
	require.NoError(t, err)
	assert.True(t, useJSON)
	assert.Nil(t, patch)
}

func TestPrepareJSONPatchForCNCTargetConfig_NewEntryFallback(t *testing.T) {
	t.Parallel()

	gvrA := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "a"}
	gvrB := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "b"}
	old := makeCNCWithStatus("n1", []configapis.TargetConfig{
		{ConfigType: gvrA, ConfigName: "ca", Hash: "h1"},
	})
	newC := makeCNCWithStatus("n1", []configapis.TargetConfig{
		{ConfigType: gvrA, ConfigName: "ca", Hash: "h1"},
		{ConfigType: gvrB, ConfigName: "cb", Hash: "h2"},
	})

	patch, useJSON, err := prepareJSONPatchForCNCTargetConfig(old, newC)
	require.NoError(t, err)
	// New entry -> fallback to merge patch
	assert.False(t, useJSON)
	assert.NotNil(t, patch)
	// Should be a merge patch document with status object inside
	assert.Contains(t, string(patch), "katalystCustomConfigList")
}

func TestPrepareJSONPatchForCNCTargetConfig_DeletionFallback(t *testing.T) {
	t.Parallel()

	gvrA := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "a"}
	gvrB := metav1.GroupVersionResource{Group: "g", Version: "v", Resource: "b"}
	old := makeCNCWithStatus("n1", []configapis.TargetConfig{
		{ConfigType: gvrA, ConfigName: "ca", Hash: "h1"},
		{ConfigType: gvrB, ConfigName: "cb", Hash: "h2"},
	})
	newC := makeCNCWithStatus("n1", []configapis.TargetConfig{
		{ConfigType: gvrA, ConfigName: "ca", Hash: "h1"},
	})

	patch, useJSON, err := prepareJSONPatchForCNCTargetConfig(old, newC)
	require.NoError(t, err)
	assert.False(t, useJSON)
	assert.NotNil(t, patch)
}

func TestPrepareJSONPatchForCNCTargetConfig_NilArgs(t *testing.T) {
	t.Parallel()

	_, _, err := prepareJSONPatchForCNCTargetConfig(nil, nil)
	assert.Error(t, err)
}

func TestDummyCNCControl_PatchCNCTargetConfig(t *testing.T) {
	t.Parallel()

	d := DummyCNCControl{}
	cnc := makeCNCWithStatus("n1", nil)
	got, err := d.PatchCNCTargetConfig(nil, "n1", nil, cnc) // nolint:staticcheck
	require.NoError(t, err)
	assert.Equal(t, cnc, got)
}
