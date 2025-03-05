package manager

import (
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	"github.com/kubewharf/katalyst-core/pkg/util/procfs/common"
)

type manager struct{}

// NewProcFSManager return a manager for procfs
func NewProcFSManager() *manager {
	return &manager{}
}

func (m *manager) ApplyProcInterrupts(irqNumber int, cpuset machine.CPUSet) error {
	return nil
}

func (m *manager) GetProcStat() ([]common.Stat, error) {
	return nil, nil
}

func (m *manager) GetProcInterrupts() ([]common.Interrupts, error) {
	return nil, nil
}
