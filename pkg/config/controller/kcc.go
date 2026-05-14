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

package controller

import (
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
)

type KCCConfig struct {
	// ValidAPIGroupSet indicates the api-groups that kcc allows.
	ValidAPIGroupSet sets.String
	// DefaultGVRs indicates the gvr that need to watch by default.
	// value is gvr string, e.g. "nodeprofiledescriptors.v1alpha1.node.katalyst.kubewharf.io"
	DefaultGVRs []string

	// ===== Performance tuning knobs =====
	// All zero/empty values fall back to safe defaults defined in the controller package,
	// so admins can leave them unset for backward compatibility.

	// KCCTWorkerCount is the number of workers reconciling KCCT GVRs in parallel.
	// 0 means use default.
	KCCTWorkerCount int
	// CNCWorkerCount is the parallelism for patching CNCs in a single reconcile round.
	// 0 means use default.
	CNCWorkerCount int

	// CNCEnqueueDelay is the debounce delay before enqueuing GVRs triggered by CNC events.
	// 0 means use default.
	CNCEnqueueDelay time.Duration
	// KCCTEnqueueDelay is the debounce delay before enqueuing GVRs triggered by KCCT events.
	// 0 means use default.
	KCCTEnqueueDelay time.Duration

	// CNCUpdateQPS is the per-GVR rate limit (req/s) for CNC status patches.
	// 0 means use default.
	CNCUpdateQPS int
	// CNCUpdateBurst is the per-GVR burst size for CNC status patches.
	// 0 means use default.
	CNCUpdateBurst int

	// CNCStatusFullReconcileInterval controls how often the controller falls back to
	// a full O(N*M) status recompute even when the incremental fast path is enabled.
	// 0 means use default.
	CNCStatusFullReconcileInterval time.Duration

	// ===== Feature toggles (default: enabled) =====
	// These flags allow operators to roll back to legacy behaviour without redeploying.
	// We use *bool so callers can distinguish "unset" from "explicitly disabled".

	// EnablePreciseCNCDispatch toggles the precise CNC event dispatch path.
	// When nil or true, only affected GVRs are enqueued on CNC change; otherwise all GVRs are enqueued.
	EnablePreciseCNCDispatch *bool
	// EnableCNCJSONPatch toggles the JSON-Patch fast path for CNC status updates.
	// When nil or true, KCCT controller uses RFC6902 patch on the target config slice;
	// otherwise it falls back to the legacy MergePatch path.
	EnableCNCJSONPatch *bool
	// EnableCNCSSA toggles the Server-Side Apply path for CNC status updates.
	// When nil or false, KCCT controller uses JSON-Patch or MergePatch.
	// Currently defaults to false because CustomNodeConfig API lacks listMapKey markers.
	EnableCNCSSA *bool
	// EnableIncrementalProgress toggles the incremental updatedNodes accounting.
	// When nil or true, the controller maintains progress in-memory and only reconciles
	// status fully on hash change or full-reconcile interval; otherwise it always recomputes.
	EnableIncrementalProgress *bool
}

func NewKCCConfig() *KCCConfig {
	return &KCCConfig{}
}
