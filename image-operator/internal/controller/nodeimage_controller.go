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

// Package controller reconciles NodeImage objects. The decisions are in ladder.go; this file
// observes, acts on the decision and writes the status.
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	imagev1 "github.com/suse/elemental-lifecycle-manager/image-operator/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/cachekey"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/release"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
)

// Backends gives the target and the builder for a request.
type Backends interface {
	For(ctx context.Context, ni *imagev1.NodeImage) (target.Target, target.Builder, error)
}

// BackendError is an error from Backends.For. Terminal errors carry the reason to report.
type BackendError struct {
	Reason   string
	Terminal bool
	Err      error
}

func (e *BackendError) Error() string { return e.Err.Error() }
func (e *BackendError) Unwrap() error { return e.Err }

// Tooling identifies what images are built with; it is part of every cache key.
type Tooling struct {
	RecipeDigest string
	ToolRepo     string
	ToolCommit   string
}

// Options are the operator-wide limits.
type Options struct {
	MaxAttempts     int
	BuildTimeout    time.Duration
	DeliveryTimeout time.Duration
	// PollBuild and PollDelivery are how often a running builder or import is looked at.
	PollBuild    time.Duration
	PollDelivery time.Duration
}

// NodeImageReconciler reconciles NodeImage objects.
type NodeImageReconciler struct {
	client.Client
	Recorder record.EventRecorder
	Resolver release.Resolver
	Backends Backends
	Tooling  Tooling
	Options  Options
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=image.lifecycle.suse.com,resources=nodeimages,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=image.lifecycle.suse.com,resources=nodeimages/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=image.lifecycle.suse.com,resources=nodeimages/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *NodeImageReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *NodeImageReconciler) event(ni *imagev1.NodeImage, kind, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(ni, kind, reason, msg)
	}
}

// Request builds the target request of a NodeImage whose release is resolved.
func (r *NodeImageReconciler) request(ni *imagev1.NodeImage) (target.Request, error) {
	if ni.Status.Release == nil || ni.Status.Release.ManifestDigest == "" {
		return target.Request{}, errors.New("release is not resolved")
	}
	in := cachekey.Inputs{
		ManifestDigest: ni.Status.Release.ManifestDigest, Target: ni.Spec.Target.Type, Arch: ni.Spec.Arch,
		DiskSize: ni.Spec.Config.DiskSize, RecipeDigest: r.Tooling.RecipeDigest,
		ToolRepo: r.Tooling.ToolRepo, ToolCommit: r.Tooling.ToolCommit,
	}
	req := target.Request{
		ManifestRef: ni.Spec.Release.Manifest, ManifestDigest: in.ManifestDigest, Arch: in.Arch, DiskSize: in.DiskSize,
	}
	for _, cv := range ni.Spec.Config.ChartValues {
		in.ChartValues = append(in.ChartValues, cachekey.ChartValues{Chart: cv.Chart, Values: cv.Values})
		req.ChartValues = append(req.ChartValues, target.ChartValues{Chart: cv.Chart, Values: cv.Values})
	}
	key, err := cachekey.Compute(in)
	req.Key = key
	return req, err
}

// Reconcile moves one NodeImage one step.
func (r *NodeImageReconciler) Reconcile(ctx context.Context, rq ctrl.Request) (ctrl.Result, error) {
	ni := &imagev1.NodeImage{}
	if err := r.Get(ctx, rq.NamespacedName, ni); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ni.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, ni)
	}
	if controllerutil.AddFinalizer(ni, imagev1.Finalizer) {
		if err := r.Update(ctx, ni); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Failed is terminal for a generation: nothing is retried behind the user's back.
	if ni.Status.Phase == imagev1.PhaseFailed && ni.Status.ObservedGeneration == ni.Generation {
		return ctrl.Result{}, nil
	}
	before := ni.Status.DeepCopy()
	res, err := r.step(ctx, ni)
	ni.Status.ObservedGeneration = ni.Generation
	if before.Phase != ni.Status.Phase {
		log.FromContext(ctx).Info("phase", "from", before.Phase, "to", ni.Status.Phase, "key", ni.Status.CacheKey)
	}
	if uerr := r.Status().Update(ctx, ni); uerr != nil {
		return ctrl.Result{}, uerr
	}
	return res, err
}

// fail ends the request.
func (r *NodeImageReconciler) fail(ni *imagev1.NodeImage, reason, msg string) {
	ni.Status.Phase = imagev1.PhaseFailed
	setCond(ni, imagev1.ConditionReady, metav1.ConditionFalse, reason, msg)
	r.event(ni, corev1.EventTypeWarning, reason, msg)
}

func setCond(ni *imagev1.NodeImage, typ string, status metav1.ConditionStatus, reason, msg string) {
	if len(msg) > 4096 {
		msg = msg[len(msg)-4096:]
	}
	meta.SetStatusCondition(&ni.Status.Conditions, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg, ObservedGeneration: ni.Generation,
	})
}

