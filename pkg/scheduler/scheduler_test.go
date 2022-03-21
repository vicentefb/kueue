/*
Copyright 2022 The Kubernetes Authors.

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

package scheduler

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	logrtesting "github.com/go-logr/logr/testing"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kueue "sigs.k8s.io/kueue/api/v1alpha1"
	"sigs.k8s.io/kueue/pkg/cache"
	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/queue"
	"sigs.k8s.io/kueue/pkg/util/routine"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	"sigs.k8s.io/kueue/pkg/workload"
)

const (
	watchTimeout    = 2 * time.Second
	queueingTimeout = time.Second
)

func TestSchedule(t *testing.T) {
	clusterQueues := []kueue.ClusterQueue{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "sales"},
			Spec: kueue.ClusterQueueSpec{
				NamespaceSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "dep",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"sales"},
						},
					},
				},
				QueueingStrategy: kueue.StrictFIFO,
				RequestableResources: []kueue.Resource{
					{
						Name: corev1.ResourceCPU,
						Flavors: []kueue.Flavor{
							{
								Name: "default",
								Quota: kueue.Quota{
									Guaranteed: resource.MustParse("50"),
									Ceiling:    resource.MustParse("50"),
								},
							},
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "eng-alpha"},
			Spec: kueue.ClusterQueueSpec{
				Cohort: "eng",
				NamespaceSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "dep",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"eng"},
						},
					},
				},
				QueueingStrategy: kueue.StrictFIFO,
				RequestableResources: []kueue.Resource{
					{
						Name: corev1.ResourceCPU,
						Flavors: []kueue.Flavor{
							{
								Name: "on-demand",
								Quota: kueue.Quota{
									Guaranteed: resource.MustParse("50"),
									Ceiling:    resource.MustParse("100"),
								},
							},
							{
								Name: "spot",
								Quota: kueue.Quota{
									Guaranteed: resource.MustParse("100"),
									Ceiling:    resource.MustParse("100"),
								},
							},
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "eng-beta"},
			Spec: kueue.ClusterQueueSpec{
				Cohort: "eng",
				NamespaceSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "dep",
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"eng"},
						},
					},
				},
				QueueingStrategy: kueue.StrictFIFO,
				RequestableResources: []kueue.Resource{
					{
						Name: corev1.ResourceCPU,
						Flavors: []kueue.Flavor{
							{
								Name: "on-demand",
								Quota: kueue.Quota{
									Guaranteed: resource.MustParse("50"),
									Ceiling:    resource.MustParse("60"),
								},
							},
							{
								Name: "spot",
								Quota: kueue.Quota{
									Guaranteed: resource.MustParse("0"),
									Ceiling:    resource.MustParse("100"),
								},
							},
						},
					},
					{
						Name: "example.com/gpu",
						Flavors: []kueue.Flavor{
							{
								Name: "model-a",
								Quota: kueue.Quota{
									Guaranteed: resource.MustParse("20"),
									Ceiling:    resource.MustParse("20"),
								},
							},
						},
					},
				},
			},
		},
	}
	queues := []kueue.Queue{
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "sales",
				Name:      "main",
			},
			Spec: kueue.QueueSpec{
				ClusterQueue: "sales",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "sales",
				Name:      "blocked",
			},
			Spec: kueue.QueueSpec{
				ClusterQueue: "eng-alpha",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "eng-alpha",
				Name:      "main",
			},
			Spec: kueue.QueueSpec{
				ClusterQueue: "eng-alpha",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "eng-beta",
				Name:      "main",
			},
			Spec: kueue.QueueSpec{
				ClusterQueue: "eng-beta",
			},
		},
	}
	cases := map[string]struct {
		workloads []kueue.QueuedWorkload
		// wantAssignments is a summary of all the admissions in the cache after this cycle.
		wantAssignments map[string]kueue.Admission
		// wantScheduled is the subset of workloads that got scheduled/admitted in this cycle.
		wantScheduled []string
		// wantLeft is the workload keys that are left in the queues after this cycle.
		wantLeft map[string]sets.String
	}{
		"workload fits in single clusterQueue": {
			workloads: []kueue.QueuedWorkload{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "sales",
						Name:      "foo",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "main",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 10,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
					},
				},
			},
			wantAssignments: map[string]kueue.Admission{
				"sales/foo": {
					ClusterQueue: "sales",
					PodSetFlavors: []kueue.PodSetFlavors{
						{
							Name: "one",
							Flavors: map[corev1.ResourceName]string{
								corev1.ResourceCPU: "default",
							},
						},
					},
				},
			},
			wantScheduled: []string{"sales/foo"},
		},
		"single clusterQueue full": {
			workloads: []kueue.QueuedWorkload{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "sales",
						Name:      "new",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "main",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 11,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "sales",
						Name:      "assigned",
					},
					Spec: kueue.QueuedWorkloadSpec{
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 40,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
						Admission: &kueue.Admission{
							ClusterQueue: "sales",
							PodSetFlavors: []kueue.PodSetFlavors{
								{
									Name: "one",
									Flavors: map[corev1.ResourceName]string{
										corev1.ResourceCPU: "default",
									},
								},
							},
						},
					},
				},
			},
			wantAssignments: map[string]kueue.Admission{
				"sales/assigned": {
					ClusterQueue: "sales",
					PodSetFlavors: []kueue.PodSetFlavors{
						{
							Name: "one",
							Flavors: map[corev1.ResourceName]string{
								corev1.ResourceCPU: "default",
							},
						},
					},
				},
			},
			wantLeft: map[string]sets.String{
				"sales": sets.NewString("new"),
			},
		},
		"failed to match clusterQueue selector": {
			workloads: []kueue.QueuedWorkload{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "sales",
						Name:      "new",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "blocked",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 1,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
					},
				},
			},
			wantLeft: map[string]sets.String{
				"eng-alpha": sets.NewString("new"),
			},
		},
		"assign to different cohorts": {
			workloads: []kueue.QueuedWorkload{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "sales",
						Name:      "new",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "main",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 1,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "eng-alpha",
						Name:      "new",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "main",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 51, // will borrow.
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
					},
				},
			},
			wantAssignments: map[string]kueue.Admission{
				"sales/new": {
					ClusterQueue: "sales",
					PodSetFlavors: []kueue.PodSetFlavors{
						{
							Name: "one",
							Flavors: map[corev1.ResourceName]string{
								corev1.ResourceCPU: "default",
							},
						},
					},
				},
				"eng-alpha/new": {
					ClusterQueue: "eng-alpha",
					PodSetFlavors: []kueue.PodSetFlavors{
						{
							Name: "one",
							Flavors: map[corev1.ResourceName]string{
								corev1.ResourceCPU: "on-demand",
							},
						},
					},
				},
			},
			wantScheduled: []string{"sales/new", "eng-alpha/new"},
		},
		"assign to same cohort no borrowing": {
			workloads: []kueue.QueuedWorkload{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "eng-alpha",
						Name:      "new",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "main",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 40,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "eng-beta",
						Name:      "new",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "main",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 40,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
					},
				},
			},
			wantAssignments: map[string]kueue.Admission{
				"eng-alpha/new": {
					ClusterQueue: "eng-alpha",
					PodSetFlavors: []kueue.PodSetFlavors{
						{
							Name: "one",
							Flavors: map[corev1.ResourceName]string{
								corev1.ResourceCPU: "on-demand",
							},
						},
					},
				},
				"eng-beta/new": {
					ClusterQueue: "eng-beta",
					PodSetFlavors: []kueue.PodSetFlavors{
						{
							Name: "one",
							Flavors: map[corev1.ResourceName]string{
								corev1.ResourceCPU: "on-demand",
							},
						},
					},
				},
			},
			wantScheduled: []string{"eng-alpha/new", "eng-beta/new"},
		},
		"assign multiple resources and flavors": {
			workloads: []kueue.QueuedWorkload{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "eng-beta",
						Name:      "new",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "main",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 10,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "6", // Needs to borrow.
									"example.com/gpu":  "1",
								}),
							},
							{
								Name:  "two",
								Count: 40,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
					},
				},
			},
			wantAssignments: map[string]kueue.Admission{
				"eng-beta/new": {
					ClusterQueue: "eng-beta",
					PodSetFlavors: []kueue.PodSetFlavors{
						{
							Name: "one",
							Flavors: map[corev1.ResourceName]string{
								corev1.ResourceCPU: "on-demand",
								"example.com/gpu":  "model-a",
							},
						},
						{
							Name: "two",
							Flavors: map[corev1.ResourceName]string{
								corev1.ResourceCPU: "spot",
							},
						},
					},
				},
			},
			wantScheduled: []string{"eng-beta/new"},
		},
		"cannot borrow if cohort was assigned": {
			workloads: []kueue.QueuedWorkload{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "eng-alpha",
						Name:      "new",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "main",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 40,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "eng-beta",
						Name:      "new",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "main",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 51,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
					},
				},
			},
			wantAssignments: map[string]kueue.Admission{
				"eng-alpha/new": {
					ClusterQueue: "eng-alpha",
					PodSetFlavors: []kueue.PodSetFlavors{
						{
							Name: "one",
							Flavors: map[corev1.ResourceName]string{
								corev1.ResourceCPU: "on-demand",
							},
						},
					},
				},
			},
			wantScheduled: []string{"eng-alpha/new"},
			wantLeft: map[string]sets.String{
				"eng-beta": sets.NewString("new"),
			},
		},
		"cannot borrow resource not listed in clusterQueue": {
			workloads: []kueue.QueuedWorkload{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "eng-alpha",
						Name:      "new",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "main",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 1,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									"example.com/gpu": "1",
								}),
							},
						},
					},
				},
			},
			wantLeft: map[string]sets.String{
				"eng-alpha": sets.NewString("new"),
			},
		},
		"not enough resources to borrow, fallback to next flavor": {
			workloads: []kueue.QueuedWorkload{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "eng-alpha",
						Name:      "new",
					},
					Spec: kueue.QueuedWorkloadSpec{
						QueueName: "main",
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 60,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "eng-beta",
						Name:      "existing",
					},
					Spec: kueue.QueuedWorkloadSpec{
						PodSets: []kueue.PodSet{
							{
								Name:  "one",
								Count: 45,
								Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
									corev1.ResourceCPU: "1",
								}),
							},
						},
						Admission: &kueue.Admission{
							ClusterQueue: "eng-beta",
							PodSetFlavors: []kueue.PodSetFlavors{
								{
									Name: "one",
									Flavors: map[corev1.ResourceName]string{
										corev1.ResourceCPU: "on-demand",
									},
								},
							},
						},
					},
				},
			},
			wantAssignments: map[string]kueue.Admission{
				"eng-alpha/new": {
					ClusterQueue: "eng-alpha",
					PodSetFlavors: []kueue.PodSetFlavors{
						{
							Name: "one",
							Flavors: map[corev1.ResourceName]string{
								corev1.ResourceCPU: "spot",
							},
						},
					},
				},
				"eng-beta/existing": {
					ClusterQueue: "eng-beta",
					PodSetFlavors: []kueue.PodSetFlavors{
						{
							Name: "one",
							Flavors: map[corev1.ResourceName]string{
								corev1.ResourceCPU: "on-demand",
							},
						},
					},
				},
			},
			wantScheduled: []string{"eng-alpha/new"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			log := logrtesting.NewTestLoggerWithOptions(t, logrtesting.Options{
				Verbosity: 2,
			})
			ctx := ctrl.LoggerInto(context.Background(), log)
			scheme := runtime.NewScheme()
			if err := kueue.AddToScheme(scheme); err != nil {
				t.Fatalf("Failed adding kueue scheme: %v", err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatalf("Failed adding kueue scheme: %v", err)
			}
			clientBuilder := fake.NewClientBuilder().WithScheme(scheme).
				WithLists(&kueue.QueuedWorkloadList{Items: tc.workloads}, &kueue.QueueList{Items: queues}).
				WithObjects(
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "eng-alpha", Labels: map[string]string{"dep": "eng"}}},
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "eng-beta", Labels: map[string]string{"dep": "eng"}}},
					&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "sales", Labels: map[string]string{"dep": "sales"}}},
				)
			cl := clientBuilder.Build()
			broadcaster := record.NewBroadcaster()
			recorder := broadcaster.NewRecorder(scheme,
				corev1.EventSource{Component: constants.ManagerName})
			qManager := queue.NewManager(cl)
			cqCache := cache.New(cl)
			// Workloads are loaded into queues or clusterQueues as we add them.
			for _, q := range queues {
				if err := qManager.AddQueue(ctx, &q); err != nil {
					t.Fatalf("Inserting queue %s/%s in manager: %v", q.Namespace, q.Name, err)
				}
			}
			for _, cq := range clusterQueues {
				if err := cqCache.AddClusterQueue(ctx, &cq); err != nil {
					t.Fatalf("Inserting clusterQueue %s in cache: %v", cq.Name, err)
				}
				if err := qManager.AddClusterQueue(ctx, &cq); err != nil {
					t.Fatalf("Inserting clusterQueue %s in manager: %v", cq.Name, err)
				}
			}
			workloadWatch, err := cl.Watch(ctx, &kueue.QueuedWorkloadList{})
			if err != nil {
				t.Fatalf("Failed setting up watch: %v", err)
			}
			scheduler := New(qManager, cqCache, cl, recorder)
			wg := sync.WaitGroup{}
			scheduler.setAdmissionRoutineWrapper(routine.NewWrapper(
				func() { wg.Add(1) },
				func() { wg.Done() },
			))

			ctx, cancel := context.WithTimeout(ctx, queueingTimeout)
			go qManager.CleanUpOnContext(ctx)
			defer cancel()
			scheduler.schedule(ctx)

			// Verify assignments in API.
			gotScheduled := make(map[string]kueue.Admission)
			timedOut := false
			for !timedOut && len(gotScheduled) < len(tc.wantScheduled) {
				select {
				case evt := <-workloadWatch.ResultChan():
					w, ok := evt.Object.(*kueue.QueuedWorkload)
					if !ok {
						t.Fatalf("Received update for %T, want QueuedWorkload", evt.Object)
					}
					if w.Spec.Admission != nil {
						gotScheduled[workload.Key(w)] = *w.Spec.Admission
					}
				case <-time.After(watchTimeout):
					t.Errorf("Timed out waiting for QueuedWorkload updates")
					timedOut = true
				}
			}
			wg.Wait()
			wantScheduled := make(map[string]kueue.Admission)
			for _, key := range tc.wantScheduled {
				wantScheduled[key] = tc.wantAssignments[key]
			}
			if diff := cmp.Diff(wantScheduled, gotScheduled); diff != "" {
				t.Errorf("Unexpected scheduled workloads (-want,+got):\n%s", diff)
			}

			// Verify assignments in cache.
			gotAssignments := make(map[string]kueue.Admission)
			snapshot := cqCache.Snapshot()
			for cqName, c := range snapshot.ClusterQueues {
				for name, w := range c.Workloads {
					if w.Obj.Spec.Admission == nil {
						t.Errorf("Workload %s is not admitted by a clusterQueue, but it is found as member of clusterQueue %s in the cache", name, cqName)
					} else if string(w.Obj.Spec.Admission.ClusterQueue) != cqName {
						t.Errorf("Workload %s is admitted by clusterQueue %s, but it is found as member of clusterQueue %s in the cache", name, w.Obj.Spec.Admission.ClusterQueue, cqName)
					}
					gotAssignments[name] = *w.Obj.Spec.Admission
				}
			}
			if len(gotAssignments) == 0 {
				gotAssignments = nil
			}
			if diff := cmp.Diff(tc.wantAssignments, gotAssignments); diff != "" {
				t.Errorf("Unexpected assigned clusterQueues in cache (-want,+got):\n%s", diff)
			}

			qDump := qManager.Dump()
			if diff := cmp.Diff(tc.wantLeft, qDump); diff != "" {
				t.Errorf("Unexpected elements left in the queue (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestEntryAssignFlavors(t *testing.T) {
	cases := map[string]struct {
		wlPods       []kueue.PodSet
		clusterQueue cache.ClusterQueue
		wantFits     bool
		wantFlavors  map[string]map[corev1.ResourceName]string
		wantBorrows  cache.Resources
	}{
		"single flavor, fits": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
						corev1.ResourceCPU:    "1",
						corev1.ResourceMemory: "1Mi",
					}),
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU:    defaultFlavorNoBorrowing(1000),
					corev1.ResourceMemory: defaultFlavorNoBorrowing(2 * utiltesting.Mi),
				},
			},
			wantFits: true,
			wantFlavors: map[string]map[corev1.ResourceName]string{
				"main": {
					corev1.ResourceCPU:    "default",
					corev1.ResourceMemory: "default",
				},
			},
		},
		"single flavor, fits tainted flavor": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
								},
							},
						},
						Tolerations: []corev1.Toleration{corev1.Toleration{
							Key:      "instance",
							Operator: corev1.TolerationOpEqual,
							Value:    "spot",
							Effect:   corev1.TaintEffectNoSchedule,
						}},
					}}},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: noBorrowing([]cache.FlavorLimits{
						{Name: "default", Guaranteed: 4000, Taints: []corev1.Taint{{
							Key:    "instance",
							Value:  "spot",
							Effect: corev1.TaintEffectNoSchedule,
						}}},
					}),
				},
			},
			wantFits: true,
			wantFlavors: map[string]map[corev1.ResourceName]string{
				"main": {
					corev1.ResourceCPU: "default",
				},
			},
		},
		"single flavor, used resources, doesn't fit": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
						corev1.ResourceCPU: "2",
					}),
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: defaultFlavorNoBorrowing(4000),
				},
				UsedResources: cache.Resources{
					corev1.ResourceCPU: {
						"default": 3_000,
					},
				},
			},
		},
		"multiple flavors, fits": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
						corev1.ResourceCPU:    "3",
						corev1.ResourceMemory: "10Mi",
					}),
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: noBorrowing([]cache.FlavorLimits{
						{Name: "one", Guaranteed: 2000},
						{Name: "two", Guaranteed: 4000},
					}),
					corev1.ResourceMemory: noBorrowing([]cache.FlavorLimits{
						{Name: "one", Guaranteed: utiltesting.Gi},
						{Name: "two", Guaranteed: 5 * utiltesting.Mi},
					}),
				},
			},
			wantFits: true,
			wantFlavors: map[string]map[corev1.ResourceName]string{
				"main": {
					corev1.ResourceCPU:    "two",
					corev1.ResourceMemory: "one",
				},
			},
		},
		"multiple flavors, doesn't fit": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
						corev1.ResourceCPU:    "4.1",
						corev1.ResourceMemory: "0.5Gi",
					}),
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: noBorrowing([]cache.FlavorLimits{
						{Name: "one", Guaranteed: 2000},
						{Name: "two", Guaranteed: 4000},
					}),
					corev1.ResourceMemory: noBorrowing([]cache.FlavorLimits{
						{Name: "one", Guaranteed: utiltesting.Gi},
						{Name: "two", Guaranteed: 5 * utiltesting.Mi},
					}),
				},
			},
		},
		"multiple flavors, fits while skipping tainted flavor": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
						corev1.ResourceCPU: "3",
					}),
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: noBorrowing([]cache.FlavorLimits{
						{Name: "one", Guaranteed: 4000, Taints: []corev1.Taint{{
							Key:    "instance",
							Value:  "spot",
							Effect: corev1.TaintEffectNoSchedule,
						}}},
						{Name: "two", Guaranteed: 4000},
					}),
				},
			},
			wantFits: true,
			wantFlavors: map[string]map[corev1.ResourceName]string{
				"main": {
					corev1.ResourceCPU: "two",
				},
			},
		},
		"multiple flavors, fits a node selector": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
								},
							},
						},
						// ignored:foo should get ignored
						NodeSelector: map[string]string{"cpuType": "two", "ignored1": "foo"},
						Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												// this expression should get ignored
												Key:      "ignored2",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"bar"},
											},
										},
									},
								},
							}},
						},
					},
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: noBorrowing([]cache.FlavorLimits{
						{Name: "one", Guaranteed: 4000, Labels: map[string]string{"cpuType": "one"}},
						{Name: "two", Guaranteed: 4000, Labels: map[string]string{"cpuType": "two"}},
					}),
				},
				LabelKeys: map[corev1.ResourceName]sets.String{corev1.ResourceCPU: sets.NewString("cpuType")},
			},
			wantFits: true,
			wantFlavors: map[string]map[corev1.ResourceName]string{
				"main": {
					corev1.ResourceCPU: "two",
				},
			},
		},
		"multiple flavors, fits with node affinity": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("1"),
										corev1.ResourceMemory: resource.MustParse("1Mi"),
									},
								},
							},
						},
						NodeSelector: map[string]string{"ignored1": "foo"},
						Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "cpuType",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"two"},
											},
											{
												Key:      "memType",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"two"},
											},
										},
									},
								},
							}},
						},
					},
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: noBorrowing([]cache.FlavorLimits{
						{Name: "one", Guaranteed: 4000, Labels: map[string]string{"cpuType": "one", "group": "group1"}},
						{Name: "two", Guaranteed: 4000, Labels: map[string]string{"cpuType": "two"}},
					}),
					corev1.ResourceMemory: noBorrowing([]cache.FlavorLimits{
						{Name: "one", Guaranteed: utiltesting.Gi, Labels: map[string]string{"memType": "one"}},
						{Name: "two", Guaranteed: utiltesting.Gi, Labels: map[string]string{"memType": "two"}},
					}),
				},
				LabelKeys: map[corev1.ResourceName]sets.String{
					corev1.ResourceCPU:    sets.NewString("cpuType", "group"),
					corev1.ResourceMemory: sets.NewString("memType"),
				},
			},
			wantFits: true,
			wantFlavors: map[string]map[corev1.ResourceName]string{
				"main": {
					corev1.ResourceCPU:    "two",
					corev1.ResourceMemory: "two",
				},
			},
		},
		"multiple flavors, node affinity fits any flavor": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
								},
							},
						},
						Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "ignored2",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"bar"},
											},
										},
									},
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												// although this terms selects two
												// the first term practically matches
												// any flavor; and since the terms
												// are ORed, any flavor can be selected.
												Key:      "cpuType",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"two"},
											},
										},
									},
								},
							}},
						},
					},
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: noBorrowing([]cache.FlavorLimits{
						{Name: "one", Guaranteed: 4000, Labels: map[string]string{"cpuType": "one"}},
						{Name: "two", Guaranteed: 4000, Labels: map[string]string{"cpuType": "two"}},
					}),
				},
				LabelKeys: map[corev1.ResourceName]sets.String{corev1.ResourceCPU: sets.NewString("cpuType")},
			},
			wantFits: true,
			wantFlavors: map[string]map[corev1.ResourceName]string{
				"main": {
					corev1.ResourceCPU: "one",
				},
			},
		},
		"multiple flavor, doesn't fit node affinity": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
								},
							},
						},
						Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "cpuType",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"three"},
											},
										},
									},
								},
							}},
						},
					},
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: noBorrowing([]cache.FlavorLimits{
						{Name: "one", Guaranteed: 4000, Labels: map[string]string{"cpuType": "one"}},
						{Name: "two", Guaranteed: 4000, Labels: map[string]string{"cpuType": "two"}},
					}),
				},
				LabelKeys: map[corev1.ResourceName]sets.String{corev1.ResourceCPU: sets.NewString("cpuType")},
			},
			wantFits: false,
		},
		"multiple specs, fit different flavors": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "driver",
					Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
						corev1.ResourceCPU: "5",
					}),
				},
				{
					Count: 1,
					Name:  "worker",
					Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
						corev1.ResourceCPU: "3",
					}),
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: noBorrowing([]cache.FlavorLimits{
						{Name: "one", Guaranteed: 4000},
						{Name: "two", Guaranteed: 10_000},
					}),
				},
			},
			wantFits: true,
			wantFlavors: map[string]map[corev1.ResourceName]string{
				"driver": {
					corev1.ResourceCPU: "two",
				},
				"worker": {
					corev1.ResourceCPU: "one",
				},
			},
		},
		"multiple specs, fits borrowing": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "driver",
					Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
						corev1.ResourceCPU:    "4",
						corev1.ResourceMemory: "1Gi",
					}),
				},
				{
					Count: 1,
					Name:  "worker",
					Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
						corev1.ResourceCPU:    "6",
						corev1.ResourceMemory: "4Gi",
					}),
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: {
						{
							Name:       "default",
							Guaranteed: 2000,
							Ceiling:    100_000,
						},
					},
					corev1.ResourceMemory: {
						{
							Name:       "default",
							Guaranteed: 2 * utiltesting.Gi,
							Ceiling:    100 * utiltesting.Gi,
						},
					},
				},
				Cohort: &cache.Cohort{
					RequestableResources: cache.Resources{
						corev1.ResourceCPU: {
							"default": 200_000,
						},
						corev1.ResourceMemory: {
							"default": 200 * utiltesting.Gi,
						},
					},
				},
			},
			wantFits: true,
			wantFlavors: map[string]map[corev1.ResourceName]string{
				"driver": {
					corev1.ResourceCPU:    "default",
					corev1.ResourceMemory: "default",
				},
				"worker": {
					corev1.ResourceCPU:    "default",
					corev1.ResourceMemory: "default",
				},
			},
			wantBorrows: cache.Resources{
				corev1.ResourceCPU: {
					"default": 8_000,
				},
				corev1.ResourceMemory: {
					"default": 3 * utiltesting.Gi,
				},
			},
		},
		"not enough space to borrow": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
						corev1.ResourceCPU: "2",
					}),
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: {
						{
							Name:       "one",
							Guaranteed: 1000,
							Ceiling:    10_000,
						},
					},
				},
				Cohort: &cache.Cohort{
					RequestableResources: cache.Resources{
						corev1.ResourceCPU: {"one": 10_000},
					},
					UsedResources: cache.Resources{
						corev1.ResourceCPU: {"one": 9_000},
					},
				},
			},
		},
		"past ceiling": {
			wlPods: []kueue.PodSet{
				{
					Count: 1,
					Name:  "main",
					Spec: utiltesting.PodSpecForRequest(map[corev1.ResourceName]string{
						corev1.ResourceCPU: "2",
					}),
				},
			},
			clusterQueue: cache.ClusterQueue{
				RequestableResources: map[corev1.ResourceName][]cache.FlavorLimits{
					corev1.ResourceCPU: {
						{
							Name:       "one",
							Guaranteed: 1000,
							Ceiling:    10_000,
						},
					},
				},
				UsedResources: cache.Resources{
					corev1.ResourceCPU: {"one": 9_000},
				},
				Cohort: &cache.Cohort{
					RequestableResources: cache.Resources{
						corev1.ResourceCPU: {"one": 100_000},
					},
					UsedResources: cache.Resources{
						corev1.ResourceCPU: {"one": 9_000},
					},
				},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			log := logrtesting.NewTestLoggerWithOptions(t, logrtesting.Options{
				Verbosity: 2,
			})
			e := entry{
				Info: *workload.NewInfo(&kueue.QueuedWorkload{
					Spec: kueue.QueuedWorkloadSpec{
						PodSets: tc.wlPods,
					},
				}),
			}
			fits := e.assignFlavors(log, &tc.clusterQueue)
			if fits != tc.wantFits {
				t.Errorf("e.assignFlavors(_)=%t, want %t", fits, tc.wantFits)
			}
			var flavors map[string]map[corev1.ResourceName]string
			if fits {
				flavors = make(map[string]map[corev1.ResourceName]string)
				for _, podSet := range e.TotalRequests {
					flavors[podSet.Name] = podSet.Flavors
				}
			}
			if diff := cmp.Diff(tc.wantFlavors, flavors); diff != "" {
				t.Errorf("Assigned unexpected flavors (-want,+got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantBorrows, e.borrows); diff != "" {
				t.Errorf("Calculated unexpected borrowing (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestEntryOrdering(t *testing.T) {
	now := time.Now()
	input := []entry{
		{
			Info: workload.Info{
				Obj: &kueue.QueuedWorkload{ObjectMeta: metav1.ObjectMeta{
					Name:              "alpha",
					CreationTimestamp: metav1.NewTime(now),
				}},
			},
			borrows: cache.Resources{
				corev1.ResourceCPU: {},
			},
		},
		{
			Info: workload.Info{
				Obj: &kueue.QueuedWorkload{ObjectMeta: metav1.ObjectMeta{
					Name:              "beta",
					CreationTimestamp: metav1.NewTime(now.Add(time.Second)),
				}},
			},
		},
		{
			Info: workload.Info{
				Obj: &kueue.QueuedWorkload{ObjectMeta: metav1.ObjectMeta{
					Name:              "gamma",
					CreationTimestamp: metav1.NewTime(now.Add(2 * time.Second)),
				}},
			},
		},
		{
			Info: workload.Info{
				Obj: &kueue.QueuedWorkload{ObjectMeta: metav1.ObjectMeta{
					Name:              "delta",
					CreationTimestamp: metav1.NewTime(now.Add(time.Second)),
				}},
			},
			borrows: cache.Resources{
				corev1.ResourceCPU: {},
			},
		},
	}
	sort.Sort(entryOrdering(input))
	order := make([]string, len(input))
	for i, e := range input {
		order[i] = e.Obj.Name
	}
	wantOrder := []string{"beta", "gamma", "alpha", "delta"}
	if diff := cmp.Diff(wantOrder, order); diff != "" {
		t.Errorf("Unexpected order (-want,+got):\n%s", diff)
	}
}

func defaultFlavorNoBorrowing(guaranteed int64) []cache.FlavorLimits {
	return noBorrowing([]cache.FlavorLimits{{Name: "default", Guaranteed: guaranteed}})
}

func noBorrowing(flavors []cache.FlavorLimits) []cache.FlavorLimits {
	for i := range flavors {
		flavors[i].Ceiling = flavors[i].Guaranteed
	}
	return flavors
}