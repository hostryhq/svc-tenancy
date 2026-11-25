// Package nulldb is a database/sql driver that accepts every statement and stores nothing. The test
// double uses it so that go-auth's audited transaction (auth.WithAuditedTx, which needs a *sql.Tx) runs
// the same code path as in production while the data lives in memory.
package nulldb

import (
	"database/sql"
	"database/sql/driver"
	"errors"
)

func init() { sql.Register("hostry-nulldb", drv{}) }

// Open returns a handle whose transactions do nothing.
func Open() *sql.DB {
	db, err := sql.Open("hostry-nulldb", "")
	if err != nil {
		panic(err)
	}
	return db
}

type drv struct{}

func (drv) Open(string) (driver.Conn, error) { return conn{}, nil }

type conn struct{}

func (conn) Prepare(string) (driver.Stmt, error) { return stmt{}, nil }
func (conn) Close() error                        { return nil }
func (conn) Begin() (driver.Tx, error)           { return tx{}, nil }

type tx struct{}

func (tx) Commit() error   { return nil }
func (tx) Rollback() error { return nil }

type stmt struct{}

func (stmt) Close() error  { return nil }
func (stmt) NumInput() int { return -1 }
func (stmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}
func (stmt) Query([]driver.Value) (driver.Rows, error) {
	return nil, errors.New("nulldb: queries are not supported")
}
