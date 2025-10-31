package strategy

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	pluginapi "github.com/kubewharf/katalyst-api/pkg/protocol/evictionplugin/v1alpha1"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/commonstate"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/cpueviction/rules"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/state"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/util"
	"github.com/kubewharf/katalyst-core/pkg/config"
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic"
	"github.com/kubewharf/katalyst-core/pkg/consts"
	"github.com/kubewharf/katalyst-core/pkg/metaserver"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent/metric/helper"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
	"github.com/kubewharf/katalyst-core/pkg/util/native"
)

const EvictionNameNumaSysCpuPressure = "numa-sys-cpu-pressure-plugin"

const evictionConditionSysCPUUsagePressure = "NumaSysCPUPressure"

const (
	numaMetricRawMetricName = "numa_sys_cpu_pressure_numa_raw"

	getPodsEvictMetricName         = "numa_sys_cpu_pressure_get_pods_evict"
	evictCandidateCountMetricName  = "numa_sys_cpu_pressure_evict_candidate_count"
	numaThresholdMetMetricsName    = "numa_sys_cpu_pressure_threshold_met"
	numaSysOverloadCountMetricName = "numa_sys_cpu_pressure_overload_count"
	numaSysOverloadRatioMetricName = "numa_sys_cpu_pressure_overload_ratio"

	metricTagReason = "reason"
)

var (
	containerCPUUsageMetric    = consts.MetricCPUUsageContainer
	containerSysCPUUsageMetric = consts.MetricCPUUsageSysContainer
	metricLists                = []string{containerCPUUsageMetric, containerSysCPUUsageMetric}
)

type PodFilter func(pod *v1.Pod) (bool, error)

type EvictionConfigGetter func() *NumaSysCPUPressureEvictionConfig

var (
	defaultPodFilter = func(pod *v1.Pod) (bool, error) { return true, nil }
)

type NumaSysCPUPressureEviction struct {
	sync.RWMutex
	pluginName string

	conf       *config.Configuration
	state      state.ReadonlyState
	emitter    metrics.MetricEmitter
	metaServer *metaserver.MetaServer

	enabled             bool
	syncPeriod          time.Duration
	deletionGracePeriod int64

	podFilter            []PodFilter
	evictionConfig       *NumaSysCPUPressureEvictionConfig
	evictionConfigGetter EvictionConfigGetter

	overloadNumaCount int
	numaSysOverStats  []rules.NumaSysOverStat
	metricsHistory    *util.NumaMetricHistory
}

func NewSysCPUPressureUsageEviction(emitter metrics.MetricEmitter, metaServer *metaserver.MetaServer,
	conf *config.Configuration, state state.ReadonlyState,
) (CPUPressureEviction, error) {
	evictionConfigGetter := func() *NumaSysCPUPressureEvictionConfig {
		return getNumaSysPressureConfig(conf.GetDynamicConfiguration())
	}
	evictionConfig := evictionConfigGetter()

	return &NumaSysCPUPressureEviction{
		conf:       conf,
		state:      state,
		emitter:    emitter,
		metaServer: metaServer,
		pluginName: EvictionNameNumaCpuPressure,

		enabled:             evictionConfig.EnableEviction,
		syncPeriod:          time.Duration(evictionConfig.SyncPeriod) * time.Second,
		deletionGracePeriod: evictionConfig.GracePeriod,

		podFilter:            []PodFilter{defaultPodFilter},
		evictionConfig:       evictionConfig,
		evictionConfigGetter: evictionConfigGetter,

		numaSysOverStats: make([]rules.NumaSysOverStat, 0),
		metricsHistory:   util.NewMetricHistory(evictionConfig.MetricRingSize),
	}, nil
}

func (p *NumaSysCPUPressureEviction) Start(ctx context.Context) (err error) {
	general.Infof("%s start", p.pluginName)
	go wait.UntilWithContext(ctx, p.sync, p.syncPeriod)
	return
}

func (p *NumaSysCPUPressureEviction) Name() string { return p.pluginName }

func (p *NumaSysCPUPressureEviction) GetEvictPods(_ context.Context, _ *pluginapi.GetEvictPodsRequest) (*pluginapi.GetEvictPodsResponse, error) {
	return &pluginapi.GetEvictPodsResponse{}, nil
}

