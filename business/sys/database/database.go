// Package database provides support for access the database.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ardanlabs/service/foundation/web"
	"github.com/jmoiron/sqlx"
	"github.com/lib/pq" // Calls init function.
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
)

// lib/pq errorCodeNames
// https://github.com/lib/pq/blob/master/error.go#L178
const uniqueViolation = "23505"

// Set of error variables for CRUD operations.
var (
	ErrDBNotFound        = errors.New("not found")
	ErrDBDuplicatedEntry = errors.New("duplicated entry")
)

// Config is the required properties to use the database.
type Config struct {
	User         string
	Password     string
	Host         string
	Name         string
	MaxIdleConns int
	MaxOpenConns int
	DisableTLS   bool
}

// Open knows how to open a database connection based on the configuration.
func Open(cfg Config) (*sqlx.DB, error) {
	sslMode := "require"
	if cfg.DisableTLS {
		sslMode = "disable"
	}

	q := make(url.Values)
	q.Set("sslmode", sslMode)
	q.Set("timezone", "utc")

	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     cfg.Host,
		Path:     cfg.Name,
		RawQuery: q.Encode(),
	}

	db, err := sqlx.Open("postgres", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetMaxOpenConns(cfg.MaxOpenConns)

	return db, nil
}

// StatusCheck returns nil if it can successfully talk to the database. It
// returns a non-nil error otherwise.
func StatusCheck(ctx context.Context, db *sqlx.DB) error {

	// First check we can ping the database.
	var pingError error
	for attempts := 1; ; attempts++ {
		pingError = db.Ping()
		if pingError == nil {
			break
		}
		time.Sleep(time.Duration(attempts) * 100 * time.Millisecond)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	// Make sure we didn't timeout or be cancelled.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Run a simple query to determine connectivity. Running this query forces a
	// round trip through the database.
	const q = `SELECT true`
	var tmp bool
	return db.QueryRowContext(ctx, q).Scan(&tmp)
}

// WithinTran runs passed function and do commit/rollback at the end.
func WithinTran(ctx context.Context, log *zap.SugaredLogger, db *sqlx.DB, fn func(*sqlx.Tx) error) error {
	traceID := web.GetTraceID(ctx)

	// Begin the transaction.
	log.Infow("begin tran")
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("begin tran: %w", err)
	}

	// We can defer the rollback since the code checks if the transaction
	// has already been committed.
	defer func() {
		if err := tx.Rollback(); err != nil {
			if errors.Is(err, sql.ErrTxDone) {
				return
			}
			log.Errorw("unable to rollback tran", "trace_id", traceID, "ERROR", err)
		}
		log.Infow("rollback tran", "trace_id", traceID)
	}()

	// Execute the code inside the transaction. If the function
	// fails, return the error and the defer function will roll back.
	if err := fn(tx); err != nil {

		// Checks if the error is of code 23505 (unique_violation).
		if pqerr, ok := err.(*pq.Error); ok && pqerr.Code == uniqueViolation {
			return ErrDBDuplicatedEntry
		}
		return fmt.Errorf("exec tran: %w", err)
	}

	// Commit the transaction.
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tran: %w", err)
	}
	log.Infow("commit tran", "trace_id", traceID)

	return nil
}

// ExecContext is a helper function to execute a CUD operation with
// logging and tracing.
func ExecContext(ctx context.Context, log *zap.SugaredLogger, db sqlx.ExtContext, query string) error {
	return NamedExecContext(ctx, log, db, query, struct{}{})
}

// NamedExecContext is a helper function to execute a CUD operation with
// logging and tracing where field replacement is necessary.
func NamedExecContext(ctx context.Context, log *zap.SugaredLogger, db sqlx.ExtContext, query string, data any) error {
	q := queryString(query, data)
	log.Infow("database.NamedExecContext", "trace_id", web.GetTraceID(ctx), "query", q)

	ctx, span := web.AddSpan(ctx, "business.sys.database.exec", attribute.String("query", q))
	defer span.End()

	if _, err := sqlx.NamedExecContext(ctx, db, query, data); err != nil {

		// Checks if the error is of code 23505 (unique_violation).
		if pqerr, ok := err.(*pq.Error); ok && pqerr.Code == uniqueViolation {
			return ErrDBDuplicatedEntry
		}
		return err
	}

	return nil
}

// QuerySlice is a helper function for executing queries that return a
// collection of data to be unmarshalled into a slice.
func QuerySlice[T any](ctx context.Context, log *zap.SugaredLogger, db sqlx.ExtContext, query string, dest *[]T) error {
	return NamedQuerySlice(ctx, log, db, query, struct{}{}, dest)
}

// NamedQuerySlice is a helper function for executing queries that return a
// collection of data to be unmarshalled into a slice where field replacement is necessary.
func NamedQuerySlice[T any](ctx context.Context, log *zap.SugaredLogger, db sqlx.ExtContext, query string, data any, dest *[]T) error {
	q := queryString(query, data)
	log.Infow("database.NamedQuerySlice", "trace_id", web.GetTraceID(ctx), "query", q)

	ctx, span := web.AddSpan(ctx, "business.sys.database.queryslice", attribute.String("query", q))
	defer span.End()

	rows, err := sqlx.NamedQueryContext(ctx, db, query, data)
	if err != nil {
		return err
	}
	defer rows.Close()

	var slice []T
	for rows.Next() {
		v := new(T)
		if err := rows.StructScan(v); err != nil {
			return err
		}
		slice = append(slice, *v)
	}
	*dest = slice

	return nil
}

// QueryStruct is a helper function for executing queries that return a
// single value to be unmarshalled into a struct type where field replacement is necessary.
func QueryStruct(ctx context.Context, log *zap.SugaredLogger, db sqlx.ExtContext, query string, dest any) error {
	return NamedQueryStruct(ctx, log, db, query, struct{}{}, dest)
}

// NamedQueryStruct is a helper function for executing queries that return a
// single value to be unmarshalled into a struct type where field replacement is necessary.
func NamedQueryStruct(ctx context.Context, log *zap.SugaredLogger, db sqlx.ExtContext, query string, data any, dest any) error {
	q := queryString(query, data)
	log.Infow("database.NamedQueryStruct", "trace_id", web.GetTraceID(ctx), "query", q)

	ctx, span := web.AddSpan(ctx, "business.sys.database.query", attribute.String("query", q))
	defer span.End()

	rows, err := sqlx.NamedQueryContext(ctx, db, query, data)
	if err != nil {
		return err
	}
	defer rows.Close()

	if !rows.Next() {
		return ErrDBNotFound
	}

	if err := rows.StructScan(dest); err != nil {
		return err
	}

	return nil
}

// queryString provides a pretty print version of the query and parameters.
func queryString(query string, args ...any) string {
	query, params, err := sqlx.Named(query, args)
	if err != nil {
		return err.Error()
	}

	for _, param := range params {
		var value string
		switch v := param.(type) {
		case string:
			value = fmt.Sprintf("%q", v)
		case []byte:
			value = fmt.Sprintf("%q", string(v))
		default:
			value = fmt.Sprintf("%v", v)
		}
		query = strings.Replace(query, "?", value, 1)
	}

	query = strings.ReplaceAll(query, "\t", "")
	query = strings.ReplaceAll(query, "\n", " ")

	return strings.Trim(query, " ")
}
