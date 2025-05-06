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
	cliflag "k8s.io/component-base/cli/flag"

	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/irqtuning/coresadjust"
)

type IRQCoresIncOptions struct {
	// interval of two successive irq cores increase MUST greater-equal this interval
	SuccessiveIncInterval int
	// when irq cores cpu util hit this thresh, then fallback to balance-fair policy
	CpuFullThresh float64

	Thresholds *IRQCoresIncThresholds
}

type IRQCoresIncThresholds struct {
	// threshold of increasing irq cores, generally this thresh equal to or a litter greater-than IrqCoresExpectedCpuUtil
	AvgCPUUtilThresh int
}

func NewIRQCoresIncOptions() *IRQCoresIncOptions {
	return &IRQCoresIncOptions{
		SuccessiveIncInterval: 5,
		CpuFullThresh:         85,
		Thresholds: &IRQCoresIncThresholds{
			AvgCPUUtilThresh: 60,
		},
	}
}

func (o *IRQCoresIncOptions) AddFlags(fss *cliflag.NamedFlagSets) {
	fs := fss.FlagSet("irq-cores-increase")
	fs.IntVar(&o.SuccessiveIncInterval, "successive-inc-interval", o.SuccessiveIncInterval, "interval of two successive irq cores increase MUST greater-equal this interval")
	fs.Float64Var(&o.CpuFullThresh, "cpu-full-thresh", o.CpuFullThresh, "when irq cores cpu util hit this thresh, then fallback to balance-fair policy")
	fs.IntVar(&o.Thresholds.AvgCPUUtilThresh, "avg-cpu-util-thresh", o.Thresholds.AvgCPUUtilThresh, "threshold of increasing irq cores, generally this thresh equal to or a litter greater-than IrqCoresExpectedCpuUtil")
}

func (o *IRQCoresIncOptions) ApplyTo(c *coresadjust.IRQCoresIncConfig) error {
	c.SuccessiveIncInterval = o.SuccessiveIncInterval
	c.CpuFullThresh = int(o.CpuFullThresh)
	c.Thresholds.AvgCPUUtilThresh = o.Thresholds.AvgCPUUtilThresh

	return nil
}