// step runs the ladder once: resolve, key, look at the target, decide, act, report.
func (r *NodeImageReconciler) step(ctx context.Context, ni *imagev1.NodeImage) (ctrl.Result, error) {
	if ni.Status.Phase == "" {
		ni.Status.Phase = imagev1.PhasePending
	}
	if ni.Spec.Target.Type != imagev1.TargetHarvester || ni.Spec.Target.Harvester == nil {
		r.fail(ni, imagev1.ReasonUnsupportedTarget,
			fmt.Sprintf("target type %q is not implemented (known: harvester)", ni.Spec.Target.Type))
		return ctrl.Result{}, nil
	}
	if done, err := r.resolve(ctx, ni); done {
		return ctrl.Result{}, err
	}
	req, err := r.request(ni)
	if err != nil {
		r.fail(ni, imagev1.ReasonInvalidSpec, err.Error())
		return ctrl.Result{}, nil
	}
	ni.Status.CacheKey = req.Key

	s, done, err := r.observe(ctx, ni, req)
	if done {
		return ctrl.Result{}, err
	}
	if err := r.act(ctx, ni, s); err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			// The target refuses the object itself; asking again can not help. Free the builder and stop.
			if cerr := s.bld.Cleanup(ctx, s.req); cerr != nil {
				return ctrl.Result{}, cerr
			}
			r.fail(ni, imagev1.ReasonTargetRejected, "the target refused an object of this request: "+err.Error())
			return ctrl.Result{}, nil
		}
		setCond(ni, imagev1.ConditionReady, metav1.ConditionFalse, imagev1.ReasonTargetUnavailable, err.Error())
		return ctrl.Result{}, err
	}
	return r.report(ni, s), nil
}

// resolve turns the manifest reference into a digest, once; the digest, not the tag, is what the
// request stands for. done is true when this reconcile ends here (terminal failure or retry).
func (r *NodeImageReconciler) resolve(ctx context.Context, ni *imagev1.NodeImage) (done bool, err error) {
	if ni.Status.Release != nil && ni.Status.Release.ManifestDigest != "" {
		return false, nil
	}
	digest, err := r.Resolver.Resolve(ctx, ni.Spec.Release.Manifest)
	if err != nil {
		if re, ok := errors.AsType[*release.Error](err); ok && re.Terminal() {
			setCond(ni, imagev1.ConditionResolved, metav1.ConditionFalse, string(re.Kind), err.Error())
			r.fail(ni, string(re.Kind), err.Error())
			return true, nil
		}
		setCond(ni, imagev1.ConditionResolved, metav1.ConditionFalse, imagev1.ReasonResolving, err.Error())
		setCond(ni, imagev1.ConditionReady, metav1.ConditionFalse, imagev1.ReasonResolving,
			"waiting for the registry: "+err.Error())
		return true, err // retried with backoff
	}
	t := metav1.NewTime(r.now())
	ni.Status.Release = &imagev1.ReleaseStatus{ManifestDigest: digest, ResolvedAt: &t}
	setCond(ni, imagev1.ConditionResolved, metav1.ConditionTrue, imagev1.ReasonResolved,
		ni.Spec.Release.Manifest+" is "+digest)
	return false, nil
}

// seen is one look at the target and what was decided from it.
type seen struct {
	req   target.Request
	tgt   target.Target
	bld   target.Builder
	img   target.Image
	build target.Build
	d     Decision
	now   metav1.Time
}

// observe looks at the target and decides. done is true when this reconcile ends here.
func (r *NodeImageReconciler) observe(ctx context.Context, ni *imagev1.NodeImage, req target.Request) (s seen, done bool, err error) {
	s.req, s.now = req, metav1.NewTime(r.now())
	unavailable := func(err error) (seen, bool, error) {
		setCond(ni, imagev1.ConditionReady, metav1.ConditionFalse, imagev1.ReasonTargetUnavailable, err.Error())
		return s, true, err
	}
	if s.tgt, s.bld, err = r.Backends.For(ctx, ni); err != nil {
		if be, ok := errors.AsType[*BackendError](err); ok && be.Terminal {
			r.fail(ni, be.Reason, err.Error())
			return s, true, nil
		}
		return unavailable(err)
	}
	if s.img, err = s.tgt.FindImage(ctx, req); err != nil {
		return unavailable(err)
	}
	if s.build, err = s.bld.Status(ctx, req); err != nil {
		return unavailable(err)
	}
	s.d = Decide(Observation{
		Image: s.img.State, Builder: s.build.State, BuilderTerminal: s.build.Terminal,
		HadBuilder: ni.Status.Build != nil, Attempts: int(attemptsOf(ni)), MaxAttempts: r.Options.MaxAttempts,
		BuildOverdue:    !s.build.Created.IsZero() && s.now.Sub(s.build.Created) > r.Options.BuildTimeout,
		DeliveryOverdue: !s.img.Created.IsZero() && s.now.Sub(s.img.Created) > r.Options.DeliveryTimeout,
	})
	return s, false, nil
}

