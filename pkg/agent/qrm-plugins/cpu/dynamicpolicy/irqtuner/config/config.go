package config

import (
	"flag"
)

const (
	IrqTuningBalanceFair       string = "balance-fair"
	IrqTuningIrqCoresExclusive string = "irq-cores-exclusive"
	IrqTuningAuto              string = "auto"
)

const (
	// no matter how many nics, each nic's irqs affinity all sockets balancely
	EachNicBalanceAllSockets string = "each-nic-sockets-balance"
	// according number of nics and nics's physical topo binded numa, decide nic's irqs affinity which socket(s)
	OverallNicsBalanceAllSockets string = "overall-nics-sockets-balance"
	// nic's irqs affitnied socket strictly follow whose physical topology binded socket
	NicPhysicalTopoBindNuma string = "physical-topo-bind"
)

// when there are one or more irq cores's ratio of softnet_stat 3rd col time_squeeze packets / 1st col processed packets
// greater-equal IrqCoreSoftNetTimeSqueezeRatio,
// then tring to tune irq load balance first, if failed to tune irq load balance, then increase irq cores.
type IrqCoreNetOverloadThresholds struct {
	IrqCoreSoftNetTimeSqueezeRatio float64 // ratio of softnet_stat 3rd col time_squeeze packets / softnet_stat 1st col processed packets
}

// when there are one or more irq cores's cpu util greater-equal IrqCoreCpuUtilThresh or irq cores's net load greater-equal IrqCoreNetOverloadThresholds,
// then tring to tuning irq load balance, that need to find at least one other irq core with relatively low cpu util, their cpu util gap MUST greater-equal IrqCoreCpuUtilGapThresh,
// if succeed to find irq cores with eligible cpu util, then start to tuning load balance,
// or increase irq cores immediately.
type IrqLoadBalanceTuningThresholds struct {
	IrqCoreCpuUtilThresh    int // irq core cpu util threshold, which will trigger irq cores load balance, generally this value should greater-equal IrqCoresExpectedCpuUtil
	IrqCoreCpuUtilGapThresh int // threshold of cpu util gap between source core and dest core of irq affinity changing
}

type IrqLoadBalanceConfig struct {
	SuccessiveTuningInterval    int // interval of two successive irq load balance MUST greater-equal this interval
	Thresholds                  IrqLoadBalanceTuningThresholds
	PingPongIntervalThresh      int // two successive tunes whose interval is less-equal this threshold will be considered as pingpong tunings
	PingPongCountThresh         int // ping pong count greater-equal this threshold will trigger increasing irq cores
	IrqsTunedNumMaxEachTime     int // max number of irqs are permitted to be tuned from some irq cores to other cores in each time, allowed value {1, 2}
	IrqCoresTunedNumMaxEachTime int // max number of irq cores whose affinitied irqs are permitted to tuned to other cores in each time, allowed value {1,2}
}

// when irq cores average cpu util greater-equal IrqCoresAvgCpuUtilThresh, then increase irq cores,
// when there are one or more irq cores's net load greater-equal IrqCoreNetOverloadThresholds, and failed to tune to irq load balance,
// then increase irq cores.
type IrqCoresIncThresholds struct {
	IrqCoresAvgCpuUtilThresh int // threshold of increasing irq cores, generally this thresh equal to or a litter greater-than IrqCoresExpectedCpuUtil
}

// when irq cores cpu util nearly full(e.g., greater-equal 85%), in order to reduce the impact time on the applications, it is necessary to immediately
// fallback to the balance-fair policy first, and later irq tuning manager will auto switch back to IrqCoresExclusive policy based on policies and conditions.
type IrqCoresIncConfig struct {
	SuccessiveIncInterval int // interval of two successive irq cores increase MUST greater-equal this interval
	IrqCoresCpuFullThresh int // when irq cores cpu util hit this thresh, then fallback to balance-fair policy
	Thresholds            IrqCoresIncThresholds
}

// when irq cores average cpu util less-equal IrqCoresAvgCpuUtilThresh, then decrease irq cores.
type IrqCoresDecThresholds struct {
	IrqCoresAvgCpuUtilThresh int // threshold of decreasing irq cores, generally this thresh should be less-than IrqCoresExpectedCpuUtil
}

type IrqCoresDecConfig struct {
	SuccessiveDecInterval    int // interval of two successive irq cores decrease MUST greater-equal this interval
	PingPongAdjustInterval   int // interval of pingpong adjust MUST greater-equal this interval, pingpong adjust means last adjust is increase and current adjust is decrease
	SinceLastBalanceInterval int // interval of decrease and last irq load balance MUST greater-equal this interval
	Thresholds               IrqCoresDecThresholds
	DecCoresMaxEachTime      int // max cores to decrease each time, deault 1
}

