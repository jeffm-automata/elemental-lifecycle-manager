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

package catalog

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
)

const (
	// DefaultInterval between two queries of a source.
	DefaultInterval = 5 * time.Minute
	// MinInterval is the shortest interval a source may ask for.
	MinInterval = 30 * time.Second

	listTimeout    = 45 * time.Second
	releaseTimeout = 90 * time.Second
	maxSkipped     = 50
)

// SourceReconciler queries the repository of a ReleaseSource and keeps one AvailableRelease per
// release found there.
type SourceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Registry Registry
	// Namespace is where pull secrets are read from (the controller's own namespace).
	Namespace string
	// Now is replaceable in tests.
	Now func() time.Time
}

var nameRegex = regexp.MustCompile(`[^a-z0-9.-]+`)

// ReleaseName is the object name of the AvailableRelease for a version found in a source.
func ReleaseName(source, version string) string {
	v := strings.Trim(nameRegex.ReplaceAllString(strings.ToLower(version), "-"), "-.")
	return source + "-" + v
}

func (r *SourceReconciler) now() metav1.Time {
	if r.Now != nil {
		return metav1.NewTime(r.Now())
	}
	return metav1.Now()
}

func interval(src *lifecyclev1alpha1.ReleaseSource) time.Duration {
	if src.Spec.Interval == nil || src.Spec.Interval.Duration <= 0 {
		return DefaultInterval
	}
	return max(src.Spec.Interval.Duration, MinInterval)
}

