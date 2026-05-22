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

package spd

import (
	"context"
	"io/ioutil"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/kubewharf/katalyst-api/pkg/apis/config/v1alpha1"
	workloadapis "github.com/kubewharf/katalyst-api/pkg/apis/workload/v1alpha1"
	externalfake "github.com/kubewharf/katalyst-api/pkg/client/clientset/versioned/fake"
	"github.com/kubewharf/katalyst-api/pkg/consts"
	katalyst_base "github.com/kubewharf/katalyst-core/cmd/base"
	pkgconfig "github.com/kubewharf/katalyst-core/pkg/config"
	pkgconsts "github.com/kubewharf/katalyst-core/pkg/consts"
	"github.com/kubewharf/katalyst-core/pkg/metaserver/agent/cnc"
	"github.com/kubewharf/katalyst-core/pkg/metrics"
)

func generateTestConfiguration(t *testing.T, nodeName string, checkpoint string) *pkgconfig.Configuration {
	testConfiguration := pkgconfig.NewConfiguration()
	require.NotNil(t, testConfiguration)

	testConfiguration.NodeName = nodeName
	testConfiguration.ServiceProfileCacheTTL = 1 * time.Minute
	testConfiguration.CheckpointManagerDir = checkpoint
	testConfiguration.ServiceProfileEnableNamespaces = []string{"*"}
	testConfiguration.SPDGetFromRemote = true
	return testConfiguration
}

func Test_spdManager_GetSPD(t *testing.T) {
	t.Parallel()

	type fields struct {
		nodeName string
		spd      *workloadapis.ServiceProfileDescriptor
		cnc      *v1alpha1.CustomNodeConfig
	}
	type args struct {
		pod *v1.Pod
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    *workloadapis.ServiceProfileDescriptor
		wantErr bool
	}{
		{
			name: "test-1",
			fields: fields{
				nodeName: "node-1",
				spd: &workloadapis.ServiceProfileDescriptor{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "spd-1",
						Namespace: "default",
						Annotations: map[string]string{
							pkgconsts.ServiceProfileDescriptorAnnotationKeyConfigHash: "3c7e3ff3f218",
						},
					},
				},
				cnc: &v1alpha1.CustomNodeConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-1",
					},
					Status: v1alpha1.CustomNodeConfigStatus{
						ServiceProfileConfigList: []v1alpha1.TargetConfig{
							{
								ConfigName:      "spd-1",
								ConfigNamespace: "default",
								Hash:            "3c7e3ff3f218",
							},
						},
					},
				},
			},
			args: args{
				pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-1",
						Namespace: "default",
						Annotations: map[string]string{
							consts.PodAnnotationSPDNameKey: "spd-1",
						},
					},
				},
			},
			want: &workloadapis.ServiceProfileDescriptor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "spd-1",
					Namespace: "default",
					Annotations: map[string]string{
						pkgconsts.ServiceProfileDescriptorAnnotationKeyConfigHash: "3c7e3ff3f218",
					},
				},
			},
		},
		{
			name: "test-2",
			fields: fields{
				nodeName: "node-1",
				spd: &workloadapis.ServiceProfileDescriptor{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "spd-1",
						Namespace: "default",
						Annotations: map[string]string{
							pkgconsts.ServiceProfileDescriptorAnnotationKeyConfigHash: "3c7e3ff3f218",
						},
					},
					Spec: workloadapis.ServiceProfileDescriptorSpec{
						BusinessIndicator: []workloadapis.ServiceBusinessIndicatorSpec{
							{
								Name: workloadapis.ServiceBusinessIndicatorNameRPCLatency,
							},
						},
					},
				},
				cnc: &v1alpha1.CustomNodeConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-1",
					},
					Status: v1alpha1.CustomNodeConfigStatus{
						ServiceProfileConfigList: []v1alpha1.TargetConfig{
							{
								ConfigName:      "spd-1",
								ConfigNamespace: "default",
								Hash:            "3c7e3ff3f218",
							},
						},
					},
				},
			},
			args: args{
				pod: &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-1",
						Namespace: "default",
						Annotations: map[string]string{
							consts.PodAnnotationSPDNameKey: "spd-1",
						},
					},
				},
			},
			want: &workloadapis.ServiceProfileDescriptor{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "spd-1",
					Namespace: "default",
					Annotations: map[string]string{
						pkgconsts.ServiceProfileDescriptorAnnotationKeyConfigHash: "3c7e3ff3f218",
					},
				},
				Spec: workloadapis.ServiceProfileDescriptorSpec{
					BusinessIndicator: []workloadapis.ServiceBusinessIndicatorSpec{
						{
							Name: workloadapis.ServiceBusinessIndicatorNameRPCLatency,
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir, err := ioutil.TempDir("", "checkpoint-Test_spdManager_GetSPD")
			require.NoError(t, err)
			defer os.RemoveAll(dir)

			conf := generateTestConfiguration(t, tt.fields.nodeName, dir)
			genericCtx, err := katalyst_base.GenerateFakeGenericContext(nil, []runtime.Object{
				tt.fields.spd,
				tt.fields.cnc,
			})
			require.NoError(t, err)

			cncFetcher := cnc.NewCachedCNCFetcher(conf.BaseConfiguration, conf.CNCConfiguration, genericCtx.Client.InternalClient.ConfigV1alpha1().CustomNodeConfigs())
			s, err := NewSPDFetcher(genericCtx.Client, metrics.DummyMetrics{}, cncFetcher, conf)
			require.NoError(t, err)
			require.NotNil(t, s)

			ctx := context.TODO()

			_, _ = s.GetSPD(ctx, tt.args.pod.ObjectMeta)
			go s.Run(ctx)
			time.Sleep(1 * time.Second)

			got, err := s.GetSPD(ctx, tt.args.pod.ObjectMeta)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetSPD() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			require.Equal(t, tt.want.Spec, got.Spec)
			require.Equal(t, tt.want.Status, got.Status)

			// second GetSPD from local cache
			got, err = s.GetSPD(ctx, tt.args.pod.ObjectMeta)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetSPD() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			require.Equal(t, tt.want.Spec, got.Spec)
			require.Equal(t, tt.want.Status, got.Status)
		})
	}
}

