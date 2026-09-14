package reconciler

import (
	"context"
	"errors"
	"testing"

	pb "github.com/openshift-online/hypershell/components/api-server/pkg/api/grpc/hypershell/v1"
	"google.golang.org/grpc"
)

// gatewayWith builds a minimal Gateway proto carrying an id, desired release, and
// currently observed release, so advanceObservedRelease's convergence and
// idempotency can be exercised.
func gatewayWith(id, releaseID, observedReleaseID string) *pb.Gateway {
	gw := &pb.Gateway{
		Metadata:  &pb.ObjectReference{Id: id},
		ReleaseId: releaseID,
	}
	if observedReleaseID != "" {
		gw.ObservedReleaseId = &observedReleaseID
	}
	return gw
}

// TestAdvanceObservedRelease pins the write-back contract: the observed release is
// advanced to the desired release only once the new revision has passed its health
// gates, the write is skipped when it would be redundant (unchanged release or a
// direct-image gateway), and a write-back failure is surfaced for retry rather
// than swallowed. See gateway-release-rollout.spec.md.
func TestAdvanceObservedRelease(t *testing.T) {
	t.Run("advances to desired release", func(t *testing.T) {
		var got *pb.UpdateGatewayRequest
		client := &fakeGatewayClient{updateFn: func(_ context.Context, in *pb.UpdateGatewayRequest, _ ...grpc.CallOption) (*pb.UpdateGatewayResponse, error) {
			got = in
			return &pb.UpdateGatewayResponse{}, nil
		}}
		err := advanceObservedRelease(context.Background(), client, gatewayWith("gw-1", "rel-new", "rel-old"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected an UpdateGateway call, got none")
		}
		if got.GetId() != "gw-1" {
			t.Errorf("Id = %q, want %q", got.GetId(), "gw-1")
		}
		if got.GetObservedReleaseId() != "rel-new" {
			t.Errorf("ObservedReleaseId = %q, want %q", got.GetObservedReleaseId(), "rel-new")
		}
		// Only the observed release is written back; phase/status stay untouched.
		if got.Phase != nil || got.Status != nil {
			t.Errorf("expected a narrow observed-release update, got phase=%v status=%v", got.Phase, got.Status)
		}
	})

	t.Run("no write when observed already matches desired", func(t *testing.T) {
		called := false
		client := &fakeGatewayClient{updateFn: func(_ context.Context, _ *pb.UpdateGatewayRequest, _ ...grpc.CallOption) (*pb.UpdateGatewayResponse, error) {
			called = true
			return &pb.UpdateGatewayResponse{}, nil
		}}
		if err := advanceObservedRelease(context.Background(), client, gatewayWith("gw-1", "rel-new", "rel-new")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if called {
			t.Error("expected no UpdateGateway call for an unchanged release")
		}
	})

	t.Run("no write for a direct-image gateway without a release", func(t *testing.T) {
		called := false
		client := &fakeGatewayClient{updateFn: func(_ context.Context, _ *pb.UpdateGatewayRequest, _ ...grpc.CallOption) (*pb.UpdateGatewayResponse, error) {
			called = true
			return &pb.UpdateGatewayResponse{}, nil
		}}
		if err := advanceObservedRelease(context.Background(), client, gatewayWith("gw-1", "", "")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if called {
			t.Error("expected no UpdateGateway call when the gateway has no release_id")
		}
	})

	t.Run("write-back failure is surfaced", func(t *testing.T) {
		client := &fakeGatewayClient{updateFn: func(_ context.Context, _ *pb.UpdateGatewayRequest, _ ...grpc.CallOption) (*pb.UpdateGatewayResponse, error) {
			return nil, errors.New("grpc down")
		}}
		err := advanceObservedRelease(context.Background(), client, gatewayWith("gw-1", "rel-new", "rel-old"))
		if err == nil {
			t.Fatal("expected the write-back failure to be surfaced, got nil")
		}
	})
}
