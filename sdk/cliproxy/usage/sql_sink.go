package usage

// This file contains the durable billing ledger boundary. It intentionally
// does not model accounts, balances, or API-key quotas: those are application
// policy and can be added by the OnInsert transaction hook. The ledger itself
// provides an idempotent, auditable record of every priced request.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SQLChargeHook runs inside the same SQL transaction as the ledger insert,
// and only when a new event was inserted. It can atomically update an account,
// quota, or subscription table. Returning an error rolls back the whole
// transaction, including the ledger row.
type SQLChargeHook func(context.Context, *sql.Tx, Charge) error

// SQLChargeSink is a PostgreSQL-compatible implementation of ChargeSink. The
// CLIProxyAPI store already uses github.com/jackc/pgx/v5/stdlib, so this type
// depends only on database/sql and does not add a database driver.
type SQLChargeSink struct {
	DB       *sql.DB
	Table    string
	OnInsert SQLChargeHook
}

// PostgresChargeSink is kept as an explicit alias for callers that want to
// document the PostgreSQL dialect used by this implementation.
type PostgresChargeSink = SQLChargeSink

// Aggregate returns durable account totals directly from the ledger. Account
// identity is read from usage_record.auth_id, falling back to the hashed API
// key when auth_id is absent. The final row has Account="total".
func (s *SQLChargeSink) Aggregate(ctx context.Context) ([]AccountTotal, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("sql charge sink: database is nil")
	}
	query := fmt.Sprintf(`SELECT COALESCE(NULLIF(usage_record->>'AuthID',''), NULLIF(usage_record->>'auth_id',''), NULLIF(api_key_sha256,''), 'unknown') AS account,
COUNT(*)::BIGINT, COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(amount_micros),0), COALESCE(SUM(total_usd),0)
FROM %s
WHERE LOWER(provider) = 'claude' OR LOWER(provider) = 'anthropic' OR LOWER(provider) LIKE '%%claude%%'
GROUP BY 1 ORDER BY 1`, s.tableName())
	rows, err := s.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sql charge sink: aggregate: %w", err)
	}
	defer rows.Close()
	result := make([]AccountTotal, 0)
	var total AccountTotal
	total.Account = "total"
	for rows.Next() {
		var t AccountTotal
		if err := rows.Scan(&t.Account, &t.Requests, &t.InputTokens, &t.OutputTokens, &t.CacheRead, &t.CacheWrite, &t.TotalTokens, &t.TotalMicros, &t.TotalUSD); err != nil {
			return nil, err
		}
		result = append(result, t)
		total.Requests += t.Requests
		total.InputTokens += t.InputTokens
		total.OutputTokens += t.OutputTokens
		total.CacheRead += t.CacheRead
		total.CacheWrite += t.CacheWrite
		total.TotalTokens += t.TotalTokens
		total.TotalMicros += t.TotalMicros
		total.TotalUSD += t.TotalUSD
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return append(result, total), nil
}

func NewPostgresChargeSink(db *sql.DB, table string) *SQLChargeSink {
	return NewSQLChargeSink(db, table)
}

func NewSQLChargeSink(db *sql.DB, table string) *SQLChargeSink {
	if strings.TrimSpace(table) == "" {
		table = "billing_ledger"
	}
	return &SQLChargeSink{DB: db, Table: table}
}

func sqlQuoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func (s *SQLChargeSink) tableName() string {
	parts := strings.Split(strings.TrimSpace(s.Table), ".")
	for i, part := range parts {
		parts[i] = sqlQuoteIdentifier(part)
	}
	return strings.Join(parts, ".")
}

