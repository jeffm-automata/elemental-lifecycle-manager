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
	imagev1 "github.com/suse/elemental-lifecycle-manager/image-operator/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
)

// Action is the one thing a reconcile does after looking at the target.
type Action string

const (
	// ActNone: nothing to do on the target.
	ActNone Action = "None"
	// ActEnsureBuilder: create the builder, or push a builder that is still being set up one step on.
	ActEnsureBuilder Action = "EnsureBuilder"
	// ActDeliver: the build is done; hand the artifact to the target.
	ActDeliver Action = "Deliver"
	// ActWait: the target is working (importing); look again later.
	ActWait Action = "Wait"
	// ActCleanup: the image is usable; remove the builder.
	ActCleanup Action = "Cleanup"
	// ActFailAttempt: count a failure and remove the builder and any half-delivered image.
	ActFailAttempt Action = "FailAttempt"
)

// Observation is what one reconcile saw. It holds no clock and no client, so Decide is a pure function.
type Observation struct {
	Image   target.ImageState
	Builder target.BuildState
	// BuilderTerminal: the builder failed in a way a new attempt can not fix.
	BuilderTerminal bool
	// HadBuilder: this request has, now or earlier, waited for a builder.
	HadBuilder bool
	// Attempts is the number of failures counted so far; MaxAttempts is the limit.
	Attempts, MaxAttempts int
	// BuildOverdue and DeliveryOverdue: the builder or the import is older than its deadline.
	BuildOverdue, DeliveryOverdue bool
}

// Decision is what to do and what to report.
type Decision struct {
	Phase  imagev1.Phase
	Action Action
	// Reason is the reason of the Ready condition.
	Reason string
	// CacheHit is meaningful when Phase is Ready.
	CacheHit bool
	// Terminal: with Phase Failed, nothing is retried until the object is replaced.
	Terminal bool
}

// Decide is the whole state machine. It is evaluated from the top on every reconcile; the
// state lives on the target (is there an image, is there a builder), not in the request.
func Decide(o Observation) Decision {
	switch o.Image {
	case target.ImageReady:
		d := Decision{Phase: imagev1.PhaseReady, Action: ActNone, Reason: imagev1.ReasonReady, CacheHit: !o.HadBuilder}
		if d.CacheHit {
			d.Reason = imagev1.ReasonCacheHit
		}
		if o.Builder != target.BuildAbsent {
			d.Action = ActCleanup
		}
		return d
	case target.ImageImporting:
		if o.DeliveryOverdue {
			return failAttempt(o)
		}
		return Decision{Phase: imagev1.PhaseDelivering, Action: ActWait, Reason: imagev1.ReasonDelivering}
	case target.ImageFailed:
		return failAttempt(o)
	}

	// No image on the target.
	switch o.Builder {
	case target.BuildSucceeded:
		return Decision{Phase: imagev1.PhaseDelivering, Action: ActDeliver, Reason: imagev1.ReasonDelivering}
	case target.BuildFailed:
		if o.BuilderTerminal {
			return Decision{Phase: imagev1.PhaseFailed, Action: ActFailAttempt, Reason: imagev1.ReasonImageRejected, Terminal: true}
		}
		return failAttempt(o)
	case target.BuildRunning:
		if o.BuildOverdue {
			return failAttempt(o)
		}
		return Decision{Phase: imagev1.PhaseBuilding, Action: ActEnsureBuilder, Reason: imagev1.ReasonBuilding}
	}
	if o.Attempts >= o.MaxAttempts {
		return Decision{Phase: imagev1.PhaseFailed, Action: ActNone, Reason: imagev1.ReasonBuildFailed, Terminal: true}
	}
	return Decision{Phase: imagev1.PhaseBuilding, Action: ActEnsureBuilder, Reason: imagev1.ReasonBuilding}
}

// failAttempt counts one failure. The attempt that reaches the limit ends the request.
func failAttempt(o Observation) Decision {
	if o.Attempts+1 >= o.MaxAttempts {
		return Decision{Phase: imagev1.PhaseFailed, Action: ActFailAttempt, Reason: imagev1.ReasonBuildFailed, Terminal: true}
	}
	return Decision{Phase: imagev1.PhaseBuilding, Action: ActFailAttempt, Reason: imagev1.ReasonBuilding}
}
