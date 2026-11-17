/*
Copyright © 2026 SUSE LLC
SPDX-License-Identifier: Apache-2.0

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

package controller

import (
	"testing"

	imagev1 "github.com/suse/elemental-lifecycle-manager/image-operator/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		in   Observation
		want Decision
	}{
		{
			name: "image already on the target, request never built: cache hit",
			in:   Observation{Image: target.ImageReady, Builder: target.BuildAbsent, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseReady, Action: ActNone, Reason: imagev1.ReasonCacheHit, CacheHit: true},
		},
		{
			name: "image imported after this request's build: ready, builder removed, not a hit",
			in:   Observation{Image: target.ImageReady, Builder: target.BuildSucceeded, HadBuilder: true, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseReady, Action: ActCleanup, Reason: imagev1.ReasonReady},
		},
		{
			name: "ready after cleanup stays ready and stays a miss",
			in:   Observation{Image: target.ImageReady, Builder: target.BuildAbsent, HadBuilder: true, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseReady, Action: ActNone, Reason: imagev1.ReasonReady},
		},
		{
			name: "cache hit removes a builder somebody else left behind",
			in:   Observation{Image: target.ImageReady, Builder: target.BuildRunning, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseReady, Action: ActCleanup, Reason: imagev1.ReasonCacheHit, CacheHit: true},
		},
		{
			name: "nothing there: start a builder",
			in:   Observation{Image: target.ImageAbsent, Builder: target.BuildAbsent, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseBuilding, Action: ActEnsureBuilder, Reason: imagev1.ReasonBuilding},
		},
		{
			name: "builder running: keep driving it",
			in:   Observation{Image: target.ImageAbsent, Builder: target.BuildRunning, HadBuilder: true, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseBuilding, Action: ActEnsureBuilder, Reason: imagev1.ReasonBuilding},
		},
		{
			name: "second request finds a running builder for the same key: waits for it",
			in:   Observation{Image: target.ImageAbsent, Builder: target.BuildRunning, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseBuilding, Action: ActEnsureBuilder, Reason: imagev1.ReasonBuilding},
		},
		{
			name: "build done: deliver",
			in:   Observation{Image: target.ImageAbsent, Builder: target.BuildSucceeded, HadBuilder: true, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseDelivering, Action: ActDeliver, Reason: imagev1.ReasonDelivering},
		},
		{
			name: "target importing: wait",
			in:   Observation{Image: target.ImageImporting, Builder: target.BuildSucceeded, HadBuilder: true, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseDelivering, Action: ActWait, Reason: imagev1.ReasonDelivering},
		},
		{
			name: "target importing for another request, no builder left: wait",
			in:   Observation{Image: target.ImageImporting, Builder: target.BuildAbsent, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseDelivering, Action: ActWait, Reason: imagev1.ReasonDelivering},
		},
		{
			name: "first build failure: count it and try again",
			in:   Observation{Image: target.ImageAbsent, Builder: target.BuildFailed, HadBuilder: true, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseBuilding, Action: ActFailAttempt, Reason: imagev1.ReasonBuilding},
		},
		{
			name: "failure that reaches the limit: failed, terminal",
			in:   Observation{Image: target.ImageAbsent, Builder: target.BuildFailed, HadBuilder: true, Attempts: 1, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseFailed, Action: ActFailAttempt, Reason: imagev1.ReasonBuildFailed, Terminal: true},
		},
		{
			name: "limit reached and nothing left on the target: stays failed, no new builder",
			in:   Observation{Image: target.ImageAbsent, Builder: target.BuildAbsent, HadBuilder: true, Attempts: 2, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseFailed, Action: ActNone, Reason: imagev1.ReasonBuildFailed, Terminal: true},
		},
		{
			name: "inspection rejected the image: terminal at once, whatever the attempt count",
			in:   Observation{Image: target.ImageAbsent, Builder: target.BuildFailed, BuilderTerminal: true, HadBuilder: true, MaxAttempts: 5},
			want: Decision{Phase: imagev1.PhaseFailed, Action: ActFailAttempt, Reason: imagev1.ReasonImageRejected, Terminal: true},
		},
		{
			name: "build over its deadline counts as a failure",
			in:   Observation{Image: target.ImageAbsent, Builder: target.BuildRunning, BuildOverdue: true, HadBuilder: true, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseBuilding, Action: ActFailAttempt, Reason: imagev1.ReasonBuilding},
		},
		{
			name: "deadline does not touch a finished build",
			in:   Observation{Image: target.ImageAbsent, Builder: target.BuildSucceeded, BuildOverdue: true, HadBuilder: true, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseDelivering, Action: ActDeliver, Reason: imagev1.ReasonDelivering},
		},
		{
			name: "import over its deadline counts as a failure",
			in:   Observation{Image: target.ImageImporting, Builder: target.BuildSucceeded, DeliveryOverdue: true, HadBuilder: true, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseBuilding, Action: ActFailAttempt, Reason: imagev1.ReasonBuilding},
		},
		{
			name: "import failed on the last attempt: failed, terminal",
			in:   Observation{Image: target.ImageFailed, Builder: target.BuildSucceeded, HadBuilder: true, Attempts: 1, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseFailed, Action: ActFailAttempt, Reason: imagev1.ReasonBuildFailed, Terminal: true},
		},
		{
			name: "a ready image wins over a failed builder",
			in:   Observation{Image: target.ImageReady, Builder: target.BuildFailed, HadBuilder: true, Attempts: 1, MaxAttempts: 2},
			want: Decision{Phase: imagev1.PhaseReady, Action: ActCleanup, Reason: imagev1.ReasonReady},
		},
	}
	for _, c := range cases {
		if got := Decide(c.in); got != c.want {
			t.Errorf("%s:\n got  %+v\n want %+v", c.name, got, c.want)
		}
	}
}

// Whatever is observed, a request that keeps failing reaches Failed within MaxAttempts failures
// and then asks for nothing more: there is no endless retry.
func TestFailuresAreBounded(t *testing.T) {
	const limit = 3
	o := Observation{Image: target.ImageAbsent, Builder: target.BuildAbsent, MaxAttempts: limit}
	builders := 0
	for round := range 50 {
		d := Decide(o)
		switch d.Action {
		case ActEnsureBuilder:
			if o.Builder == target.BuildAbsent {
				builders++
			}
			o.HadBuilder = true
			o.Builder = target.BuildFailed // every build fails
		case ActFailAttempt:
			o.Attempts++
			o.Builder = target.BuildAbsent
		case ActNone:
			if d.Phase != imagev1.PhaseFailed || !d.Terminal {
				t.Fatalf("round %d: idle but not failed: %+v", round, d)
			}
			if builders != limit {
				t.Fatalf("%d builders were started, want %d", builders, limit)
			}
			return
		default:
			t.Fatalf("round %d: unexpected action %s", round, d.Action)
		}
	}
	t.Fatal("never reached a terminal state")
}
