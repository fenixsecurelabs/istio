// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package repair

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"golang.org/x/exp/slices"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"istio.io/istio/cni/pkg/config"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/tools/istio-iptables/pkg/constants"
	"istio.io/pkg/monitoring"
)

func TestMatchesFilter(t *testing.T) {
	makeDetectPod := func(name string, terminationMessage string, exitCode int) *corev1.Pod {
		return makePod(makePodArgs{
			PodName:     name,
			Annotations: map[string]string{"sidecar.istio.io/status": "something"},
			InitContainerStatus: &corev1.ContainerStatus{
				Name: constants.ValidationContainerName,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "CrashLoopBackOff",
						Message: "Back-off 5m0s restarting failed blah blah blah",
					},
				},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Message:  terminationMessage,
						ExitCode: int32(exitCode),
					},
				},
			},
		})
	}

	cases := []struct {
		name   string
		config config.RepairConfig
		pod    *corev1.Pod
		want   bool
	}{
		{
			"Testing OK pod with only ExitCode check",
			config.RepairConfig{
				SidecarAnnotation: "sidecar.istio.io/status",
				InitContainerName: constants.ValidationContainerName,
				InitExitCode:      126,
			},
			workingPod,
			false,
		},
		{
			"Testing working pod (that previously died) with only ExitCode check",
			config.RepairConfig{
				SidecarAnnotation: "sidecar.istio.io/status",
				InitContainerName: constants.ValidationContainerName,
				InitExitCode:      126,
			},
			workingPodDiedPreviously,
			false,
		},
		{
			"Testing broken pod (in waiting state) with only ExitCode check",
			config.RepairConfig{
				SidecarAnnotation: "sidecar.istio.io/status",
				InitContainerName: constants.ValidationContainerName,
				InitExitCode:      126,
			},
			brokenPodWaiting,
			true,
		},
		{
			"Testing broken pod (in terminating state) with only ExitCode check",
			config.RepairConfig{
				SidecarAnnotation: "sidecar.istio.io/status",
				InitContainerName: constants.ValidationContainerName,
				InitExitCode:      126,
			},
			brokenPodTerminating,
			true,
		},
		{
			"Testing broken pod with wrong ExitCode",
			config.RepairConfig{
				SidecarAnnotation: "sidecar.istio.io/status",
				InitContainerName: constants.ValidationContainerName,
				InitExitCode:      55,
			},
			brokenPodWaiting,
			false,
		},
		{
			"Testing broken pod with no annotation (should be ignored)",
			config.RepairConfig{
				SidecarAnnotation: "sidecar.istio.io/status",
				InitContainerName: constants.ValidationContainerName,
				InitExitCode:      126,
			},
			brokenPodNoAnnotation,
			false,
		},
		{
			"Check termination message match false",
			config.RepairConfig{
				SidecarAnnotation:  "sidecar.istio.io/status",
				InitContainerName:  constants.ValidationContainerName,
				InitTerminationMsg: "Termination Message",
			},
			makeDetectPod(
				"TerminationMessageMatchFalse",
				"This Does Not Match",
				0),
			false,
		},
		{
			"Check termination message match true",
			config.RepairConfig{
				SidecarAnnotation:  "sidecar.istio.io/status",
				InitContainerName:  constants.ValidationContainerName,
				InitTerminationMsg: "Termination Message",
			},
			makeDetectPod(
				"TerminationMessageMatchTrue",
				"Termination Message",
				0),
			true,
		},
		{
			"Check termination message match true for trailing and leading space",
			config.RepairConfig{
				SidecarAnnotation:  "sidecar.istio.io/status",
				InitContainerName:  constants.ValidationContainerName,
				InitTerminationMsg: "            Termination Message",
			},
			makeDetectPod(
				"TerminationMessageMatchTrueLeadingSpace",
				"Termination Message              ",
				0),
			true,
		},
		{
			"Check termination code match false",
			config.RepairConfig{
				SidecarAnnotation: "sidecar.istio.io/status",
				InitContainerName: constants.ValidationContainerName,
				InitExitCode:      126,
			},
			makeDetectPod(
				"TerminationCodeMatchFalse",
				"",
				121),
			false,
		},
		{
			"Check termination code match true",
			config.RepairConfig{
				SidecarAnnotation: "sidecar.istio.io/status",
				InitContainerName: constants.ValidationContainerName,
				InitExitCode:      126,
			},
			makeDetectPod(
				"TerminationCodeMatchTrue",
				"",
				126),
			true,
		},
		{
			"Check badly formatted pod",
			config.RepairConfig{
				SidecarAnnotation: "sidecar.istio.io/status",
				InitContainerName: constants.ValidationContainerName,
				InitExitCode:      126,
			},
			makePod(makePodArgs{
				PodName:             "Test",
				Annotations:         map[string]string{"sidecar.istio.io/status": "something"},
				InitContainerStatus: &corev1.ContainerStatus{},
			}),
			false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := &Controller{cfg: tt.config}
			assert.Equal(t, c.matchesFilter(tt.pod), tt.want)
		})
	}
}

