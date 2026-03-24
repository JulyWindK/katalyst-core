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

package validator

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
)

type AnnotationValidator interface {
	ValidatePodAnnotation(ctx context.Context, podUID, podNameSpace, podName string) (bool, error)
}

type DummyAnnotationValidator struct{}

func (d DummyAnnotationValidator) ValidatePodAnnotation(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}

// ValidatePodAnnotations validates the pod annotations
// if any annotation is forbidden, it returns true and the annotation name
// otherwise, it returns false and empty string
func ValidatePodAnnotations(pod *v1.Pod, forbiddenAnnos map[string]string) (bool, error) {
	if pod == nil || pod.Annotations == nil {
		return true, nil
	}

	for _, anno := range pod.Annotations {
		if _, ok := forbiddenAnnos[anno]; ok {
			return false, fmt.Errorf("the pod contains an invalid annotation: %s", anno)
		}
	}

	return true, nil
}
