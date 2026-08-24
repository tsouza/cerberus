package surfaceparity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// inventoryDir is the checked-in shard directory that replaces the
// single-file test/surface-parity/inventory.json artifact (#2565, mirroring
// test/rejection-parity/catalogue.go's shard design from #2564). Every
// PromQL/LogQL/TraceQL symbol lands in its own shard — the finest
// granularity the ledger has — because the collision this design exists to
// avoid is two PRs each closing a coverage gap for a DIFFERENT function:
// under the old single-file artifact both rewrote the same 2000-line file
// and fought over unrelated entries, exactly the internal/promql/lower.go
// shape #2564 fixed for the rejection catalogue.
const inventoryDir = "inventory"

// inventoryShardSeparator / inventoryShardExt mirror
// catalogue.go's shardPathSeparator / shardExt.
const (
	inventoryShardSeparator = "__"
	inventoryShardExt       = ".json"
)

// inventoryShardFileMode / inventoryShardDirMode are the permissions a
// regenerated shard and its parent directory get.
const (
	inventoryShardFileMode = 0o644
	inventoryShardDirMode  = 0o755
)

// inventoryShard is the on-disk shape of one shard file: a single-entry
// slice, deliberately the same shape as test/rejection-parity/catalogue.go's
// catalogueShard so both sharded ledgers read the same at a glance. There is
// no other field — anything about the inventory as a whole would be
// duplicated across every shard and would itself become a merge conflict.
type inventoryShard struct {
	Entries []Entry `json:"entries"`
}

// encodeInventorySegment escapes one shard-key segment (either the head, or
// one ":"-delimited piece of the symbol) for safe, reversible embedding in a
// flat filename. Unlike catalogue.go's shardName — which REJECTS an
// ambiguous path component — this escapes instead, because a symbol segment
// can legitimately BE a literal "/" (the LogQL "op:/" division symbol) or
// contain "_", and an outright rejection would leave that symbol unshardable.
// "%" is escaped first so the escaping itself round-trips through
// decodeInventorySegment, then "_" (so no segment's own content can spell the
// "__" join separator), then "/" (so no segment is mistaken for a path
// separator once written to disk).
func encodeInventorySegment(seg string) (string, error) {
	if seg == "" {
		return "", fmt.Errorf("inventory shard key has an empty segment")
	}
	var b strings.Builder
	for _, r := range seg {
		switch r {
		case '%':
			b.WriteString("%25")
		case '_':
			b.WriteString("%5F")
		case '/':
			b.WriteString("%2F")
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}

// decodeInventorySegment reverses encodeInventorySegment via a generic %XX
// decode.
func decodeInventorySegment(seg string) string {
	var b strings.Builder
	for i := 0; i < len(seg); {
		if seg[i] == '%' && i+2 < len(seg) {
			if n, err := strconv.ParseUint(seg[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(seg[i])
		i++
	}
	return b.String()
}

// inventoryShardName maps a (head, symbol) pair — an inventory entry's
// unique identity, see TestInventoryShapeInvariants's duplicate check — to
// the shard file that owns it. The symbol is itself split on ":" so a
// multi-level symbol like "intrinsic:span:duration" reads as three joined
// segments rather than one segment carrying raw colons.
func inventoryShardName(head, symbol string) (string, error) {
	segs := append([]string{head}, strings.Split(symbol, ":")...)
	encoded := make([]string, len(segs))
	for i, s := range segs {
		e, err := encodeInventorySegment(s)
		if err != nil {
			return "", fmt.Errorf("shard key (head=%q, symbol=%q): %w", head, symbol, err)
		}
		encoded[i] = e
	}
	return strings.Join(encoded, inventoryShardSeparator) + inventoryShardExt, nil
}

// inventoryShardHeadSymbol reverses inventoryShardName.
func inventoryShardHeadSymbol(name string) (head, symbol string, err error) {
	stem, ok := strings.CutSuffix(name, inventoryShardExt)
	if !ok || stem == "" {
		return "", "", fmt.Errorf("shard file %q is not a %q-suffixed shard name", name, inventoryShardExt)
	}
	parts := strings.Split(stem, inventoryShardSeparator)
	if len(parts) < 2 {
		return "", "", fmt.Errorf("shard file %q does not encode a head/symbol pair", name)
	}
	decoded := make([]string, len(parts))
	for i, p := range parts {
		decoded[i] = decodeInventorySegment(p)
	}
	return decoded[0], strings.Join(decoded[1:], ":"), nil
}

// listInventoryShards returns the shard file names in dir, sorted.
func listInventoryShards(dir string) ([]string, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), inventoryShardExt) {
			continue
		}
		out = append(out, de.Name())
	}
	sort.Strings(out)
	return out, nil
}

// sortInventoryEntries orders entries the same way Generate does: by head,
// then by symbol within a head. Applied both to the merged in-memory value
// and (trivially, one entry at a time) within each shard.
func sortInventoryEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Head != b.Head {
			return headOrder(a.Head) < headOrder(b.Head)
		}
		return a.Symbol < b.Symbol
	})
}

