package grpc

// MapEngineErrorForTest exposes mapEngineError to the external grpc_test package
// so the resource-exhausted / invalid-argument classification can be pinned
// without standing up a full streaming RPC.
var MapEngineErrorForTest = mapEngineError
