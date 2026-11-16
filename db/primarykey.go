package db

import (
	"fmt"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

// A table's PRIMARY KEY names the attributes whose values determine a row's storage key.
//
// The store has always HAD a primary key -- every ad lives under a storage key, and
// Txn.NewClassAd replaces an existing ad at that key -- but nothing ever declared how a
// row's CONTENTS determine it. A writer supplied the key out of band: the SQL layer
// stamped a generated one into a "Key" attribute, and the HTCondor sync paths passed a
// job id they had in hand. That is workable for a writer that knows each row's identity
// and useless for one deriving rows from a query, which is why INSERT ... SELECT needs
// this and why nothing before it did.
//
// Declared here rather than per statement because that is what every SQL system does and,
// more to the point, what the storage model already implies: the key is a property of the
// table -- the thing that decides whether two writes are the same row -- not of the
// statement that happens to be writing. Keeping it on the table is also what makes a
// re-run idempotent without the re-run having to remember anything.
//
// It is OPTIONAL, and a table without one behaves exactly as before: INSERT ... VALUES
// keeps generating keys, and the sync paths keep passing their own. Only a writer that
// asks the table to derive a key (INSERT ... SELECT) requires the declaration, and it is
// refused rather than defaulted when absent -- see DeriveKey.

// primaryKeySep separates the rendered key columns in a derived key.
//
// Any separator would do for correctness. Each component is rendered by GroupValueText,
// which is ClassAd literal syntax and therefore self-delimiting -- a string carries its
// quotes and escapes, a list its braces -- so at the fixed arity a declaration fixes, the
// decomposition of a joined key is unique whatever character joins it, including one that
// appears raw inside a string. "/" is chosen because keys are read by operators (and
// appear in logs and in opQueryKeys output), and it is the separator the existing
// HTCondor-shaped keys already look like.
const primaryKeySep = "/"

// PrimaryKey returns the attributes whose values derive this table's storage keys, or nil
// if none is declared. The slice is a copy: the caller may keep or modify it.
func (db *DB) PrimaryKey() []string {
	var out []string
	db.cfg.with(func(cfg *persistedIndexConfig) {
		out = append([]string(nil), cfg.PrimaryKey...)
	})
	return out
}

// SetPrimaryKey declares the attributes whose values derive this table's storage keys, and
// persists the declaration. An empty attrs clears it.
//
// It does NOT rewrite the existing rows. Rows already stored keep the keys they were
// written under, so declaring a key on a populated table does not reorganise it or make
// the old rows addressable by the new rule -- the declaration governs writes that ask for
// a derived key from here on. That is the honest behaviour for a store whose key is its
// physical address, and the alternative (rekeying every row) is a table rewrite that
// should be asked for explicitly rather than implied by a DDL statement.
func (db *DB) SetPrimaryKey(attrs []string) error {
	cols, err := validatePrimaryKey(attrs)
	if err != nil {
		return err
	}
	return db.cfg.update(func(cfg *persistedIndexConfig) { cfg.PrimaryKey = cols })
}

// validatePrimaryKey checks a declaration and returns the columns to store.
//
// A private attribute is refused. Storage keys are not private data: opQueryKeys streams
// the keys of matching rows precisely so a caller can address rows without reading their
// bodies, and it is documented as exposing no private attribute because a key never held
// one. Deriving a key from a private attribute would put that value in every key the
// table hands out, which is a disclosure no reader of this declaration would expect and
// which no later redaction could take back.
func validatePrimaryKey(attrs []string) ([]string, error) {
	if len(attrs) == 0 {
		return nil, nil
	}
	cols := make([]string, 0, len(attrs))
	seen := make(map[string]bool, len(attrs))
	for _, a := range attrs {
		a = strings.TrimSpace(a)
		if a == "" {
			return nil, fmt.Errorf("primary key: empty attribute name")
		}
		if !validAttrName(a) {
			return nil, fmt.Errorf("primary key: %q is not a valid attribute name", a)
		}
		if classad.IsPrivateAttribute(a) {
			return nil, fmt.Errorf("primary key: %q is a private attribute, and a storage key is not private", a)
		}
		// ClassAd attribute names are case-insensitive, so two spellings of one name are
		// one column -- and would otherwise render that column's value into the key twice.
		lower := strings.ToLower(a)
		if seen[lower] {
			return nil, fmt.Errorf("primary key: attribute %q named twice", a)
		}
		seen[lower] = true
		cols = append(cols, a)
	}
	return cols, nil
}

// validAttrName reports whether name is a legal ClassAd attribute name: a letter or
// underscore followed by letters, digits or underscores. It is here rather than taken
// from a parser because the check is on a DECLARATION -- the point is to refuse
// `COUNT(*)` and friends at the moment someone declares them, with a message naming the
// attribute, rather than to discover later that no ad can ever carry one.
func validAttrName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_':
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return name != ""
}

