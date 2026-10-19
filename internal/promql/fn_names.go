package promql

// PromQL function names the lowering matches on in more than one place.
const (
	// fnPredictLinear is predict_linear, also the plan IR name of its
	// RangeWindow.
	fnPredictLinear = "predict_linear"
)
