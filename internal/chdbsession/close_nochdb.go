//go:build !chdb

package chdbsession

// CloseForExit does nothing in the default build: without the `chdb` build
// tag the binary never links libchdb, so there is no native session to shut
// down. The no-op exists so an UNTAGGED TestMain — the only TestMain a
// package is allowed to declare — can call CloseForExit unconditionally.
func CloseForExit() {}
