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

package strategy

import (
	"time"

	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/poweraware/advisor/action"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/poweraware/advisor/action/strategy/assess"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/poweraware/capper"
	"github.com/kubewharf/katalyst-core/pkg/agent/sysadvisor/plugin/poweraware/spec"
	"github.com/kubewharf/katalyst-core/pkg/consts"
	metrictypes "github.com/kubewharf/katalyst-core/pkg/metaserver/agent/metric/types"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
)

// threshold of cpu usage that allows voluntary dvfs
const (
	voluntaryDVFSCPUUsageThreshold = 0.45

	metricPowerAwareDVFSEffect = "node_power_accu_dvfs_effect"
)

type EvictableProber interface {
	HasEvictablePods() bool
}

// CapperProber is only applicable to advisor; capper actor(client) won't be required to implement
type CapperProber interface {
	IsCapperReady() bool
}

// evictFirstStrategy always attempts to evict low priority pods if any; only after all are exhausted will it resort to DVFS means.
// besides, it will continue to try the best to meet the alert spec, regardless of the alert update time.
// alert level has the following meanings in this strategy:
// P2 - noop and expecting scheduler to bias against the node
// P1 - noop and expecting scheduler not to schedule to the node
// P0 - evict if applicable; otherwise conduct DVFS once if needed (DVFS is limited to 10%);
// S0 - DVFS in urgency (no limit on DVFS)
type evictFirstStrategy struct {
	emitter         metrics.MetricEmitter
	coefficient     exponentialDecay
	evictableProber EvictableProber
	dvfsTracker     dvfsTracker
	metricsReader   metrictypes.MetricsReader
}

func (e *evictFirstStrategy) OnDVFSReset() {
	e.dvfsTracker.clear()
}

func (e *evictFirstStrategy) allowVoluntaryFreqCap() bool {
	if e.metricsReader != nil {
		if cpuUsage, err := e.metricsReader.GetNodeMetric(consts.MetricCPUUsageRatio); err == nil {
			general.InfofV(6, "pap: cpu usage %v", cpuUsage.Value)
			if cpuUsage.Value <= voluntaryDVFSCPUUsageThreshold {
				return false
			}
		}
	}

	return e.dvfsTracker.getDVFSAllowPercent() > 0
}

func (e *evictFirstStrategy) recommendEvictFirstOp() spec.InternalOp {
	// always prefer eviction over dvfs if possible
	if e.evictableProber.HasEvictablePods() {
		return spec.InternalOpEvict
	}

	if e.allowVoluntaryFreqCap() {
		general.InfofV(6, "pap: may have voluntary dvfs")
		return spec.InternalOpFreqCap
	}

	general.InfofV(6, "pap: no suitable action at this moment; neither evictable nor room for dvfs")
	return spec.InternalOpNoop
}

func (e *evictFirstStrategy) recommendOp(alert spec.PowerAlert, internalOp spec.InternalOp) spec.InternalOp {
	if internalOp != spec.InternalOpAuto {
		// internal op is only applicable to dvfs related levels, i.e. s0 + p0
		if alert == spec.PowerAlertS0 || alert == spec.PowerAlertP0 {
			return internalOp
		}
	}

	switch alert {
	case spec.PowerAlertS0:
		return spec.InternalOpFreqCap
	case spec.PowerAlertP0:
		return e.recommendEvictFirstOp()
	default:
		return spec.InternalOpNoop
	}
}

func (e *evictFirstStrategy) adjustTargetForConstraintDVFS(actualWatt, desiredWatt int) (int, error) {
	return e.dvfsTracker.adjustTargetWatt(actualWatt, desiredWatt)
}

func (e *evictFirstStrategy) yieldActionPlan(op, internalOp spec.InternalOp, actualWatt, desiredWatt int, alert spec.PowerAlert, ttl time.Duration) action.PowerAction {
	switch op {
	case spec.InternalOpFreqCap:
		// try to conduct freq capping within the allowed limit except for the unconstrained dvfs
		if alert != spec.PowerAlertS0 {
			var err error
			desiredWatt, err = e.adjustTargetForConstraintDVFS(actualWatt, desiredWatt)
			if err != nil {
				general.Warningf("pap: noop to skip temporary failure %v", err)
				return action.PowerAction{Op: spec.InternalOpNoop, Arg: 0}
			}
		}
		return action.PowerAction{Op: spec.InternalOpFreqCap, Arg: desiredWatt}
	case spec.InternalOpEvict:
		return action.PowerAction{
			Op:  spec.InternalOpEvict,
			Arg: e.coefficient.calcExcessiveInPercent(desiredWatt, actualWatt, ttl),
		}
	default:
		return action.PowerAction{Op: spec.InternalOpNoop, Arg: 0}
	}
}

func (e *evictFirstStrategy) RecommendAction(actualWatt int, desiredWatt int, alert spec.PowerAlert, internalOp spec.InternalOp, ttl time.Duration) action.PowerAction {
	e.dvfsTracker.update(actualWatt)
	e.emitDVFSAccumulatedEffect(e.dvfsTracker.dvfsAccumEffect)

	if actualWatt <= desiredWatt {
		e.dvfsTracker.dvfsExit()
		return action.PowerAction{Op: spec.InternalOpNoop, Arg: 0}
	}

	op := e.recommendOp(alert, internalOp)
	actionPlan := e.yieldActionPlan(op, internalOp, actualWatt, desiredWatt, alert, ttl)
	if actionPlan.Op == spec.InternalOpFreqCap {
		e.dvfsTracker.dvfsEnter()
	} else {
		e.dvfsTracker.dvfsExit()
	}

	return actionPlan
}

func (e *evictFirstStrategy) emitDVFSAccumulatedEffect(percentage int) {
	_ = e.emitter.StoreInt64(metricPowerAwareDVFSEffect, int64(percentage), metrics.MetricTypeNameRaw)
}

func NewEvictFirstStrategy(emitter metrics.MetricEmitter, prober EvictableProber,
	metricsReader metrictypes.MetricsReader, capper capper.PowerCapper, assessor assess.Assessor,
) PowerActionStrategy {
	general.Infof("pap: using EvictFirst strategy")
	capperProber, _ := capper.(CapperProber)
	return &evictFirstStrategy{
		emitter:         emitter,
		coefficient:     exponentialDecay{b: defaultDecayB},
		evictableProber: prober,
		dvfsTracker: dvfsTracker{
			dvfsAccumEffect: 0,
			isEffectCurrent: true,
			capperProber:    capperProber,
			assessor:        assessor,
		},
		metricsReader: metricsReader,
	}
}
