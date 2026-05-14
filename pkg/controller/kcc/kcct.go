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
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	configapis "github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	configinformers "github.com/kubewharf/katalyst-api/pkg/client/informers/externalversions/config/v1alpha1"
	"github.com/kubewharf/katalyst-api/pkg/client/listers/config/v1alpha1"
	kcclient "github.com/kubewharf/katalyst-core/pkg/client"
	"github.com/kubewharf/katalyst-core/pkg/client/control"
	kccconfig "github.com/kubewharf/katalyst-core/pkg/config/controller"
	"github.com/kubewharf/katalyst-core/pkg/config/generic"
	"github.com/kubewharf/katalyst-core/pkg/consts"
	kcctarget "github.com/kubewharf/katalyst-core/pkg/controller/kcc/target"
	kccutil "github.com/kubewharf/katalyst-core/pkg/controller/kcc/util"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	"github.com/kubewharf/katalyst-core/pkg/util"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
	"github.com/kubewharf/katalyst-core/pkg/util/native"
)

const (
	kccTargetControllerName = "kcct"
)

const (
	defaultKCCTWorkerCount = 1
	defaultCNCWorkerCount  = 16
)

const (
	defaultCNCEnqueueDelay  = 20 * time.Second
	defaultKCCTEnqueueDelay = 10 * time.Second
)

const (
	defaultCNCUpdateQPS   = 10
	defaultCNCUpdateBurst = 100
)

const (
	defaultCNCStatusFullReconcileInterval = 5 * time.Minute
	kcctStatusMinEmitInterval             = 5 * time.Second
)

const (
	metricKCCTReconcileDuration = "kcct_reconcile_duration_ms"
	metricKCCTCNCPatchTotal     = "kcct_cnc_patch_total"
	metricKCCTQueueDepth        = "kcct_queue_depth"
	metricKCCTEventDispatch     = "kcct_event_dispatch_total"
	metricKCCTRolloutDuration   = "kcct_rollout_duration_ms"
)

const (
	kccTargetConditionReasonNormal                      = "Normal"
	kccTargetConditionReasonHashFailed                  = "HashFailed"
	kccTargetConditionReasonMatchMoreOrLessThanOneKCC   = "MatchMoreOrLessThanOneKCC"
	kccTargetConditionReasonValidateFailed              = "ValidateFailed"
	kccTargetConditionReasonCalculateCanaryCutoffFailed = "CalculateCanaryCutoffFailed"
)

type KatalystCustomConfigTargetHandlerInterface interface {
	HasSynced() bool
	Run()
	RegisterTargetHandler(name string, handlerFunc kcctarget.KatalystCustomConfigTargetHandlerFunc)
	GetKCCKeyListByGVR(gvr metav1.GroupVersionResource) []string
	GetTargetAccessorByGVR(gvr metav1.GroupVersionResource) (kcctarget.KatalystCustomConfigTargetAccessor, bool)
	RangeGVRTargetAccessor(f func(gvr metav1.GroupVersionResource, accessor kcctarget.KatalystCustomConfigTargetAccessor) bool)
	GetKCCTargetResource(gvr metav1.GroupVersionResource, obj *unstructured.Unstructured) (util.KCCTargetResource, error)
}

type KatalystCustomConfigTargetController struct {
	ctx       context.Context
	dryRun    bool
	kccConfig *kccconfig.KCCConfig

	client              *kcclient.GenericClientSet
	kccControl          control.KCCControl
	unstructuredControl control.UnstructuredControl
	cncControl          control.CNCControl

	katalystCustomConfigLister v1alpha1.KatalystCustomConfigLister
	customNodeConfigLister     v1alpha1.CustomNodeConfigLister

	syncedFunc []cache.InformerSynced

	queue workqueue.RateLimitingInterface

	rateLimiters   sync.Map
	targetHandler  KatalystCustomConfigTargetHandlerInterface
	metricsEmitter metrics.MetricEmitter

	kcctWorkerCount  int
	cncWorkerCount   int
	cncEnqueueDelay  time.Duration
	kcctEnqueueDelay time.Duration
	cncUpdateQPS     int
	cncUpdateBurst   int

	cncStatusFullReconcileInterval time.Duration

	enablePreciseCNCDispatch  bool
	enableCNCJSONPatch        bool
	enableIncrementalProgress bool

	gvrCNCStateVersions sync.Map
	groupCache          sync.Map

	progressMu      sync.Mutex
	progressCache   map[string]*kcctProgress
	lastStatusEmit  map[string]time.Time
	pendingRetrySet map[string]struct{}
}

type kcctProgress struct {
	hash                string
	updatedTargetNodes  int32
	updatedNodes        int32
	targetNodes         int32
	canaryNodes         int32
	rolloutStartedAt    time.Time
	rolloutDone         bool
	lastFullReconcileAt time.Time
}

type kcctGroupCacheEntry struct {
	kcctCombinedRV   string
	cncStateVersion  int64
	targetCNCIndexes map[string][]int
}

func NewKatalystCustomConfigTargetController(
	ctx context.Context,
	genericConf *generic.GenericConfiguration,
	_ *kccconfig.GenericControllerConfiguration,
	kccConfig *kccconfig.KCCConfig,
	client *kcclient.GenericClientSet,
	katalystCustomConfigInformer configinformers.KatalystCustomConfigInformer,
	customNodeConfigInformer configinformers.CustomNodeConfigInformer,
	metricsEmitter metrics.MetricEmitter,
	targetHandler KatalystCustomConfigTargetHandlerInterface,
) (*KatalystCustomConfigTargetController, error) {
	k := &KatalystCustomConfigTargetController{
		ctx:                        ctx,
		client:                     client,
		dryRun:                     genericConf.DryRun,
		kccConfig:                  kccConfig,
		katalystCustomConfigLister: katalystCustomConfigInformer.Lister(),
		customNodeConfigLister:     customNodeConfigInformer.Lister(),
		targetHandler:              targetHandler,
		syncedFunc: []cache.InformerSynced{
			katalystCustomConfigInformer.Informer().HasSynced,
			customNodeConfigInformer.Informer().HasSynced,
			targetHandler.HasSynced,
		},
		queue:                          workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), kccTargetControllerName),
		rateLimiters:                   sync.Map{},
		kcctWorkerCount:                resolveInt(kccConfig.KCCTWorkerCount, defaultKCCTWorkerCount),
		cncWorkerCount:                 resolveInt(kccConfig.CNCWorkerCount, defaultCNCWorkerCount),
		cncEnqueueDelay:                resolveDuration(kccConfig.CNCEnqueueDelay, defaultCNCEnqueueDelay),
		kcctEnqueueDelay:               resolveDuration(kccConfig.KCCTEnqueueDelay, defaultKCCTEnqueueDelay),
		cncUpdateQPS:                   resolveInt(kccConfig.CNCUpdateQPS, defaultCNCUpdateQPS),
		cncUpdateBurst:                 resolveInt(kccConfig.CNCUpdateBurst, defaultCNCUpdateBurst),
		cncStatusFullReconcileInterval: resolveDuration(kccConfig.CNCStatusFullReconcileInterval, defaultCNCStatusFullReconcileInterval),
		enablePreciseCNCDispatch:       resolveBool(kccConfig.EnablePreciseCNCDispatch, true),
		enableCNCJSONPatch:             resolveBool(kccConfig.EnableCNCJSONPatch, true),
		enableIncrementalProgress:      resolveBool(kccConfig.EnableIncrementalProgress, true),
		progressCache:                  make(map[string]*kcctProgress),
		lastStatusEmit:                 make(map[string]time.Time),
		pendingRetrySet:                make(map[string]struct{}),
	}

	if metricsEmitter == nil {
		k.metricsEmitter = metrics.DummyMetrics{}
	} else {
		k.metricsEmitter = metricsEmitter.WithTags(kccTargetControllerName)
	}

	k.kccControl = control.DummyKCCControl{}
	k.unstructuredControl = control.DummyUnstructuredControl{}
	k.cncControl = control.DummyCNCControl{}
	if !k.dryRun {
		k.kccControl = control.NewRealKCCControl(client.InternalClient)
		k.unstructuredControl = control.NewRealUnstructuredControl(client.DynamicClient)
		k.cncControl = control.NewRealCNCControl(client.InternalClient)
	}

	customNodeConfigInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    k.handleCNCAdd,
		UpdateFunc: k.handleCNCUpdate,
		DeleteFunc: k.handleCNCDelete,
	})

	targetHandler.RegisterTargetHandler(kccTargetControllerName, k.handleTargetEvent)
	return k, nil
}