// buildStatus returns the build history of the request, starting it if this is the first builder.
func buildStatus(ni *imagev1.NodeImage, s seen) *imagev1.BuildStatus {
	if ni.Status.Build == nil {
		ni.Status.Build = &imagev1.BuildStatus{StartedAt: &s.now}
	}
	ni.Status.Build.Builder = s.build.Name
	return ni.Status.Build
}

// act does the one thing the decision asks for. Errors are retried by the controller.
func (r *NodeImageReconciler) act(ctx context.Context, ni *imagev1.NodeImage, s seen) error {
	switch s.d.Action {
	case ActEnsureBuilder:
		first := ni.Status.Build == nil
		buildStatus(ni, s)
		if err := s.bld.Ensure(ctx, s.req); err != nil {
			return err
		}
		if first {
			r.event(ni, corev1.EventTypeNormal, imagev1.ReasonBuilding,
				"builder "+s.build.Name+" requested for cache key "+s.req.Key)
		}
		step := s.build.Step
		if step == "" {
			step = "starting"
		}
		setCond(ni, imagev1.ConditionBuilt, metav1.ConditionFalse, imagev1.ReasonBuilding,
			"builder "+s.build.Name+": "+step)
	case ActDeliver:
		if b := buildStatus(ni, s); b.FinishedAt == nil {
			b.FinishedAt = &s.now
		}
		a := s.build.Artifact
		setCond(ni, imagev1.ConditionBuilt, metav1.ConditionTrue, imagev1.ReasonBuilt,
			fmt.Sprintf("%s, sha256 %s, %d bytes", a.File, a.SHA256, a.SizeBytes))
		if err := s.tgt.Deliver(ctx, s.req, *a); err != nil {
			return err
		}
		setCond(ni, imagev1.ConditionDelivered, metav1.ConditionFalse, imagev1.ReasonDelivering, importing(s.img))
	case ActWait:
		setCond(ni, imagev1.ConditionDelivered, metav1.ConditionFalse, imagev1.ReasonDelivering, importing(s.img))
	case ActCleanup:
		return s.bld.Cleanup(ctx, s.req)
	case ActFailAttempt:
		return r.failAttempt(ctx, ni, s)
	}
	return nil
}

func importing(img target.Image) string {
	if img.Message != "" {
		return "the target is importing the image: " + img.Message
	}
	return "the target is importing the image"
}

// failAttempt records one failure and removes the builder and any half-delivered image.
func (r *NodeImageReconciler) failAttempt(ctx context.Context, ni *imagev1.NodeImage, s seen) error {
	b := buildStatus(ni, s)
	b.Attempts++
	switch {
	case s.build.State == target.BuildFailed:
		b.LastError = s.build.Message
	case s.img.State == target.ImageFailed:
		b.LastError = "delivery failed: " + s.img.Message
	case s.img.State == target.ImageImporting:
		b.LastError = fmt.Sprintf("delivery did not finish within %s: %s", r.Options.DeliveryTimeout, s.img.Message)
	default:
		b.LastError = fmt.Sprintf("build did not finish within %s (last step: %s)", r.Options.BuildTimeout, s.build.Step)
	}
	r.event(ni, corev1.EventTypeWarning, imagev1.ReasonBuildFailed,
		fmt.Sprintf("attempt %d of %d failed: %s", b.Attempts, r.Options.MaxAttempts, firstLine(b.LastError)))
	if s.img.State != target.ImageAbsent {
		if err := s.tgt.DeleteImage(ctx, s.req); err != nil {
			return err
		}
	}
	return s.bld.Cleanup(ctx, s.req)
}

