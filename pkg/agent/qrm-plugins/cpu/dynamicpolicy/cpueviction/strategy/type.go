package strategy

type NumaSysCPUPressureEvictionConfig struct {
	EnableEviction bool
	MetricRingSize int
	GracePeriod    int64
	SyncPeriod     int64

	ThresholdMetPercentage                 float64
	NumaCPUUsageSoftThreshold              float64
	NumaCPUUsageHardThreshold              float64
	NumaSysOverTotalUsageSoftThreshold     float64
	NumaSysOverTotalUsageHardThreshold     float64
	NumaSysOverTotalUsageEvictionThreshold float64
}