func (k *KatalystCustomConfigTargetController) Run() {
	defer utilruntime.HandleCrash()
	defer k.queue.ShutDown()
	defer klog.Infof("shutting down %s controller", kccTargetControllerName)

	if !cache.WaitForCacheSync(k.ctx.Done(), k.syncedFunc...) {
		utilruntime.HandleError(fmt.Errorf("unable to sync caches for %s controller", kccTargetControllerName))
		return
	}
	klog.Infof("caches are synced for %s controller", kccTargetControllerName)
	klog.Infof("start %d workers for %s controller", k.kcctWorkerCount, kccTargetControllerName)

	for i := 0; i < k.kcctWorkerCount; i++ {
		go wait.Until(k.worker, time.Second, k.ctx.Done())
	}
	go wait.Until(k.clearUnusedConfig, 5*time.Minute, k.ctx.Done())
	go wait.Until(k.emitQueueDepthMetric, 10*time.Second, k.ctx.Done())

	<-k.ctx.Done()
}

func (k *KatalystCustomConfigTargetController) handleCNCAdd(obj interface{}) {
	cnc, ok := obj.(*configapis.CustomNodeConfig)
	if !ok {
		general.Errorf("received invalid CNC type on add: %T", obj)
		return
	}
	if k.enablePreciseCNCDispatch {
		gvrs := configTypesOf(cnc)
		if len(gvrs) == 0 {
			k.handleCNCStatusUpdateAll("add_no_entries")
			return
		}
		k.enqueueGVRsWithStateBump(gvrs, "add", k.cncEnqueueDelay)
		return
	}
	k.handleCNCStatusUpdateAll("add_legacy")
}

func (k *KatalystCustomConfigTargetController) handleCNCUpdate(old, new interface{}) {
	oldCNC, ok := old.(*configapis.CustomNodeConfig)
	if !ok {
		general.Errorf("received invalid old CNC type: %T", old)
		return
	}
	newCNC, ok := new.(*configapis.CustomNodeConfig)
	if !ok {
		general.Errorf("received invalid new CNC type: %T", new)
		return
	}

	if apiequality.Semantic.DeepEqual(oldCNC, newCNC) {
		return
	}

	if !k.enablePreciseCNCDispatch {
		k.handleCNCStatusUpdateAll("update_legacy")
		return
	}

	if !apiequality.Semantic.DeepEqual(oldCNC.Labels, newCNC.Labels) ||
		!apiequality.Semantic.DeepEqual(oldCNC.Spec, newCNC.Spec) {
		affectedGVRs := k.getAffectedGVRsByCNCChange(oldCNC, newCNC)
		if len(affectedGVRs) > 0 {
			k.enqueueGVRsWithStateBump(affectedGVRs, "update_label_or_spec_optimized", k.cncEnqueueDelay)
		}
	}

	gvrs := diffTargetConfigGVRs(oldCNC.Status.KatalystCustomConfigList, newCNC.Status.KatalystCustomConfigList)
	if len(gvrs) == 0 {
		return
	}
	k.enqueueGVRs(gvrs, "update_status", k.cncEnqueueDelay)
}

func (k *KatalystCustomConfigTargetController) handleCNCDelete(obj interface{}) {
	cnc, ok := obj.(*configapis.CustomNodeConfig)
	if !ok {
		k.handleCNCStatusUpdateAll("delete_tombstone")
		return
	}
	if !k.enablePreciseCNCDispatch {
		k.handleCNCStatusUpdateAll("delete_legacy")
		return
	}
	gvrs := configTypesOf(cnc)
	if len(gvrs) == 0 {
		return
	}
	k.enqueueGVRsWithStateBump(gvrs, "delete", k.cncEnqueueDelay)
}

func (k *KatalystCustomConfigTargetController) handleCNCStatusUpdateAll(reason string) {
	gvrs := make([]metav1.GroupVersionResource, 0)
	k.targetHandler.RangeGVRTargetAccessor(func(gvr metav1.GroupVersionResource, _ kcctarget.KatalystCustomConfigTargetAccessor) bool {
		gvrs = append(gvrs, gvr)
		return true
	})
	count := k.enqueueUniqueGVRs(gvrs, k.cncEnqueueDelay, true)
	_ = k.metricsEmitter.StoreInt64(metricKCCTEventDispatch, int64(count), metrics.MetricTypeNameCount,
		metrics.MetricTag{Key: "reason", Val: reason})
}

func (k *KatalystCustomConfigTargetController) getAffectedGVRsByCNCChange(oldCNC, newCNC *configapis.CustomNodeConfig) []metav1.GroupVersionResource {
	var affected []metav1.GroupVersionResource
	k.targetHandler.RangeGVRTargetAccessor(func(gvr metav1.GroupVersionResource, accessor kcctarget.KatalystCustomConfigTargetAccessor) bool {
		targets, err := accessor.List(labels.Everything())
		if err != nil {
			affected = append(affected, gvr)
			return true
		}

		targetResources := make([]util.KCCTargetResource, 0, len(targets))
		for _, t := range targets {
			tr, err := k.targetHandler.GetKCCTargetResource(gvr, t)
			if err != nil {
				continue
			}
			targetResources = append(targetResources, tr)
		}

		oldSnap := kccutil.GetCNCMatchSnapshot(oldCNC, targetResources)
		newSnap := kccutil.GetCNCMatchSnapshot(newCNC, targetResources)

		if !reflect.DeepEqual(oldSnap, newSnap) {
			affected = append(affected, gvr)
		} else if newSnap.IsPartialCanary() {
			affected = append(affected, gvr)
		}
		return true
	})
	return affected
}

func (k *KatalystCustomConfigTargetController) enqueueGVRs(gvrs []metav1.GroupVersionResource, reason string, delay time.Duration) {
	enqueued := k.enqueueUniqueGVRs(gvrs, delay, false)
	if enqueued > 0 {
		_ = k.metricsEmitter.StoreInt64(metricKCCTEventDispatch, int64(enqueued), metrics.MetricTypeNameCount,
			metrics.MetricTag{Key: "reason", Val: reason})
	}
}

func (k *KatalystCustomConfigTargetController) enqueueGVRsWithStateBump(gvrs []metav1.GroupVersionResource, reason string, delay time.Duration) {
	enqueued := k.enqueueUniqueGVRs(gvrs, delay, true)
	if enqueued > 0 {
		_ = k.metricsEmitter.StoreInt64(metricKCCTEventDispatch, int64(enqueued), metrics.MetricTypeNameCount,
			metrics.MetricTag{Key: "reason", Val: reason})
	}
}

