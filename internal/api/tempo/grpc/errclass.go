package grpc

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tsouza/cerberus/internal/api/tempo"
)

// grpcStatusFor is the gRPC transport encoder for the Tempo head's single
// error classification (tempo.ClassifyErr — see
// internal/api/tempo/errclass.go for the class definitions and why the
// classification is shared while the encoding is not).
//
// Every RPC in this package routes its pipeline errors through here, so the
// Search and metrics RPCs cannot drift from each other or from the HTTP
// handlers. The HTTP↔gRPC correspondence is the canonical one:
//
//	ErrClassParse             400 Bad Request           InvalidArgument
//	ErrClassLower             422 Unprocessable Entity  InvalidArgument
//	ErrClassResourceExhausted 422 Unprocessable Entity  ResourceExhausted
//	ErrClassUnavailable       503 Service Unavailable   Unavailable
//	ErrClassCanceled          499 Client Closed Request Canceled
//	ErrClassUpstream          502 Bad Gateway           Unavailable
//	ErrClassInternal          500 Internal Server Error Internal
//
// Parse and lower both land on InvalidArgument because gRPC has no separate
// code for "parsed but unevaluable"; the HTTP encoder keeps the distinction
// its status space affords. ErrClassUpstream joins ErrClassUnavailable on
// Unavailable for the same reason — gRPC has no Bad Gateway, and Unavailable
// is the canonical mapping for a failed upstream hop.
func grpcStatusFor(err error) error {
	if err == nil {
		return nil
	}
	return status.Errorf(grpcCodeFor(err), "%v", err)
}

// grpcCodeFor is grpcStatusFor's classification half, split out so the
// streaming paths can reuse the code without re-wrapping a message.
func grpcCodeFor(err error) codes.Code {
	switch tempo.ClassifyErr(err) {
	case tempo.ErrClassCanceled:
		return codes.Canceled
	case tempo.ErrClassParse, tempo.ErrClassLower:
		return codes.InvalidArgument
	case tempo.ErrClassResourceExhausted:
		return codes.ResourceExhausted
	case tempo.ErrClassUnavailable, tempo.ErrClassUpstream:
		return codes.Unavailable
	case tempo.ErrClassInternal:
		return codes.Internal
	}
	return codes.Internal
}
