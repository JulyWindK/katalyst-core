package tuner

import (
	"time"

	cpuconsts "github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/consts"
	"github.com/kubewharf/katalyst-core/pkg/agent/qrm-plugins/cpu/dynamicpolicy/irq"
	"github.com/kubewharf/katalyst-core/pkg/util/general"
)

type IrqTunerStub struct {
	irq.StateAdapter
}

func NewIrqTunerStub(sa irq.StateAdapter) irq.Tuner {
	return &IrqTunerStub{sa}
}

func (*IrqTunerStub) Run(stopCh <-chan struct{}) {
	general.RegisterHeartbeatCheck(cpuconsts.IrqAffinity, 2*time.Minute, general.HealthzCheckStateNotReady, 2*time.Minute)
}

func (*IrqTunerStub) Stop() {

}