func configTypesOf(cnc *configapis.CustomNodeConfig) []metav1.GroupVersionResource {
	if cnc == nil || len(cnc.Status.KatalystCustomConfigList) == 0 {
		return nil
	}
	gvrs := make([]metav1.GroupVersionResource, 0, len(cnc.Status.KatalystCustomConfigList))
	for _, entry := range cnc.Status.KatalystCustomConfigList {
		gvrs = append(gvrs, entry.ConfigType)
	}
	return gvrs
}

func diffTargetConfigGVRs(oldList, newList []configapis.TargetConfig) []metav1.GroupVersionResource {
	type key struct{ gvr metav1.GroupVersionResource }
	oldMap := make(map[key]configapis.TargetConfig, len(oldList))
	for _, e := range oldList {
		oldMap[key{e.ConfigType}] = e
	}
	seen := make(map[key]struct{}, len(newList))
	var diffs []metav1.GroupVersionResource

	for _, e := range newList {
		seen[key{e.ConfigType}] = struct{}{}
		old, ok := oldMap[key{e.ConfigType}]
		if !ok || old.ConfigName != e.ConfigName || old.ConfigNamespace != e.ConfigNamespace || old.Hash != e.Hash {
			diffs = append(diffs, e.ConfigType)
		}
	}
	for _, e := range oldList {
		if _, ok := seen[key{e.ConfigType}]; !ok {
			diffs = append(diffs, e.ConfigType)
		}
	}
	return diffs
}

func (k *KatalystCustomConfigTargetController) handleCNCStatusUpdate() {
	k.handleCNCStatusUpdateAll("legacy_call")
}

func (k *KatalystCustomConfigTargetController) handleTargetEvent(gvr metav1.GroupVersionResource, _ *unstructured.Unstructured) error {
	k.queue.AddAfter(gvr, k.kcctEnqueueDelay)
	return nil
}

func (k *KatalystCustomConfigTargetController) worker() {
	for k.processNextWorkItem() {
	}
}

func (k *KatalystCustomConfigTargetController) processNextWorkItem() bool {
	key, quit := k.queue.Get()
	if quit {
		return false
	}
	defer k.queue.Done(key)

	gvr, ok := key.(metav1.GroupVersionResource)
	if !ok {
		k.queue.Forget(key)
		general.Errorf("received invalid key type: %T", key)
		return true
	}

	err := k.syncKCCTs(gvr)
	if err == nil {
		k.queue.Forget(key)
		return true
	}
	general.Errorf("sync kcct gvr %s failed with %v", gvr, err)
	k.queue.AddRateLimited(key)
	return true
}

func (k *KatalystCustomConfigTargetController) syncKCCTs(gvr metav1.GroupVersionResource) error {
	reconcileStartTime := time.Now()
	general.InfofV(4, "reconcile KCCT GVR %s", gvr.String())
	defer func() {
		duration := time.Since(reconcileStartTime)
		_ = k.metricsEmitter.StoreInt64(metricKCCTReconcileDuration, duration.Milliseconds(), metrics.MetricTypeNameRaw,
			metrics.MetricTag{Key: "gvr", Val: gvr.String()})
		general.InfofV(4, "reconcile KCCT GVR %s finished in %v", gvr.String(), duration)
	}()

	accessor, ok := k.targetHandler.GetTargetAccessorByGVR(gvr)
	if !ok {
		k.clearProgressForGVR(gvr)
		return nil
	}
	list, err := accessor.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("list kcc targets failed: %w", err)
	}

	var errors []error
	targetResources := make([]util.KCCTargetResource, 0, len(list))
	for _, obj := range list {
		if obj.GetDeletionTimestamp() != nil {
			if err := k.handleKCCTargetFinalizers(gvr, obj); err != nil {
				errors = append(errors, fmt.Errorf("handle kcc target finalizer failed: %w", err))
			}
			continue
		}

		targetResource := util.ToKCCTargetResource(obj.DeepCopy())
		if validityPeriod := targetResource.GetLastDuration(); validityPeriod != nil {
			expiry := targetResource.GetCreationTimestamp().Add(*validityPeriod)
			untilExpiry := time.Until(expiry)
			if untilExpiry <= 0 {
				general.Infof("delete expired kcc target %s %s", gvr.String(), native.GenerateUniqObjectNameKey(targetResource))
				err := k.unstructuredControl.DeleteUnstructured(k.ctx, gvr, obj, metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					errors = append(errors, fmt.Errorf("delete expired kcc target failed: %w", err))
				}
				continue
			}
			k.queue.AddAfter(gvr, untilExpiry)
		}
		targetResources = append(targetResources, targetResource)
	}

	if len(targetResources) == 0 {
		k.clearProgressForGVR(gvr)
		return utilerrors.NewAggregate(errors)
	}
	invalidKCCTs, errs := k.validateKCCTs(gvr, targetResources)
	errors = append(errors, errs...)
	if len(errors) > 0 {
		return utilerrors.NewAggregate(errors)
	}
	if len(invalidKCCTs) > 0 {
		general.Infof("skip manage CNCs for KCCT GVR %s due to presence of invalid KCCTs %v", gvr.String(), invalidKCCTs)
		return nil
	}
	return k.manageCNCs(gvr, targetResources)
}

func (k *KatalystCustomConfigTargetController) validateKCCTs(gvr metav1.GroupVersionResource, targetResources []util.KCCTargetResource) (invalidKCCTs []string, errors []error) {
	if !targetResources[0].NeedValidateKCC() {
		return nil, nil
	}
	kccKeys := k.targetHandler.GetKCCKeyListByGVR(gvr)
	if len(kccKeys) != 1 {
		message := fmt.Sprintf("more or less than one kcc %v match same gvr %s", kccKeys, gvr.String())
		for _, targetResource := range targetResources {
			newTargetResource := targetResource.DeepCopy()
			updateInvalidTargetResourceStatus(newTargetResource, message, kccTargetConditionReasonMatchMoreOrLessThanOneKCC)
			if !apiequality.Semantic.DeepEqual(newTargetResource, targetResource) {
				general.Infof("gvr: %s, target: %s need update status due to more or less than one kcc keys %v", gvr.String(), native.GenerateUniqObjectNameKey(targetResource), kccKeys)
				_, err := k.unstructuredControl.UpdateUnstructuredStatus(k.ctx, gvr, newTargetResource.GetUnstructured(), metav1.UpdateOptions{})
				if err != nil {
					errors = append(errors, fmt.Errorf("update kcc target status failed: %w", err))
				}
			}
		}
		return invalidKCCTs, errors
	}

	kccKey := kccKeys[0]
	namespace, name, err := cache.SplitMetaNamespaceKey(kccKey)
	if err != nil {
		errors = append(errors, fmt.Errorf("failed to split namespace and name from kcc key %s: %w", kccKey, err))
		return invalidKCCTs, errors
	}
	kcc, err := k.katalystCustomConfigLister.KatalystCustomConfigs(namespace).Get(name)
	if apierrors.IsNotFound(err) {
		errors = append(errors, fmt.Errorf("kcc %s is not found", kccKey))
		return invalidKCCTs, errors
	} else if err != nil {
		errors = append(errors, fmt.Errorf("get kcc %s failed: %w", kccKey, err))
		return invalidKCCTs, errors
	}

	for _, targetResource := range targetResources {
		isValid, message, err := k.validateTargetResourceGenericSpec(kcc, targetResource, targetResources)
		if err != nil {
			errors = append(errors, fmt.Errorf("validate kcc target resource failed: %w", err))
			invalidKCCTs = append(invalidKCCTs, native.GenerateUniqObjectNameKey(targetResource))
		}
		if isValid {
			continue
		}
		invalidKCCTs = append(invalidKCCTs, native.GenerateUniqObjectNameKey(targetResource))
		newTargetResource := targetResource.DeepCopy()
		updateInvalidTargetResourceStatus(newTargetResource, message, kccTargetConditionReasonValidateFailed)
		if !apiequality.Semantic.DeepEqual(newTargetResource, targetResource) {
			general.Infof("gvr: %s, target: %s need update status due to failed validation: %s", gvr.String(), native.GenerateUniqObjectNameKey(targetResource), message)
			_, err := k.unstructuredControl.UpdateUnstructuredStatus(k.ctx, gvr, newTargetResource.GetUnstructured(), metav1.UpdateOptions{})
			if err != nil {
				errors = append(errors, fmt.Errorf("update kcc target %s %s status failed: %w", gvr.String(), native.GenerateUniqObjectNameKey(targetResource), err))
			}
		}
	}
	return invalidKCCTs, errors
}

