package common

import (
	"strings"
	"time"

	"github.com/kubewharf/katalyst-core/pkg/consts"
	"github.com/kubewharf/katalyst-core/pkg/util/eventbus"
	"github.com/opencontainers/runc/libcontainer/cgroups"
)

// InstrumentedWriteFileIfChange wraps WriteFileIfChange with audit logic
func InstrumentedWriteFileIfChange(dir, file, data string) (err error, applied bool, oldData string) {
	startTime := time.Now()
	defer func() {
		if applied {
			_ = eventbus.GetDefaultEventBus().Publish(consts.TopicNameApplyProcFS, eventbus.RawCGroupEvent{
				BaseEventImpl: eventbus.BaseEventImpl{
					Time: startTime,
				},
				Cost:       time.Now().Sub(startTime),
				CGroupPath: dir,
				CGroupFile: file,
				Data:       data,
				OldData:    oldData,
			})
		}
	}()

	err, applied, oldData = writeFileIfChange(dir, file, data)
	return
}

// writeFileIfChange writes data to the cgroup joined by dir and
// file if new data is not equal to the old data and return the old data.
func writeFileIfChange(dir, file, data string) (error, bool, string) {
	oldData, err := cgroups.ReadFile(dir, file)
	if err != nil {
		return err, false, ""
	}

	if strings.TrimSpace(data) != strings.TrimSpace(oldData) {
		if err := cgroups.WriteFile(dir, file, data); err != nil {
			return err, false, oldData
		} else {
			return nil, true, oldData
		}
	}
	return nil, false, oldData
}
