package db

import (
	"fmt"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

// INSERT ... SELECT: writing a query's answer back into a table.
//
// The motivating shape is a roll-up -- "INSERT INTO daily_summaries SELECT <bucket>, Owner,
// COUNT(*), SUM(...) FROM history GROUP BY ..." -- run on the server so the scan, the
// reduction and the write all happen where the data is and only a row count comes back.
//
// This file is the WRITE half only. Computing the rows is the existing aggregate path,
// unchanged and reached by whichever caller already knows how to resolve a source table
// (a mutable table and an archive have separate entry points and separate ladders). Keeping
// the halves apart is what lets the roll-up inherit every aggregate optimisation there is --
// the columnar group-by, the archive's zone maps, the O(1) counts -- rather than growing a
// second, worse copy of them here.
//
// What this half owns is the part that is new and is where the quiet mistakes live: naming
// the columns, carrying the values across WITHOUT going through text, and deciding each
// row's storage key from the target's declared primary key.

// InsertSpec names the columns of the rows being inserted.
//
// Names has one entry per group column followed by one per aggregate -- the same order
// AggRow lays out Group and Values -- and each is the ATTRIBUTE the value is written under
// in the target table.
//
// They are carried explicitly rather than derived from the source query because a group
// column's name is only sometimes its attribute (a time bucket has none) and an aggregate
// has none at all: `COUNT(*)` is a legal column header and not a legal ClassAd attribute
// name. A caller that let the header stand in for the attribute would be writing ads that
// no query can read back.
type InsertSpec struct {
	GroupCols []GroupCol
	Aggs      []AggSpec
	Names     []string
}

// Validate checks the spec on its own: one name per column, each a usable attribute, none
// repeated and none private.
//
// A private name is refused for the same reason a private primary key column is (see
// validatePrimaryKey): the roll-up's output is an ordinary table that ordinary readers
// query, and an aggregate over private data written into a non-private attribute would
// launder it. Refusing the NAME is the narrow, checkable half of that -- it does not stop
// someone aggregating a private attribute into a public one, which is a question for
// whoever authorises the statement, but it does stop the result being stored somewhere
// that redaction no longer applies.
func (s InsertSpec) Validate() error {
	want := len(s.GroupCols) + len(s.Aggs)
	if len(s.Names) != want {
		return fmt.Errorf("insert: %d column names for %d group columns and %d aggregates",
			len(s.Names), len(s.GroupCols), len(s.Aggs))
	}
	if want == 0 {
		return fmt.Errorf("insert: nothing to insert (no group columns and no aggregates)")
	}
	seen := make(map[string]bool, want)
	for _, n := range s.Names {
		if !validAttrName(n) {
			return fmt.Errorf("insert: %q is not a valid attribute name (an aggregate or an "+
				"expression needs an alias to name the column it is written to)", n)
		}
		if classad.IsPrivateAttribute(n) {
			return fmt.Errorf("insert: %q is a private attribute", n)
		}
		lower := strings.ToLower(n)
		if seen[lower] {
			return fmt.Errorf("insert: column %q named twice", n)
		}
		seen[lower] = true
	}
	return nil
}

// covers reports whether every declared key column is among the inserted column names.
func (s InsertSpec) covers(keyCols []string) error {
	have := make(map[string]bool, len(s.Names))
	for _, n := range s.Names {
		have[strings.ToLower(n)] = true
	}
	var missing []string
	for _, k := range keyCols {
		if !have[strings.ToLower(k)] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"insert: the target's primary key is (%s) but the inserted columns (%s) do not "+
				"include %s, so no row can be given a key",
			strings.Join(keyCols, ", "), strings.Join(s.Names, ", "), strings.Join(missing, ", "))
	}
	return nil
}

// BuildInsertAds turns aggregate result rows into the ads to be written.
//
// The group half and the aggregate half are read back DIFFERENTLY, and the asymmetry is
// not an oversight -- it is what the two texts can support:
//
//   - A group's text is a faithful ClassAd literal, and ParseGroupValue inverts every one
//     of them exactly. So a row whose producer supplied GroupValues is read from those, and
//     one whose producer did not is read back from the label with no loss.
//   - An aggregate's text is a RENDERING with no inverse: ValueText drops a string's
//     quotes, so "4" and 4 arrive identically. There is nothing to invert, so the typed
//     value is REQUIRED and a row without one is refused rather than guessed at.
//
// Refusing is the right failure because the alternative is silent and permanent: a count
// guessed back as the string "12" would be stored as text and would compare and sort as
// text for the rest of that table's life, with nothing to say it happened. Every in-process
// aggregate path fills TypedValues; a row lacking it came from somewhere that does not, and
// that is a bug to fix at the producer rather than paper over here.
func BuildInsertAds(rows []AggRow, spec InsertSpec) ([]*classad.ClassAd, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	nGroup := len(spec.GroupCols)
	ads := make([]*classad.ClassAd, 0, len(rows))
	for ri, row := range rows {
		if len(row.Group) < nGroup {
			return nil, fmt.Errorf("insert: row %d has %d group cells, want %d",
				ri+1, len(row.Group), nGroup)
		}
		if len(spec.Aggs) > 0 && len(row.TypedValues) != len(spec.Aggs) {
			return nil, fmt.Errorf(
				"insert: row %d carries no typed aggregate values (%d of %d), so its "+
					"aggregates can only be read back as text; refusing rather than guessing "+
					"their types", ri+1, len(row.TypedValues), len(spec.Aggs))
		}
		ad := classad.New()
		for gi := 0; gi < nGroup; gi++ {
			v := groupValueAt(row, gi)
			if err := setInsertAttr(ad, spec.Names[gi], v); err != nil {
				return nil, fmt.Errorf("insert: row %d, column %q: %w", ri+1, spec.Names[gi], err)
			}
		}
		for ai := range spec.Aggs {
			name := spec.Names[nGroup+ai]
			if err := setInsertAttr(ad, name, row.TypedValues[ai]); err != nil {
				return nil, fmt.Errorf("insert: row %d, column %q: %w", ri+1, name, err)
			}
		}
		ads = append(ads, ad)
	}
	return ads, nil
}

