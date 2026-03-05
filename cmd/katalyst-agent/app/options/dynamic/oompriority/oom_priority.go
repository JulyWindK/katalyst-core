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

package oompriority

import (
	cliflag "k8s.io/component-base/cli/flag"

	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/oompriority"
)

// OOMPriorityOptions holds the configurations for oom priority
type OOMPriorityOptions struct {
	EnableOOMPriority                     bool
	Interval                              int
	LowPriorityWartermarkRatioWithSwapOn  int
	LowPriorityWartermarkRatioWithSwapOff int
	HighPriorityKatalystQoS               []string
	LowPriorityKatalystQoS                []string
	HighPriorityCgroupPaths               []string
	LowPriorityCgroupPaths                []string
}

func NewOOMPriorityOptions() *OOMPriorityOptions {
	return &OOMPriorityOptions{}
}

func (o *OOMPriorityOptions) AddFlags(fss *cliflag.NamedFlagSets) {
	fs := fss.FlagSet("oom-priority")
	fs.BoolVar(&o.EnableOOMPriority, "enable-oom-priority",
		o.EnableOOMPriority, "if set true, we will enable oom priority")
	fs.IntVar(&o.Interval, "interval",
		o.Interval, "the interval to check oom priority")
	fs.IntVar(&o.LowPriorityWartermarkRatioWithSwapOn, "low-priority-wartermark-ratio-with-swap-on",
		o.LowPriorityWartermarkRatioWithSwapOn, "the low priority watermark ratio with swap on")
	fs.IntVar(&o.LowPriorityWartermarkRatioWithSwapOff, "low-priority-wartermark-ratio-with-swap-off",
		o.LowPriorityWartermarkRatioWithSwapOff, "the low priority watermark ratio with swap off")
	fs.StringSliceVar(&o.HighPriorityKatalystQoS, "high-priority-katalyst-qos",
		o.HighPriorityKatalystQoS, "the high priority katalyst qos")
	fs.StringSliceVar(&o.LowPriorityKatalystQoS, "low-priority-katalyst-qos",
		o.LowPriorityKatalystQoS, "the low priority katalyst qos")
	fs.StringSliceVar(&o.HighPriorityCgroupPaths, "high-priority-cgroup-paths",
		o.HighPriorityCgroupPaths, "the high priority cgroup paths")
	fs.StringSliceVar(&o.LowPriorityCgroupPaths, "low-priority-cgroup-paths",
		o.LowPriorityCgroupPaths, "the low priority cgroup paths")
}

func (o *OOMPriorityOptions) ApplyTo(c *oompriority.OOMPriorityConfiguration) error {
	c.EnableOOMPriority = o.EnableOOMPriority
	c.Interval = o.Interval
	c.LowPriorityWartermarkRatioWithSwapOn = o.LowPriorityWartermarkRatioWithSwapOn
	c.LowPriorityWartermarkRatioWithSwapOff = o.LowPriorityWartermarkRatioWithSwapOff
	for _, qos := range o.HighPriorityKatalystQoS {
		c.HighPriorityKatalystQoS = append(c.HighPriorityKatalystQoS, qos)
	}
	for _, qos := range o.LowPriorityKatalystQoS {
		c.LowPriorityKatalystQoS = append(c.LowPriorityKatalystQoS, qos)
	}
	for _, path := range o.HighPriorityCgroupPaths {
		c.HighPriorityCgroupPaths = append(c.HighPriorityCgroupPaths, path)
	}
	for _, path := range o.LowPriorityCgroupPaths {
		c.LowPriorityCgroupPaths = append(c.LowPriorityCgroupPaths, path)
	}

	return nil
}
