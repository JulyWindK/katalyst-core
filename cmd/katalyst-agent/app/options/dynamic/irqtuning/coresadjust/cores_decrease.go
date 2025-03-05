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

package coresadjust

import (
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/irqtuning/coresadjust"
	cliflag "k8s.io/component-base/cli/flag"
)

type IRQCoresDecOptions struct {
	// interval of two successive irq cores decrease MUST greater-equal this interval
	SuccessiveDecInterval int
	Thresholds            *IRQCoresDecThresholds
	// max cores to decrease each time, deault 1
	DecCoresMaxEachTime int
}

type IRQCoresDecThresholds struct {
	// threshold of decreasing irq cores, generally this thresh should be less-than IrqCoresExpectedCpuUtil
	IRQCoresAvgCPUUtilThresh int
}

func NewIRQCoresDecOptions() *IRQCoresDecOptions {
	return &IRQCoresDecOptions{
		SuccessiveDecInterval: 120,
		Thresholds: &IRQCoresDecThresholds{
			IRQCoresAvgCPUUtilThresh: 35,
		},
		DecCoresMaxEachTime: 1,
	}
}

func (o *IRQCoresDecOptions) AddFlags(fss *cliflag.NamedFlagSets) {
	fs := fss.FlagSet("irq-cores-decrease")
	fs.IntVar(&o.SuccessiveDecInterval, "irq-cores-adjust-successive-dec-interval", o.SuccessiveDecInterval, "interval of two successive irq cores decrease MUST greater-equal this interval")
	fs.IntVar(&o.Thresholds.IRQCoresAvgCPUUtilThresh, "irq-cores-adjust-thresholds-irq-cores-avg-cpu-util-thresh", o.Thresholds.IRQCoresAvgCPUUtilThresh, "threshold of decreasing irq cores, generally this thresh should be less-than IrqCoresExpectedCpuUtil")
	fs.IntVar(&o.DecCoresMaxEachTime, "irq-cores-adjust-dec-cores-max-each-time", o.DecCoresMaxEachTime, "max cores to decrease each time, deault 1")
}

func (o *IRQCoresDecOptions) ApplyTo(c *coresadjust.IRQCoresDecConfig) error {
	c.SuccessiveDecInterval = o.SuccessiveDecInterval
	c.Thresholds.IRQCoresAvgCPUUtilThresh = o.Thresholds.IRQCoresAvgCPUUtilThresh
	c.DecCoresMaxEachTime = o.DecCoresMaxEachTime

	return nil
}