// groupValueAt reads group column gi as a value: the producer's typed one when it supplied
// them, otherwise the label read back, which is exact for a group (see BuildInsertAds).
func groupValueAt(row AggRow, gi int) classad.Value {
	if gi < len(row.GroupValues) {
		return row.GroupValues[gi]
	}
	return ParseGroupValue(row.Group[gi])
}

// setInsertAttr writes one value into the ad being built.
//
// UNDEFINED LEAVES THE ATTRIBUTE OUT. In a ClassAd an absent attribute IS undefined, so
// writing `X = undefined` does not record the same thing -- it creates an attribute where
// the query found none, and every later `is undefined` test and every projection sees a
// different shape. An aggregate legitimately yields undefined (MIN over a group with no
// defined value), so this is a normal row, not an error.
//
// ERROR is refused. It is not a value a summary table can usefully hold: stored, it
// propagates into everything that reads the column and there is nothing in the row to say
// which input produced it. Better to fail the statement while the column name is still in
// hand.
func setInsertAttr(ad *classad.ClassAd, name string, v classad.Value) error {
	switch {
	case v.IsUndefined():
		return nil
	case v.IsError():
		return fmt.Errorf("value is error")
	case v.IsInteger():
		if i, err := v.IntValue(); err == nil {
			ad.InsertAttr(name, i)
			return nil
		}
	case v.IsReal():
		if f, err := v.RealValue(); err == nil {
			ad.InsertAttrFloat(name, f)
			return nil
		}
	case v.IsBool():
		if b, err := v.BoolValue(); err == nil {
			ad.InsertAttrBool(name, b)
			return nil
		}
	case v.IsString():
		if s, err := v.StringValue(); err == nil {
			ad.InsertAttrString(name, s)
			return nil
		}
	}
	// A list or a nested ad: round-trip through the canonical literal, which is the one
	// rendering of these kinds that parses back as the same value (GroupValueText exists
	// for exactly that property). The scalar kinds above do not come this way because
	// they have a direct setter and need no parser.
	text := GroupValueText(v)
	expr, err := classad.ParseExpr(text)
	if err != nil {
		return fmt.Errorf("value %s cannot be written as a ClassAd expression: %w", text, err)
	}
	ad.InsertExpr(name, expr)
	return nil
}

// InsertDerived writes ads into the table under keys derived from its declared primary key,
// as ONE transaction.
//
// One transaction because a roll-up that half-applied would be worse than one that did not
// run: the table would hold a mixture of this run's rows and the last run's, with no marker
// saying where the boundary is, and the natural fix (run it again) would not repair it.
//
// Re-running a roll-up over the same source IS the expected use, and it converges rather
// than accumulating: a derived key is a function of the row's key columns, so a second run
// writes the same keys and Txn.NewClassAd replaces each. That is the whole reason the key
// is declared on the table and derived from the data instead of generated.
func (db *DB) InsertDerived(ads []*classad.ClassAd) (int, error) {
	keyCols := db.PrimaryKey()
	if len(keyCols) == 0 {
		return 0, fmt.Errorf(
			"insert: this table declares no primary key, so a row's key cannot be derived " +
				"from its contents; declare one (CREATE TABLE ... PRIMARY KEY (...)) or " +
				"insert rows with explicit keys")
	}
	keys, err := DeriveKeys(ads, keyCols)
	if err != nil {
		return 0, err
	}
	if len(ads) == 0 {
		return 0, nil
	}
	tx := db.Begin()
	for i, ad := range ads {
		tx.NewClassAd(keys[i], ad)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ads), nil
}

// InsertSelectInto is the write half end to end: validate the spec against the target's
// declared key, build the ads and write them.
//
// The key coverage is checked BEFORE any ad is built so the failure names the mismatch
// between the declaration and the statement -- "the key is (Day, Owner) and you inserted
// (Day, Jobs)" -- rather than surfacing later as the same complaint about every row in
// turn, which says the same thing len(rows) times and buries which column was meant.
func (db *DB) InsertSelectInto(rows []AggRow, spec InsertSpec) (int, error) {
	if err := spec.Validate(); err != nil {
		return 0, err
	}
	keyCols := db.PrimaryKey()
	if len(keyCols) == 0 {
		return 0, fmt.Errorf(
			"insert: table declares no primary key, so a row's key cannot be derived from " +
				"its contents; declare one with PRIMARY KEY (...)")
	}
	if err := spec.covers(keyCols); err != nil {
		return 0, err
	}
	ads, err := BuildInsertAds(rows, spec)
	if err != nil {
		return 0, err
	}
	return db.InsertDerived(ads)
}
