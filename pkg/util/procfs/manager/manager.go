package manager

import (
	"sync"

	"github.com/kubewharf/katalyst-core/pkg/util/machine"
	"github.com/kubewharf/katalyst-core/pkg/util/procfs/common"
)

var (
	initManagerOnce sync.Once
	procFSManager   ProcFSManager
)

type ProcFSManager interface {
	// ApplyProcInterrupts echo 64-69,192-197 > /proc/irq/$irq/smp_affinity_list
	ApplyProcInterrupts(irqNumber int, cpuset machine.CPUSet) error

	// GetProcStat cat /proc/stat
	GetProcStat() ([]common.Stat, error)

	// GetProcInterrupts cat /proc/interrupts | grep eth0- | awk -F ":" '{print $1}'`
	GetProcInterrupts() ([]common.Interrupts, error)
}

// GetProcFSManager
func GetProcFSManager() ProcFSManager {
	initManagerOnce.Do(func() {
		procFSManager = NewProcFSManager()
	})
	return procFSManager
}
