package canon

import "strconv"

func formatInt(i int64) string { return strconv.FormatInt(i, 10) }

func quote(s string) string { return strconv.Quote(s) }
