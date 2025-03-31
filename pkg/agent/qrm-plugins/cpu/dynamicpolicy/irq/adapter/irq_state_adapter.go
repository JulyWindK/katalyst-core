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
	"sync"

	"github.com/kubewharf/katalyst-core/cmd/katalyst-agent/app/agent"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/irq"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/state"
	"github.com/kubewharf/katalyst-core/pkg/config"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
)

type irqStateAdapterImpl struct {
	sync.RWMutex
	state state.State
	// ...
}

func NewIrqStateAdapter(agentCtx *agent.GenericContext, conf *config.Configuration, state state.State) irq.StateAdapter {
	isa := &irqStateAdapterImpl{
		state: state,
	}

	// ...
	// ...

	return isa
}

func (c *irqStateAdapterImpl) ListContainers() ([]irq.ContainerInfo, error) {
	ContainerInfos := []irq.ContainerInfo{}

	// TODO: get container infos
	// 1. get container info from pod entries

	return ContainerInfos, nil
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
	// 1.1 check cpuset nums（max）
	// 1.2 check cpuset is intersection of irq forbidden cores

	// 2. measuring the rate at which the irq-affinity core pool expands and scales

	// 3. update cpu plugin checkpoint

	return nil
}