func fakeClient(pods ...*corev1.Pod) kube.Client {
	var csPods []runtime.Object

	for _, pod := range pods {
		csPods = append(csPods, pod.DeepCopy())
	}
	return kube.NewFakeClient(csPods...)
}

func makePodLabelMap(pods []*corev1.Pod) (podmap map[string]string) {
	podmap = map[string]string{}
	for _, pod := range pods {
		podmap[pod.Name] = ""
		for key, value := range pod.Labels {
			podmap[pod.Name] = strings.Join([]string{podmap[pod.Name], fmt.Sprintf("%s=%s", key, value)}, ",")
		}
		podmap[pod.Name] = strings.Trim(podmap[pod.Name], " ,")
	}
	return
}

func TestLabelPods(t *testing.T) {
	tests := []struct {
		name       string
		client     kube.Client
		config     config.RepairConfig
		wantLabels map[string]string
		wantCount  float64
		wantTags   []tag.Tag
	}{
		{
			name:   "No broken pods",
			client: fakeClient(workingPod, workingPodDiedPreviously),
			config: config.RepairConfig{
				InitContainerName:  constants.ValidationContainerName,
				InitExitCode:       126,
				InitTerminationMsg: "Died for some reason",
				LabelKey:           "testkey",
				LabelValue:         "testval",
			},
			wantLabels: map[string]string{workingPod.Name: "", workingPodDiedPreviously.Name: ""},
			wantCount:  0,
		},
		{
			name:   "With broken pods",
			client: fakeClient(workingPod, workingPodDiedPreviously, brokenPodWaiting),
			config: config.RepairConfig{
				InitContainerName:  constants.ValidationContainerName,
				InitExitCode:       126,
				InitTerminationMsg: "Died for some reason",
				LabelKey:           "testkey",
				LabelValue:         "testval",
			},
			wantLabels: map[string]string{workingPod.Name: "", workingPodDiedPreviously.Name: "", brokenPodWaiting.Name: "testkey=testval"},
			wantCount:  1,
			wantTags:   []tag.Tag{{Key: tag.Key(resultLabel), Value: resultSuccess}, {Key: tag.Key(typeLabel), Value: labelType}},
		},
		{
			name:   "With already labeled pod",
			client: fakeClient(workingPod, workingPodDiedPreviously, brokenPodTerminating),
			config: config.RepairConfig{
				InitContainerName:  constants.ValidationContainerName,
				InitExitCode:       126,
				InitTerminationMsg: "Died for some reason",
				LabelKey:           "testlabel",
				LabelValue:         "true",
			},
			wantLabels: map[string]string{workingPod.Name: "", workingPodDiedPreviously.Name: "", brokenPodTerminating.Name: "testlabel=true"},
			wantCount:  1,
			wantTags:   []tag.Tag{{Key: tag.Key(resultLabel), Value: resultSkip}, {Key: tag.Key(typeLabel), Value: labelType}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.config.LabelPods = true
			exp := initStats(tt.name)
			c, err := NewRepairController(tt.client, tt.config)
			assert.NoError(t, err)
			t.Cleanup(func() {
				assert.NoError(t, c.queue.WaitForClose(time.Second))
			})
			stop := test.NewStop(t)
			tt.client.RunAndWait(stop)
			go c.Run(stop)
			kube.WaitForCacheSync("test", stop, c.queue.HasSynced)

			assert.EventuallyEqual(t, func() map[string]string {
				havePods := c.pods.List(metav1.NamespaceAll, klabels.Everything())
				slices.SortFunc(havePods, func(a, b *corev1.Pod) bool {
					return a.Name < b.Name
				})
				return makePodLabelMap(havePods)
			}, tt.wantLabels)
			checkStats(t, tt.wantCount, tt.wantTags, exp)
		})
	}
}