func (p *NumaSysCPUPressureEviction) ThresholdMet(_ context.Context, req *pluginapi.GetThresholdMetRequest,
) (*pluginapi.ThresholdMetResponse, error) {
	p.RLock()
	defer p.RUnlock()

	if !p.enabled {
		general.Infof("%s plugin is disabled", p.pluginName)
		return &pluginapi.ThresholdMetResponse{
			MetType: pluginapi.ThresholdMetType_NOT_MET,
		}, nil
	}

	if req == nil || req.ActivePods == nil {
		general.Warningf("[%s] no active pods in request", p.pluginName)
		_ = p.emitter.StoreFloat64(numaThresholdMetMetricsName, 0, metrics.MetricTypeNameRaw,
			metrics.ConvertMapToTags(map[string]string{
				metricTagReason: "no active pods in request",
			})...,
		)
		return &pluginapi.ThresholdMetResponse{
			MetType: pluginapi.ThresholdMetType_NOT_MET,
		}, nil
	}

	if len(p.numaSysOverStats) == 0 {
		_ = p.emitter.StoreFloat64(numaThresholdMetMetricsName, 0, metrics.MetricTypeNameRaw,
			metrics.ConvertMapToTags(map[string]string{
				metricTagReason: "no numa sys over load",
			})...,
		)
		return &pluginapi.ThresholdMetResponse{
			MetType: pluginapi.ThresholdMetType_NOT_MET,
		}, nil
	}

	isNumaCPUUsageSoftOver := p.numaSysOverStats[0].IsNumaCPUUsageSoftOver
	isNumaCPUUsageHardOver := p.numaSysOverStats[0].IsNumaCPUUsageHardOver
	isNumaSysCPUUsageSoftOver := p.numaSysOverStats[0].IsNumaSysCPUUsageSoftOver
	isNumaSysCPUUsageHardOver := p.numaSysOverStats[0].IsNumaSysCPUUsageHardOver

	general.Infof("%s plugin,isNumaCPUUsageSoftOver: %v, isNumaCPUUsageHardOver: %v, softOverPercentage: %v, "+
		"hardOverPercentage: %v", p.pluginName, isNumaSysCPUUsageSoftOver, isNumaSysCPUUsageHardOver, p.evictionConfig.ThresholdMetPercentage, p.evictionConfig.ThresholdMetPercentage)
	general.Infof("%s plugin,isNumaSysCPUUsageSoftOver: %v, isNumaSysCPUUsageHardOver: %v, sysOverTotalUsageSoftThreshold: %v, "+
		"sysOverTotalUsageHardThreshold: %v", p.pluginName, isNumaSysCPUUsageSoftOver, isNumaSysCPUUsageHardOver, p.evictionConfig.NumaSysOverTotalUsageSoftThreshold, p.evictionConfig.NumaSysOverTotalUsageHardThreshold)

	// If the overall CPU utilization of NUMA reaches the threshold and the utilization of the system also reaches the threshold,
	// the eviction condition is considered to be met.
	if isNumaCPUUsageHardOver && isNumaSysCPUUsageHardOver {
		_ = p.emitter.StoreFloat64(numaThresholdMetMetricsName, 1, metrics.MetricTypeNameRaw)
		return &pluginapi.ThresholdMetResponse{
			ThresholdValue:    1,
			ObservedValue:     1,
			ThresholdOperator: pluginapi.ThresholdOperator_GREATER_THAN,
			MetType:           pluginapi.ThresholdMetType_HARD_MET,
			EvictionScope:     targetMetric,
			Condition: &pluginapi.Condition{
				ConditionType: pluginapi.ConditionType_NODE_CONDITION,
				Effects:       []string{string(v1.TaintEffectNoSchedule)},
				ConditionName: evictionConditionSysCPUUsagePressure,
				MetCondition:  true,
			},
		}, nil
	}

	// If only the soft threshold is reached, the node will be tainted and cannot be scheduled for the time being.
	if isNumaCPUUsageSoftOver && isNumaSysCPUUsageSoftOver {
		_ = p.emitter.StoreFloat64(numaThresholdMetMetricsName, 1, metrics.MetricTypeNameRaw)
		return &pluginapi.ThresholdMetResponse{
			ThresholdValue:    1,
			ObservedValue:     1,
			ThresholdOperator: pluginapi.ThresholdOperator_GREATER_THAN,
			MetType:           pluginapi.ThresholdMetType_SOFT_MET,
			EvictionScope:     targetMetric,
			Condition: &pluginapi.Condition{
				ConditionType: pluginapi.ConditionType_NODE_CONDITION,
				Effects:       []string{string(v1.TaintEffectNoSchedule)},
				ConditionName: evictionConditionSysCPUUsagePressure,
				MetCondition:  true,
			},
		}, nil
	}

	return &pluginapi.ThresholdMetResponse{MetType: pluginapi.ThresholdMetType_NOT_MET}, nil
}