// EnsureSchema creates the append-only ledger and a time index. It is safe to
// call during every startup. Table is configuration, and each identifier is
// quoted to prevent accidental SQL syntax or keyword conflicts.
func (s *SQLChargeSink) EnsureSchema(ctx context.Context) error {
	if s == nil || s.DB == nil {
		return errors.New("sql charge sink: database is nil")
	}
	table := s.tableName()
	create := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
event_id TEXT PRIMARY KEY,
request_id TEXT NOT NULL DEFAULT '',
fingerprint TEXT NOT NULL DEFAULT '',
api_key_sha256 TEXT NOT NULL DEFAULT '',
provider TEXT NOT NULL DEFAULT '',
model TEXT NOT NULL DEFAULT '',
input_tokens BIGINT NOT NULL DEFAULT 0,
output_tokens BIGINT NOT NULL DEFAULT 0,
cache_read_tokens BIGINT NOT NULL DEFAULT 0,
cache_write_tokens BIGINT NOT NULL DEFAULT 0,
total_tokens BIGINT NOT NULL DEFAULT 0,
amount_micros BIGINT NOT NULL DEFAULT 0,
total_usd NUMERIC(30,12) NOT NULL DEFAULT 0,
price_revision TEXT NOT NULL DEFAULT '',
price_source TEXT NOT NULL DEFAULT '',
quote JSONB NOT NULL,
usage_record JSONB NOT NULL,
created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`, table)
	if _, err := s.DB.ExecContext(ctx, create); err != nil {
		return fmt.Errorf("sql charge sink: create ledger: %w", err)
	}
	// Derive a deterministic, quoted index name from the configured table.
	indexName := strings.ReplaceAll(strings.TrimSpace(s.Table), ".", "_") + "_created_idx"
	index := fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (created_at)", sqlQuoteIdentifier(indexName), table)
	if _, err := s.DB.ExecContext(ctx, index); err != nil {
		return fmt.Errorf("sql charge sink: create time index: %w", err)
	}
	return nil
}

// Apply inserts a charge exactly once. Replaying an event with the same
// fingerprint is successful; reusing an event ID for different usage returns
// ErrChargeConflict. OnInsert is called only for a newly inserted event.
func (s *SQLChargeSink) Apply(ctx context.Context, charge Charge) (err error) {
	if s == nil || s.DB == nil {
		return errors.New("sql charge sink: database is nil")
	}
	charge.EventID = strings.TrimSpace(charge.EventID)
	if charge.EventID == "" {
		return errors.New("billing event id is empty")
	}
	if charge.CreatedAt.IsZero() {
		charge.CreatedAt = time.Now().UTC()
	}
	quoteJSON, marshalErr := json.Marshal(charge.Quote)
	if marshalErr != nil {
		return fmt.Errorf("sql charge sink: marshal quote: %w", marshalErr)
	}
	// Keep credentials and potentially sensitive upstream headers out of the
	// durable ledger; the API key is represented by api_key_sha256 instead.
	record := charge.Record
	record.APIKey = ""
	record.ResponseHeaders = nil
	recordJSON, marshalErr := json.Marshal(record)
	if marshalErr != nil {
		return fmt.Errorf("sql charge sink: marshal usage record: %w", marshalErr)
	}
	tx, beginErr := s.DB.BeginTx(ctx, nil)
	if beginErr != nil {
		return fmt.Errorf("sql charge sink: begin transaction: %w", beginErr)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	table := s.tableName()
	insert := fmt.Sprintf(`INSERT INTO %s
(event_id,request_id,fingerprint,api_key_sha256,provider,model,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,total_tokens,amount_micros,total_usd,price_revision,price_source,quote,usage_record,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
ON CONFLICT (event_id) DO NOTHING RETURNING event_id`, table)
	totalTokens := charge.Record.Detail.TokenBreakdown.TotalTokens
	if totalTokens <= 0 {
		totalTokens = charge.Quote.InputTokens + charge.Quote.OutputTokens + charge.Quote.CacheReadTokens + charge.Quote.CacheWriteTokens
	}
	var insertedID string
	apiKeyHash := ""
	if charge.APIKey != "" {
		sum := sha256.Sum256([]byte(charge.APIKey))
		apiKeyHash = hex.EncodeToString(sum[:])
	}
	queryErr := tx.QueryRowContext(ctx, insert, charge.EventID, charge.RequestID, charge.Fingerprint, apiKeyHash,
		charge.Record.Provider, charge.Quote.Model, charge.Quote.InputTokens, charge.Quote.OutputTokens,
		charge.Quote.CacheReadTokens, charge.Quote.CacheWriteTokens, totalTokens, charge.Quote.TotalMicros,
		charge.Quote.TotalUSD, charge.Quote.PriceRevision, charge.Quote.PriceSource, quoteJSON, recordJSON, charge.CreatedAt).Scan(&insertedID)
	if queryErr != nil && !errors.Is(queryErr, sql.ErrNoRows) {
		err = queryErr
		return fmt.Errorf("sql charge sink: insert event: %w", err)
	}
	var existingFingerprint string
	if err = tx.QueryRowContext(ctx, fmt.Sprintf("SELECT fingerprint FROM %s WHERE event_id=$1 FOR UPDATE", table), charge.EventID).Scan(&existingFingerprint); err != nil {
		return fmt.Errorf("sql charge sink: read event: %w", err)
	}
	if existingFingerprint != charge.Fingerprint {
		return ErrChargeConflict
	}
	// If this is a replay, skip application side effects. PostgreSQL's result
	// count is not portable through every mock driver, so the fingerprint check
	// above is followed by a cheap row comparison before invoking the hook.
	if insertedID != "" && s.OnInsert != nil {
		if err = s.OnInsert(ctx, tx, charge); err != nil {
			return fmt.Errorf("sql charge sink: apply transaction hook: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sql charge sink: commit transaction: %w", err)
	}
	return nil
}

var _ ChargeSink = (*SQLChargeSink)(nil)
