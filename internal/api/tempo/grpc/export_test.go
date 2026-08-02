package grpc

// GRPCStatusForTest exposes grpcStatusFor — the gRPC transport encoder over
// the Tempo head's shared tempo.ClassifyErr classification — to the external
// grpc_test package, so every class's code mapping can be pinned without
// standing up a full streaming RPC.
var GRPCStatusForTest = grpcStatusFor