func (p *NumaSysCPUPressureEviction) GetTopEvictionPods(ctx context.Context, request *pluginapi.GetTopEvictionPodsRequest,
) (*pluginapi.GetTopEvictionPodsResponse, error) {
	if request == nil {
		return nil, fmt.Errorf("GetTopEvictionPods got nil request")
	} else if len(request.ActivePods) == 0 {
		general.Warningf("[%s] got empty active pods list", p.pluginName)
		return &pluginapi.GetTopEvictionPodsResponse{}, nil
	}

	p.RLock()
	defer p.RUnlock()

	if !p.enabled {
		general.Infof("%s plugin is disabled", p.pluginName)
		return &pluginapi.GetTopEvictionPodsResponse{}, nil
	}

	if len(p.numaSysOverStats) == 0 {
		general.Infof("no numa sys over load currently")
		_ = p.emitter.StoreInt64(getPodsEvictMetricName, 0, metrics.MetricTypeNameRaw,
			metrics.ConvertMapToTags(map[string]string{
				metricTagReason: "no numa sys over load",
			})...)
		return &pluginapi.GetTopEvictionPodsResponse{}, nil
	}

	activeFilteredPods := make([]*v1.Pod, 0)
	for _, filter := range p.podFilter {
		activeFilteredPods = append(activeFilteredPods, native.FilterPods(request.ActivePods, filter)...)
	}
	if len(activeFilteredPods) == 0 {
		_ = p.emitter.StoreInt64(getPodsEvictMetricName, 0, metrics.MetricTypeNameRaw,
			metrics.ConvertMapToTags(map[string]string{
				metricTagReason: "no active pods in request",
			})...)
		general.Warningf("[%s] got empty active pods list after filter", p.pluginName)
		return &pluginapi.GetTopEvictionPodsResponse{}, nil
	}

	numaPodsMap := make(map[int][]*v1.Pod)
	for _, pod := range activeFilteredPods {
		numaIDStr, err := ParseNumaIDFormPod(pod)
		if err != nil {
			klog.Warningf("[%s] failed to parse pod numaID: %v", p.pluginName, err)
			continue
		}

		numaID, err := strconv.Atoi(numaIDStr)
		if err != nil {
			klog.Warningf("[%s] failed to parse pod numaID: %v", p.pluginName, err)
			continue
		}

		numaPodsMap[numaID] = append(numaPodsMap[numaID], pod)
	}

	candidatePods := make([]*v1.Pod, 0)
	for _, pod := range numaPodsMap[p.numaSysOverStats[0].NumaID] {
		sysCPUUsage, err := helper.GetPodMetric(p.metaServer.MetricsFetcher, p.emitter, pod, containerSysCPUUsageMetric, -1)
		if err != nil {
			klog.Warningf("[%s] failed to get pod metric: %v", p.pluginName, err)
			continue
		}
		cpuUsage, err := helper.GetPodMetric(p.metaServer.MetricsFetcher, p.emitter, pod, containerCPUUsageMetric, -1)
		if err != nil {
			klog.Warningf("[%s] failed to get pod metric: %v", p.pluginName, err)
			continue
		}

		// The current pod is evicted only when the proportion of sys CPU utilization exceeds a certain threshold.
		if sysCPUUsage/cpuUsage >= p.evictionConfig.NumaSysOverTotalUsageEvictionThreshold {
			candidatePods = append(candidatePods, pod)
		}
	}

	_ = p.emitter.StoreInt64(evictCandidateCountMetricName, int64(len(candidatePods)), metrics.MetricTypeNameRaw)
	if len(candidatePods) == 0 {
		general.Infof("[%s] got empty candidatePods pods list after filter", p.pluginName)
		return &pluginapi.GetTopEvictionPodsResponse{}, nil
	}

	// sort by sys cpu utilization
	sort.Slice(candidatePods, func(i, j int) bool {
		sysCPUUsagePodI, err := helper.GetPodMetric(p.metaServer.MetricsFetcher, p.emitter, candidatePods[i], containerSysCPUUsageMetric, -1)
		if err != nil {
			klog.Warningf("[%s] failed to get pod metric: %v", p.pluginName, err)

		}
		sysCPUUsagePodJ, err := helper.GetPodMetric(p.metaServer.MetricsFetcher, p.emitter, candidatePods[j], containerSysCPUUsageMetric, -1)
		if err != nil {
			klog.Warningf("[%s] failed to get pod metric: %v", p.pluginName, err)
		}

		return sysCPUUsagePodI > sysCPUUsagePodJ
	})

	retLen := general.MinUInt64(request.TopN, uint64(len(candidatePods)))

	var deletionOptions *pluginapi.DeletionOptions
	if gracePeriod := p.deletionGracePeriod; gracePeriod > 0 {
		deletionOptions = &pluginapi.DeletionOptions{
			GracePeriodSeconds: gracePeriod,
		}
	}

	_ = p.emitter.StoreInt64(getPodsEvictMetricName, 1, metrics.MetricTypeNameRaw)
	return &pluginapi.GetTopEvictionPodsResponse{
		TargetPods:      candidatePods[:retLen],
		DeletionOptions: deletionOptions,
	}, nil
}

