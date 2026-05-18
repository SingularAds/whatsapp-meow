package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

func main() {
	path := "./data/smba.db"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	fmt.Printf("Inspecting DB: %s\n\n", path)

	rows, err := db.Query("SELECT name, type, sql FROM sqlite_master WHERE type IN ('table','view') ORDER BY name")
	if err != nil {
		log.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	tables := []string{}
	for rows.Next() {
		var name, typ, sqlStmt string
		if err := rows.Scan(&name, &typ, &sqlStmt); err != nil {
			log.Fatalf("scan sqlite_master: %v", err)
		}
		fmt.Printf("- %s (%s)\n", name, typ)
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("rows err: %v", err)
	}

	if len(tables) == 0 {
		fmt.Println("No tables found in DB.")
		return
	}

	for _, t := range tables {
		// PRAGMA table_info
		colRows, err := db.Query(fmt.Sprintf("PRAGMA table_info(\"%s\")", t))
		if err != nil {
			fmt.Printf("\nTable %s — cannot read schema: %v\n", t, err)
			continue
		}
		cols := []string{}
		for colRows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt sql.NullString
			if err := colRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				fmt.Printf("error scanning pragma for %s: %v\n", t, err)
				break
			}
			cols = append(cols, name)
		}
		colRows.Close()

		fmt.Printf("\nTable %s — columns: %s\n", t, strings.Join(cols, ", "))

		// row count
		var cnt int
		if err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM \"%s\"", t)).Scan(&cnt); err != nil {
			fmt.Printf("  count error: %v\n", err)
			continue
		}
		fmt.Printf("  Rows: %d\n", cnt)

		// sample up to 5 rows
		lim := 5
		q := fmt.Sprintf("SELECT * FROM \"%s\" LIMIT %d", t, lim)
		r2, err := db.Query(q)
		if err != nil {
			fmt.Printf("  select sample error: %v\n", err)
			continue
		}
		colsNames, _ := r2.Columns()
		for r2.Next() {
			vals := make([]interface{}, len(colsNames))
			ptrs := make([]interface{}, len(colsNames))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r2.Scan(ptrs...); err != nil {
				fmt.Printf("  row scan error: %v\n", err)
				break
			}
			fmt.Print("  - ")
			for i, cn := range colsNames {
				v := vals[i]
				var s string
				switch vv := v.(type) {
				case nil:
					s = "NULL"
				case []byte:
					bs := vv
					if isPrintable(bs) && len(bs) < 200 {
						s = fmt.Sprintf("%q", string(bs))
					} else {
						s = fmt.Sprintf("<%d bytes>", len(bs))
					}
				default:
					s = fmt.Sprintf("%v", vv)
				}
				fmt.Printf("%s=%s; ", cn, s)
			}
			fmt.Println()
		}
		r2.Close()
	}
}

func isPrintable(b []byte) bool {
	for _, c := range b {
		if c == 9 || c == 10 || c == 13 {
			continue
		}
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}
