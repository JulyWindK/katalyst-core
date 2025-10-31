package strategy

import (
	"fmt"

	constsapi "github.com/kubewharf/katalyst-api/pkg/consts"
	v1 "k8s.io/api/core/v1"
)

func ParseNumaIDFormPod(pod *v1.Pod) (string, error) {
	if pod == nil {
		return "", fmt.Errorf("pod is nil")
	}

	if pod.Annotations == nil {
		return "", fmt.Errorf("pod anno is nil ")
	}

	numaID, ok := pod.Annotations[constsapi.PodAnnotationNUMABindResultKey]
	if !ok {
		return "", fmt.Errorf("pod numa binding annotation does not exist")
	}

	return numaID, nil
}