// fakeStaticCNCFetcher is a CNCFetcher that always returns a fixed CNC.
// Used to drive the agent's default-SPD hash limiter from a known CNC state.
type fakeStaticCNCFetcher struct {
	cnc *v1alpha1.CustomNodeConfig
	err error
}

func (f *fakeStaticCNCFetcher) GetCNC(_ context.Context) (*v1alpha1.CustomNodeConfig, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cnc, nil
}

func newDefaultSPDFetcherForTest(
	t *testing.T,
	defaultSPD *workloadapis.ServiceProfileDescriptor,
	cncObj *v1alpha1.CustomNodeConfig,
	defaultSPDNamespace, defaultSPDName string,
) (*spdFetcher, *int64) {
	t.Helper()

	dir, err := ioutil.TempDir("", "checkpoint-default-spd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	conf := generateTestConfiguration(t, "node-1", dir)
	conf.EnableDefaultSPDFallback = true
	conf.DefaultSPDNamespace = defaultSPDNamespace
	conf.DefaultSPDName = defaultSPDName

	internalObjs := []runtime.Object{}
	if defaultSPD != nil {
		internalObjs = append(internalObjs, defaultSPD)
	}
	if cncObj != nil {
		internalObjs = append(internalObjs, cncObj)
	}

	genericCtx, err := katalyst_base.GenerateFakeGenericContext(nil, internalObjs)
	require.NoError(t, err)

	// Count Get-spd calls so tests can assert hash-limited remote fetches.
	var getCount int64
	fakeClient, ok := genericCtx.Client.InternalClient.(*externalfake.Clientset)
	require.True(t, ok, "expected fake internal clientset")
	fakeClient.PrependReactor("get", "serviceprofiledescriptors", func(action clienttesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt64(&getCount, 1)
		// fall through to default tracker so the actual object is returned/NotFound
		return false, nil, nil
	})

	cncFetcher := &fakeStaticCNCFetcher{cnc: cncObj}
	s, err := NewSPDFetcher(genericCtx.Client, metrics.DummyMetrics{}, cncFetcher, conf)
	require.NoError(t, err)

	sf, ok := s.(*spdFetcher)
	require.True(t, ok)
	return sf, &getCount
}

func TestSPDFetcher_RefreshDefaultSPD_HashHit(t *testing.T) {
	t.Parallel()

	defaultSPD := &workloadapis.ServiceProfileDescriptor{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "katalyst-system",
			Name:      "default-spd",
			Annotations: map[string]string{
				pkgconsts.ServiceProfileDescriptorAnnotationKeyConfigHash: "hash-v1",
			},
		},
	}
	cncObj := &v1alpha1.CustomNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: v1alpha1.CustomNodeConfigStatus{
			DefaultServiceProfileConfig: &v1alpha1.TargetConfig{
				ConfigNamespace: "katalyst-system",
				ConfigName:      "default-spd",
				Hash:            "hash-v1",
			},
		},
	}

	sf, getCount := newDefaultSPDFetcherForTest(t, defaultSPD, cncObj, "katalyst-system", "default-spd")

	// Pre-seed the in-memory snapshot with a matching hash, so refresh should
	// short-circuit on the hash limiter and skip the remote Get.
	sf.storeDefaultSPD(defaultSPD)

	sf.refreshDefaultSPD(context.TODO())
	require.EqualValues(t, 0, atomic.LoadInt64(getCount), "expected no remote Get when CNC hash matches local snapshot")
}

