package grpc

// GRPCStatusForTest exposes grpcStatusFor — the gRPC transport encoder over
// the Tempo head's shared tempo.ClassifyErr classification — to the external
// grpc_test package, so every class's code mapping can be pinned without
// standing up a full streaming RPC.
var GRPCStatusForTest = grpcStatusFor

// GRPCCodeToHTTPStatusTest exposes grpcCodeToHTTPStatus — the reverse of
// grpcCodeFor used by the query-telemetry interceptor (telemetry.go) to
// classify a gRPC status code through the same telemetry.ClassifyStatus
// the HTTP QueryMiddleware uses — so the mapping table can be pinned
// directly.
var GRPCCodeToHTTPStatusTest = grpcCodeToHTTPStatus