// Reconcile queries the source once and schedules the next query.
func (r *SourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	src := &lifecyclev1alpha1.ReleaseSource{}
	if err := r.Get(ctx, req.NamespacedName, src); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !src.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // the AvailableReleases are owned and garbage collected
	}

	owned := &lifecyclev1alpha1.AvailableReleaseList{}
	if err := r.List(ctx, owned, client.MatchingLabels{lifecyclev1alpha1.CatalogSourceLabel: src.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing releases of source %s: %w", src.Name, err)
	}
	existing := make(map[string]*lifecyclev1alpha1.AvailableRelease, len(owned.Items))
	for i := range owned.Items {
		existing[owned.Items[i].Name] = &owned.Items[i]
	}

	status := src.Status.DeepCopy()
	status.ObservedGeneration = src.Generation
	now := r.now()
	status.LastSyncTime = &now

	access, err := r.access(ctx, src)
	var tags []string
	if err == nil {
		listCtx, cancel := context.WithTimeout(ctx, listTimeout)
		tags, err = r.Registry.ListTags(listCtx, src.Spec.Registry, access)
		cancel()
	}
	if err != nil {
		// The last known list is kept: nothing is deleted, everything is marked stale.
		reason := lifecyclev1alpha1.SourceUnreachable
		if _, ok := errors.AsType[*InvalidSourceError](err); ok {
			reason = lifecyclev1alpha1.SourceInvalid
		}
		logger.Info("Source could not be queried; keeping the last known releases", "reason", reason, "error", err.Error())
		for _, rel := range existing {
			if serr := r.setStale(ctx, rel, true); serr != nil {
				return ctrl.Result{}, serr
			}
		}
		status.Stale = true
		status.Releases = int32(len(existing)) //nolint:gosec // bounded by the number of tags
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: lifecyclev1alpha1.ConditionSourceReady, Status: metav1.ConditionFalse, Reason: reason,
			Message: brief(err.Error()), ObservedGeneration: src.Generation,
		})
		return ctrl.Result{RequeueAfter: interval(src)}, r.updateStatus(ctx, src, status)
	}

	sort.Strings(tags)
	var skipped []lifecyclev1alpha1.SkippedTag
	failed := 0
	seen := map[string]bool{}

	for _, tag := range tags {
		if !IsReleaseTag(tag) {
			skipped = append(skipped, lifecyclev1alpha1.SkippedTag{Tag: tag, Reason: "not a release version"})
			continue
		}
		objName := ReleaseName(src.Name, tag)
		if seen[objName] {
			skipped = append(skipped, lifecyclev1alpha1.SkippedTag{Tag: tag, Reason: "same object name as another tag"})
			continue
		}

		relCtx, cancel := context.WithTimeout(ctx, releaseTimeout)
		err = r.syncRelease(relCtx, src, tag, objName, existing[objName], access)
		cancel()

		switch {
		case err == nil:
			seen[objName] = true
		case isNotARelease(err):
			// Read, and not a release a cluster can use: not published.
			skipped = append(skipped, lifecyclev1alpha1.SkippedTag{Tag: tag, Reason: brief(err.Error())})
		case isAPIError(err):
			return ctrl.Result{}, err
		default:
			// Could not be read: an entry known from before is kept and marked stale.
			failed++
			skipped = append(skipped, lifecyclev1alpha1.SkippedTag{Tag: tag, Reason: "could not be read: " + brief(err.Error())})
			if rel := existing[objName]; rel != nil {
				seen[objName] = true
				if serr := r.setStale(ctx, rel, true); serr != nil {
					return ctrl.Result{}, serr
				}
			}
		}
	}

	// The registry answered: entries whose tag is gone are removed.
	for objName, rel := range existing {
		if seen[objName] {
			continue
		}
		if err := r.Delete(ctx, rel); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("deleting release %s: %w", objName, err)
		}
		logger.Info("Release is no longer in the source; removed", "release", objName)
	}

	status.Stale = false
	status.LastSuccessfulSyncTime = &now
	status.Releases = int32(len(seen)) //nolint:gosec // bounded by the number of tags
	if len(skipped) > maxSkipped {
		skipped = skipped[:maxSkipped]
	}
	status.Skipped = skipped
	cond := metav1.Condition{
		Type: lifecyclev1alpha1.ConditionSourceReady, Status: metav1.ConditionTrue, Reason: lifecyclev1alpha1.SourceSynced,
		Message: fmt.Sprintf("%d releases from %d tags", len(seen), len(tags)), ObservedGeneration: src.Generation,
	}
	if failed > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Reason = lifecyclev1alpha1.SourcePartiallySynced
		cond.Message = fmt.Sprintf("%d of %d tags could not be read", failed, len(tags))
	}
	apimeta.SetStatusCondition(&status.Conditions, cond)

	return ctrl.Result{RequeueAfter: interval(src)}, r.updateStatus(ctx, src, status)
}

func isNotARelease(err error) bool {
	_, ok := errors.AsType[*NotAReleaseError](err)
	return ok
}

func isAPIError(err error) bool {
	var status apierrors.APIStatus
	return errors.As(err, &status)
}

