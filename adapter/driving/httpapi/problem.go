package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/brnom/ledger/domain"
)

// Problem is an RFC 9457 "problem details" response. Using the standard shape
// means a client can handle an error generically -- status plus a stable type
// URI -- without parsing prose.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// problemBaseURI namespaces this service's error types.
const problemBaseURI = "https://github.com/brnom/ledger/problems/"

// errorMap turns the ledger's sentinel errors into HTTP responses. It is the
// single place the domain meets the protocol: handlers return errors and let
// this decide what they mean on the wire.
//
// Order matters only in that each error is listed once; errors.Is walks the
// wrapping chain, so a wrapped sentinel is found wherever it sits.
var errorMap = []struct {
	err    error
	status int
	slug   string
	title  string
}{
	{domain.ErrAccountNotFound, http.StatusNotFound, "account-not-found", "Account not found"},
	{domain.ErrTransactionNotFound, http.StatusNotFound, "transaction-not-found", "Transaction not found"},

	{domain.ErrAccountExists, http.StatusConflict, "account-exists", "Account already exists"},
	{domain.ErrTransactionExists, http.StatusConflict, "transaction-exists", "Transaction already exists"},
	{domain.ErrAlreadyReverted, http.StatusConflict, "already-reverted", "Transaction already reverted"},
	{domain.ErrIdempotencyConflict, http.StatusConflict, "idempotency-conflict", "Idempotency key reused with a different request"},

	// Unprocessable rather than Bad Request: the request is well formed, but
	// the ledger's state or rules do not admit it.
	{domain.ErrInsufficientFunds, http.StatusUnprocessableEntity, "insufficient-funds", "Insufficient funds"},
	{domain.ErrEffectiveOutOfRange, http.StatusUnprocessableEntity, "effective-out-of-range", "Effective time out of range"},
	{domain.ErrOverflow, http.StatusUnprocessableEntity, "amount-overflow", "Amount out of range"},

	{domain.ErrInvalidTransaction, http.StatusBadRequest, "invalid-transaction", "Invalid transaction"},
	{domain.ErrInvalidAccount, http.StatusBadRequest, "invalid-account", "Invalid account"},
	{domain.ErrCurrencyMismatch, http.StatusBadRequest, "currency-mismatch", "Currency mismatch"},
	{domain.ErrInvalidAmount, http.StatusBadRequest, "invalid-amount", "Invalid amount"},
	{domain.ErrInvalidCurrency, http.StatusBadRequest, "invalid-currency", "Invalid currency"},
	{domain.ErrInvalidID, http.StatusBadRequest, "invalid-id", "Invalid identifier"},

	// Retryable: another writer won the race and the caller may try again.
	{domain.ErrConflict, http.StatusServiceUnavailable, "write-conflict", "Concurrent write conflict"},

	// Not the caller's fault and not recoverable by retrying.
	{domain.ErrChainBroken, http.StatusInternalServerError, "chain-broken", "Event chain verification failed"},
	{domain.ErrEncoding, http.StatusInternalServerError, "encoding", "Encoding failure"},
	{domain.ErrUnknownEvent, http.StatusInternalServerError, "unknown-event", "Unknown event"},
}

// badRequest marks an error as a malformed request, for failures that arise in
// the HTTP layer itself rather than in the ledger.
type badRequest struct{ err error }

func (b badRequest) Error() string { return b.err.Error() }
func (b badRequest) Unwrap() error { return b.err }

func errBadRequest(err error) error { return badRequest{err} }

func problemFor(err error) Problem {
	var br badRequest
	if errors.As(err, &br) {
		return Problem{
			Type:   problemBaseURI + "bad-request",
			Title:  "Malformed request",
			Status: http.StatusBadRequest,
			Detail: err.Error(),
		}
	}
	for _, m := range errorMap {
		if errors.Is(err, m.err) {
			p := Problem{
				Type:   problemBaseURI + m.slug,
				Title:  m.title,
				Status: m.status,
			}
			// Detail describes what the caller did wrong, so it belongs on a
			// 4xx. At 5xx the cause is ours and the wrapped message can carry
			// internals -- a sequence number, a constraint name, a driver
			// error. The client still gets Type, which is all it needs to
			// decide whether to retry; the cause goes to the log instead.
			if m.status < 500 {
				p.Detail = err.Error()
			}
			return p
		}
	}
	return Problem{
		Type:   problemBaseURI + "internal",
		Title:  "Internal error",
		Status: http.StatusInternalServerError,
	}
}

func (s *Server) writeProblem(w http.ResponseWriter, r *http.Request, err error) {
	p := problemFor(err)
	p.Instance = r.URL.Path

	if p.Status >= 500 {
		// Server-side failures carry no detail to the client -- it could leak
		// internals -- so the log is the only place the cause survives.
		s.log.ErrorContext(r.Context(), "request failed",
			slog.String("path", r.URL.Path),
			slog.String("method", r.Method),
			slog.Int("status", p.Status),
			slog.Any("error", err))
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		s.log.ErrorContext(r.Context(), "writing problem response", slog.Any("error", err))
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.ErrorContext(r.Context(), "writing response", slog.Any("error", err))
	}
}