func (p *NumaSysCPUPressureEviction) sync(_ context.Context) {
	p.Lock()
	defer p.Unlock()

	general.Infof("[%s] start to sync metrics and stats", p.pluginName)
	// sync eviction config
	p.evictionConfig = p.evictionConfigGetter()
	p.enabled = p.evictionConfig.EnableEviction
	p.syncPeriod = time.Duration(p.evictionConfig.SyncPeriod) * time.Second
	p.deletionGracePeriod = p.evictionConfig.GracePeriod

	if !p.enabled {
		general.Infof("%s plugin is disabled", p.pluginName)
		return
	}
	// sync numa metrics
	machineState := p.state.GetMachineState()
	for _, metricName := range metricLists {
		// numa -> pod -> ring
		for numaID := 0; numaID < p.metaServer.NumNUMANodes; numaID++ {
			numaSize := p.metaServer.NUMAToCPUs.CPUSizeInNUMAs(numaID)
			snbEntries := machineState[numaID].PodEntries.GetFilteredPodEntries(state.WrapAllocationMetaFilter((*commonstate.AllocationMeta).CheckSharedNUMABinding))

			sum := 0.0
			for podUID, containerEntries := range snbEntries {
				for containerName := range containerEntries {
					val, err := p.metaServer.GetContainerMetric(podUID, containerName, metricName)
					if err != nil {
						general.Warningf("failed to get pod metric, numa %v, pod %v, metric %v err: %v",
							numaID, podUID, metricName, err)
					}
					valRatio := val.Value / float64(numaSize)
					p.metricsHistory.Push(numaID, podUID, metricName, valRatio)
					sum += valRatio
				}
			}
			p.metricsHistory.PushNuma(numaID, metricName, sum)
			general.Infof("[%s] Push numa metric %s, numa %d, value %f", p.pluginName, metricName, numaID, sum)
			_ = p.emitter.StoreFloat64(numaMetricRawMetricName, sum, metrics.MetricTypeNameRaw,
				metrics.ConvertMapToTags(map[string]string{
					metricTagMetricName: metricName,
					metricTagNuma:       strconv.Itoa(numaID),
				})...)
		}
	}

	// update numa sys over stat
	p.updateNumaSysOverStat()
	general.Infof("[%s] overload numa count %v", p.pluginName, p.overloadNumaCount)
}