func (k *KatalystCustomConfigTargetController) manageCNCs(gvr metav1.GroupVersionResource, targetResources []util.KCCTargetResource) error {
	allCNCs, err := k.customNodeConfigLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("list CNCs failed: %w", err)
	}
	sort.Slice(allCNCs, func(i, j int) bool { return allCNCs[i].GetName() < allCNCs[j].GetName() })

	var errors []error
	targetResources, errs := k.validateLabelSelectorAndMaybeUpdateStatus(gvr, targetResources)
	if len(errs) > 0 {
		errors = append(errors, errs...)
	}
	if len(targetResources) == 0 {
		return utilerrors.NewAggregate(errors)
	}

	targetCNCIndexes, errs := k.groupCNCsByKCCTWithCache(gvr, allCNCs, targetResources)
	if len(errs) > 0 {
		errors = append(errors, errs...)
	}
	targetResources, hashes, errs := k.generateConfigHashesAndMaybeUpdateStatus(gvr, targetResources)
	if len(errs) > 0 {
		errors = append(errors, errs...)
	}
	targetResources, canaryCutoffPoints, errs := k.computeCanaryCutoffPointsAndMaybeUpdateStatus(gvr, targetResources, targetCNCIndexes)
	if len(errs) > 0 {
		errors = append(errors, errs...)
	}
	rateLimited, errs := k.updateCNCs(gvr, targetResources, hashes, canaryCutoffPoints, targetCNCIndexes, allCNCs)
	if len(errs) > 0 {
		errors = append(errors, errs...)
	}
	if rateLimited {
		k.queue.AddAfter(gvr, time.Duration(k.cncUpdateBurst/k.cncUpdateQPS/2)*time.Second)
	}
	errs = k.updateTargetStatuses(gvr, targetResources, hashes, canaryCutoffPoints, targetCNCIndexes, allCNCs)
	if len(errs) > 0 {
		errors = append(errors, errs...)
	}
	return utilerrors.NewAggregate(errors)
}

func (k *KatalystCustomConfigTargetController) groupCNCsByKCCT(allCNCs []*configapis.CustomNodeConfig, targetResources []util.KCCTargetResource) (map[string][]int, []error) {
	var errors []error
	targetCNCIndexes := make(map[string][]int)
	for i, cnc := range allCNCs {
		if cnc.GetDeletionTimestamp() != nil {
			continue
		}
		if targetResources[0].IsPerNode() {
			kcctName := native.GenerateUniqObjectNameKey(cnc)
			targetCNCIndexes[kcctName] = append(targetCNCIndexes[kcctName], i)
			continue
		}
		matchedTarget, err := kccutil.FindMatchedKCCTargetConfigForNode(cnc, targetResources)
		if err != nil {
			errors = append(errors, fmt.Errorf("find matched target for CNC %s failed: %w", cnc.GetName(), err))
			continue
		}
		kcctName := native.GenerateUniqObjectNameKey(matchedTarget)
		targetCNCIndexes[kcctName] = append(targetCNCIndexes[kcctName], i)
	}
	return targetCNCIndexes, errors
}

func (k *KatalystCustomConfigTargetController) groupCNCsByKCCTWithCache(
	gvr metav1.GroupVersionResource,
	allCNCs []*configapis.CustomNodeConfig,
	targetResources []util.KCCTargetResource,
) (map[string][]int, []error) {
	combinedRV := combinedKCCTRV(targetResources)
	cncStateVersion := k.getGVRStateVersion(gvr)
	if cached, ok := k.getCachedGroupCNCIndexes(gvr, combinedRV, cncStateVersion); ok {
		return cached, nil
	}

	targetCNCIndexes, errs := k.groupCNCsByKCCT(allCNCs, targetResources)
	if len(errs) == 0 {
		k.setCachedGroupCNCIndexes(gvr, combinedRV, cncStateVersion, targetCNCIndexes)
	}
	return targetCNCIndexes, errs
}

func (k *KatalystCustomConfigTargetController) enqueueUniqueGVRs(gvrs []metav1.GroupVersionResource, delay time.Duration, bumpState bool) int {
	seen := make(map[metav1.GroupVersionResource]struct{}, len(gvrs))
	enqueued := 0
	for _, gvr := range gvrs {
		if _, ok := seen[gvr]; ok {
			continue
		}
		seen[gvr] = struct{}{}
		if _, ok := k.targetHandler.GetTargetAccessorByGVR(gvr); !ok {
			continue
		}
		if bumpState {
			k.bumpGVRStateVersion(gvr)
		}
		k.queue.AddAfter(gvr, delay)
		enqueued++
	}
	return enqueued
}

func (k *KatalystCustomConfigTargetController) bumpGVRStateVersion(gvr metav1.GroupVersionResource) {
	raw, _ := k.gvrCNCStateVersions.LoadOrStore(gvr, new(int64))
	atomic.AddInt64(raw.(*int64), 1)
	k.groupCache.Delete(gvr)
}

func (k *KatalystCustomConfigTargetController) getGVRStateVersion(gvr metav1.GroupVersionResource) int64 {
	raw, ok := k.gvrCNCStateVersions.Load(gvr)
	if !ok {
		return 0
	}
	return atomic.LoadInt64(raw.(*int64))
}

func (k *KatalystCustomConfigTargetController) getCachedGroupCNCIndexes(
	gvr metav1.GroupVersionResource,
	combinedRV string,
	cncStateVersion int64,
) (map[string][]int, bool) {
	raw, ok := k.groupCache.Load(gvr)
	if !ok {
		return nil, false
	}
	entry := raw.(*kcctGroupCacheEntry)
	if entry.kcctCombinedRV != combinedRV || entry.cncStateVersion != cncStateVersion {
		return nil, false
	}
	return cloneTargetCNCIndexes(entry.targetCNCIndexes), true
}

func (k *KatalystCustomConfigTargetController) setCachedGroupCNCIndexes(
	gvr metav1.GroupVersionResource,
	combinedRV string,
	cncStateVersion int64,
	targetCNCIndexes map[string][]int,
) {
	k.groupCache.Store(gvr, &kcctGroupCacheEntry{
		kcctCombinedRV:   combinedRV,
		cncStateVersion:  cncStateVersion,
		targetCNCIndexes: cloneTargetCNCIndexes(targetCNCIndexes),
	})
}

