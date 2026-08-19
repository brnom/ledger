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
func New(l Ledger, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{ledger: l, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
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

func newAccountResponse(a domain.Account) accountResponse {
	return accountResponse{
		Name:          a.Name,
		Currency:      a.Currency.Code,
		Scale:         a.Currency.Scale,
		Normal:        a.Normal.String(),
		AllowNegative: a.AllowNegative,
		Metadata:      a.Metadata,
		OpenedAt:      a.OpenedAt,
		OpenedSeq:     a.OpenedSeq,
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
	for i, a := range accts {
		out[i] = newAccountResponse(a)
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
	q := r.URL.Query()

	query := domain.BalanceQuery{Account: name}
	var err error
	if query.AsOfEffective, err = optionalTime(q, "as_of_effective"); err != nil {
		return err
	}
	if query.AsOfRecorded, err = optionalTime(q, "as_of_recorded"); err != nil {
		return err
	}
	if query.AsOfSeq, err = optionalInt64(q, "as_of_seq"); err != nil {
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

func newPostingResponse(p domain.Posting) postingResponse {
	direction := "credit"
	if p.IsDebit() {
		direction = "debit"
	}
	return postingResponse{
		Account:   p.Account,
		Amount:    p.Amount.Format(),
		Currency:  p.Amount.Currency().Code,
		Scale:     p.Amount.Currency().Scale,
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
	for i, p := range rec.Postings {
		resp.Postings[i] = newPostingResponse(p)
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

func newEntryResponse(e domain.Entry) entryResponse {
	direction := "credit"
	if e.Amount.Sign() > 0 {
		direction = "debit"
	}
	out := entryResponse{
		Seq:         e.Seq,
		Index:       e.Index,
		Account:     e.Account,
		Amount:      e.Amount.Format(),
		Currency:    e.Amount.Currency().Code,
		Scale:       e.Amount.Currency().Scale,
		Direction:   direction,
		TxID:        e.TxID.String(),
		Reference:   e.Reference,
		EffectiveAt: e.EffectiveAt,
		RecordedAt:  e.RecordedAt,
	}
	if !e.Reverts.IsZero() {
		out.Reverts = e.Reverts.String()
	}
	return out
}

func (s *Server) handleAccountEntries(w http.ResponseWriter, r *http.Request) error {
	q, err := parseEntryQuery(r)
	if err != nil {
		return err
	}
	q.Account = domain.AccountName(r.PathValue("name"))
	q.AccountPrefix = ""
	return s.serveEntries(w, r, q)
}

func (s *Server) handleEntries(w http.ResponseWriter, r *http.Request) error {
	q, err := parseEntryQuery(r)
	if err != nil {
		return err
	}
	return s.serveEntries(w, r, q)
}

func (s *Server) serveEntries(w http.ResponseWriter, r *http.Request, q domain.EntryQuery) error {
	entries, err := s.ledger.Entries(r.Context(), q)
	if err != nil {
		return err
	}
	out := make([]entryResponse, len(entries))
	for i, e := range entries {
		out[i] = newEntryResponse(e)
	}

	body := map[string]any{"entries": out}
	// A cursor is present exactly when a full page came back, which is the
	// only case where more entries might exist.
	if q.Limit > 0 && len(entries) == q.Limit {
		last := entries[len(entries)-1]
		body["next"] = map[string]any{"after_seq": last.Seq, "after_index": last.Index}
	}
	s.writeJSON(w, r, http.StatusOK, body)
	return nil
}

func parseEntryQuery(r *http.Request) (domain.EntryQuery, error) {
	v := r.URL.Query()
	var (
		q   domain.EntryQuery
		err error
	)
	q.AccountPrefix = domain.AccountName(v.Get("prefix"))
	q.Account = domain.AccountName(v.Get("account"))
	if id := v.Get("transaction_id"); id != "" {
		if q.TxID, err = domain.ParseID(id); err != nil {
			return q, err
		}
	}
	if q.EffectiveFrom, err = optionalTime(v, "effective_from"); err != nil {
		return q, err
	}
	if q.EffectiveTo, err = optionalTime(v, "effective_to"); err != nil {
		return q, err
	}
	if q.RecordedFrom, err = optionalTime(v, "recorded_from"); err != nil {
		return q, err
	}
	if q.RecordedTo, err = optionalTime(v, "recorded_to"); err != nil {
		return q, err
	}
	if q.FromSeq, err = optionalInt64(v, "from_seq"); err != nil {
		return q, err
	}
	if q.ToSeq, err = optionalInt64(v, "to_seq"); err != nil {
		return q, err
	}
	if q.AfterSeq, err = optionalInt64(v, "after_seq"); err != nil {
		return q, err
	}
	after, err := optionalInt64(v, "after_index")
	if err != nil {
		return q, err
	}
	q.AfterIndex = int(after)
	limit, err := optionalInt64(v, "limit")
	if err != nil {
		return q, err
	}
	q.Limit = int(limit)
	return q, nil
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
	v := r.URL.Query()
	fromSeq, err := optionalInt64(v, "from_seq")
	if err != nil {
		return err
	}
	limit, err := optionalInt64(v, "limit")
	if err != nil {
		return err
	}

	events, err := s.ledger.Events(r.Context(), fromSeq, int(limit))
	if err != nil {
		return err
	}
	out := make([]eventResponse, len(events))
	for i, e := range events {
		out[i] = eventResponse{
			Seq:            e.Seq,
			ID:             e.ID.String(),
			Type:           string(e.Type),
			Payload:        json.RawMessage(e.Payload),
			RecordedAt:     e.RecordedAt,
			IdempotencyKey: e.IdempotencyKey,
			PrevHash:       fmt.Sprintf("%x", e.PrevHash),
			Hash:           fmt.Sprintf("%x", e.Hash),
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
	var v T
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, errBadRequest(fmt.Errorf("parsing body: %w", err))
	}
	return v, nil
}

// decodeOptional accepts an empty body, for endpoints where every field has a
// sensible default.
func decodeOptional[T any](r *http.Request) (T, error) {
	var v T
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil && !errors.Is(err, io.EOF) {
		return v, errBadRequest(fmt.Errorf("parsing body: %w", err))
	}
	return v, nil
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
	for i, p := range reqs {
		cur, err := resolveCurrency(p.Currency, p.Scale)
		if err != nil {
			return nil, fmt.Errorf("posting %d: %w", i, err)
		}
		amount, err := domain.ParseAmount(cur, p.Amount)
		if err != nil {
			return nil, fmt.Errorf("posting %d: %w", i, err)
		}
		out[i] = domain.Posting{Account: p.Account, Amount: amount}
	}
	return out, nil
}

type values interface{ Get(string) string }

func optionalTime(v values, key string) (time.Time, error) {
	raw := v.Get(key)
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errBadRequest(fmt.Errorf("%s must be an RFC 3339 timestamp: %w", key, err))
	}
	return t, nil
}

func optionalInt64(v values, key string) (int64, error) {
	raw := v.Get(key)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errBadRequest(fmt.Errorf("%s must be an integer: %w", key, err))
	}
	return n, nil
}