func (p *NumaSysCPUPressureEviction) updateNumaSysOverStat() {
	var numaSysOverCount int

	for numaID, numaHis := range p.metricsHistory.Inner {
		numaHisInner := numaHis[util.FakePodUID]

		// Get the current CPU usage metrics of NUMA
		numaCPUUsageMetric, ok := numaHisInner[containerCPUUsageMetric]
		if !ok || numaCPUUsageMetric.Len() == 0 {
			continue
		}
		// Count the number of NUMA CPUs whose usage exceeds both hardware and software thresholds.
		numaCPUUsageSoftOverCount, numaCPUUsageHardOverCount := numaCPUUsageMetric.Count()
		numaCPUUsageAvg := numaCPUUsageMetric.Avg()

		// Judge whether the NUMA CPU usage exceeds the threshold.
		numaCPUUsageSoftOverPercentage := float64(numaCPUUsageSoftOverCount) / float64(p.evictionConfig.MetricRingSize)
		numaCPUUsageHardOverPercentage := float64(numaCPUUsageHardOverCount) / float64(p.evictionConfig.MetricRingSize)
		isNumaCPUUsageSoftOver := numaCPUUsageSoftOverPercentage >= p.evictionConfig.ThresholdMetPercentage
		isNumaCPUUsageHardOver := numaCPUUsageHardOverPercentage >= p.evictionConfig.ThresholdMetPercentage

		// Get the current CPU usage of the system in numa.
		numaSysCPUUsageMetricRing, ok := numaHisInner[containerSysCPUUsageMetric]
		if !ok || numaSysCPUUsageMetricRing.Len() == 0 {
			continue
		}
		numaSysCPUUsageAvg := numaSysCPUUsageMetricRing.Avg()

		// Calculate the ratio of the system CPU usage in the current numa to the total CPU usage.
		percentage := numaSysCPUUsageAvg / numaCPUUsageAvg

		isNumaSysCPUUsageSoftOver := percentage >= p.evictionConfig.NumaSysOverTotalUsageSoftThreshold
		isNumaSysCPUUsageHardOver := percentage >= p.evictionConfig.NumaSysOverTotalUsageHardThreshold

		_ = p.emitter.StoreFloat64(numaSysOverloadRatioMetricName, percentage, metrics.MetricTypeNameRaw,
			metrics.ConvertMapToTags(map[string]string{
				metricTagMetricName: numaSysOverloadRatioMetricName,
				metricTagNuma:       strconv.Itoa(numaID),
				metricTagIsOverload: strconv.FormatBool(isNumaSysCPUUsageHardOver),
			})...)

		p.numaSysOverStats = append(p.numaSysOverStats, rules.NumaSysOverStat{
			NumaID:             numaID,
			NumaCPUUsageAvg:    numaCPUUsageAvg,
			NumaSysCPUUsageAvg: numaSysCPUUsageAvg,

			IsNumaCPUUsageSoftOver:    isNumaCPUUsageSoftOver,
			IsNumaCPUUsageHardOver:    isNumaCPUUsageHardOver,
			IsNumaSysCPUUsageSoftOver: isNumaSysCPUUsageSoftOver,
			IsNumaSysCPUUsageHardOver: isNumaSysCPUUsageHardOver,
		})
		if isNumaCPUUsageHardOver && isNumaSysCPUUsageHardOver {
			numaSysOverCount++
		}
	}

	sort.SliceStable(p.numaSysOverStats, func(i, j int) bool {
		// Sort by the ratio of the system CPU usage in the current numa to the total CPU usage.
		ratioI := p.numaSysOverStats[i].NumaSysCPUUsageAvg / p.numaSysOverStats[i].NumaCPUUsageAvg
		ratioJ := p.numaSysOverStats[j].NumaSysCPUUsageAvg / p.numaSysOverStats[j].NumaCPUUsageAvg
		return ratioI > ratioJ
	})

	p.overloadNumaCount = numaSysOverCount

	_ = p.emitter.StoreInt64(numaSysOverloadCountMetricName, int64(numaSysOverCount), metrics.MetricTypeNameRaw)
	general.Infof("[%s] Update numa sys cpu pressure overload count %v", p.pluginName, numaSysOverCount)
	return
}

func getNumaSysPressureConfig(conf *dynamic.Configuration) *NumaSysCPUPressureEvictionConfig {
	if conf == nil || conf.AdminQoSConfiguration == nil || conf.EvictionConfiguration == nil ||
		conf.CPUPressureEvictionConfiguration == nil {
		return &NumaSysCPUPressureEvictionConfig{}
	}

	return &NumaSysCPUPressureEvictionConfig{
		EnableEviction: conf.NumaSysCPUPressureEvictionConfiguration.EnableEviction,
		MetricRingSize: conf.NumaSysCPUPressureEvictionConfiguration.MetricRingSize,
		GracePeriod:    conf.NumaSysCPUPressureEvictionConfiguration.GracePeriod,
		SyncPeriod:     conf.NumaSysCPUPressureEvictionConfiguration.SyncPeriod,

		ThresholdMetPercentage:                 conf.NumaSysCPUPressureEvictionConfiguration.ThresholdMetPercentage,
		NumaCPUUsageSoftThreshold:              conf.NumaSysCPUPressureEvictionConfiguration.NumaCPUUsageSoftThreshold,
		NumaCPUUsageHardThreshold:              conf.NumaSysCPUPressureEvictionConfiguration.NumaCPUUsageHardThreshold,
		NumaSysOverTotalUsageSoftThreshold:     conf.NumaSysCPUPressureEvictionConfiguration.NUMASysOverTotalUsageSoftThreshold,
		NumaSysOverTotalUsageHardThreshold:     conf.NumaSysCPUPressureEvictionConfiguration.NUMASysOverTotalUsageHardThreshold,
		NumaSysOverTotalUsageEvictionThreshold: conf.NumaSysCPUPressureEvictionConfiguration.NUMASysOverTotalUsageEvictionThreshold,
	}
}
