package qrm

import (
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/adminqos/qrm"
	cliflag "k8s.io/component-base/cli/flag"
)

type CPUPluginOptions struct {
	UseExistNUMAHintResult bool
}

func NewCPUPluginOptions() *CPUPluginOptions {
	return &CPUPluginOptions{}
}

func (o *CPUPluginOptions) AddFlags(fss *cliflag.NamedFlagSets) {
	fs := fss.FlagSet("qrm-cpu-plugin")
	fs.BoolVar(&o.UseExistNUMAHintResult, "use-exist-numa-hint-result", o.UseExistNUMAHintResult,
		"use existing numa hints to calculate results directly")
}

func (o *CPUPluginOptions) ApplyTo(c *qrm.CPUPluginConfiguration) error {
	c.UseExistNUMAHintResult = o.UseExistNUMAHintResult

	return nil
}
