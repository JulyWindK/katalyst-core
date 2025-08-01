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

package types

type MalachiteSysFreqResponse struct {
	Status int         `json:"status"`
	Data   SysFreqData `json:"data"`
}

type SysFreqData struct {
	SysFreq SysFreq `json:"sysfreq"`
}

type SysFreq struct {
	CPUFreq    []CurFreq `json:"cpu_freq"`
	UpdateTime int64     `json:"update_time"`
}

type CurFreq struct {
	ScalingCurFreqKHZ int64 `json:"scaling_cur_freq"`
}
