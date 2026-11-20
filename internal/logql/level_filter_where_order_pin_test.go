package logql_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// levelFilterInWhereOrderDigests holds, per query, the SHA-256 of the
// statement a range aggregation lowers to with the WHERE-order fact set
// (chplan.SchemaStrategies.PrewhereInWhereOrder), so that a change to the
// arrangement a reordering server reads cannot move the statements of a
// fleet that keeps WHERE order.
var levelFilterInWhereOrderDigests = map[string]string{
	`sum(count_over_time({job="jsonapp"} | detected_level="unknown" [5m]))`:                                               "d6ffc238ca94d1eaba6550c6c15c9b73081627d84aa608077955317ba16cf03f",
	`sum(count_over_time({job="jsonapp"} | detected_level="info" or detected_level="unknown" [5m]))`:                      "f0b95a593bdc764d71db30be85ad8b406835d160a3c4154e617573eb28abd9cc",
	`sum(count_over_time({job="jsonapp"} | detected_level!="info" [5m]))`:                                                 "f3a4b6bd9b1adc209b81d522d7a54879f0bb7cdf72e4d72743b68d3786d72618",
	`sum(count_over_time({job="jsonapp"} | detected_level=~"unknown" [5m]))`:                                              "e8fe16d567e6d9bb35d9b4da51e666e220dd15faac6ecdb9c9092500de9a68a7",
	`sum(count_over_time({job="jsonapp"} | detected_level!~"unknown" [5m]))`:                                              "22ce44c9b0e2ee092aa96481a2733ac27633d9134a405ea59a823ab805d90a09",
	`sum(count_over_time({job="jsonapp"} | detected_level="unknown" |= "req" [5m]))`:                                      "38de428e5e1b88b74b3b6d43212bdf7564fe6344b66a2066695a71412bef9556",
	`sum(count_over_time({service_name="web-server"} | detected_level="error" |= "error" [5m]))`:                          "919d21fcf5803e2e172363c6da7bc091df8082b7276428404215f0fe931f45bd",
	`sum(count_over_time({service_name="tempo"} | detected_level=~"error|fatal" |= "request" [5m]))`:                      "9de777d6ec29a1fa73f45dc776c40afb0692d597647076d6818de587c8d94bed",
	`sum(count_over_time({service_name="web-server"} | detected_level!="info" |= "error" [5m]))`:                          "6d1b4564057d53b27fe21f8412e418cf78fbb3ceccf3d52ec74ae0efee7f5d89",
	`sum(count_over_time({service_name="tempo"} | detected_level!~"info|debug" |= "request" [5m]))`:                       "27b9e20c821103f2e031eaa78e20f507afefda6a3c2e15d2df1ea068b91946ec",
	`sum(count_over_time({service_name="web-server"} | detected_level="error" or detected_level="warn" |= "error" [5m]))`: "b26b99a6ce9fbdc84c51c103010bbb7d953f6fc06556752e97ebc7f8f3302798",
	`sum(count_over_time({service_name="web-server"} |= "error" | detected_level="error" [5m]))`:                          "04b15c45592897cd0894aa59fec703577fc13eb2e17fba457393ce58102ca76b",
	`sum by (detected_level) (count_over_time({service_name="web-server"} | detected_level=~"warn|error" [5m]))`:          "1f17e986ca27fb9533dbdba9d91ad2ea7b438c793fa6d90f91e66e1e8bedcf2d",
	`sum(count_over_time({service_name="web-server"} | detected_level="error" | json | status >= 400 [5m]))`:              "419884f7b3edf46fa2712c3f302162d7a788ee6198822dada5a3a10678940758",
	`sum(count_over_time({service_name="web-server"} | detected_level="error" | logfmt | status >= 400 [5m]))`:            "38804f267688927f93bfaab5084d9250b0f00eace2127b1c664652dbcb80b2db",
	`sum(rate({service_name="tempo"} | detected_level=~"error|fatal" |= "status=500" [1m]))`:                              "f10076c6fc809b02d99a7802f0a647da62241166b800bcac45d40fa64a17715f",
	`sum(count_over_time({service_name="tempo"} | logfmt | detected_level="error" [5m]))`:                                 "5e331a5a539f3e465ccb206668e9fd46a91d14b612186d7994a55f25b31db089",
	`count(sum_over_time({service_name="tempo"} | detected_level="error" | logfmt | unwrap spans | status > 100 [5m]))`:   "6be65c1169afae276e4f2010a55bc3f28fe3fdba1559cff20d47b44c93b7ff8a",
	`sum(count_over_time({job="wide"} | detected_level="fatal" | tenant!="t7" [5m]))`:                                     "40617b97a161442ffa878fad837682612fd74e0f3f11cd8d73b1a4b706757372",
	`sum(count_over_time({job="wide"} | detected_level="error" | tenant!="t7" |= "status=500" [5m]))`:                     "79e6304cf14b10509323ec127c5ad04e0af3d6184f9eebcb02db5b80366c0576",
	`sum(count_over_time({job="wide"} | detected_level="error" | route="/api/r7" [5m]))`:                                  "e62969c43e43afecbef8f66d4ab90157c25e4aea0f823f9666b49ab3b59f91ad",
	`sum(count_over_time({job="wide"} | detected_level=~"err.*" |= "error" [5m]))`:                                        "d02795bf5abf612ab6d3f3d5be6292f27e484c6e97b9d3ce10f669857ff44d33",
	`sum(count_over_time({job="wide"} | tenant="t7" |= "status=500" [5m]))`:                                               "80361d5438a2cbbee5c7caae0a0a821ff11803b2f42a0271465d663d984e7155",
	`sum(count_over_time({job="wide"} | tenant!="t7" [5m]))`:                                                              "377714d2410c29aa08dc1dbe58ca52d2390522324083c23b4a9f337d681b9fdb",
}

// TestRangeAggregationInWhereOrderStatementsArePinned pins the statement
// each query in levelFilterInWhereOrderDigests lowers to with the
// WHERE-order fact set, byte for byte.
func TestRangeAggregationInWhereOrderStatementsArePinned(t *testing.T) {
	t.Parallel()
	for query, want := range levelFilterInWhereOrderDigests {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			sql, _ := rangeAggregationSQLUnder(t, query, chplan.SchemaStrategies{PrewhereInWhereOrder: true})
			sum := sha256.Sum256([]byte(sql))
			if got := hex.EncodeToString(sum[:]); got != want {
				t.Errorf("in WHERE order, the statement's SHA-256 is %s, want %s: %s", got, want, sql)
			}
		})
	}
}
