package dynamicpolicy

import (
	"context"
	"fmt"
	"math"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/commonstate"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/irqtuner"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/irqtuner/utils"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/state"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/util"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
)

func (p *DynamicPolicy) SetIRQTuner(irqTuner irqtuner.Tuner) {
	p.irqTuner = irqTuner
}

// ListContainers retrieves the container info of all running containers.
func (p *DynamicPolicy) ListContainers() ([]irqtuner.ContainerInfo, error) {
	var cis []irqtuner.ContainerInfo

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 1. get container info from pod entries
	for podUID, entry := range p.state.GetPodEntries() {
		if entry.IsPoolEntry() {
			continue
		}

		// get the pod from meta server
		pod, err := p.metaServer.PodFetcher.GetPod(ctx, podUID)
		if err != nil || pod == nil {
			return nil, err
		}
		// TODO(KFX): Whether it is necessary to filter out running pods

		// get the runtime class from pod spec
		runtime := pod.Spec.RuntimeClassName

		// get the pod qos
		qosClass := pod.Status.QOSClass

		// get the container status
		var containerStatus map[string]v1.ContainerStatus
		for _, cs := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
			containerStatus[cs.Name] = cs
		}

		// get the container info from the current persistent entry of the node
		for containerName, allocationInfo := range entry {
			if allocationInfo == nil {
				general.Warningf("container %s allocation info is nil, skip it", containerName)
				continue
			}

			// get the container ID
			containerID, err := p.metaServer.PodFetcher.GetContainerID(podUID, containerName)
			if err != nil {
				return nil, err
			}

			// get the cgroup path
			cp := fmt.Sprintf("/kubepods/%s/pod%s/%s", string(qosClass), podUID, containerID)

			// get the started time
			var startedAt metav1.Time
			if cs, exist := containerStatus[containerName]; exist && cs.State.Running != nil {
				startedAt = cs.State.Running.StartedAt
			} else {
				general.Infof("container %s not running, skip it", containerName)
				continue
			}

			cis = append(cis, irqtuner.ContainerInfo{
				AllocationMeta:   allocationInfo.AllocationMeta.Clone(),
				ContainerID:      containerID,
				CgroupPath:       cp,
				RuntimeClassName: *runtime,
				ActualCPUSet:     allocationInfo.TopologyAwareAssignments,
				StartedAt:        startedAt,
			})
		}
	}

	return cis, nil
}

// GetIRQForbiddenCores retrieves the cpu set of cores that are forbidden for irq binding.
func (p *DynamicPolicy) GetIRQForbiddenCores() (machine.CPUSet, error) {
	forbiddenCores := machine.NewCPUSet()

	// get irq forbidden cores from cpu plugin checkpoint
	forbiddenCores.Union(p.reservedCPUs)
	// TODO: add katabm cores

	general.Infof("get the irq forbidden cores %v", forbiddenCores)
	return forbiddenCores, nil
}

// GetExclusiveIRQCPUSet retrieves the cpu set of cores that are exclusive for irq binding.
func (p *DynamicPolicy) GetExclusiveIRQCPUSet() (machine.CPUSet, error) {
	currentIrqCPUSet := machine.NewCPUSet()
	podEntries := p.state.GetPodEntries()
	if containerEntry, ok := podEntries[commonstate.PoolNameInterrupt]; ok {
		if allocateInfo, ok := containerEntry[commonstate.FakedContainerName]; ok && allocateInfo != nil {
			currentIrqCPUSet = allocateInfo.AllocationResult
		}
	}

	general.Infof("get the current irq exclusive cpu set: %v", currentIrqCPUSet)
	return currentIrqCPUSet, nil
}

