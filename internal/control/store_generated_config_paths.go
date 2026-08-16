package control

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type generatedConfigPathQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func generatedConfigManagedPathsQuery(ctx context.Context, queryer generatedConfigPathQueryer, instanceID string) (map[string]string, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT package_id,path
		FROM addon_package_files
		WHERE instance_id=?1 AND lower(path) GLOB 'left4dead2/cfg/sourcemod/*.cfg'
		ORDER BY lower(path),package_id
		LIMIT ?2`, instanceID, maxSmartPluginGeneratedConfigs+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	managed := make(map[string]string)
	count := 0
	for rows.Next() {
		count++
		if count > maxSmartPluginGeneratedConfigs {
			return nil, errors.New("SourceMod generated config inventory contains too many entries")
		}
		var packageID, path string
		if err := rows.Scan(&packageID, &path); err != nil {
			return nil, err
		}
		if _, valid := generatedConfigManagedCFGBase(path); !valid {
			continue
		}
		managed[strings.ToLower(path)] = packageID
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return managed, nil
}
