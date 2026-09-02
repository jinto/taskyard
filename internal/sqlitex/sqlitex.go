// Package sqlitex는 두 SQLite 원장(server store, runner spool)이 함께 쓰는
// 작은 도움 함수다.
package sqlitex

import (
	"database/sql"
	"fmt"
)

// Column은 뒤늦게 추가된 컬럼 하나다. DDL은 "TEXT NOT NULL DEFAULT ''"처럼
// 타입부터 시작한다.
type Column struct {
	Name string
	DDL  string
}

// AddMissingColumns는 table에 없는 컬럼만 ALTER TABLE로 붙인다.
// CREATE TABLE IF NOT EXISTS는 이미 있는 테이블에 컬럼을 붙여 주지 않으므로,
// 옛 DB를 새 코드로 열 때 필요하다. PRAGMA table_info로 현재 컬럼을 읽고
// 비교하며, 드라이버 오류 문자열에는 기대지 않는다. 두 번 불러도 같다.
func AddMissingColumns(db *sql.DB, table string, cols []Column) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan %s schema: %w", table, err)
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}

	for _, c := range cols {
		if have[c.Name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + c.Name + ` ` + c.DDL); err != nil {
			return fmt.Errorf("add %s.%s: %w", table, c.Name, err)
		}
	}
	return nil
}
