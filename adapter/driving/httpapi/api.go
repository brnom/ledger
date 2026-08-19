// Package httpapi serves a ledger over HTTP.
//
// It is deliberately thin: it parses, delegates to [app.Ledger], and maps
// the domain's errors onto status codes. Every rule that matters lives in the
// ledger, so the same guarantees hold whether a caller comes through this API
// or imports the package directly.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/brnom/ledger/app"
	"github.com/brnom/ledger/domain"
)

// MaxBodyBytes caps a request body. A ledger takes small, structured commands;
// anything larger is a mistake or an attack.
const MaxBodyBytes = 1 << 20

// IdempotencyHeader is the header a caller uses to make a write safe to retry.
const IdempotencyHeader = "Idempotency-Key"

// Ledger is the driving port: everything the HTTP transport needs from the
// application, and nothing more.
//
// It is declared here, beside the code that consumes it, rather than exported
// from the application package. That is the Go idiom and it is also what makes
// the dependency point inward: the transport states its requirements, and the
// application happens to satisfy them without knowing this package exists.
//
// The practical payoff is that the transport can be exercised against a stub —
// including failure paths a real ledger will not produce on demand — without a
// store, a database, or a single event.
type Ledger interface {
	ID() string

	OpenAccount(context.Context, app.OpenAccountCommand) (domain.Account, app.Result, error)
	Commit(context.Context, app.CommitCommand) (app.Result, error)
	Revert(context.Context, app.RevertCommand) (app.Result, error)

	Balance(context.Context, domain.BalanceQuery) (domain.Amount, error)
	Entries(context.Context, domain.EntryQuery) ([]domain.Entry, error)
	Account(context.Context, domain.AccountName) (domain.Account, error)
	Accounts(context.Context, domain.AccountName) ([]domain.Account, error)
	Transaction(context.Context, domain.ID) (domain.RecordedTransaction, error)

	Head(context.Context) (domain.Head, error)
	Events(context.Context, int64, int) ([]domain.Event, error)
	Verify(context.Context) (domain.Head, error)
}

// The concrete ledger satisfies the port. This assertion turns a signature
// drift into a build failure at the boundary rather than a puzzle at the call
// site.
var _ Ledger = (*app.Ledger)(nil)

// Server serves one ledger.
type Server struct {
	ledger Ledger
	log    *slog.Logger
	mux    *http.ServeMux
}

// New returns a server for the given ledger. A nil logger discards output.
func New(ledger Ledger, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	srv := &Server{ledger: ledger, log: log, mux: http.NewServeMux()}
	srv.routes()
	return srv
}

// ServeHTTP implements [http.Handler].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	handle := func(pattern string, fn func(http.ResponseWriter, *http.Request) error) {
		s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
			if err := fn(w, r); err != nil {
				s.writeProblem(w, r, err)
			}
		})
	}

	handle("GET /healthz", s.handleHealth)

	handle("POST /v1/accounts", s.handleOpenAccount)
	handle("GET /v1/accounts", s.handleListAccounts)
	handle("GET /v1/accounts/{name}", s.handleGetAccount)
	handle("GET /v1/accounts/{name}/balance", s.handleBalance)
	handle("GET /v1/accounts/{name}/entries", s.handleAccountEntries)

	handle("POST /v1/transactions", s.handleCommit)
	handle("GET /v1/transactions/{id}", s.handleGetTransaction)
	handle("POST /v1/transactions/{id}/revert", s.handleRevert)

	handle("GET /v1/entries", s.handleEntries)
	handle("GET /v1/events", s.handleEvents)
	handle("GET /v1/verify", s.handleVerify)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) error {
	head, err := s.ledger.Head(r.Context())
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"status": "ok",
		"ledger": s.ledger.ID(),
		"seq":    head.Seq,
	})
	return nil
}

// --- accounts ---

type openAccountRequest struct {
	Name          domain.AccountName `json:"name"`
	Currency      string             `json:"currency"`
	Scale         *uint8             `json:"scale,omitempty"`
	Normal        string             `json:"normal"`
	AllowNegative bool               `json:"allow_negative"`
	EffectiveAt   *time.Time         `json:"effective_at,omitempty"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
}

type accountResponse struct {
	Name          domain.AccountName `json:"name"`
	Currency      string             `json:"currency"`
	Scale         uint8              `json:"scale"`
	Normal        string             `json:"normal"`
	AllowNegative bool               `json:"allow_negative"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
	OpenedAt      time.Time          `json:"opened_at"`
	OpenedSeq     int64              `json:"opened_seq"`
}