// LoadInventoryDir reads every shard in dir and merges them into one
// Inventory, sorted exactly the way Generate produces it — byte-for-byte the
// same in-memory value the single-file artifact used to parse to, so nothing
// downstream of this function knows the artifact is sharded. A missing
// directory is returned as-is (os.IsNotExist holds) so the regen path can
// bootstrap from nothing.
func LoadInventoryDir(dir string) (*Inventory, error) {
	names, err := listInventoryShards(dir)
	if err != nil {
		return nil, err
	}
	inv := &Inventory{Source: inventorySource}
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path) //nolint:gosec // repo-relative artifact path
		if err != nil {
			return nil, err
		}
		var shard inventoryShard
		if err := json.Unmarshal(raw, &shard); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		inv.Entries = append(inv.Entries, shard.Entries...)
	}
	sortInventoryEntries(inv.Entries)
	return inv, nil
}

// ShardInventory renders the canonical on-disk form: shard file name -> file
// bytes (2-space indent + trailing newline), one entry per shard. An
// inventory with no entry for a given (head, symbol) yields no shard for it,
// which is what makes pruning in WriteInventoryDir a total operation rather
// than a guess.
func ShardInventory(inv *Inventory) (map[string][]byte, error) {
	out := make(map[string][]byte, len(inv.Entries))
	for _, e := range inv.Entries {
		name, err := inventoryShardName(e.Head, e.Symbol)
		if err != nil {
			return nil, err
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("shard %s is claimed by more than one entry — (head, symbol) is not unique", name)
		}
		b, err := json.MarshalIndent(inventoryShard{Entries: []Entry{e}}, "", "  ")
		if err != nil {
			return nil, err
		}
		out[name] = append(b, '\n')
	}
	return out, nil
}

// WriteInventoryDir writes the sharded form into dir and REMOVES shards that
// carry no entry any more. Pruning is not housekeeping: a shard left behind
// after its (head, symbol) drops out of the live parser surface keeps
// feeding a stale verdict into every future LoadInventoryDir call — mirrors
// catalogue.go's WriteCatalogue.
func WriteInventoryDir(dir string, inv *Inventory) error {
	shards, err := ShardInventory(inv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, inventoryShardDirMode); err != nil {
		return err
	}
	for name, body := range shards {
		if err := os.WriteFile(filepath.Join(dir, name), body, inventoryShardFileMode); err != nil {
			return err
		}
	}
	existing, err := listInventoryShards(dir)
	if err != nil {
		return err
	}
	for _, name := range existing {
		if _, keep := shards[name]; keep {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// DiffInventoryShards compares the rendered shards in want against the files
// in dir, returning one human-readable line per difference (a shard that is
// missing, one that lingers with no entry left, or one whose bytes drifted).
// An empty result means the checked-in directory is byte-for-byte what
// regeneration would write. Mirrors catalogue.go's DiffShards.
func DiffInventoryShards(dir string, want map[string][]byte) ([]string, error) {
	existing, err := listInventoryShards(dir)
	if err != nil {
		return nil, err
	}
	onDisk := map[string]bool{}
	for _, name := range existing {
		onDisk[name] = true
	}

	var diffs []string
	wantNames := make([]string, 0, len(want))
	for name := range want {
		wantNames = append(wantNames, name)
	}
	sort.Strings(wantNames)
	for _, name := range wantNames {
		path := filepath.Join(dir, name)
		if !onDisk[name] {
			diffs = append(diffs, fmt.Sprintf("%s: missing — the symbol gained its first catalogued verdict", path))
			continue
		}
		got, err := os.ReadFile(path) //nolint:gosec // repo-relative artifact path
		if err != nil {
			return nil, err
		}
		if string(got) != string(want[name]) {
			diffs = append(diffs, fmt.Sprintf("%s: stale — want %d bytes, got %d bytes", path, len(want[name]), len(got)))
		}
	}
	for _, name := range existing {
		if _, keep := want[name]; !keep {
			diffs = append(diffs, fmt.Sprintf("%s: stale shard — no symbol names it any more", filepath.Join(dir, name)))
		}
	}
	return diffs, nil
}
