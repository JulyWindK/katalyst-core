package qrm

import (
	"github.com/kubewharf/katalyst-core/pkg/config/agent/dynamic/adminqos/qrm"
	"k8s.io/apimachinery/pkg/util/errors"
	cliflag "k8s.io/component-base/cli/flag"
)

type QRMPluginOptions struct {
	*CPUPluginOptions
}

func NewQRMPluginOptions() *QRMPluginOptions {
	return &QRMPluginOptions{
		CPUPluginOptions: NewCPUPluginOptions(),
	}
}

func (o *QRMPluginOptions) AddFlags(fss *cliflag.NamedFlagSets) {
	o.CPUPluginOptions.AddFlags(fss)
}

func (o *QRMPluginOptions) ApplyTo(c *qrm.QRMPluginConfiguration) error {
	var errList []error

	errList = append(errList, o.CPUPluginOptions.ApplyTo(c.CPUPluginConfiguration))
	return errors.NewAggregate(errList)
}