func newAccountResponse(acct domain.Account) accountResponse {
	return accountResponse{
		Name:          acct.Name,
		Currency:      acct.Currency.Code,
		Scale:         acct.Currency.Scale,
		Normal:        acct.Normal.String(),
		AllowNegative: acct.AllowNegative,
		Metadata:      acct.Metadata,
		OpenedAt:      acct.OpenedAt,
		OpenedSeq:     acct.OpenedSeq,
	}
}

func (s *Server) handleOpenAccount(w http.ResponseWriter, r *http.Request) error {
	req, err := decode[openAccountRequest](r)
	if err != nil {
		return err
	}
	cur, err := resolveCurrency(req.Currency, req.Scale)
	if err != nil {
		return err
	}
	normal, err := domain.ParseNormal(req.Normal)
	if err != nil {
		return err
	}

	cmd := app.OpenAccountCommand{
		Name:           req.Name,
		Currency:       cur,
		Normal:         normal,
		AllowNegative:  req.AllowNegative,
		Metadata:       req.Metadata,
		IdempotencyKey: r.Header.Get(IdempotencyHeader),
	}
	if req.EffectiveAt != nil {
		cmd.EffectiveAt = *req.EffectiveAt
	}

	acct, res, err := s.ledger.OpenAccount(r.Context(), cmd)
	if err != nil {
		return err
	}
	status := http.StatusCreated
	if res.Replayed {
		// A replay created nothing, so 200 rather than 201, and the header
		// tells the caller why the body looks familiar.
		status = http.StatusOK
		w.Header().Set("Idempotent-Replay", "true")
	}
	s.writeJSON(w, r, status, map[string]any{
		"account": newAccountResponse(acct),
		"result":  newResultResponse(res),
	})
	return nil
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) error {
	acct, err := s.ledger.Account(r.Context(), domain.AccountName(r.PathValue("name")))
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, newAccountResponse(acct))
	return nil
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) error {
	accts, err := s.ledger.Accounts(r.Context(), domain.AccountName(r.URL.Query().Get("prefix")))
	if err != nil {
		return err
	}
	out := make([]accountResponse, len(accts))
	for i, acct := range accts {
		out[i] = newAccountResponse(acct)
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"accounts": out})
	return nil
}

// --- balances ---

type balanceResponse struct {
	Account domain.AccountName `json:"account"`

	// Balance is the account's own reading: positive when it holds what it is
	// meant to hold, whichever side of the book it sits on.
	Balance  string `json:"balance"`
	Currency string `json:"currency"`
	Scale    uint8  `json:"scale"`

	// Signed is the internal, debit-positive value. It is what sums to zero
	// across the book, and what a caller reconciling the whole ledger wants.
	Signed string `json:"signed"`

	// The bounds the balance was computed under, echoed back so a client can
	// tell a cached answer from a fresh one.
	AsOfEffective *time.Time `json:"as_of_effective,omitempty"`
	AsOfRecorded  *time.Time `json:"as_of_recorded,omitempty"`
	AsOfSeq       int64      `json:"as_of_seq,omitempty"`
}

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) error {
	name := domain.AccountName(r.PathValue("name"))
	params := r.URL.Query()

	query := domain.BalanceQuery{Account: name}
	var err error
	if query.AsOfEffective, err = optionalTime(params, "as_of_effective"); err != nil {
		return err
	}
	if query.AsOfRecorded, err = optionalTime(params, "as_of_recorded"); err != nil {
		return err
	}
	if query.AsOfSeq, err = optionalInt64(params, "as_of_seq"); err != nil {
		return err
	}

	acct, err := s.ledger.Account(r.Context(), name)
	if err != nil {
		return err
	}
	signed, err := s.ledger.Balance(r.Context(), query)
	if err != nil {
		return err
	}
	presented, err := acct.Presented(signed)
	if err != nil {
		return err
	}

	resp := balanceResponse{
		Account:  name,
		Balance:  presented.Format(),
		Currency: acct.Currency.Code,
		Scale:    acct.Currency.Scale,
		Signed:   signed.Format(),
		AsOfSeq:  query.AsOfSeq,
	}
	if !query.AsOfEffective.IsZero() {
		resp.AsOfEffective = &query.AsOfEffective
	}
	if !query.AsOfRecorded.IsZero() {
		resp.AsOfRecorded = &query.AsOfRecorded
	}
	s.writeJSON(w, r, http.StatusOK, resp)
	return nil
}

