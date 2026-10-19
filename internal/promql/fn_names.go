package promql

// PromQL function names the lowering refers to in more than one place.
const (
	// fnHoltWinters is the PromQL and plan IR name of holt_winters, to
	// which its double_exponential_smoothing alias is lowered.
	fnHoltWinters = "holt_winters"
	// fnPredictLinear is the PromQL and plan IR name of predict_linear.
	fnPredictLinear = "predict_linear"
	// fnQuantileOverTime is the PromQL and plan IR name of
	// quantile_over_time.
	fnQuantileOverTime = "quantile_over_time"
)
