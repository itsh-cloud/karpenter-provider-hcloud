package instance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/hcloudapi"
	"github.com/itsh-cloud/karpenter-provider-hcloud/internal/providers/instancetype"
)

const rebootClaim = "general-abcde"

// rebootable is a running server that belongs to rebootClaim.
func rebootable() *hcloudapi.Server {
	srv := ownedServer(7, "worker")
	srv.Labels[hcloudapi.LabelNodeClaim] = rebootClaim
	return srv
}

// TestReboot: core retries Reboot with the same operation until it returns
// nil, so a reset already issued for this request must not be issued again,
// and anything retrying cannot fix must be terminal rather than retried.
func TestReboot(t *testing.T) {
	requestedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	off := rebootable()
	off.Status = "off"
	otherCluster := rebootable()
	otherCluster.Labels[hcloudapi.LabelManagedBy] = "other"
	unlabelled := rebootable()
	unlabelled.Labels = nil
	otherClaim := rebootable()
	otherClaim.Labels[hcloudapi.LabelNodeClaim] = "general-fghij"
	forbidden := hcloud.Error{Code: hcloud.ErrorCodeForbidden, Message: "insufficient permissions"}

	action := func(command, status string, started time.Time) []hcloudapi.Action {
		return []hcloudapi.Action{{Command: command, Status: status, Started: started}}
	}
	monthAgo := requestedAt.Add(-30 * 24 * time.Hour)
	isNil := func(err error) bool { return err == nil }
	isRetried := func(err error) bool { return err != nil && !cloudprovider.IsNodeRebootFailedError(err) }
	isRefused := cloudprovider.IsNodeRebootFailedError

	for _, tc := range []struct {
		name        string
		srv         *hcloudapi.Server
		providerID  string
		zeroRequest bool
		getErr      error
		actions     []hcloudapi.Action
		actionsErr  error
		resetErr    error
		wantResets  int
		wantListing bool
		wantErr     func(error) bool
	}{
		{name: "runningIsReset", srv: rebootable(), wantResets: 1, wantListing: true, wantErr: isNil},
		{name: "offIsRefused", srv: off, wantListing: true, wantErr: isRefused},
		// A retry can catch the server mid-reset.
		{name: "offAfterOwnResetIsIssued", srv: off,
			actions: action("reset_server", "running", requestedAt), wantListing: true, wantErr: isNil},
		{name: "runningResetSinceRequestSkips", srv: rebootable(),
			actions: action("reset_server", "running", requestedAt), wantListing: true, wantErr: isNil},
		{name: "succeededResetSinceRequestSkips", srv: rebootable(),
			actions: action("reset_server", "success", requestedAt.Add(time.Minute)), wantListing: true, wantErr: isNil},
		{name: "olderResetStillResets", srv: rebootable(),
			actions: action("reset_server", "success", requestedAt.Add(-time.Second)), wantResets: 1, wantListing: true, wantErr: isNil},
		{name: "erroredResetStillResets", srv: rebootable(),
			actions: action("reset_server", "error", requestedAt.Add(time.Minute)), wantResets: 1, wantListing: true, wantErr: isNil},
		{name: "startIsNotAReset", srv: rebootable(),
			actions: action("start_server", "success", requestedAt.Add(time.Minute)), wantResets: 1, wantListing: true, wantErr: isNil},
		{name: "softRebootIsNotAReset", srv: rebootable(),
			actions: action("reboot_server", "success", requestedAt.Add(time.Minute)), wantResets: 1, wantListing: true, wantErr: isNil},
		{name: "stopIsRefused", srv: off,
			actions: action("stop_server", "success", requestedAt.Add(-time.Hour)), wantListing: true, wantErr: isRefused},
		{name: "stopOnRunningServerIsRefused", srv: rebootable(),
			actions: action("stop_server", "success", requestedAt.Add(-time.Hour)), wantListing: true, wantErr: isRefused},
		{name: "oldShutdownIsRefused", srv: rebootable(),
			actions: action("shutdown_server", "success", monthAgo), wantListing: true, wantErr: isRefused},
		{name: "rescueIsRefused", srv: rebootable(),
			actions: action("enable_rescue", "success", requestedAt.Add(-time.Hour)), wantListing: true, wantErr: isRefused},
		{name: "erroredStopIsIgnored", srv: rebootable(),
			actions: action("stop_server", "error", requestedAt.Add(-time.Hour)), wantResets: 1, wantListing: true, wantErr: isNil},
		// The action here would skip the reset if a zero time were compared.
		{name: "zeroRequestedAtNeverCountsAsIssued", srv: rebootable(), zeroRequest: true,
			actions: action("reset_server", "success", requestedAt), wantResets: 1, wantListing: true, wantErr: isNil},
		{name: "listingErrorIssuesNothing", srv: rebootable(),
			actionsErr: errors.New("dial tcp: i/o timeout"), wantListing: true, wantErr: isRetried},
		{name: "forbiddenListingIsTerminal", srv: rebootable(),
			actionsErr: forbidden, wantListing: true, wantErr: isRefused},
		{name: "otherClustersServerIsRefused", srv: otherCluster, wantErr: isRefused},
		{name: "unlabelledServerIsRefused", srv: unlabelled, wantErr: isRefused},
		{name: "otherNodeClaimsServerIsRefused", srv: otherClaim, wantErr: isRefused},
		{name: "unparseableProviderIDIsRefused", srv: rebootable(), providerID: "hcloud://7abc", wantErr: isRefused},
		{name: "missingServerIsNotFound", wantErr: hcloudapi.IsNotFound},
		{name: "forbiddenGetIsTerminal", srv: rebootable(), getErr: forbidden, wantErr: isRefused},
		{name: "transientResetErrorIsRetried", srv: rebootable(),
			resetErr:   hcloud.Error{Code: hcloud.ErrorCodeRateLimitExceeded, Message: "limit reached"},
			wantResets: 1, wantListing: true, wantErr: isRetried},
		{name: "forbiddenResetIsTerminal", srv: rebootable(), resetErr: forbidden,
			wantResets: 1, wantListing: true, wantErr: isRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeServers()
			if tc.srv != nil {
				f.byID[tc.srv.ID] = tc.srv
			}
			f.getErr, f.actions, f.actionsErr, f.resetErr = tc.getErr, tc.actions, tc.actionsErr, tc.resetErr
			p := NewProvider(f, instancetype.NewUnavailable(), testCluster)

			providerID := tc.providerID
			if providerID == "" {
				providerID = "hcloud://7"
			}
			at := requestedAt
			if tc.zeroRequest {
				at = time.Time{}
			}

			err := p.Reboot(context.Background(), providerID, rebootClaim, at)
			if !tc.wantErr(err) {
				t.Errorf("err = %v (reboot failed: %t)", err, cloudprovider.IsNodeRebootFailedError(err))
			}
			if len(f.resets) != tc.wantResets {
				t.Errorf("resets = %v, want %d", f.resets, tc.wantResets)
			}
			if listed := f.actionsCall > 0; listed != tc.wantListing {
				t.Errorf("listed actions = %t, want %t", listed, tc.wantListing)
			}
		})
	}
}

// TestRebootDoesNotUseTheListCache: like Delete, the ownership gate in front of
// a reset reads live state, since labels can change out of band.
func TestRebootDoesNotUseTheListCache(t *testing.T) {
	f := newFakeServers()
	f.listed = []*hcloudapi.Server{rebootable()}
	p := NewProvider(f, instancetype.NewUnavailable(), testCluster)
	if _, err := p.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	live := rebootable()
	live.Labels[hcloudapi.LabelManagedBy] = "someone-else"
	f.byID[7] = live

	err := p.Reboot(context.Background(), "hcloud://7", rebootClaim, time.Time{})
	if len(f.resets) != 0 || !cloudprovider.IsNodeRebootFailedError(err) {
		t.Errorf("resets = %v, err = %v; want none and a terminal refusal", f.resets, err)
	}
}
