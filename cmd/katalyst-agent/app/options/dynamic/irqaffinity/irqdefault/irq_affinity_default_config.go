package irqdefault

import (
	"time"

	irqdynamicconf "github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/irqaffinity"
	cliflag "k8s.io/component-base/cli/flag"
)

type DefaultOptions struct {
	DefaultEnableIrqAffinity    bool
	DefaultIrqReconcileInterval time.Duration
	DefaultIrqPolicyName        string
}

func NewDefaultOptions() *DefaultOptions {
	return &DefaultOptions{
		DefaultEnableIrqAffinity:    irqdynamicconf.DefaultEnableIrqAffinity,
		DefaultIrqReconcileInterval: irqdynamicconf.DefaultIrqReconcileInterval,
		DefaultIrqPolicyName:        irqdynamicconf.DefaultIrqPolicyName,
	}
}

func (o *DefaultOptions) AddFlags(fss *cliflag.NamedFlagSets) {
	fs := fss.FlagSet("irq-affinity-default-config")

	fs.BoolVar(&o.DefaultEnableIrqAffinity, "default-enable-irq-affinity", o.DefaultEnableIrqAffinity,
		"whether to enable irq affinity by default")
	fs.DurationVar(&o.DefaultIrqReconcileInterval, "default-irq-reconcile-interval", o.DefaultIrqReconcileInterval,
		"default minimum interval to trigger irq reconcile")
	fs.StringVar(&o.DefaultIrqPolicyName, "default-irq-policy-name", o.DefaultIrqPolicyName,
		"default policy used to calculate irq affinity")
}

func (o *DefaultOptions) ApplyTo(c *irqdynamicconf.IrqAffinityDefaultConfigurations) error {
	c.DefaultEnableIrqAffinity = o.DefaultEnableIrqAffinity
	c.DefaultIrqReconcileInterval = o.DefaultIrqReconcileInterval
	c.DefaultIrqPolicyName = (o.DefaultIrqPolicyName)

	return nil
}
