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

package irq

import (
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/commonstate"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ContainerInfo struct {
	*commonstate.AllocationMeta

	ContainerID string
	// CgroupPath is the relative cgroup path, e.g. /kubepods/burstable/pod<uuid>/<container id>.
	CgroupPath string
	// RuntimeClassName refers to pod spec runtime class.
	RuntimeClassName string
	// ActualCPUSet represents the actual cpuset, which is real-time data, with the numa id as the map key.
	ActualCPUSet map[int]machine.CPUSet

	StartedAt v1.Time
}

type StateAdapter interface {
	// ListContainers only return running containers, not include daemonset, because katalyst has no knowledge about daemonset pod creation,
	// and usually doesn't fail.
	ListContainers() ([]ContainerInfo, error)

	// GetIrqForbiddenCores get irq forbidden cores from qrm state manager.
	GetIrqForbiddenCores() (machine.CPUSet, error)

	// GetExclusiveIrqCPUSet get exclusive cores from qrm state manager.
	GetExclusiveIrqCPUSet() (machine.CPUSet, error)

	// SetExclusiveIrqCPUSet irq tuning controller only set exclusive irq cores to qrm-state manager, irq affinity tuning operation performed by irq-tuning
	// controller is transparent to qrm-stat manager.
	SetExclusiveIrqCPUSet(cpuSet machine.CPUSet) error
}

type Tuner interface {
	Run(stopCh <-chan struct{})
	Stop()
}
