package format

import "sort"

// CanonicalKey is a deterministic string form of a label set, used as a
// map key for grouping. Keys are sorted ASCII-ascending; pairs are
// joined as "k=v\x00" so two distinct label sets can't alias.
//
// Behaviour matches what each of api/{prom,loki,tempo} previously
// produced inline: prom used an inline insertion sort + []byte buffer;
// loki used sort.Strings + []byte; tempo used sort.Strings +
// strings.Builder. The wire output is identical across all three —
// they differed only in micro-implementation. This is the
// sort.Strings + []byte form (loki's), which is the fastest of the
// three for typical label-set sizes (<20 keys) without losing on
// allocation count.
func CanonicalKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, labels[k]...)
		b = append(b, 0)
	}
	return string(b)
}