func brief(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// access builds the registry access of a source, reading its pull secret.
func (r *SourceReconciler) access(ctx context.Context, src *lifecyclev1alpha1.ReleaseSource) (Access, error) {
	access := Access{Insecure: src.Spec.Insecure, CABundle: src.Spec.CABundle}
	if src.Spec.PullSecretRef == nil || src.Spec.PullSecretRef.Name == "" {
		return access, nil
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: r.Namespace, Name: src.Spec.PullSecretRef.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		return access, &InvalidSourceError{Err: fmt.Errorf("reading pull secret %s: %w", key, err)}
	}
	data := secret.Data[corev1.DockerConfigJsonKey]
	if len(data) == 0 {
		return access, &InvalidSourceError{Err: fmt.Errorf("pull secret %s has no %s", key, corev1.DockerConfigJsonKey)}
	}
	access.DockerConfigJSON = data
	return access, nil
}

// syncRelease makes the AvailableRelease of one tag match the registry. The manifest is only
// downloaded when the digest of the tag is not the one already published.
func (r *SourceReconciler) syncRelease(ctx context.Context, src *lifecyclev1alpha1.ReleaseSource,
	tag, objName string, current *lifecyclev1alpha1.AvailableRelease, access Access) error {
	repository := CleanRepository(src.Spec.Registry)

	digest, err := r.Registry.Digest(ctx, repository, tag, access)
	if err != nil {
		return err
	}
	if current != nil && current.Spec.Digest == digest && current.Spec.Registry == repository {
		return r.setStale(ctx, current, false)
	}

	pinned := repository + "@" + digest
	data, err := r.Registry.ReadManifest(ctx, pinned, access)
	if err != nil {
		return err
	}
	info, err := ParseManifest(ctx, data, func(ctx context.Context, ref string) ([]byte, error) {
		return r.Registry.ReadManifest(ctx, ref, access)
	})
	if err != nil {
		return err
	}

	spec := lifecyclev1alpha1.AvailableReleaseSpec{
		Source:          src.Name,
		Registry:        repository,
		Version:         tag,
		Digest:          digest,
		Manifest:        pinned,
		Lineage:         info.Lineage,
		CreationDate:    info.CreationDate,
		OperatingSystem: info.OperatingSystem,
		Kubernetes:      info.Kubernetes,
		CorePlatform:    info.CorePlatform,
	}

	rel := &lifecyclev1alpha1.AvailableRelease{ObjectMeta: metav1.ObjectMeta{Name: objName}}
	if _, err = controllerutil.CreateOrUpdate(ctx, r.Client, rel, func() error {
		if rel.Labels == nil {
			rel.Labels = map[string]string{}
		}
		rel.Labels[lifecyclev1alpha1.CatalogSourceLabel] = src.Name
		rel.Labels[lifecyclev1alpha1.CatalogLineageLabel] = labelValue(info.Lineage)
		rel.Spec = spec
		return controllerutil.SetControllerReference(src, rel, r.Scheme)
	}); err != nil {
		return fmt.Errorf("writing release %s: %w", objName, err)
	}

	now := r.now()
	base := rel.DeepCopy()
	rel.Status.DiscoveredAt = &now
	rel.Status.Stale = false
	if err = r.Status().Patch(ctx, rel, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("writing status of release %s: %w", objName, err)
	}
	log.FromContext(ctx).Info("Release published", "release", objName, "digest", digest,
		"lineage", info.Lineage, "kubernetes", info.Kubernetes.Version)
	return nil
}

var labelRegex = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// labelValue makes a lineage usable as a label value.
func labelValue(s string) string {
	s = strings.Trim(labelRegex.ReplaceAllString(s, "-"), "-._")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-._")
	}
	return s
}

func (r *SourceReconciler) setStale(ctx context.Context, rel *lifecyclev1alpha1.AvailableRelease, stale bool) error {
	if rel.Status.Stale == stale {
		return nil
	}
	base := rel.DeepCopy()
	rel.Status.Stale = stale
	if err := r.Status().Patch(ctx, rel, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("marking release %s stale=%t: %w", rel.Name, stale, err)
	}
	return nil
}

func (r *SourceReconciler) updateStatus(ctx context.Context, src *lifecyclev1alpha1.ReleaseSource,
	status *lifecyclev1alpha1.ReleaseSourceStatus) error {
	if apiequality.Semantic.DeepEqual(&src.Status, status) {
		return nil
	}
	base := src.DeepCopy()
	src.Status = *status
	if err := r.Status().Patch(ctx, src, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("writing status of source %s: %w", src.Name, err)
	}
	return nil
}

// SetupWithManager registers the controller. A source is queried when it is created or its spec
// changes, and then every interval; a deleted AvailableRelease is published again at once.
func (r *SourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	deletedOnly := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("releasesource").
		For(&lifecyclev1alpha1.ReleaseSource{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&lifecyclev1alpha1.AvailableRelease{}, builder.WithPredicates(deletedOnly)).
		Complete(r)
}