// DeriveKey builds the storage key of an ad from the declared key columns.
//
// Each column's value is rendered by GroupValueText, the same function that decides which
// rows form one GROUP. That is deliberate and it is the whole reason the derived key is
// trustworthy for the statement that motivates it: in `INSERT INTO summaries SELECT ...
// GROUP BY day, Owner`, the rows arriving are groups, and deriving their keys with the
// grouping relation means one group maps to one key -- always, and without this code
// having to re-derive what "the same" means. It also inherits the canonicalisation that
// relation already makes: 1 and 1.0 are one group and so one key, and the string "4" is
// kept apart from the number 4 because the rendering quotes it.
//
// An undefined or error component is REFUSED rather than rendered. A missing key
// attribute is a mis-specified statement -- the SELECT did not produce the column the
// table says identifies a row -- and the alternative is worse than an error: every row
// missing that attribute would render the same `undefined` into its key, so they would
// all derive ONE key and silently overwrite each other down to a single surviving row.
// That is the exact failure this declaration exists to prevent, so it is refused at the
// one place that can still name which column was missing.
func DeriveKey(ad *classad.ClassAd, cols []string) (string, error) {
	if len(cols) == 0 {
		return "", fmt.Errorf("no primary key is declared")
	}
	var b strings.Builder
	for i, col := range cols {
		v := GroupValue(ad.EvaluateAttr(col))
		if v.IsUndefined() {
			return "", fmt.Errorf("primary key attribute %q is undefined in this row", col)
		}
		if v.IsError() {
			return "", fmt.Errorf("primary key attribute %q evaluates to error in this row", col)
		}
		if i > 0 {
			b.WriteString(primaryKeySep)
		}
		b.WriteString(GroupValueText(v))
	}
	return b.String(), nil
}

// DeriveKeys builds the keys for a whole batch and refuses a batch in which two rows
// derive the same key.
//
// The duplicate check is the point, not a nicety. Writing a batch whose keys collide is
// not an error anywhere below this: Txn.NewClassAd REPLACES the ad at a key, so N rows
// sharing a key would be written, counted as N, and leave ONE row behind -- a silent loss
// reported as a success. It happens whenever the declared key is coarser than the rows
// being inserted, which for `INSERT ... SELECT` means a GROUP BY that groups by more
// columns than the key names: a plausible statement, not a perverse one.
//
// So the batch is refused whole, naming the colliding key, and nothing is written. The
// caller re-runs with a key that matches the grain of its rows.
func DeriveKeys(ads []*classad.ClassAd, cols []string) ([]string, error) {
	keys := make([]string, len(ads))
	seen := make(map[string]int, len(ads))
	for i, ad := range ads {
		k, err := DeriveKey(ad, cols)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i+1, err)
		}
		if first, dup := seen[k]; dup {
			return nil, fmt.Errorf(
				"rows %d and %d both derive primary key %s from (%s): the declared key is "+
					"coarser than the rows being inserted, so writing them would keep only one",
				first+1, i+1, k, strings.Join(cols, ", "))
		}
		seen[k] = i
		keys[i] = k
	}
	return keys, nil
}
