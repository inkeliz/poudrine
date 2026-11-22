package database

import (
	"context"
	"encoding/json"
)

type compiledPackageTime struct {
	Package, Origin, ABI string
	Seconds              *int64
}

func (s DB) CompiledPackageTimes(ctx context.Context, names []string) ([]compiledPackageTime, error) {
	keys, err := json.Marshal(names)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT package,origin,abi,seconds FROM compiled_package_times
 WHERE package IN (SELECT value FROM json_each(?))
 ORDER BY package, started_ns DESC, seconds IS NOT NULL DESC`, string(keys))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []compiledPackageTime
	for rows.Next() {
		var t compiledPackageTime
		if err := rows.Scan(&t.Package, &t.Origin, &t.ABI, &t.Seconds); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

const compiledPackageTimesSchema = `CREATE TABLE IF NOT EXISTS compiled_package_times (
 package TEXT NOT NULL, origin TEXT NOT NULL, abi TEXT NOT NULL,
 started_ns INTEGER NOT NULL, seconds INTEGER CHECK(seconds >= 0),
 PRIMARY KEY(package,origin,abi)
) WITHOUT ROWID;
`
