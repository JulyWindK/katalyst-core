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

package adapter

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubewharf/katalyst-core/cmd/katalyst-agent/app/agent"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/commonstate"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/irq"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/irq/utils"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/state"
	cpuutil "github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/util"
	"github.com/kubewharf/katalyst-core/pkg/config"
	"github.com/kubewharf/katalyst-core/pkg/metaserver"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
)

type irqStateAdapterImpl struct {
	sync.RWMutex
	state       state.State
	metaServer  *metaserver.MetaServer
	machineInfo *machine.KatalystMachineInfo

	reservedCPUs machine.CPUSet
}

func NewIrqStateAdapter(agentCtx *agent.GenericContext, conf *config.Configuration, state state.State) (irq.StateAdapter, error) {
	reservedCPUs, reserveErr := cpuutil.GetCoresReservedForSystem(conf, agentCtx.MetaServer, agentCtx.KatalystMachineInfo, agentCtx.CPUDetails.CPUs().Clone())
	if reserveErr != nil {
		return nil, fmt.Errorf("GetCoresReservedForSystem for reservedCPUsNum: %d failed with error: %v",
			conf.ReservedCPUCores, reserveErr)
	}

	isa := &irqStateAdapterImpl{
		state:        state,
		metaServer:   agentCtx.MetaServer,
		machineInfo:  agentCtx.KatalystMachineInfo,
		reservedCPUs: reservedCPUs,
	}

	return isa, nil
}

// ListContainers retrieves information about all containers managed by the irq state adapter.
func (c *irqStateAdapterImpl) ListContainers() ([]irq.ContainerInfo, error) {
	var cis []irq.ContainerInfo

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 1. get container info from pod entries
	for podUID, entry := range c.state.GetPodEntries() {
		if entry.IsPoolEntry() {
			continue
		}

		// get the pod from meta server
		pod, err := c.metaServer.PodFetcher.GetPod(ctx, podUID)
		if err != nil || pod == nil {
			return nil, err
		}
		// TODO(KFX): Whether it is necessary to filter out running pods

		// get the runtime class from pod spec
		runtime := pod.Spec.RuntimeClassName
		if runtime == nil {
			return nil, fmt.Errorf("get pod runtime class err")
		}
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
			containerID, err := c.metaServer.PodFetcher.GetContainerID(podUID, containerName)
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

			cis = append(cis, irq.ContainerInfo{
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

// GetIrqForbiddenCores retrieves the cpu set of cores that are forbidden for irq binding.
func (c *irqStateAdapterImpl) GetIrqForbiddenCores() (machine.CPUSet, error) {
	forbiddenCores := machine.NewCPUSet()

	// get irq forbidden cores from cpu plugin checkpoint
	forbiddenCores.Union(c.reservedCPUs)
	// TODO: add katabm cores

	return forbiddenCores, nil
}

// GetExclusiveIrqCPUSet retrieves the cpu set of cores that are exclusive for irq binding.
func (c *irqStateAdapterImpl) GetExclusiveIrqCPUSet() (machine.CPUSet, error) {
	currentIrqCPUSet := machine.NewCPUSet()
	podEntries := c.state.GetPodEntries()
	if containerEntry, ok := podEntries[commonstate.PoolNameInterrupt]; ok {
		if allocateInfo, ok := containerEntry[commonstate.FakedContainerName]; ok && allocateInfo != nil {
			currentIrqCPUSet = allocateInfo.AllocationResult
		}
	}

	general.Infof("get the current irq exclusive cpu set: %v", currentIrqCPUSet)
	return currentIrqCPUSet, nil
}

// SetExclusiveIrqCPUSet sets the exclusive cpu set for Interrupt.
func (c *irqStateAdapterImpl) SetExclusiveIrqCPUSet(irqCPUSet machine.CPUSet) error {
	general.Infof("set the current irq exclusive cpu set: %v", irqCPUSet)

	// 1. exception validation
	forbidden, err := c.GetIrqForbiddenCores()
	if err != nil {
		general.Errorf("get irq forbidden cores failed, err:%v", err)
		return err
	}
	// 1.1 check cpuSet nums（max）
	irqCPUSetSize := irqCPUSet.Size()
	if irqCPUSetSize >= c.state.GetMachineState().GetAvailableCPUSet(forbidden).Size() {
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
	podEntries := c.state.GetPodEntries()
	if containerEntry, ok := podEntries[commonstate.PoolNameInterrupt]; ok {
		if allocateInfo, ok := containerEntry[commonstate.FakedContainerName]; ok && allocateInfo != nil {
			currentIrqCPUSet = allocateInfo.AllocationResult
		}
	}

	var expandRate, shrinkRate float64
	currentIrqCPUSetSize := currentIrqCPUSet.Size()
	stepRate := math.Abs(float64(irqCPUSetSize-currentIrqCPUSetSize)) / float64(currentIrqCPUSetSize) * 100
	if irqCPUSetSize > currentIrqCPUSetSize {
		expandRate = stepRate
	} else {
		shrinkRate = stepRate
	}

	if expandRate > utils.DefaultMaxExpansionRate || shrinkRate > utils.DefaultMaxShrinkRate {
		general.Errorf("the expansion or shrinkage rate exceeds the threshold, expandRate: %f, shrinkRate: %f", expandRate, shrinkRate)
		return fmt.Errorf("the expansion or shrinkage rate exceeds the threshold, expandRate: %f, shrinkRate: %f", expandRate, shrinkRate)
	}

	// 3. update cpu plugin checkpoint
	topologyAwareAssignments, err := machine.GetNumaAwareAssignments(c.machineInfo.CPUTopology, irqCPUSet)
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

	newPodEntries := c.state.GetPodEntries()
	if _, ok := newPodEntries[commonstate.PoolNameInterrupt]; !ok {
		newPodEntries[commonstate.PoolNameInterrupt] = state.ContainerEntries{}
	}
	if _, ok := newPodEntries[commonstate.PoolNameInterrupt][commonstate.FakedContainerName]; !ok {
		newPodEntries[commonstate.PoolNameInterrupt][commonstate.FakedContainerName] = &state.AllocationInfo{}
	}
	newPodEntries[commonstate.PoolNameInterrupt][commonstate.FakedContainerName] = ai

	machineState, err := state.GenerateMachineStateFromPodEntries(c.machineInfo.CPUTopology, newPodEntries)
	if err != nil {
		return fmt.Errorf("calculate machineState by newPodEntries failed with error: %v", err)
	}
	c.state.SetPodEntries(newPodEntries)
	c.state.SetMachineState(machineState)

	general.Infof("persistent irq exclusive cpu set %v successful", irqCPUSet.String())

	return nil
}
