package irq

import (
	"time"

	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/commonstate"
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
)

type ContainerInfo struct {
	*commonstate.AllocationMeta

	ContainerID  string
	CgroupPath   string                 // relative cgroup path, like /kubepods/burstable/pod<uuid>/<container id>
	ActualCPUSet map[int]machine.CPUSet // numa id as map key, real-time data
	// pod spec runtime class
	RuntimeClassName string

	StartedAt time.Time
}

type StateAdapter interface {
	// ListContainers only return running containers, not include daemonset, because katalyst has no knowledge about daemonset pod creation,
	// and usually doesn't fail.
	ListContainers() ([]ContainerInfo, error)

	// GetIrqForbiddenCores get irq forbidden cores from qrm state manager.
	GetIrqForbiddenCores() (machine.CPUSet, error)

	// SetExclusiveIrqCpuset irq tuning controller only set exclusive irq cores to qrm-state manager, irq affinity tuning operation performed by irq-tuning
	// controller is transparent to qrm-stat manager.
	SetExclusiveIrqCpuset(cpuset machine.CPUSet) error
}

type Tuner interface {
	Run(stopCh <-chan struct{})
	Stop()
}