// --- transactions ---

type commitRequest struct {
	ID          string            `json:"id,omitempty"`
	EffectiveAt *time.Time        `json:"effective_at,omitempty"`
	Postings    []postingRequest  `json:"postings"`
	Reference   string            `json:"reference,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type postingRequest struct {
	Account  domain.AccountName `json:"account"`
	Amount   string             `json:"amount"`
	Currency string             `json:"currency"`
	Scale    *uint8             `json:"scale,omitempty"`
}

type resultResponse struct {
	Seq           int64     `json:"seq"`
	EventID       string    `json:"event_id,omitempty"`
	TransactionID string    `json:"transaction_id,omitempty"`
	RecordedAt    time.Time `json:"recorded_at"`
	Replayed      bool      `json:"replayed,omitempty"`
}

func newResultResponse(res app.Result) resultResponse {
	out := resultResponse{
		Seq:        res.Seq,
		RecordedAt: res.RecordedAt,
		Replayed:   res.Replayed,
	}
	if !res.EventID.IsZero() {
		out.EventID = res.EventID.String()
	}
	if !res.TransactionID.IsZero() {
		out.TransactionID = res.TransactionID.String()
	}
	return out
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) error {
	req, err := decode[commitRequest](r)
	if err != nil {
		return err
	}
	postings, err := parsePostings(req.Postings)
	if err != nil {
		return err
	}

	cmd := app.CommitCommand{
		Postings:       postings,
		Reference:      req.Reference,
		Metadata:       req.Metadata,
		IdempotencyKey: r.Header.Get(IdempotencyHeader),
	}
	if req.ID != "" {
		if cmd.ID, err = domain.ParseID(req.ID); err != nil {
			return err
		}
	}
	if req.EffectiveAt != nil {
		cmd.EffectiveAt = *req.EffectiveAt
	}

	res, err := s.ledger.Commit(r.Context(), cmd)
	if err != nil {
		return err
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replay", "true")
	}
	s.writeJSON(w, r, status, newResultResponse(res))
	return nil
}

type revertRequest struct {
	EffectiveAt *time.Time `json:"effective_at,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}

func (s *Server) handleRevert(w http.ResponseWriter, r *http.Request) error {
	id, err := domain.ParseID(r.PathValue("id"))
	if err != nil {
		return err
	}
	req, err := decodeOptional[revertRequest](r)
	if err != nil {
		return err
	}

	cmd := app.RevertCommand{
		TransactionID:  id,
		Reason:         req.Reason,
		IdempotencyKey: r.Header.Get(IdempotencyHeader),
	}
	if req.EffectiveAt != nil {
		cmd.EffectiveAt = *req.EffectiveAt
	}

	res, err := s.ledger.Revert(r.Context(), cmd)
	if err != nil {
		return err
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replay", "true")
	}
	s.writeJSON(w, r, status, newResultResponse(res))
	return nil
}

type transactionResponse struct {
	ID          string            `json:"id"`
	Seq         int64             `json:"seq"`
	EffectiveAt time.Time         `json:"effective_at"`
	RecordedAt  time.Time         `json:"recorded_at"`
	Reference   string            `json:"reference,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Postings    []postingResponse `json:"postings"`
	Reverts     string            `json:"reverts,omitempty"`
	RevertedBy  string            `json:"reverted_by,omitempty"`
}

type postingResponse struct {
	Account   domain.AccountName `json:"account"`
	Amount    string             `json:"amount"`
	Currency  string             `json:"currency"`
	Scale     uint8              `json:"scale"`
	Direction string             `json:"direction"`
}

func newPostingResponse(posting domain.Posting) postingResponse {
	direction := "credit"
	if posting.IsDebit() {
		direction = "debit"
	}
	return postingResponse{
		Account:   posting.Account,
		Amount:    posting.Amount.Format(),
		Currency:  posting.Amount.Currency().Code,
		Scale:     posting.Amount.Currency().Scale,
		Direction: direction,
	}
}

func (s *Server) handleGetTransaction(w http.ResponseWriter, r *http.Request) error {
	id, err := domain.ParseID(r.PathValue("id"))
	if err != nil {
		return err
	}
	rec, err := s.ledger.Transaction(r.Context(), id)
	if err != nil {
		return err
	}

	resp := transactionResponse{
		ID:          rec.ID.String(),
		Seq:         rec.Seq,
		EffectiveAt: rec.EffectiveAt,
		RecordedAt:  rec.RecordedAt,
		Reference:   rec.Reference,
		Metadata:    rec.Metadata,
		Postings:    make([]postingResponse, len(rec.Postings)),
	}
	for i, posting := range rec.Postings {
		resp.Postings[i] = newPostingResponse(posting)
	}
	if !rec.Reverts.IsZero() {
		resp.Reverts = rec.Reverts.String()
	}
	if !rec.RevertedBy.IsZero() {
		resp.RevertedBy = rec.RevertedBy.String()
	}
	s.writeJSON(w, r, http.StatusOK, resp)
	return nil
}

// --- entries ---

type entryResponse struct {
	Seq         int64              `json:"seq"`
	Index       int                `json:"index"`
	Account     domain.AccountName `json:"account"`
	Amount      string             `json:"amount"`
	Currency    string             `json:"currency"`
	Scale       uint8              `json:"scale"`
	Direction   string             `json:"direction"`
	TxID        string             `json:"transaction_id"`
	Reference   string             `json:"reference,omitempty"`
	EffectiveAt time.Time          `json:"effective_at"`
	RecordedAt  time.Time          `json:"recorded_at"`
	Reverts     string             `json:"reverts,omitempty"`
}

func newEntryResponse(entry domain.Entry) entryResponse {
	direction := "credit"
	if entry.Amount.Sign() > 0 {
		direction = "debit"
	}
	out := entryResponse{
		Seq:         entry.Seq,
		Index:       entry.Index,
		Account:     entry.Account,
		Amount:      entry.Amount.Format(),
		Currency:    entry.Amount.Currency().Code,
		Scale:       entry.Amount.Currency().Scale,
		Direction:   direction,
		TxID:        entry.TxID.String(),
		Reference:   entry.Reference,
		EffectiveAt: entry.EffectiveAt,
		RecordedAt:  entry.RecordedAt,
	}
	if !entry.Reverts.IsZero() {
		out.Reverts = entry.Reverts.String()
	}
	return out
}

func (s *Server) handleAccountEntries(w http.ResponseWriter, r *http.Request) error {
	query, err := parseEntryQuery(r)
	if err != nil {
		return err
	}
	query.Account = domain.AccountName(r.PathValue("name"))
	query.AccountPrefix = ""
	return s.serveEntries(w, r, query)
}

func (s *Server) handleEntries(w http.ResponseWriter, r *http.Request) error {
	query, err := parseEntryQuery(r)
	if err != nil {
		return err
	}
	return s.serveEntries(w, r, query)
}

func (s *Server) serveEntries(w http.ResponseWriter, r *http.Request, query domain.EntryQuery) error {
	entries, err := s.ledger.Entries(r.Context(), query)
	if err != nil {
		return err
	}
	out := make([]entryResponse, len(entries))
	for i, entry := range entries {
		out[i] = newEntryResponse(entry)
	}

	body := map[string]any{"entries": out}
	// A cursor is present exactly when a full page came back, which is the
	// only case where more entries might exist.
	if query.Limit > 0 && len(entries) == query.Limit {
		last := entries[len(entries)-1]
		body["next"] = map[string]any{"after_seq": last.Seq, "after_index": last.Index}
	}
	s.writeJSON(w, r, http.StatusOK, body)
	return nil
}

func parseEntryQuery(r *http.Request) (domain.EntryQuery, error) {
	params := r.URL.Query()
	var (
		query domain.EntryQuery
		err   error
	)
	query.AccountPrefix = domain.AccountName(params.Get("prefix"))
	query.Account = domain.AccountName(params.Get("account"))
	if id := params.Get("transaction_id"); id != "" {
		if query.TxID, err = domain.ParseID(id); err != nil {
			return query, err
		}
	}
	if query.EffectiveFrom, err = optionalTime(params, "effective_from"); err != nil {
		return query, err
	}
	if query.EffectiveTo, err = optionalTime(params, "effective_to"); err != nil {
		return query, err
	}
	if query.RecordedFrom, err = optionalTime(params, "recorded_from"); err != nil {
		return query, err
	}
	if query.RecordedTo, err = optionalTime(params, "recorded_to"); err != nil {
		return query, err
	}
	if query.FromSeq, err = optionalInt64(params, "from_seq"); err != nil {
		return query, err
	}
	if query.ToSeq, err = optionalInt64(params, "to_seq"); err != nil {
		return query, err
	}
	if query.AfterSeq, err = optionalInt64(params, "after_seq"); err != nil {
		return query, err
	}
	after, err := optionalInt64(params, "after_index")
	if err != nil {
		return query, err
	}
	query.AfterIndex = int(after)
	limit, err := optionalInt64(params, "limit")
	if err != nil {
		return query, err
	}
	query.Limit = int(limit)
	return query, nil
}

// --- events and verification ---

type eventResponse struct {
	Seq            int64           `json:"seq"`
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	RecordedAt     time.Time       `json:"recorded_at"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	PrevHash       string          `json:"prev_hash"`
	Hash           string          `json:"hash"`
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) error {
	params := r.URL.Query()
	fromSeq, err := optionalInt64(params, "from_seq")
	if err != nil {
		return err
	}
	limit, err := optionalInt64(params, "limit")
	if err != nil {
		return err
	}

	events, err := s.ledger.Events(r.Context(), fromSeq, int(limit))
	if err != nil {
		return err
	}
	out := make([]eventResponse, len(events))
	for i, event := range events {
		out[i] = eventResponse{
			Seq:            event.Seq,
			ID:             event.ID.String(),
			Type:           string(event.Type),
			Payload:        json.RawMessage(event.Payload),
			RecordedAt:     event.RecordedAt,
			IdempotencyKey: event.IdempotencyKey,
			PrevHash:       fmt.Sprintf("%x", event.PrevHash),
			Hash:           fmt.Sprintf("%x", event.Hash),
		}
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"events": out})
	return nil
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) error {
	head, err := s.ledger.Verify(r.Context())
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"verified": true,
		"seq":      head.Seq,
		"hash":     fmt.Sprintf("%x", head.Hash),
	})
	return nil
}

