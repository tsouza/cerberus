package prom

import (
	"errors"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
)

// guardExceptionMessage accepts typed guard exceptions on both ClickHouse
// dials, or an anchored chDB exception envelope at the unwrapped driver error.
// A guard literal appearing only in failing SQL is not an exception message.
func guardExceptionMessage(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		// A typed non-guard exception is authoritative; never reinterpret its
		// SQL or any enclosing text as an untyped guard rejection.
		msg, ok := chclient.ThrowIfMessage(ex)
		if !ok {
			return "", false
		}
		return decodedGuardMessage(msg), true
	}
	if msg, ok := chclient.ThrowIfMessage(err); ok {
		return decodedGuardMessage(msg), true
	}
	for errors.Unwrap(err) != nil {
		err = errors.Unwrap(err)
	}
	// chdb-go's parquetStreamingRows.readNextChunkFromStream formats this
	// prefix with %s rather than %w; its QueryContext returns the bare engine
	// envelope. No other ambient prefix is accepted.
	raw := strings.TrimPrefix(err.Error(), "error in chunk: ")
	return stripGuardExceptionEnvelope(raw)
}

func decodedGuardMessage(msg string) string {
	// ch-go can retain the engine's envelope inside the typed Message field;
	// the native dial supplies the message without that envelope.
	if clean, ok := stripGuardExceptionEnvelope(msg); ok {
		return clean
	}
	return msg
}

// stripGuardExceptionEnvelope cuts ClickHouse's text envelope,
// `Code: <n>. DB::Exception: `, off a guard message for each code an
// emitted guard can raise (chclient.EmittedGuardCodes — the same set
// chclient wraps into a ThrowIfError on the typed path), so the two paths
// cannot recognise different code sets.
func stripGuardExceptionEnvelope(raw string) (string, bool) {
	for _, code := range chclient.EmittedGuardCodes() {
		if msg, ok := strings.CutPrefix(raw, guardExceptionEnvelopePrefix(code)); ok {
			return msg, true
		}
	}
	return "", false
}

// guardExceptionEnvelopePrefix renders the text envelope ClickHouse puts in
// front of an exception's message for the given error code.
func guardExceptionEnvelopePrefix(code int32) string {
	return "Code: " + strconv.Itoa(int(code)) + ". DB::Exception: "
}
