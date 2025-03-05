package irqaffinity

import (
	"time"

	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/crd"
)

const (
	DefaultEnableIrqAffinity    bool          = false
	DefaultIrqReconcileInterval time.Duration = 5 * time.Second
	// TODO: update katelyst api
	DefaultIrqPolicyName = "default"
)

type IrqAffinityConfiguration struct {
	DefaultConfigurations *IrqAffinityDefaultConfigurations
	// ...
}

func NewIrqAffinityConfiguration() *IrqAffinityConfiguration {
	return &IrqAffinityConfiguration{
		DefaultConfigurations: NewIrqAffinityDefaultConfigurations(),
	}
}

type IrqAffinityDefaultConfigurations struct {
	DefaultEnableIrqAffinity    bool
	DefaultIrqReconcileInterval time.Duration
	DefaultIrqPolicyName        string
}

func NewIrqAffinityDefaultConfigurations() *IrqAffinityDefaultConfigurations {
	return &IrqAffinityDefaultConfigurations{
		DefaultEnableIrqAffinity:    DefaultEnableIrqAffinity,
		DefaultIrqReconcileInterval: DefaultIrqReconcileInterval,
		DefaultIrqPolicyName:        DefaultIrqPolicyName,
	}
}

func (c *IrqAffinityConfiguration) ApplyConfiguration(conf *crd.DynamicConfigCRD) {
	if irqConf := conf.IrqAffinityConfiguration; irqConf != nil {
		if irqConf.Spec.Config.QoSLevelConfig != nil {
			// ...
		}
		if irqConf.Spec.Config.CgroupConfig != nil {
			// ...
		}
		if irqConf.Spec.Config.BlockConfig != nil {
			// ...
		}
	}
}
