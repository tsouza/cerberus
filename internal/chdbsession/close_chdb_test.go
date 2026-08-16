//go:build chdb

package chdbsession

import (
	"testing"

	"github.com/chdb-io/chdb-go/chdb"
)

// TestCloseForExitClosesTheCachedSessionOnce observes both promises through
// chdb-go's public cache contract: the first call replaces the cached session,
// while the second leaves that replacement intact because closeOnce is spent.
func TestCloseForExitClosesTheCachedSessionOnce(t *testing.T) {
	var replacement *chdb.Session
	t.Cleanup(func() {
		if replacement != nil {
			replacement.Close()
		}
	})

	first, err := chdb.NewSession()
	if err != nil {
		t.Fatalf("open first session: %v", err)
	}
	CloseForExit()

	second, err := chdb.NewSession()
	if err != nil {
		t.Fatalf("open replacement session: %v", err)
	}
	replacement = second
	if first == second {
		t.Fatal("CloseForExit retained the cached session instead of closing it")
	}

	CloseForExit()
	third, err := chdb.NewSession()
	if err != nil {
		t.Fatalf("read cached replacement session: %v", err)
	}
	if second != third {
		t.Error("the second CloseForExit call closed the replacement session; closeOnce did not hold")
	}
	third.Close()
	replacement = nil
}