func TestSPDFetcher_RefreshDefaultSPD_HashMismatch(t *testing.T) {
	t.Parallel()

	remoteSPD := &workloadapis.ServiceProfileDescriptor{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "katalyst-system",
			Name:      "default-spd",
			Annotations: map[string]string{
				pkgconsts.ServiceProfileDescriptorAnnotationKeyConfigHash: "hash-v2",
			},
		},
	}
	cncObj := &v1alpha1.CustomNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: v1alpha1.CustomNodeConfigStatus{
			DefaultServiceProfileConfig: &v1alpha1.TargetConfig{
				ConfigNamespace: "katalyst-system",
				ConfigName:      "default-spd",
				Hash:            "hash-v2",
			},
		},
	}

	sf, getCount := newDefaultSPDFetcherForTest(t, remoteSPD, cncObj, "katalyst-system", "default-spd")

	// Pre-seed the snapshot with a stale hash; refresh should fetch remote.
	stale := remoteSPD.DeepCopy()
	stale.Annotations[pkgconsts.ServiceProfileDescriptorAnnotationKeyConfigHash] = "hash-v1"
	sf.storeDefaultSPD(stale)

	sf.refreshDefaultSPD(context.TODO())
	require.EqualValues(t, 1, atomic.LoadInt64(getCount), "expected one remote Get when CNC hash differs from snapshot")

	got := sf.loadDefaultSPD()
	require.NotNil(t, got)
	require.Equal(t, "hash-v2", got.Annotations[pkgconsts.ServiceProfileDescriptorAnnotationKeyConfigHash])
}

func TestSPDFetcher_RefreshDefaultSPD_CNCFieldNil(t *testing.T) {
	t.Parallel()

	remoteSPD := &workloadapis.ServiceProfileDescriptor{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "katalyst-system",
			Name:      "default-spd",
			Annotations: map[string]string{
				pkgconsts.ServiceProfileDescriptorAnnotationKeyConfigHash: "hash-v1",
			},
		},
	}
	// CNC has not yet propagated default SPD identity (controller not synced).
	cncObj := &v1alpha1.CustomNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
	}

	sf, getCount := newDefaultSPDFetcherForTest(t, remoteSPD, cncObj, "katalyst-system", "default-spd")

	sf.refreshDefaultSPD(context.TODO())
	require.EqualValues(t, 1, atomic.LoadInt64(getCount), "expected fallback remote Get when CNC default field is nil")
	require.NotNil(t, sf.loadDefaultSPD())
}

