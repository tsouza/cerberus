package chplan

// OTelExpoHistogramDefaultMaxSize is the OTel exponential-histogram spec's
// documented default MaxSize: the bucket budget every major OTel SDK ships
// unconfigured, which a single series auto-narrows its own scale to stay
// within. Shared here so the query-side merge width cap and the SDK view
// cerberus collects its own duration histogram with name the same budget.
const OTelExpoHistogramDefaultMaxSize = 160
