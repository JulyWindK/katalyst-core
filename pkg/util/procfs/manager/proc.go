package manager

import (
	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	"github.com/kubewharf/katalyst-core/pkg/util/procfs/common"
)

func ApplyProcInterrupts(irqNumber int, cpuset machine.CPUSet) error {
	return procFSManager.ApplyProcInterrupts(irqNumber, cpuset)
}

func GetProcStat() ([]common.Stat, error) {
	return procFSManager.GetProcStat()
}

func GetProcInterrupts() ([]common.Interrupts, error) {
	return procFSManager.GetProcInterrupts()
}