func TestDeletePods(t *testing.T) {
	tests := []struct {
		name      string
		client    kube.Client
		config    config.RepairConfig
		wantErr   bool
		wantPods  []*corev1.Pod
		wantCount float64
		wantTags  []tag.Tag
	}{
		{
			name:   "No broken pods",
			client: fakeClient(workingPod, workingPodDiedPreviously),
			config: config.RepairConfig{
				InitContainerName:  constants.ValidationContainerName,
				InitExitCode:       126,
				InitTerminationMsg: "Died for some reason",
			},
			wantPods:  []*corev1.Pod{workingPod, workingPodDiedPreviously},
			wantErr:   false,
			wantCount: 0,
		},
		{
			name:   "With broken pods",
			client: fakeClient(workingPod, workingPodDiedPreviously, brokenPodWaiting),
			config: config.RepairConfig{
				InitContainerName:  constants.ValidationContainerName,
				InitExitCode:       126,
				InitTerminationMsg: "Died for some reason",
			},
			wantPods:  []*corev1.Pod{workingPod, workingPodDiedPreviously},
			wantErr:   false,
			wantCount: 1,
			wantTags:  []tag.Tag{{Key: tag.Key(resultLabel), Value: resultSuccess}, {Key: tag.Key(typeLabel), Value: deleteType}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.config.DeletePods = true
			exp := initStats(tt.name)
			c, err := NewRepairController(tt.client, tt.config)
			assert.NoError(t, err)
			t.Cleanup(func() {
				assert.NoError(t, c.queue.WaitForClose(time.Second))
			})
			stop := test.NewStop(t)
			tt.client.RunAndWait(stop)
			go c.Run(stop)
			kube.WaitForCacheSync("test", stop, c.queue.HasSynced)

			assert.EventuallyEqual(t, func() []*corev1.Pod {
				havePods := c.pods.List(metav1.NamespaceAll, klabels.Everything())
				slices.SortFunc(havePods, func(a, b *corev1.Pod) bool {
					return a.Name < b.Name
				})
				return havePods
			}, tt.wantPods)
			checkStats(t, tt.wantCount, tt.wantTags, exp)
		})
	}
}

type testExporter struct {
	sync.Mutex

	rows        map[string][]*view.Row
	invalidTags bool
}

func (t *testExporter) ExportView(d *view.Data) {
	t.Lock()
	for _, tk := range d.View.TagKeys {
		if len(tk.Name()) < 1 {
			t.invalidTags = true
		}
	}
	t.rows[d.View.Name] = append(t.rows[d.View.Name], d.Rows...)
	t.Unlock()
}

// returns 0 when the metric has not been incremented.
func readFloat64(exp *testExporter, metric monitoring.Metric, tags []tag.Tag) float64 {
	exp.Lock()
	defer exp.Unlock()
	for _, r := range exp.rows[metric.Name()] {
		if !reflect.DeepEqual(r.Tags, tags) {
			continue
		}
		if sd, ok := r.Data.(*view.SumData); ok {
			return sd.Value
		}
	}
	return 0
}

func initStats(name string) *testExporter {
	podsRepaired = monitoring.NewSum(name, "", monitoring.WithLabels(typeLabel, resultLabel))
	monitoring.MustRegister(podsRepaired)
	exp := &testExporter{rows: make(map[string][]*view.Row)}
	view.RegisterExporter(exp)
	view.SetReportingPeriod(1 * time.Millisecond)
	return exp
}

func checkStats(t test.Failer, wantCount float64, wantTags []tag.Tag, exp *testExporter) {
	assert.EventuallyEqual(t, func() float64 {
		return readFloat64(exp, podsRepaired, wantTags)
	}, wantCount, retry.Message(fmt.Sprintf("wanted tags %v", wantTags)))
}