type IrqCoresAdjustConfig struct {
	IrqCoresPercentMin int // minimum percent of (100 * irq cores/total(or socket) cores), valid value [0,100], default 2
	IrqCoresPercentMax int // maximum percent of (100 * irq cores/total(or socket) cores), valid value [0,100], default 30
	IrqCoresIncConf    IrqCoresIncConfig
	IrqCoresDecConf    IrqCoresDecConfig
}

// when successive count of nic's total PPS >= RxPPSThresh is greater-equal SuccessiveCount,, then enable exclusion of this nic's irq cores.
type EnableIrqCoresExclusionThresholds struct {
	RxPPSThresh     uint64
	SuccessiveCount int
}

// when successive count of nic's total PPS <= RxPPSThresh is greater-equal SuccessiveCount, then disable exclusion of this nic's irq cores.
type DisableIrqCoresExclusionThresholds struct {
	RxPPSThresh     uint64
	SuccessiveCount int
}

type IrqCoresExclusionThresholds struct {
	EnableThresholds  EnableIrqCoresExclusionThresholds
	DisableThresholds DisableIrqCoresExclusionThresholds
}

type IrqCoresExclusionConfig struct {
	Thresholds               IrqCoresExclusionThresholds
	SuccessiveSwitchInterval float64 // interval of successive enable/disable irq cores exclusion MUST >= SuccessiveSwitchInterval
}

// Configuration for irq-tuning
type IrqTuningConfig struct {
	Interval                 int
	EnableIrqTuning          bool
	IrqTuningPolicy          string
	EnableRPS                bool   // only balance-fair policy support enable rps
	NicAffinitySocketsPolicy string // nics's irqs affinity sockets policy
	IrqCoresExpectedCpuUtil  int
	ReniceIrqCoresKsoftirqd  bool
	IrqCoresKsoftirqdNice    int
	IrqCoreNetOverLoadThresh IrqCoreNetOverloadThresholds
	IrqLoadBalanceConf       IrqLoadBalanceConfig
	IrqCoresAdjustConf       IrqCoresAdjustConfig
	IrqCoresExclusionConf    IrqCoresExclusionConfig
}

func NewConfiguration() *IrqTuningConfig {
	return &IrqTuningConfig{
		Interval:                 5,
		EnableIrqTuning:          false,
		IrqTuningPolicy:          IrqTuningBalanceFair,
		EnableRPS:                false,
		NicAffinitySocketsPolicy: EachNicBalanceAllSockets,
		IrqCoresExpectedCpuUtil:  50,
		ReniceIrqCoresKsoftirqd:  false,
		IrqCoresKsoftirqdNice:    -20,
		IrqCoreNetOverLoadThresh: IrqCoreNetOverloadThresholds{
			IrqCoreSoftNetTimeSqueezeRatio: 0.1,
		},
		IrqLoadBalanceConf: IrqLoadBalanceConfig{
			SuccessiveTuningInterval: 10,
			Thresholds: IrqLoadBalanceTuningThresholds{
				IrqCoreCpuUtilThresh:    65,
				IrqCoreCpuUtilGapThresh: 20,
			},
			PingPongIntervalThresh:      180,
			PingPongCountThresh:         1,
			IrqsTunedNumMaxEachTime:     2,
			IrqCoresTunedNumMaxEachTime: 1,
		},
		IrqCoresAdjustConf: IrqCoresAdjustConfig{
			IrqCoresPercentMin: 2,
			IrqCoresPercentMax: 30,
			IrqCoresIncConf: IrqCoresIncConfig{
				SuccessiveIncInterval: 5,
				IrqCoresCpuFullThresh: 85,
				Thresholds: IrqCoresIncThresholds{
					IrqCoresAvgCpuUtilThresh: 60,
				},
			},
			IrqCoresDecConf: IrqCoresDecConfig{
				SuccessiveDecInterval:    30,
				PingPongAdjustInterval:   300,
				SinceLastBalanceInterval: 60,
				Thresholds: IrqCoresDecThresholds{
					IrqCoresAvgCpuUtilThresh: 40,
				},
				DecCoresMaxEachTime: 1,
			},
		},
		IrqCoresExclusionConf: IrqCoresExclusionConfig{
			Thresholds: IrqCoresExclusionThresholds{
				EnableThresholds: EnableIrqCoresExclusionThresholds{
					RxPPSThresh:     60000,
					SuccessiveCount: 30,
				},
				DisableThresholds: DisableIrqCoresExclusionThresholds{
					RxPPSThresh:     30000,
					SuccessiveCount: 30,
				},
			},
			SuccessiveSwitchInterval: 600,
		},
	}
}

func (c *IrqTuningConfig) Validate() error {
	return nil
}

