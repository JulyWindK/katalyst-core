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

package irqtuning

import (
	"k8s.io/apimachinery/pkg/util/errors"
	cliflag "k8s.io/component-base/cli/flag"

	"github.com/kubewharf/katalyst-core/cmd/katalyst-agent/app/options/dynamic/irqtuning/coresadjust"
	"github.com/kubewharf/katalyst-core/cmd/katalyst-agent/app/options/dynamic/irqtuning/coresexclusion"
	"github.com/kubewharf/katalyst-core/cmd/katalyst-agent/app/options/dynamic/irqtuning/loadbalance"
	"github.com/kubewharf/katalyst-core/cmd/katalyst-agent/app/options/dynamic/irqtuning/netoverload"
	irqdynamicconf "github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/irqtuning"
)

type IRQTuningOptions struct {
	EnableIRQTuner          bool
	EnableIRQCoresExclusion bool
	// irq tuning periodic interval
	Interval                 int
	IRQCoresExpectedCPUUtil  int
	IRQCoreNetOverLoadThresh *netoverload.IRQCoreNetOverloadThresholdOptions
	IRQLoadBalanceOptions    *loadbalance.IRQLoadBalanceOptions
	IRQCoresAdjustOptions    *coresadjust.IRQCoresAdjustOptions
	IRQCoresExclusionOptions *coresexclusion.IRQCoresExclusionOptions
}

func NewIRQTuningOptions() *IRQTuningOptions {
	return &IRQTuningOptions{
		// TODO: for debug
		EnableIRQTuner:           true,
		EnableIRQCoresExclusion:  false,
		Interval:                 5,
		IRQCoresExpectedCPUUtil:  50,
		IRQCoreNetOverLoadThresh: netoverload.NewIRQCoreNetOverloadThresholdOptions(),
		IRQLoadBalanceOptions:    loadbalance.NewIRQLoadBalanceOptions(),
		IRQCoresAdjustOptions:    coresadjust.NewIRQCoresAdjustOptions(),
		IRQCoresExclusionOptions: coresexclusion.NewIRQCoresExclusionOptions(),
	}
}

func (o *IRQTuningOptions) AddFlags(fss *cliflag.NamedFlagSets) {
	fs := fss.FlagSet("irq-tuning")
	fs.BoolVar(&o.EnableIRQTuner, "enable-irq-tuner", o.EnableIRQTuner, "enable irq tuner")
	fs.BoolVar(&o.EnableIRQCoresExclusion, "enable-irq-cores-exclusion", o.EnableIRQCoresExclusion, "enable irq cores exclusion")
	fs.IntVar(&o.Interval, "irq-tuning-interval", o.Interval, "irq tuning periodic interval")
	fs.IntVar(&o.IRQCoresExpectedCPUUtil, "irq-cores-expected-cpu-util", o.IRQCoresExpectedCPUUtil, "irq cores expected cpu util")

	o.IRQCoreNetOverLoadThresh.AddFlags(fss)
	o.IRQLoadBalanceOptions.AddFlags(fss)
	o.IRQCoresAdjustOptions.AddFlags(fss)
	o.IRQCoresExclusionOptions.AddFlags(fss)
}

func (o *IRQTuningOptions) ApplyTo(c *irqdynamicconf.IRQTuningConfiguration) error {
	var errList []error
	c.EnableIRQTuner = o.EnableIRQTuner
	c.EnableIRQCoresExclusion = o.EnableIRQCoresExclusion
	c.Interval = o.Interval
	c.IRQCoresExpectedCPUUtil = o.IRQCoresExpectedCPUUtil

	errList = append(errList, o.IRQCoreNetOverLoadThresh.ApplyTo(c.IRQCoreNetOverLoadThresh))
	errList = append(errList, o.IRQLoadBalanceOptions.ApplyTo(c.IRQLoadBalanceConf))
	errList = append(errList, o.IRQCoresAdjustOptions.ApplyTo(c.IRQCoresAdjustConf))
	errList = append(errList, o.IRQCoresExclusionOptions.ApplyTo(c.IRQCoresExclusionConf))
	return errors.NewAggregate(errList)
}
