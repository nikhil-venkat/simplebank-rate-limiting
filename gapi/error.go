package gapi

import (
	"fmt"
	"math"
	"time"

	"github.com/techschool/simplebank/ratelimit"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func fieldViolation(field string, err error) *errdetails.BadRequest_FieldViolation {
	return &errdetails.BadRequest_FieldViolation{
		Field:       field,
		Description: err.Error(),
	}
}

func invalidArgumentError(violations []*errdetails.BadRequest_FieldViolation) error {
	badRequest := &errdetails.BadRequest{FieldViolations: violations}
	statusInvalid := status.New(codes.InvalidArgument, "invalid parameters")

	statusDetails, err := statusInvalid.WithDetails(badRequest)
	if err != nil {
		return statusInvalid.Err()
	}

	return statusDetails.Err()
}

func unauthenticatedError(err error) error {
	return status.Errorf(codes.Unauthenticated, "unauthorized: %s", err)
}

// retryAfterSeconds rounds a retry delay up to whole seconds, never below 1.
// Rounding down would hand the caller a value that is still rate limited, and
// a Retry-After of 0 invites an immediate retry storm.
func retryAfterSeconds(retryAfter time.Duration) int {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}

// rateLimitError builds the status returned to a throttled caller.
//
// Both transports go through here. The gRPC interceptor returns its Err(), and
// the HTTP middleware marshals its Proto(), so a throttled client sees the same
// code, the same message, and the same RetryInfo detail on either port.
//
// grpc-gateway maps codes.ResourceExhausted to HTTP 429.
func rateLimitError(decision ratelimit.Decision) *status.Status {
	seconds := retryAfterSeconds(decision.RetryAfter)

	unit := "seconds"
	if seconds == 1 {
		unit = "second"
	}

	statusRateLimited := status.New(codes.ResourceExhausted, fmt.Sprintf(
		"rate limit exceeded: too many requests from your IP address, please retry after %d %s",
		seconds, unit,
	))

	statusDetails, err := statusRateLimited.WithDetails(&errdetails.RetryInfo{
		RetryDelay: durationpb.New(time.Duration(seconds) * time.Second),
	})
	if err != nil {
		return statusRateLimited
	}

	return statusDetails
}