func (ic *IrqTuningConfig) InitFlags(fs *flag.FlagSet) {
	fs.IntVar(&ic.Interval, "interval", ic.Interval, "irq tuning periodic interval")
	fs.BoolVar(&ic.EnableIrqTuning, "enable-irq-tuning", ic.EnableIrqTuning,
		"if enable irq-tuning feature")
	fs.StringVar(&ic.IrqTuningPolicy, "irq-tuning-policy", ic.IrqTuningPolicy, "irq tuning policy, optional choices [disable, balance-fair, irq-cores-exclusive, auto]")
	fs.BoolVar(&ic.EnableRPS, "enable-rps", ic.EnableRPS,
		"if enable rps, only can eanble rps when irq-tuning-policy is balance-fair")
	fs.StringVar(&ic.NicAffinitySocketsPolicy, "nic-affiniy-sockets-policy", ic.NicAffinitySocketsPolicy, "the policy of decide nic's irqs affinty which socket(s)")
	fs.IntVar(&ic.IrqCoresExpectedCpuUtil, "irq-cores-expected-cpu-util", ic.IrqCoresExpectedCpuUtil,
		"irq cores expeteced cpu util, valid value range [0, 100]")
	fs.BoolVar(&ic.ReniceIrqCoresKsoftirqd, "renice-ksoftirqd", ic.ReniceIrqCoresKsoftirqd,
		"if eable ksoftirqd kthread running on irq cores")
	fs.IntVar(&ic.IrqCoresKsoftirqdNice, "ksoftirqd-nice", ic.IrqCoresKsoftirqdNice,
		"ksoftirqd kthread nice value")

	fs.Float64Var(&ic.IrqCoreNetOverLoadThresh.IrqCoreSoftNetTimeSqueezeRatio, "softnet-timesqueeze-ratio-thresh", ic.IrqCoreNetOverLoadThresh.IrqCoreSoftNetTimeSqueezeRatio,
		"exceed this thresh will trigger irq load balance tuning or increase irq cores")

	fs.IntVar(&ic.IrqLoadBalanceConf.SuccessiveTuningInterval, "irq-load-balance-successive-tune-interval", ic.IrqLoadBalanceConf.SuccessiveTuningInterval,
		"min interval of successvie irq load balance tuning")
	fs.IntVar(&ic.IrqLoadBalanceConf.Thresholds.IrqCoreCpuUtilThresh, "irq-load-balance-cpu-util-thresh", ic.IrqLoadBalanceConf.Thresholds.IrqCoreCpuUtilThresh,
		"irq cores cpu util thresh of triggering irq load balance")
	fs.IntVar(&ic.IrqLoadBalanceConf.Thresholds.IrqCoreCpuUtilGapThresh, "irq-load-balance-cpu-util-gap-thresh", ic.IrqLoadBalanceConf.Thresholds.IrqCoreCpuUtilGapThresh,
		"threshold of cpu util gap between source core and dest core of irq load balance")
	fs.IntVar(&ic.IrqLoadBalanceConf.PingPongIntervalThresh, "irq-load-balance-pingpong-interval-thresh", ic.IrqLoadBalanceConf.PingPongIntervalThresh,
		"two successive tune interval less equal this thresh, then considered as pingpong tuning")
	fs.IntVar(&ic.IrqLoadBalanceConf.PingPongCountThresh, "irq-load-balance-pingpong-count-thresh", ic.IrqLoadBalanceConf.PingPongCountThresh,
		"ping pong count threshold, greater-equal this threshold will trigger increasing irq cores")
	fs.IntVar(&ic.IrqLoadBalanceConf.IrqsTunedNumMaxEachTime, "irq-load-balance-max-irqs", ic.IrqLoadBalanceConf.IrqsTunedNumMaxEachTime,
		"max number of irqs are permitted to be tuned from some irq cores to other cores in each time")
	fs.IntVar(&ic.IrqLoadBalanceConf.IrqCoresTunedNumMaxEachTime, "irq-load-balance-max-cores", ic.IrqLoadBalanceConf.IrqCoresTunedNumMaxEachTime,
		"max number of irq cores whose affinitied irqs are permitted to tuned to other cores in each time")

	fs.IntVar(&ic.IrqCoresAdjustConf.IrqCoresPercentMin, "irq-cores-pct-min", ic.IrqCoresAdjustConf.IrqCoresPercentMin,
		"minimum percent of (100 * irq cores/total(or socket) cores), valid value [0,100], default 2")
	fs.IntVar(&ic.IrqCoresAdjustConf.IrqCoresPercentMax, "irq-cores-pct-max", ic.IrqCoresAdjustConf.IrqCoresPercentMax,
		"maximum percent of (100 * irq cores/total(or socket) cores), valid value [0,100], default 30")

	fs.IntVar(&ic.IrqCoresAdjustConf.IrqCoresIncConf.SuccessiveIncInterval, "irq-cores-inc-successive-interval", ic.IrqCoresAdjustConf.IrqCoresIncConf.SuccessiveIncInterval,
		"min interval of successvie irq cores increase")
	fs.IntVar(&ic.IrqCoresAdjustConf.IrqCoresIncConf.IrqCoresCpuFullThresh, "irq-cores-cpu-full-thresh", ic.IrqCoresAdjustConf.IrqCoresIncConf.IrqCoresCpuFullThresh,
		"when irq cores cpu util hit this thresh, then fallback to balance-fair policy")
	fs.IntVar(&ic.IrqCoresAdjustConf.IrqCoresIncConf.Thresholds.IrqCoresAvgCpuUtilThresh, "irq-cores-inc-cpu-util-thresh", ic.IrqCoresAdjustConf.IrqCoresIncConf.Thresholds.IrqCoresAvgCpuUtilThresh,
		"threshold of increasing irq cores, generally this thresh equal to or a litter greater than IrqCoresExpectedCpuUtil")

	fs.IntVar(&ic.IrqCoresAdjustConf.IrqCoresDecConf.SuccessiveDecInterval, "irq-cores-dec-successive-interval", ic.IrqCoresAdjustConf.IrqCoresDecConf.SuccessiveDecInterval,
		"min interval of successvie irq cores decrease")
	fs.IntVar(&ic.IrqCoresAdjustConf.IrqCoresDecConf.PingPongAdjustInterval, "irq-cores-pingpong-adjust-interval", ic.IrqCoresAdjustConf.IrqCoresDecConf.PingPongAdjustInterval,
		"min interval of pingpong adjust irq cores, pingpong adjust means last adjust is increase and current adjust is decrease")
	fs.IntVar(&ic.IrqCoresAdjustConf.IrqCoresDecConf.SinceLastBalanceInterval, "irq-cores-since-last-balance-dec-interval", ic.IrqCoresAdjustConf.IrqCoresDecConf.SinceLastBalanceInterval,
		"min interval of decrease and last irq load balance")
	fs.IntVar(&ic.IrqCoresAdjustConf.IrqCoresDecConf.Thresholds.IrqCoresAvgCpuUtilThresh, "irq-cores-dec-cpu-util-thresh", ic.IrqCoresAdjustConf.IrqCoresDecConf.Thresholds.IrqCoresAvgCpuUtilThresh,
		"threshold of decreasing irq cores, generally this thresh should be less than IrqCoresExpectedCpuUtil")
	fs.IntVar(&ic.IrqCoresAdjustConf.IrqCoresDecConf.DecCoresMaxEachTime, "irq-cores-dec-max-eachtime", ic.IrqCoresAdjustConf.IrqCoresDecConf.DecCoresMaxEachTime,
		"max cores to decrease each time, deault 1")

	fs.Float64Var(&ic.IrqCoresExclusionConf.SuccessiveSwitchInterval, "irq-cores-exclusion-succesive-switch-interval", ic.IrqCoresExclusionConf.SuccessiveSwitchInterval,
		"interval of successive enable/disable irq cores exclusion MUST >= SuccessiveSwitchInterval")
	fs.Uint64Var(&ic.IrqCoresExclusionConf.Thresholds.EnableThresholds.RxPPSThresh, "irq-cores-exclusion-enable-rx-pps-thresh", ic.IrqCoresExclusionConf.Thresholds.EnableThresholds.RxPPSThresh,
		"threshold of enable exclusion of irq cores from shared-cores, dedicated-cores, .etc.")
	fs.IntVar(&ic.IrqCoresExclusionConf.Thresholds.EnableThresholds.SuccessiveCount, "irq-cores-exclusion-enable-rx-pps-succesive-count", ic.IrqCoresExclusionConf.Thresholds.EnableThresholds.SuccessiveCount,
		"if successive count of nic's total pps >= irq-cores-exclusion-enable-rx-pps-thresh greater-than irq-cores-exclusion-enable-rx-pps-succesive-count, then enable irq cores exclusion")
	fs.Uint64Var(&ic.IrqCoresExclusionConf.Thresholds.DisableThresholds.RxPPSThresh, "irq-cores-exclusion-disable-rx-pps-thresh", ic.IrqCoresExclusionConf.Thresholds.DisableThresholds.RxPPSThresh,
		"threshold of disable exclusion of irq cores from shared-cores, dedicated-cores, .etc.")
	fs.IntVar(&ic.IrqCoresExclusionConf.Thresholds.DisableThresholds.SuccessiveCount, "irq-cores-exclusion-disable-rx-pps-succesive-count", ic.IrqCoresExclusionConf.Thresholds.DisableThresholds.SuccessiveCount,
		"if successive count of nic's total pps <= irq-cores-exclusion-disable-rx-pps-thresh greater-than irq-cores-exclusion-disable-rx-pps-succesive-count, then disable irq cores exclusion")
}
