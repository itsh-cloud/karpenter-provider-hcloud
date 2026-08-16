package nodeclass

import (
	"context"
	"fmt"

	"github.com/awslabs/operatorpkg/status"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
)

// SupportedArchitecture is the Hetzner architecture this provider builds
// instance types for.
//
// It is the same value instancetype.Options defaults IncludeArchitectures to,
// and the two must agree: an image resolved for one architecture cannot boot on
// a server type offered for the other. The image reconciler qualifies name
// lookups with it, and the validation reconciler re-checks the resolved image
// against it so that an id-pinned image of the wrong architecture is caught
// before a server is ever ordered.
const SupportedArchitecture = "x86"

// Image resolves spec.imageSelector into status.image.
type Image struct {
	clk       clock.Clock
	resources hcloudapi.Resources
}

// NewImage returns the image sub-reconciler.
func NewImage(clk clock.Clock, resources hcloudapi.Resources) *Image {
	return &Image{clk: clk, resources: resources}
}

func (i *Image) Reconcile(ctx context.Context, nodeClass *v1alpha1.HCloudNodeClass) (reconcile.Result, error) {
	sel := nodeClass.Spec.ImageSelector
	img, err := i.resources.Image(ctx, sel.Name, sel.ID, SupportedArchitecture)
	if err != nil {
		if reason, ok := configFailure(ctx, err); ok {
			nodeClass.Status.Image = nil
			nodeClass.StatusConditions(status.WithClock(i.clk)).SetFalse(
				v1alpha1.ConditionTypeImageReady, "Image"+reason,
				failureMessage("imageSelector "+describeSelector(sel.Name, sel.ID), reason, err),
			)
			return reconcile.Result{RequeueAfter: misconfiguredRequeue}, nil
		}
		// Transient. The condition is deliberately left untouched rather than
		// set Unknown: an already-resolved class keeps its True condition at
		// the current generation, so a Hetzner blip does not ripple out into
		// NodeClassReady=False on every NodePool using it. A class that was
		// never resolved still reads Unknown from initialisation, so nothing
		// provisions against an unknown image either.
		return reconcile.Result{}, fmt.Errorf("resolving image, %w", err)
	}

	// The image is re-resolved rather than pinned because a name follows
	// Hetzner's periodic rebuilds and its id changes with each one. Whether
	// that rolls the fleet is spec.imageDriftPolicy's decision, not this
	// controller's; status simply reports what the name maps to today.
	nodeClass.Status.Image = &v1alpha1.ImageStatus{
		ID:           img.ID,
		Name:         img.Name,
		Description:  img.Description,
		Architecture: img.Architecture,
		Created:      img.Created,
	}
	nodeClass.StatusConditions(status.WithClock(i.clk)).SetTrue(v1alpha1.ConditionTypeImageReady)
	return reconcile.Result{RequeueAfter: resolvedRequeue}, nil
}

// describeSelector renders a name-or-id selector for a condition message.
// Deterministic by construction: it reads only from the spec, so the message
// cannot churn between otherwise identical reconciles.
func describeSelector(name string, id *int64) string {
	if id != nil {
		return fmt.Sprintf("id %d", *id)
	}
	return fmt.Sprintf("name %q", name)
}
