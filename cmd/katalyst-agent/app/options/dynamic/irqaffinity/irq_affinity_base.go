package irqaffinity

import (
	"github.com/kubewharf/katalyst-core/cmd/katalyst-agent/app/options/dynamic/irqaffinity/irqdefault"
	irqdynamicconf "github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/irqaffinity"
	"k8s.io/apimachinery/pkg/util/errors"
	cliflag "k8s.io/component-base/cli/flag"
)

type IrqAffinityOptions struct {
	*irqdefault.DefaultOptions
}

func NewIrqAffinityOptions() *IrqAffinityOptions {
	return &IrqAffinityOptions{
		DefaultOptions: irqdefault.NewDefaultOptions(),
	}
}

func (o *IrqAffinityOptions) AddFlags(fss *cliflag.NamedFlagSets) {
	o.DefaultOptions.AddFlags(fss)
}

func (o *IrqAffinityOptions) ApplyTo(c *irqdynamicconf.IrqAffinityConfiguration) error {
	var errList []error
	errList = append(errList, o.DefaultOptions.ApplyTo(c.DefaultConfigurations))
	return errors.NewAggregate(errList)
}