// report writes phase and conditions for the decision and says when to look again.
func (r *NodeImageReconciler) report(ni *imagev1.NodeImage, s seen) ctrl.Result {
	ni.Status.Phase = s.d.Phase
	where := s.img.Namespace + "/" + s.img.Name
	switch s.d.Phase {
	case imagev1.PhaseReady:
		if ni.Status.ReadyAt == nil {
			hit := s.d.CacheHit
			ni.Status.ReadyAt, ni.Status.CacheHit = &s.now, &hit
			if hit {
				r.event(ni, corev1.EventTypeNormal, imagev1.ReasonCacheHit,
					"image "+where+" already exists for cache key "+s.req.Key+"; nothing built")
			} else {
				r.event(ni, corev1.EventTypeNormal, imagev1.ReasonReady, "image "+where+" built and delivered")
			}
		}
		ni.Status.Image = &imagev1.ImageStatus{
			Name: s.img.Name, Namespace: s.img.Namespace, DisplayName: s.img.DisplayName,
			SHA256: s.img.SHA256, KubernetesVersion: s.img.KubernetesVersion,
		}
		builtReason, readyReason := imagev1.ReasonBuilt, imagev1.ReasonReady
		if *ni.Status.CacheHit {
			builtReason, readyReason = imagev1.ReasonCacheHit, imagev1.ReasonCacheHit
		}
		setCond(ni, imagev1.ConditionBuilt, metav1.ConditionTrue, builtReason, "")
		setCond(ni, imagev1.ConditionDelivered, metav1.ConditionTrue, imagev1.ReasonDelivered, where)
		setCond(ni, imagev1.ConditionReady, metav1.ConditionTrue, readyReason, "image "+where+" is usable")
		return ctrl.Result{}
	case imagev1.PhaseFailed:
		msg := "build failed"
		if ni.Status.Build != nil && ni.Status.Build.LastError != "" {
			msg = ni.Status.Build.LastError
		}
		setCond(ni, imagev1.ConditionBuilt, metav1.ConditionFalse, s.d.Reason, firstLine(msg))
		r.fail(ni, s.d.Reason, fmt.Sprintf("gave up after %d failed attempt(s); delete and recreate the NodeImage "+
			"to try again. Last error: %s", attemptsOf(ni), msg))
		return ctrl.Result{}
	case imagev1.PhaseDelivering:
		setCond(ni, imagev1.ConditionReady, metav1.ConditionFalse, s.d.Reason, "the target is importing the image")
		return ctrl.Result{RequeueAfter: r.Options.PollDelivery}
	}
	setCond(ni, imagev1.ConditionReady, metav1.ConditionFalse, s.d.Reason, "the image is being built")
	if s.d.Action == ActFailAttempt {
		return ctrl.Result{RequeueAfter: r.Options.PollDelivery}
	}
	return ctrl.Result{RequeueAfter: r.Options.PollBuild}
}

func attemptsOf(ni *imagev1.NodeImage) int32 {
	if ni.Status.Build == nil {
		return 0
	}
	return ni.Status.Build.Attempts
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}

// finalize removes what the request left on the target, unless another request needs it.
func (r *NodeImageReconciler) finalize(ctx context.Context, ni *imagev1.NodeImage) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ni, imagev1.Finalizer) {
		return ctrl.Result{}, nil
	}
	if ni.Status.CacheKey != "" && ni.Status.Release != nil {
		shared, err := r.keyShared(ctx, ni)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !shared {
			if err := r.cleanTarget(ctx, ni); err != nil {
				be, ok := errors.AsType[*BackendError](err)
				if !ok {
					return ctrl.Result{}, err
				}
				// The credentials are gone or unusable: nothing can be cleaned with them. Say so and let
				// go. Any other error (target unreachable) is retried above; the finalizer stays.
				r.event(ni, corev1.EventTypeWarning, be.Reason, "target not cleaned, credentials unusable: "+err.Error())
			}
		}
	}
	controllerutil.RemoveFinalizer(ni, imagev1.Finalizer)
	return ctrl.Result{}, r.Update(ctx, ni)
}

func (r *NodeImageReconciler) cleanTarget(ctx context.Context, ni *imagev1.NodeImage) error {
	req, err := r.request(ni)
	if err != nil {
		return nil // never had a key that could have created anything
	}
	tgt, bld, err := r.Backends.For(ctx, ni)
	if err != nil {
		return err
	}
	if err := bld.Cleanup(ctx, req); err != nil {
		return err
	}
	if ni.Spec.DeletionPolicy == imagev1.DeletionDelete {
		return tgt.DeleteImage(ctx, req)
	}
	return nil
}

// keyShared says whether another live NodeImage wants the same image at the same place.
func (r *NodeImageReconciler) keyShared(ctx context.Context, ni *imagev1.NodeImage) (bool, error) {
	list := &imagev1.NodeImageList{}
	if err := r.List(ctx, list); err != nil {
		return false, err
	}
	for i := range list.Items {
		o := &list.Items[i]
		if (o.Namespace == ni.Namespace && o.Name == ni.Name) || !o.DeletionTimestamp.IsZero() || o.Status.CacheKey != ni.Status.CacheKey {
			continue
		}
		if o.Namespace == ni.Namespace && o.Spec.Target.CredentialsSecretRef == ni.Spec.Target.CredentialsSecretRef &&
			o.Spec.Target.Harvester != nil && ni.Spec.Target.Harvester != nil && *o.Spec.Target.Harvester == *ni.Spec.Target.Harvester {
			return true, nil
		}
	}
	return false, nil
}

// SetupWithManager registers the controller.
func (r *NodeImageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&imagev1.NodeImage{}).Named("nodeimage").Complete(r)
}
