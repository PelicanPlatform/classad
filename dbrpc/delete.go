package dbrpc

import "context"

// DeleteWhere removes every ad matching constraint from the default table and
// returns the number removed. The delete runs server-side as a single call --
// the db.DeleteWhere pushdown (batched, optimistic-retry, self-healing) -- rather
// than a per-key query-then-delete round trip per ad. Refused on a read-only
// connection.
func (c *Client) DeleteWhere(ctx context.Context, constraint string) (int, error) {
	return c.DeleteWhereTable(ctx, DefaultTable, constraint)
}

// DeleteWhereTable is DeleteWhere against a named table. Express a time-based
// expiry sweep as the constraint (e.g. "<now> > LastHeardFrom + Lifetime").
func (c *Client) DeleteWhereTable(ctx context.Context, table, constraint string) (int, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		return putStr(putStr(req(id, opDeleteWhere), table), constraint)
	})
	if err != nil {
		return 0, err
	}
	if status != stOK {
		return 0, statusErr(status, body)
	}
	return int(body.i32()), nil
}