func TestSPDFetcher_RefreshDefaultSPD_ColdStart(t *testing.T) {
	t.Parallel()

	remoteSPD := &workloadapis.ServiceProfileDescriptor{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "katalyst-system",
			Name:      "default-spd",
			Annotations: map[string]string{
				pkgconsts.ServiceProfileDescriptorAnnotationKeyConfigHash: "hash-v1",
			},
		},
	}
	cncObj := &v1alpha1.CustomNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: v1alpha1.CustomNodeConfigStatus{
			DefaultServiceProfileConfig: &v1alpha1.TargetConfig{
				ConfigNamespace: "katalyst-system",
				ConfigName:      "default-spd",
				Hash:            "hash-v1",
			},
		},
	}

	sf, getCount := newDefaultSPDFetcherForTest(t, remoteSPD, cncObj, "katalyst-system", "default-spd")

	// In-memory snapshot is nil at startup; refresh should fetch even though
	// the CNC carries a hash, because the local snapshot has nothing to compare.
	sf.refreshDefaultSPD(context.TODO())
	require.EqualValues(t, 1, atomic.LoadInt64(getCount), "expected remote Get on cold start when snapshot is nil")
	got := sf.loadDefaultSPD()
	require.NotNil(t, got)
	require.Equal(t, "default-spd", got.Name)
}

func TestSPDFetcher_RefreshDefaultSPD_NotFoundClearsSnapshot(t *testing.T) {
	t.Parallel()

	cncObj := &v1alpha1.CustomNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: v1alpha1.CustomNodeConfigStatus{
			DefaultServiceProfileConfig: &v1alpha1.TargetConfig{
				ConfigNamespace: "katalyst-system",
				ConfigName:      "default-spd",
				Hash:            "hash-v1",
			},
		},
	}

	// no default SPD in remote -> Get returns NotFound
	sf, getCount := newDefaultSPDFetcherForTest(t, nil, cncObj, "katalyst-system", "default-spd")

	stale := &workloadapis.ServiceProfileDescriptor{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "katalyst-system",
			Name:      "default-spd",
			Annotations: map[string]string{
				pkgconsts.ServiceProfileDescriptorAnnotationKeyConfigHash: "hash-v0",
			},
		},
	}
	sf.storeDefaultSPD(stale)

	sf.refreshDefaultSPD(context.TODO())
	require.EqualValues(t, 1, atomic.LoadInt64(getCount))
	require.Nil(t, sf.loadDefaultSPD(), "remote NotFound should clear in-memory snapshot")
}

func TestSPDFetcher_RefreshDefaultSPD_IdentityMismatch(t *testing.T) {
	t.Parallel()

	remoteSPD := &workloadapis.ServiceProfileDescriptor{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "katalyst-system",
			Name:      "default-spd",
		},
	}
	// CNC reports a different identity than agent config; agent should refuse to refresh.
	cncObj := &v1alpha1.CustomNodeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: v1alpha1.CustomNodeConfigStatus{
			DefaultServiceProfileConfig: &v1alpha1.TargetConfig{
				ConfigNamespace: "other-ns",
				ConfigName:      "other-spd",
				Hash:            "hash-x",
			},
		},
	}

	sf, getCount := newDefaultSPDFetcherForTest(t, remoteSPD, cncObj, "katalyst-system", "default-spd")
	sf.refreshDefaultSPD(context.TODO())
	require.EqualValues(t, 0, atomic.LoadInt64(getCount), "identity mismatch must not trigger remote Get")
	require.Nil(t, sf.loadDefaultSPD())
}
