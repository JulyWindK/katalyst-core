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

package irqtuner

import (
	cliflag "k8s.io/component-base/cli/flag"

	"github.com/kubewharf/katalyst-core/pkg/config/agent/qrm/irqtuner"
)

type IRQTunerOptions struct {
	EnableIRQTuner                        bool
	SpecialContainerRuntimeClass          string
	SpecialContainerRuntimeAnnotationKeys []string
	SpecialContainerRuntimeAnnotationsVal string
}

func NewIRQTunerOptions() *IRQTunerOptions {
	return &IRQTunerOptions{
		EnableIRQTuner: false,
	}
}

func (o *IRQTunerOptions) AddFlags(fss *cliflag.NamedFlagSets) {
	fs := fss.FlagSet("irq_tuner")

	fs.BoolVar(&o.EnableIRQTuner, "enable-irq-tuner", o.EnableIRQTuner, "if set true, we will enable irq tuner")
	fs.StringVar(&o.SpecialContainerRuntimeClass, "special-container-runtime-class", o.SpecialContainerRuntimeClass,
		"the container runtime class that irq tuner will special treatment")
	fs.StringSliceVar(&o.SpecialContainerRuntimeAnnotationKeys, "special-container-runtime-annotation-keys", o.SpecialContainerRuntimeAnnotationKeys,
		"the annotation keys that irq tuner will special treatment")
	fs.StringVar(&o.SpecialContainerRuntimeAnnotationsVal, "special-container-runtime-annotation-vals", o.SpecialContainerRuntimeAnnotationsVal,
		"the annotation values that irq tuner will special treatment")
}

func (o *IRQTunerOptions) ApplyTo(conf *irqtuner.IRQTunerConfiguration) error {
	conf.EnableIRQTuner = o.EnableIRQTuner
	conf.SpecialContainerRuntimeClass = o.SpecialContainerRuntimeClass
	for _, anno := range o.SpecialContainerRuntimeAnnotationKeys {
		conf.SpecialContainerRuntimeAnnotationKeys = append(conf.SpecialContainerRuntimeAnnotationKeys, anno)
	}
	conf.SpecialContainerRuntimeAnnotationsVal = o.SpecialContainerRuntimeAnnotationsVal

	return nil
}