// SetExclusiveIRQCPUSet sets the exclusive cpu set for Interrupt.
func (p *DynamicPolicy) SetExclusiveIRQCPUSet(irqCPUSet machine.CPUSet) error {
	general.Infof("set the current irq exclusive cpu set: %v", irqCPUSet)

	// 1. exception validation
	forbidden, err := p.GetIRQForbiddenCores()
	if err != nil {
		general.Errorf("get irq forbidden cores failed, err:%v", err)
		return err
	}
	// 1.1 check cpuSet nums（max）
	irqCPUSetSize := irqCPUSet.Size()
	if irqCPUSetSize >= p.state.GetMachineState().GetAvailableCPUSet(forbidden).Size() {
		general.Errorf("the specified number of cpusets %d exceeds the available amount", irqCPUSetSize)
		return fmt.Errorf("the specified number of cpusets %d exceeds the available amount", irqCPUSetSize)
	}
	// 1.2 check cpuSet is intersection of irq forbidden cores
	if irqCPUSet.Intersection(forbidden).Size() != 0 {
		general.Errorf("the cpuset[%v] passed in contains the cpu that is forbidden[%v] to bind", irqCPUSet, forbidden)
		return fmt.Errorf("the cpuset[%v] passed in contains the cpu that is forbidden[%v] to bind", irqCPUSet, forbidden)
	}

	// 2. measuring the rate at which the irq-affinity core pool expansion and shrink
	var currentIrqCPUSet machine.CPUSet
	podEntries := p.state.GetPodEntries()
	if containerEntry, ok := podEntries[commonstate.PoolNameInterrupt]; ok {
		if allocateInfo, ok := containerEntry[commonstate.FakedContainerName]; ok && allocateInfo != nil {
			currentIrqCPUSet = allocateInfo.AllocationResult
		}
	}

	var expandRate, shrinkRate float64
	var scaleType utils.ScaleType
	currentIrqCPUSetSize := currentIrqCPUSet.Size()
	availableTotalCPUSetSize := p.state.GetMachineState().GetAvailableCPUSet(p.reservedCPUs).Size()
	stepRate := math.Abs(float64(irqCPUSetSize-currentIrqCPUSetSize)) / float64(availableTotalCPUSetSize) * 100
	if irqCPUSetSize > currentIrqCPUSetSize {
		expandRate = stepRate
		scaleType = utils.ScaleTypeExpand
	} else {
		shrinkRate = stepRate
		scaleType = utils.ScaleTypeShrink
	}

	if expandRate > utils.DefaultMaxExpansionRate || shrinkRate > utils.DefaultMaxShrinkRate {
		general.Errorf("the expansion or shrinkage rate exceeds the threshold, expandRate: %f, shrinkRate: %f", expandRate, shrinkRate)
		return fmt.Errorf("the expansion or shrinkage rate exceeds the threshold, expandRate: %f, shrinkRate: %f", expandRate, shrinkRate)
	}
	_ = p.emitter.StoreFloat64(util.MetricNameSetExclusiveIrqCPURate, stepRate, metrics.MetricTypeNameRaw,
		metrics.MetricTag{Key: "scale_type", Val: string(scaleType)})

	// 3. update cpu plugin checkpoint
	topologyAwareAssignments, err := machine.GetNumaAwareAssignments(p.machineInfo.CPUTopology, irqCPUSet)
	if err != nil {
		return fmt.Errorf("unable to calculate topologyAwareAssignments for entry: %s, entry cpuset: %s, error: %v",
			commonstate.PoolNameInterrupt, irqCPUSet.String(), err)
	}

	ai := &state.AllocationInfo{
		AllocationMeta: commonstate.AllocationMeta{
			PodUid:        commonstate.PoolNameInterrupt,
			OwnerPoolName: commonstate.PoolNameInterrupt,
		},
		AllocationResult:                 irqCPUSet.Clone(),
		OriginalAllocationResult:         irqCPUSet.Clone(),
		TopologyAwareAssignments:         topologyAwareAssignments,
		OriginalTopologyAwareAssignments: machine.DeepcopyCPUAssignment(topologyAwareAssignments),
	}

	newPodEntries := p.state.GetPodEntries()
	if _, ok := newPodEntries[commonstate.PoolNameInterrupt]; !ok {
		newPodEntries[commonstate.PoolNameInterrupt] = state.ContainerEntries{}
	}
	if _, ok := newPodEntries[commonstate.PoolNameInterrupt][commonstate.FakedContainerName]; !ok {
		newPodEntries[commonstate.PoolNameInterrupt][commonstate.FakedContainerName] = &state.AllocationInfo{}
	}
	newPodEntries[commonstate.PoolNameInterrupt][commonstate.FakedContainerName] = ai

	machineState, err := state.GenerateMachineStateFromPodEntries(p.machineInfo.CPUTopology, newPodEntries)
	if err != nil {
		return fmt.Errorf("calculate machineState by newPodEntries failed with error: %v", err)
	}
	p.state.SetPodEntries(newPodEntries, false)
	p.state.SetMachineState(machineState, false)

	// TODO(KFX): Whether the container currently bound to the candidate interrupt core needs to be readjusted
	//if err = p.adjustAllocationEntries(false); err != nil {
	//	return fmt.Errorf("adjustAllocationEntries failed with error: %v", err)
	//}

	general.Infof("persistent irq exclusive cpu set %v successful", irqCPUSet.String())

	return nil
}