func combinedKCCTRV(targetResources []util.KCCTargetResource) string {
	parts := make([]string, 0, len(targetResources))
	for _, targetResource := range targetResources {
		parts = append(parts, fmt.Sprintf(
			"%s=%s/%d",
			native.GenerateUniqObjectNameKey(targetResource),
			targetResource.GetResourceVersion(),
			targetResource.GetGeneration(),
		))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func cloneTargetCNCIndexes(targetCNCIndexes map[string][]int) map[string][]int {
	cloned := make(map[string][]int, len(targetCNCIndexes))
	for key, indexes := range targetCNCIndexes {
		cloned[key] = append([]int(nil), indexes...)
	}
	return cloned
}

func (k *KatalystCustomConfigTargetController) validateLabelSelectorAndMaybeUpdateStatus(gvr metav1.GroupVersionResource, targetResources []util.KCCTargetResource) ([]util.KCCTargetResource, []error) {
	validTargetResources := make([]util.KCCTargetResource, 0, len(targetResources))
	var errors []error
	for _, targetResource := range targetResources {
		kcctName := native.GenerateUniqObjectNameKey(targetResource)
		labelSelector := targetResource.GetLabelSelector()
		if labelSelector != "" {
			_, err := labels.Parse(labelSelector)
			if err != nil {
				message := fmt.Sprintf("failed to parse label selector: %v", err)
				newTargetResource := targetResource.DeepCopy()
				updateInvalidTargetResourceStatus(newTargetResource, message, kccTargetConditionReasonValidateFailed)
				if !apiequality.Semantic.DeepEqual(newTargetResource, targetResource) {
					general.Infof("gvr: %s, target: %s need update status due to label selector parse failure: %v", gvr.String(), kcctName, err)
					_, err := k.unstructuredControl.UpdateUnstructuredStatus(k.ctx, gvr, newTargetResource.GetUnstructured(), metav1.UpdateOptions{})
					if err != nil {
						errors = append(errors, fmt.Errorf("update kcc target %s %s status failed: %w", gvr.String(), kcctName, err))
					}
				}
				continue
			}
		}
		validTargetResources = append(validTargetResources, targetResource)
	}
	return validTargetResources, errors
}

func (k *KatalystCustomConfigTargetController) generateConfigHashesAndMaybeUpdateStatus(gvr metav1.GroupVersionResource, targetResources []util.KCCTargetResource) ([]util.KCCTargetResource, map[string]string, []error) {
	validTargetResources := make([]util.KCCTargetResource, 0, len(targetResources))
	hashes := make(map[string]string, len(targetResources))
	var errors []error
	for _, targetResource := range targetResources {
		kcctName := native.GenerateUniqObjectNameKey(targetResource)
		hash, err := targetResource.GenerateConfigHash()
		if err != nil {
			message := fmt.Sprintf("failed to generate hash: %v", err)
			newTargetResource := targetResource.DeepCopy()
			updateInvalidTargetResourceStatus(newTargetResource, message, kccTargetConditionReasonHashFailed)
			if !apiequality.Semantic.DeepEqual(newTargetResource, targetResource) {
				general.Infof("gvr: %s, target: %s need update status due to hash generation: %v", gvr.String(), kcctName, err)
				_, err := k.unstructuredControl.UpdateUnstructuredStatus(k.ctx, gvr, newTargetResource.GetUnstructured(), metav1.UpdateOptions{})
				if err != nil {
					errors = append(errors, fmt.Errorf("update kcc target %s %s status failed: %w", gvr.String(), kcctName, err))
				}
			}
			continue
		}
		hashes[kcctName] = hash
		validTargetResources = append(validTargetResources, targetResource)
	}
	return validTargetResources, hashes, errors
}

func (k *KatalystCustomConfigTargetController) computeCanaryCutoffPointsAndMaybeUpdateStatus(gvr metav1.GroupVersionResource, targetResources []util.KCCTargetResource, targetCNCIndexes map[string][]int) ([]util.KCCTargetResource, map[string]int, []error) {
	validTargetResources := make([]util.KCCTargetResource, 0, len(targetResources))
	canaryCutoffPoints := make(map[string]int, len(targetResources))
	var errors []error
	for _, targetResource := range targetResources {
		kcctName := native.GenerateUniqObjectNameKey(targetResource)
		numTargetCNCs := len(targetCNCIndexes[kcctName])
		canaryConfig := targetResource.GetCanary()
		if canaryConfig == nil {
			canaryCutoffPoints[kcctName] = numTargetCNCs
		} else {
			cutoffPoint, err := intstr.GetScaledValueFromIntOrPercent(canaryConfig, numTargetCNCs, false)
			if err != nil {
				newTargetResource := targetResource.DeepCopy()
				updateInvalidTargetResourceStatus(newTargetResource, fmt.Sprintf("failed to get canary cutoff point: %s", err), kccTargetConditionReasonCalculateCanaryCutoffFailed)
				if !apiequality.Semantic.DeepEqual(newTargetResource, targetResource) {
					general.Infof("gvr: %s, target: %s need update status due to canary cutoff calculation: %v", gvr.String(), kcctName, err)
					_, err := k.unstructuredControl.UpdateUnstructuredStatus(k.ctx, gvr, newTargetResource.GetUnstructured(), metav1.UpdateOptions{})
					if err != nil {
						errors = append(errors, fmt.Errorf("update kcc target %s %s status failed: %w", gvr.String(), kcctName, err))
					}
				}
				continue
			}
			if cutoffPoint < 0 {
				cutoffPoint = 0
			} else if cutoffPoint > numTargetCNCs {
				cutoffPoint = numTargetCNCs
			}
			canaryCutoffPoints[kcctName] = cutoffPoint
		}
		general.Infof("kcct %s %s targetCNCs=%d canaryCutoff=%d", gvr.String(), kcctName, numTargetCNCs, canaryCutoffPoints[kcctName])
		validTargetResources = append(validTargetResources, targetResource)
	}
	return validTargetResources, canaryCutoffPoints, errors
}

func (k *KatalystCustomConfigTargetController) updateCNCs(gvr metav1.GroupVersionResource, targetResources []util.KCCTargetResource, hashes map[string]string, canaryCutoffPoints map[string]int, targetCNCIndexes map[string][]int, allCNCs []*configapis.CustomNodeConfig) (bool, []error) {
	var errors []error
	rateLimiterRaw, _ := k.rateLimiters.LoadOrStore(gvr, rate.NewLimiter(rate.Limit(k.cncUpdateQPS), k.cncUpdateBurst))
	rateLimiter := rateLimiterRaw.(*rate.Limiter)
	rateLimited := false
	type updateTask struct {
		targetResource util.KCCTargetResource
		cncIndex       int
		kcctName       string
	}
	updateTasks := []updateTask{}

kcctLoop:
	for _, targetResource := range targetResources {
		if targetResource.GetPaused() {
			continue
		}
		kcctName := native.GenerateUniqObjectNameKey(targetResource)
		cutoffPoint := canaryCutoffPoints[kcctName]
		for _, cncIndex := range targetCNCIndexes[kcctName][:cutoffPoint] {
			if !kccutil.IsCNCUpdated(allCNCs[cncIndex], gvr, targetResource, hashes[kcctName]) {
				if !rateLimiter.Allow() {
					rateLimited = true
					break kcctLoop
				}
				updateTasks = append(updateTasks, updateTask{targetResource: targetResource, cncIndex: cncIndex, kcctName: kcctName})
			}
		}
	}

	general.Infof("updating %d CNCs for GVR %s", len(updateTasks), gvr.String())
	var mu sync.Mutex
	failedCount := 0
	workqueue.ParallelizeUntil(k.ctx, k.cncWorkerCount, len(updateTasks), func(i int) {
		task := updateTasks[i]
		oldCNC := allCNCs[task.cncIndex]
		newCNC := oldCNC.DeepCopy()
		kccutil.ApplyKCCTargetConfigToCNC(newCNC, gvr, task.targetResource, hashes[task.kcctName])
		var updatedCNC *configapis.CustomNodeConfig
		var patchErr error
		patchMode := "merge"
		if k.enableCNCJSONPatch {
			patchMode = "json"
			updatedCNC, patchErr = k.cncControl.PatchCNCTargetConfig(k.ctx, oldCNC.GetName(), oldCNC, newCNC)
		} else {
			updatedCNC, patchErr = k.cncControl.PatchCNCStatus(k.ctx, oldCNC.GetName(), oldCNC, newCNC)
		}
		mu.Lock()
		defer mu.Unlock()
		if patchErr != nil {
			errors = append(errors, fmt.Errorf("update CNC %s status failed: %w", oldCNC.GetName(), patchErr))
			failedCount++
			_ = k.metricsEmitter.StoreInt64(metricKCCTCNCPatchTotal, 1, metrics.MetricTypeNameCount,
				metrics.MetricTag{Key: "result", Val: "failed"},
				metrics.MetricTag{Key: "gvr", Val: gvr.String()},
				metrics.MetricTag{Key: "mode", Val: patchMode})
			return
		}
		allCNCs[task.cncIndex] = updatedCNC
		_ = k.metricsEmitter.StoreInt64(metricKCCTCNCPatchTotal, 1, metrics.MetricTypeNameCount,
			metrics.MetricTag{Key: "result", Val: "success"},
			metrics.MetricTag{Key: "gvr", Val: gvr.String()},
			metrics.MetricTag{Key: "mode", Val: patchMode})
	})
	general.Infof("updated %d CNCs for GVR %s, %d failed", len(updateTasks)-failedCount, gvr.String(), failedCount)
	return rateLimited, errors
}

func (k *KatalystCustomConfigTargetController) updateTargetStatuses(gvr metav1.GroupVersionResource, targetResources []util.KCCTargetResource, hashes map[string]string, canaryCutoffPoints map[string]int, targetCNCIndexes map[string][]int, allCNCs []*configapis.CustomNodeConfig) []error {
	var errors []error
	for _, targetResource := range targetResources {
		kcctName := native.GenerateUniqObjectNameKey(targetResource)
		cutoffPoint := canaryCutoffPoints[kcctName]
		targets := targetCNCIndexes[kcctName]
		hash := hashes[kcctName]
		targetNodes := int32(len(targets))
		canaryNodes := int32(cutoffPoint)
		var updatedTargetNodes, updatedNodes int32
		var forceEmit bool

		k.progressMu.Lock()
		pkey := progressKey(gvr, kcctName)
		progress := k.getOrInitProgressLocked(pkey)
		hashChanged := k.observeHashLocked(progress, hash)
		needFull := k.shouldFullReconcileLocked(progress) || hashChanged
		if needFull {
			updatedTargetNodes, updatedNodes = k.computeUpdatedNodesLocked(gvr, targetResource, hash, targets, allCNCs)
			progress.targetNodes = targetNodes
			progress.canaryNodes = canaryNodes
			progress.updatedTargetNodes = updatedTargetNodes
			progress.updatedNodes = updatedNodes
			progress.lastFullReconcileAt = time.Now()
			forceEmit = hashChanged
		} else {
			progress.targetNodes = targetNodes
			progress.canaryNodes = canaryNodes
			updatedTargetNodes = k.computeUpdatedTargetNodes(gvr, targetResource, hash, targets, allCNCs)
			updatedNodes = k.computeUpdatedNodes(gvr, targetResource, hash, allCNCs)
			progress.updatedTargetNodes = updatedTargetNodes
			progress.updatedNodes = updatedNodes
		}

		if !progress.rolloutDone && targetNodes > 0 && updatedTargetNodes >= targetNodes {
			progress.rolloutDone = true
			elapsed := time.Since(progress.rolloutStartedAt).Milliseconds()
			_ = k.metricsEmitter.StoreInt64(metricKCCTRolloutDuration, elapsed, metrics.MetricTypeNameRaw, metrics.MetricTag{Key: "gvr", Val: gvr.String()}, metrics.MetricTag{Key: "kcct", Val: kcctName})
			forceEmit = true
		}

		shouldEmit, shouldRetry := k.shouldEmitStatusLocked(pkey, forceEmit)
		if shouldEmit || forceEmit {
			delete(k.pendingRetrySet, pkey)
		}
		scheduleRetry := false
		if !shouldEmit && shouldRetry {
			if _, ok := k.pendingRetrySet[pkey]; !ok {
				k.pendingRetrySet[pkey] = struct{}{}
				scheduleRetry = true
			}
		}
		k.progressMu.Unlock()
		if !shouldEmit {
			if scheduleRetry {
				k.queue.AddAfter(gvr, kcctStatusMinEmitInterval)
			}
			continue
		}

		newTargetResource := targetResource.DeepCopy()
		updateValidTargetResourceStatus(newTargetResource, targetNodes, canaryNodes, updatedTargetNodes, updatedNodes, hash)
		if !apiequality.Semantic.DeepEqual(newTargetResource, targetResource) {
			general.Infof("kcct %s %s update status targetNodes=%d canaryNodes=%d updatedTargetNodes=%d updatedNodes=%d hash=%s", gvr.String(), kcctName, targetNodes, canaryNodes, updatedTargetNodes, updatedNodes, hash)
			_, err := k.unstructuredControl.UpdateUnstructuredStatus(k.ctx, gvr, newTargetResource.GetUnstructured(), metav1.UpdateOptions{})
			if err != nil {
				errors = append(errors, fmt.Errorf("update kcc target %s %s status failed: %w", gvr.String(), kcctName, err))
			}
		}
	}
	return errors
}

func (k *KatalystCustomConfigTargetController) computeUpdatedNodesLocked(gvr metav1.GroupVersionResource, targetResource util.KCCTargetResource, hash string, targets []int, allCNCs []*configapis.CustomNodeConfig) (int32, int32) {
	return k.computeUpdatedTargetNodes(gvr, targetResource, hash, targets, allCNCs), k.computeUpdatedNodes(gvr, targetResource, hash, allCNCs)
}

func (k *KatalystCustomConfigTargetController) computeUpdatedTargetNodes(gvr metav1.GroupVersionResource, targetResource util.KCCTargetResource, hash string, targets []int, allCNCs []*configapis.CustomNodeConfig) int32 {
	var updatedTargetNodes int32
	for _, idx := range targets {
		if idx < 0 || idx >= len(allCNCs) {
			continue
		}
		if kccutil.IsCNCUpdated(allCNCs[idx], gvr, targetResource, hash) {
			updatedTargetNodes++
		}
	}
	return updatedTargetNodes
}

func clampInt32(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (k *KatalystCustomConfigTargetController) computeUpdatedNodes(gvr metav1.GroupVersionResource, targetResource util.KCCTargetResource, hash string, allCNCs []*configapis.CustomNodeConfig) int32 {
	var updatedNodes int32
	for _, cnc := range allCNCs {
		if kccutil.IsCNCUpdated(cnc, gvr, targetResource, hash) {
			updatedNodes++
		}
	}
	return updatedNodes
}

func (k *KatalystCustomConfigTargetController) handleKCCTargetFinalizers(gvr metav1.GroupVersionResource, target *unstructured.Unstructured) error {
	k.clearProgressForTarget(gvr, native.GenerateUniqObjectNameKey(target))
	if !controllerutil.ContainsFinalizer(target, consts.KatalystCustomConfigTargetFinalizerKCCT) && !controllerutil.ContainsFinalizer(target, consts.KatalystCustomConfigTargetFinalizerCNC) {
		return nil
	}
	general.Infof("removing gvr %s kcc target %s finalizer", gvr.String(), native.GenerateUniqObjectNameKey(target))
	err := kccutil.RemoveKCCTargetFinalizers(k.ctx, k.unstructuredControl, gvr, target, consts.KatalystCustomConfigTargetFinalizerKCCT, consts.KatalystCustomConfigTargetFinalizerCNC)
	if err != nil {
		return err
	}
	general.Infof("successfully removed gvr %s kcc target %s finalizer", gvr.String(), native.GenerateUniqObjectNameKey(target))
	return nil
}

func (k *KatalystCustomConfigTargetController) validateTargetResourceGenericSpec(kcc *configapis.KatalystCustomConfig, targetResource util.KCCTargetResource, allTargetResoures []util.KCCTargetResource) (bool, string, error) {
	labelSelector := targetResource.GetLabelSelector()
	nodeNames := targetResource.GetNodeNames()
	if len(labelSelector) != 0 && len(nodeNames) != 0 {
		return false, "both labelSelector and nodeNames has been set", nil
	} else if len(labelSelector) != 0 {
		return k.validateTargetResourceLabelSelector(kcc, targetResource, allTargetResoures)
	} else if len(nodeNames) != 0 {
		return k.validateTargetResourceNodeNames(kcc, targetResource, allTargetResoures)
	}
	return k.validateTargetResourceGlobal(kcc, targetResource, allTargetResoures)
}

func (k *KatalystCustomConfigTargetController) validateTargetResourceLabelSelector(kcc *configapis.KatalystCustomConfig, targetResource util.KCCTargetResource, allTargetResources []util.KCCTargetResource) (bool, string, error) {
	priorityAllowedKeyListMap := getPriorityAllowedKeyListMap(kcc)
	if len(priorityAllowedKeyListMap) == 0 {
		return false, fmt.Sprintf("kcc %s no support label selector", native.GenerateUniqObjectNameKey(kcc)), nil
	}
	valid, msg, err := validateLabelSelectorMatchWithKCCDefinition(priorityAllowedKeyListMap, targetResource)
	if err != nil {
		return false, "", nil
	}
	if !valid {
		return false, msg, nil
	}
	return validateLabelSelectorOverlapped(priorityAllowedKeyListMap, targetResource, allTargetResources)
}

func getPriorityAllowedKeyListMap(kcc *configapis.KatalystCustomConfig) map[int32]sets.String {
	priorityAllowedKeyListMap := make(map[int32]sets.String)
	for _, allowedKey := range kcc.Spec.NodeLabelSelectorAllowedKeyList {
		priorityAllowedKeyListMap[allowedKey.Priority] = sets.NewString(allowedKey.KeyList...)
	}
	return priorityAllowedKeyListMap
}

func validateLabelSelectorMatchWithKCCDefinition(priorityAllowedKeyListMap map[int32]sets.String, targetResource util.KCCTargetResource) (bool, string, error) {
	if targetResource.GetLastDuration() != nil {
		return false, "both labelSelector and lastDuration has been set", nil
	}
	selector, err := labels.Parse(targetResource.GetLabelSelector())
	if err != nil {
		return false, fmt.Sprintf("labelSelector parse failed: %s", err), nil
	}
	priority := targetResource.GetPriority()
	allowedKeyList, ok := priorityAllowedKeyListMap[priority]
	if !ok {
		return false, fmt.Sprintf("priority %d not supported", priority), nil
	}
	reqs, selectable := selector.Requirements()
	if !selectable {
		return false, fmt.Sprintf("labelSelector cannot selectable"), nil
	}
	inValidLabelKeys := sets.String{}
	for _, r := range reqs {
		key := r.Key()
		if !allowedKeyList.Has(key) {
			inValidLabelKeys.Insert(key)
		}
	}
	if len(inValidLabelKeys) > 0 {
		return false, fmt.Sprintf("labelSelector with invalid key %v (%s)", inValidLabelKeys.List(), allowedKeyList.List()), nil
	}
	return true, "", nil
}

func validateLabelSelectorOverlapped(priorityAllowedKeyListMap map[int32]sets.String, targetResource util.KCCTargetResource, otherResources []util.KCCTargetResource) (bool, string, error) {
	selector, err := labels.Parse(targetResource.GetLabelSelector())
	if err != nil {
		return false, fmt.Sprintf("labelSelector parse failed: %s", err), nil
	}
	priority := targetResource.GetPriority()
	allowedKeyList, ok := priorityAllowedKeyListMap[priority]
	if !ok {
		return false, fmt.Sprintf("priority %d not supported", priority), nil
	}
	overlapResources := sets.String{}
	for _, res := range otherResources {
		if (res.GetNamespace() == targetResource.GetNamespace() && res.GetName() == targetResource.GetName()) || len(res.GetLabelSelector()) == 0 {
			continue
		}
		otherSelector, err := labels.Parse(res.GetLabelSelector())
		if err != nil {
			continue
		}
		if res.GetPriority() != priority {
			continue
		}
		if checkLabelSelectorOverlap(selector, otherSelector, allowedKeyList.List()) {
			overlapResources.Insert(native.GenerateUniqObjectNameKey(res))
		}
	}
	if len(overlapResources) > 0 {
		return false, fmt.Sprintf("labelSelector overlay with others: %v", overlapResources.List()), nil
	}
	return true, "", nil
}

func (k *KatalystCustomConfigTargetController) validateTargetResourceNodeNames(_ *configapis.KatalystCustomConfig, targetResource util.KCCTargetResource, allTargetResources []util.KCCTargetResource) (bool, string, error) {
	if targetResource.GetLastDuration() == nil {
		return false, "nodeNames has been set but lastDuration no set", nil
	}
	return validateTargetResourceNodeNamesOverlapped(targetResource, allTargetResources)
}

func validateTargetResourceNodeNamesOverlapped(targetResource util.KCCTargetResource, otherResources []util.KCCTargetResource) (bool, string, error) {
	nodeNames := sets.NewString(targetResource.GetNodeNames()...)
	overlapResources := sets.String{}
	for _, res := range otherResources {
		if (res.GetNamespace() == targetResource.GetNamespace() && res.GetName() == targetResource.GetName()) || len(res.GetNodeNames()) == 0 {
			continue
		}
		otherNodeNames := sets.NewString(res.GetNodeNames()...)
		if nodeNames.Intersection(otherNodeNames).Len() > 0 {
			overlapResources.Insert(native.GenerateUniqObjectNameKey(res))
		}
	}
	if len(overlapResources) > 0 {
		return false, fmt.Sprintf("nodeNames overlay with others: %v", overlapResources.List()), nil
	}
	return true, "", nil
}

func (k *KatalystCustomConfigTargetController) validateTargetResourceGlobal(_ *configapis.KatalystCustomConfig, targetResource util.KCCTargetResource, allTargetResources []util.KCCTargetResource) (bool, string, error) {
	if targetResource.GetLastDuration() != nil {
		return false, "lastDuration has been set for global config", nil
	}
	return validateTargetResourceGlobalOverlapped(targetResource, allTargetResources)
}

func validateTargetResourceGlobalOverlapped(targetResource util.KCCTargetResource, otherResources []util.KCCTargetResource) (bool, string, error) {
	overlapTargetNames := sets.String{}
	for _, res := range otherResources {
		if (res.GetNamespace() == targetResource.GetNamespace() && res.GetName() == targetResource.GetName()) || (len(res.GetNodeNames()) > 0 || len(res.GetLabelSelector()) > 0) {
			continue
		}
		overlapTargetNames.Insert(native.GenerateUniqObjectNameKey(res))
	}
	if len(overlapTargetNames) > 0 {
		return false, fmt.Sprintf("global config %s overlay with others: %v", native.GenerateUniqObjectNameKey(targetResource), overlapTargetNames.List()), nil
	}
	return true, "", nil
}

func updateInvalidTargetResourceStatus(targetResource util.KCCTargetResource, msg, reason string) {
	status := targetResource.GetGenericStatus()
	status.ObservedGeneration = targetResource.GetGeneration()
	kccutil.UpdateKCCTGenericConditions(&status, configapis.ConfigConditionTypeValid, v1.ConditionFalse, reason, msg)
	targetResource.SetGenericStatus(status)
}

func updateValidTargetResourceStatus(targetResource util.KCCTargetResource, targetNodes, canaryNodes, updatedTargetNodes, updatedNodes int32, currentHash string) {
	status := targetResource.GetGenericStatus()
	status.TargetNodes = targetNodes
	status.CanaryNodes = canaryNodes
	status.UpdatedTargetNodes = updatedTargetNodes
	status.UpdatedNodes = updatedNodes
	status.CurrentHash = currentHash
	status.ObservedGeneration = targetResource.GetGeneration()
	kccutil.UpdateKCCTGenericConditions(&status, configapis.ConfigConditionTypeValid, v1.ConditionTrue, kccTargetConditionReasonNormal, "")
	targetResource.SetGenericStatus(status)
}

func checkLabelSelectorOverlap(selector labels.Selector, otherSelector labels.Selector, keyList []string) bool {
	for _, key := range keyList {
		equalValueSet, inEqualValueSet, _ := getMatchValueSet(selector, key)
		otherEqualValueSet, otherInEqualValueSet, _ := getMatchValueSet(otherSelector, key)
		if (equalValueSet.Len() > 0 && otherEqualValueSet.Len() > 0 && equalValueSet.Intersection(otherEqualValueSet).Len() > 0) ||
			(equalValueSet.Len() == 0 && otherEqualValueSet.Len() == 0) ||
			(inEqualValueSet.Len() > 0 && !inEqualValueSet.Intersection(otherEqualValueSet).Equal(otherEqualValueSet)) ||
			(otherInEqualValueSet.Len() > 0 && !otherInEqualValueSet.Intersection(equalValueSet).Equal(equalValueSet)) ||
			(equalValueSet.Len() > 0 && otherEqualValueSet.Len() == 0 && otherInEqualValueSet.Len() == 0) ||
			(otherEqualValueSet.Len() > 0 && equalValueSet.Len() == 0 && inEqualValueSet.Len() == 0) {
			continue
		}
		return false
	}
	return true
}

func getMatchValueSet(selector labels.Selector, key string) (sets.String, sets.String, error) {
	reqs, selectable := selector.Requirements()
	if !selectable {
		return nil, nil, fmt.Errorf("labelSelector cannot selectable")
	}
	equalValueSet := sets.String{}
	inEqualValueSet := sets.String{}
	for _, r := range reqs {
		if r.Key() != key {
			continue
		}
		switch r.Operator() {
		case selection.Equals, selection.DoubleEquals, selection.In:
			equalValueSet = equalValueSet.Union(r.Values())
		case selection.NotEquals, selection.NotIn:
			inEqualValueSet = inEqualValueSet.Union(r.Values())
		default:
			return nil, nil, fmt.Errorf("labelSelector operator %s not supported", r.Operator())
		}
	}
	return equalValueSet, inEqualValueSet, nil
}

func (k *KatalystCustomConfigTargetController) clearUnusedConfig() {
	general.InfofV(4, "clearUnusedConfig start")
	defer general.InfofV(4, "clearUnusedConfig end")
	cncList, err := k.customNodeConfigLister.List(labels.Everything())
	if err != nil {
		general.Errorf("list all custom node config failed: %v", err)
		return
	}
	configGVRSet := make(map[metav1.GroupVersionResource]struct{})
	k.targetHandler.RangeGVRTargetAccessor(func(gvr metav1.GroupVersionResource, _ kcctarget.KatalystCustomConfigTargetAccessor) bool {
		configGVRSet[gvr] = struct{}{}
		return true
	})
	needToDeleteFunc := func(config configapis.TargetConfig) bool {
		_, ok := configGVRSet[config.ConfigType]
		return !ok
	}
	clearCNCConfigs := func(i int) {
		oldCNC := cncList[i]
		newCNC := oldCNC.DeepCopy()
		newCNC.Status.KatalystCustomConfigList = util.RemoveUnusedTargetConfig(newCNC.Status.KatalystCustomConfigList, needToDeleteFunc)
		if apiequality.Semantic.DeepEqual(oldCNC, newCNC) {
			return
		}
		general.Infof("clearUnusedConfig patch cnc %s", oldCNC.GetName())
		_, err := k.cncControl.PatchCNCStatus(k.ctx, oldCNC.GetName(), oldCNC, newCNC)
		if err != nil {
			general.Errorf("clearUnusedConfig patch cnc %s failed: %v", oldCNC.GetName(), err)
		}
	}
	workqueue.ParallelizeUntil(k.ctx, k.cncWorkerCount, len(cncList), clearCNCConfigs)
}

func resolveInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func resolveDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

func resolveBool(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func (k *KatalystCustomConfigTargetController) emitQueueDepthMetric() {
	_ = k.metricsEmitter.StoreInt64(metricKCCTQueueDepth, int64(k.queue.Len()), metrics.MetricTypeNameRaw)
}

func progressKey(gvr metav1.GroupVersionResource, kcctName string) string {
	return gvr.String() + "|" + kcctName
}

func progressKeyPrefix(gvr metav1.GroupVersionResource) string {
	return gvr.String() + "|"
}

func (k *KatalystCustomConfigTargetController) getOrInitProgressLocked(key string) *kcctProgress {
	p, ok := k.progressCache[key]
	if !ok {
		p = &kcctProgress{}
		k.progressCache[key] = p
	}
	return p
}

func (k *KatalystCustomConfigTargetController) observeHashLocked(p *kcctProgress, hash string) bool {
	if p.hash == hash {
		return false
	}
	p.hash = hash
	p.updatedTargetNodes = 0
	p.updatedNodes = 0
	p.rolloutStartedAt = time.Now()
	p.rolloutDone = false
	return true
}

func (k *KatalystCustomConfigTargetController) shouldFullReconcileLocked(p *kcctProgress) bool {
	if !k.enableIncrementalProgress {
		return true
	}
	if p.lastFullReconcileAt.IsZero() {
		return true
	}
	return time.Since(p.lastFullReconcileAt) >= k.cncStatusFullReconcileInterval
}

func (k *KatalystCustomConfigTargetController) shouldEmitStatusLocked(key string, force bool) (bool, bool) {
	if force {
		k.lastStatusEmit[key] = time.Now()
		return true, false
	}
	last := k.lastStatusEmit[key]
	if time.Since(last) < kcctStatusMinEmitInterval {
		return false, true
	}
	k.lastStatusEmit[key] = time.Now()
	return true, false
}

func (k *KatalystCustomConfigTargetController) clearProgressForTarget(gvr metav1.GroupVersionResource, kcctName string) {
	k.progressMu.Lock()
	defer k.progressMu.Unlock()
	key := progressKey(gvr, kcctName)
	delete(k.progressCache, key)
	delete(k.lastStatusEmit, key)
	delete(k.pendingRetrySet, key)
}

func (k *KatalystCustomConfigTargetController) clearProgressForGVR(gvr metav1.GroupVersionResource) {
	k.progressMu.Lock()
	defer k.progressMu.Unlock()
	k.groupCache.Delete(gvr)
	k.gvrCNCStateVersions.Delete(gvr)
	prefix := progressKeyPrefix(gvr)
	for key := range k.progressCache {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(k.progressCache, key)
		}
	}
	for key := range k.lastStatusEmit {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(k.lastStatusEmit, key)
		}
	}
	for key := range k.pendingRetrySet {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(k.pendingRetrySet, key)
		}
	}
}
