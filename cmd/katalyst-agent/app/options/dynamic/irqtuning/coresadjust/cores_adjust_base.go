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
	"k8s.io/apimachinery/pkg/util/errors"
	cliflag "k8s.io/component-base/cli/flag"
)

// IRQCoresAdjustConfig is the configuration for IRQCoresAdjustConfig.
type IRQCoresAdjustOptions struct {
	// minimum percent of (100 * irq cores/total(or socket) cores), valid value [0,100], default 2
	IRQCoresPercentMin int

	// maximum percent of (100 * irq cores/total(or socket) cores), valid value [0,100], default 30
	IRQCoresPercentMax int

	IRQCoresIncOptions *IRQCoresIncOptions
	IRQCoresDecOptions *IRQCoresDecOptions
}

func NewIRQCoresAdjustOptions() *IRQCoresAdjustOptions {
	return &IRQCoresAdjustOptions{
		IRQCoresPercentMin: 2,
		IRQCoresPercentMax: 30,
		IRQCoresIncOptions: NewIRQCoresIncOptions(),
		IRQCoresDecOptions: NewIRQCoresDecOptions(),
	}
}

func (o *IRQCoresAdjustOptions) AddFlags(fss *cliflag.NamedFlagSets) {
	fs := fss.FlagSet("irq-cores-adjust")
	fs.IntVar(&o.IRQCoresPercentMin, "irq-cores-adjust-percent-min", o.IRQCoresPercentMin, "minimum percent of (100 * irq cores/total(or socket) cores), valid value [0,100], default 2")
	fs.IntVar(&o.IRQCoresPercentMax, "irq-cores-adjust-percent-max", o.IRQCoresPercentMax, "maximum percent of (100 * irq cores/total(or socket) cores), valid value [0,100], default 30")

	o.IRQCoresIncOptions.AddFlags(fss)
	o.IRQCoresDecOptions.AddFlags(fss)
}

func (o *IRQCoresAdjustOptions) ApplyTo(c *coresadjust.IRQCoresAdjustConfig) error {
	var errList []error
	c.IRQCoresPercentMin = o.IRQCoresPercentMin
	c.IRQCoresPercentMax = o.IRQCoresPercentMax

	errList = append(errList, o.IRQCoresIncOptions.ApplyTo(c.IRQCoresIncConf))
	errList = append(errList, o.IRQCoresDecOptions.ApplyTo(c.IRQCoresDecConf))

	return errors.NewAggregate(errList)
}