// --- decoding helpers ---

func decode[T any](r *http.Request) (T, error) {
	var decoded T
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
		return decoded, errBadRequest(fmt.Errorf("parsing body: %w", err))
	}
	return decoded, nil
}

// decodeOptional accepts an empty body, for endpoints where every field has a
// sensible default.
func decodeOptional[T any](r *http.Request) (T, error) {
	var decoded T
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil && !errors.Is(err, io.EOF) {
		return decoded, errBadRequest(fmt.Errorf("parsing body: %w", err))
	}
	return decoded, nil
}

// resolveCurrency turns a code and an optional scale into a currency. Omitting
// the scale is allowed only for codes the ledger already knows, so "BRL" needs
// no scale but an unfamiliar code must say how many decimal places it has
// rather than defaulting to a number that would silently misprice everything.
func resolveCurrency(code string, scale *uint8) (domain.Currency, error) {
	if scale != nil {
		return domain.NewCurrency(code, *scale)
	}
	cur, ok := domain.CurrencyByCode(code)
	if !ok {
		return domain.Currency{}, fmt.Errorf("%w: %q is not a known currency, so scale is required",
			domain.ErrInvalidCurrency, code)
	}
	return cur, nil
}

func parsePostings(reqs []postingRequest) ([]domain.Posting, error) {
	out := make([]domain.Posting, len(reqs))
	for i, posting := range reqs {
		cur, err := resolveCurrency(posting.Currency, posting.Scale)
		if err != nil {
			return nil, fmt.Errorf("posting %d: %w", i, err)
		}
		amount, err := domain.ParseAmount(cur, posting.Amount)
		if err != nil {
			return nil, fmt.Errorf("posting %d: %w", i, err)
		}
		out[i] = domain.Posting{Account: posting.Account, Amount: amount}
	}
	return out, nil
}

type values interface{ Get(string) string }

func optionalTime(params values, key string) (time.Time, error) {
	raw := params.Get(key)
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errBadRequest(fmt.Errorf("%s must be an RFC 3339 timestamp: %w", key, err))
	}
	return parsed, nil
}

func optionalInt64(params values, key string) (int64, error) {
	raw := params.Get(key)
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errBadRequest(fmt.Errorf("%s must be an integer: %w", key, err))
	}
	return parsed, nil
}
