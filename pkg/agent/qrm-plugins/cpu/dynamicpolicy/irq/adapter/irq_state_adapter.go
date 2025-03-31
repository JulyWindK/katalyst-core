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
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"

	"github.com/kubewharf/katalyst-core/cmd/katalyst-agent/app/agent"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/commonstate"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/irq"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/state"
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

	// ...
}

func NewIrqStateAdapter(agentCtx *agent.GenericContext, conf *config.Configuration, state state.State) irq.StateAdapter {
	isa := &irqStateAdapterImpl{
		state:       state,
		metaServer:  agentCtx.MetaServer,
		machineInfo: agentCtx.KatalystMachineInfo,
	}

	// ...
	// ...

	return isa
}

func (c *irqStateAdapterImpl) ListContainers() ([]irq.ContainerInfo, error) {
	var cis []irq.ContainerInfo

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// TODO: get container infos
	// 1. get container info from pod entries
	for podUID, entry := range c.state.GetPodEntries() {
		if entry.IsPoolEntry() {
			continue
		}

		// get pod
		pod, err := c.metaServer.PodFetcher.GetPod(ctx, podUID)
		if err != nil || pod == nil {
			return nil, err
		}
		// get runtime class
		runtime := pod.Spec.RuntimeClassName
		if runtime == nil {
			return nil, fmt.Errorf("get pod runtime class err")
		}

		// get pod qos
		qosClass := qos.GetPodQOS(pod)

		// get container status
		var containerStatus map[string]v1.ContainerStatus
		for _, cs := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
			containerStatus[cs.Name] = cs
		}

		for containerName, allocationInfo := range entry {
			if allocationInfo == nil {
				general.Warningf("container %s allocation info is nil, skip it", containerName)
				continue
			}

			// get container ID
			containerID, err := c.metaServer.PodFetcher.GetContainerID(podUID, containerName)
			if err != nil {
				return nil, err
			}

			// get cgroup path
			cp := fmt.Sprintf("/kubepods/%s/pod%s/%s", string(qosClass), podUID, containerID)

			// get started time
			var startedAt metav1.Time
			if cs, exist := containerStatus[containerName]; exist && cs.State.Running != nil {
				startedAt = cs.State.Running.StartedAt
			} else {
				general.Warningf("container %s not running, skip it", containerName)
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

func (c *irqStateAdapterImpl) GetIrqForbiddenCores() (machine.CPUSet, error) {
	forbiddenCores := machine.NewCPUSet()

	// TODO: get irq forbidden cores
	// 1. get irq forbidden cores from cpu plugin checkpoint
	// 1.1 get reserved pool
	// 1.2 get katabm cores

	return forbiddenCores, nil
}

func (c *irqStateAdapterImpl) SetExclusiveIrqCpuset(cpuset machine.CPUSet) error {
	// 1. exception validation
	forbidden, err := c.GetIrqForbiddenCores()
	if err != nil {
		general.Errorf("get irq forbidden cores failed, err:%v", err)
		return err
	}
	// 1.1 check cpuset nums（max）
	// TODO: opt max cpuset num
	if cpuset.Size() > c.state.GetMachineState().GetAvailableCPUSet(forbidden).Size() {
		general.Errorf("")
		return fmt.Errorf("")
	}
	// 1.2 check cpuset is intersection of irq forbidden cores

	if cpuset.Intersection(forbidden).Size() != 0 {
		general.Errorf("the cpuset[%v] passed in contains a core that is forbidden[%v] to bind", cpuset, forbidden)
		return fmt.Errorf("the cpuset[%v] passed in contains a core that is forbidden[%v] to bind", cpuset, forbidden)
	}

	// 2. measuring the rate at which the irq-affinity core pool expands and scales

	// 3. update cpu plugin checkpoint
	// TODO: supplement allocation info detail
	ai := &state.AllocationInfo{
		AllocationResult: cpuset,
	}
	newPodEntries := c.state.GetPodEntries()
	// TODO: fix systemd_cores -> irq_cores
	if _, ok := newPodEntries[commonstate.PoolNameIRQ]; !ok {
		newPodEntries[commonstate.PoolNameIRQ] = state.ContainerEntries{}
	}
	if _, ok := newPodEntries[commonstate.PoolNameIRQ][""]; !ok {
		newPodEntries[commonstate.PoolNameIRQ][""] = &state.AllocationInfo{}
	}
	newPodEntries[commonstate.PoolNameIRQ][""] = ai

	machineState, err := state.GenerateMachineStateFromPodEntries(c.machineInfo.CPUTopology, newPodEntries)
	if err != nil {
		return fmt.Errorf("calculate machineState by newPodEntries failed with error: %v", err)
	}
	c.state.SetPodEntries(newPodEntries)
	c.state.SetMachineState(machineState)

	return nil
}
