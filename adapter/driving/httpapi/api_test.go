package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brnom/ledger/adapter/driven/memstore"
	"github.com/brnom/ledger/adapter/driving/httpapi"
	"github.com/brnom/ledger/app"
	"github.com/brnom/ledger/domain"
)

var base = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

type api struct {
	t   *testing.T
	srv *httptest.Server
	now *time.Time
}

func newAPI(t *testing.T) *api {
	t.Helper()
	now := base
	store := memstore.New()
	l, err := app.Open(store, "main", app.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	srv := httptest.NewServer(httpapi.New(l, nil))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { store.Close() })
	return &api{t: t, srv: srv, now: &now}
}

// do sends a request and decodes the response into out, returning the status.
func (a *api) do(method, path, body string, headers map[string]string, out any) int {
	a.t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, a.srv.URL+path, reader)
	if err != nil {
		a.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := a.srv.Client().Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	if out != nil && buf.Len() > 0 {
		if err := json.Unmarshal(buf.Bytes(), out); err != nil {
			a.t.Fatalf("%s %s: decoding %q: %v", method, path, buf.String(), err)
		}
	}
	return resp.StatusCode
}

func (a *api) openAccount(name string, normal string, allowNegative bool) {
	a.t.Helper()
	body := fmt.Sprintf(`{"name":%q,"currency":"BRL","normal":%q,"allow_negative":%t}`,
		name, normal, allowNegative)
	if code := a.do("POST", "/v1/accounts", body, nil, nil); code != http.StatusCreated {
		a.t.Fatalf("opening %q returned %d", name, code)
	}
}

func TestHealth(t *testing.T) {
	a := newAPI(t)
	var out struct {
		Status string `json:"status"`
		Ledger string `json:"ledger"`
	}
	if code := a.do("GET", "/healthz", "", nil, &out); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if out.Status != "ok" || out.Ledger != "main" {
		t.Errorf("body = %+v", out)
	}
}

func TestOpenAccountAndRead(t *testing.T) {
	a := newAPI(t)
	var created struct {
		Account struct {
			Name     string `json:"name"`
			Currency string `json:"currency"`
			Scale    uint8  `json:"scale"`
			Normal   string `json:"normal"`
		} `json:"account"`
	}
	code := a.do("POST", "/v1/accounts",
		`{"name":"liabilities:users:1","currency":"BRL","normal":"credit"}`, nil, &created)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	// The scale came from the currency table: a caller sending "BRL" should
	// not have to know it means two decimal places.
	if created.Account.Scale != 2 || created.Account.Currency != "BRL" {
		t.Errorf("account = %+v", created.Account)
	}

	var fetched map[string]any
	if code := a.do("GET", "/v1/accounts/liabilities:users:1", "", nil, &fetched); code != http.StatusOK {
		t.Fatalf("GET status = %d", code)
	}
	if fetched["name"] != "liabilities:users:1" {
		t.Errorf("fetched = %+v", fetched)
	}
}

func TestOpenAccountUnknownCurrencyNeedsScale(t *testing.T) {
	a := newAPI(t)
	var problem httpapi.Problem
	code := a.do("POST", "/v1/accounts",
		`{"name":"assets:points","currency":"PTS","normal":"debit"}`, nil, &problem)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(problem.Detail, "scale is required") {
		t.Errorf("detail = %q", problem.Detail)
	}

	code = a.do("POST", "/v1/accounts",
		`{"name":"assets:points","currency":"PTS","scale":0,"normal":"debit"}`, nil, nil)
	if code != http.StatusCreated {
		t.Errorf("with an explicit scale, status = %d, want 201", code)
	}
}

func TestCommitAndBalance(t *testing.T) {
	a := newAPI(t)
	a.openAccount("equity:opening-balances", "credit", true)
	a.openAccount("liabilities:users:1", "credit", false)

	var res struct {
		Seq           int64  `json:"seq"`
		TransactionID string `json:"transaction_id"`
	}
	code := a.do("POST", "/v1/transactions", `{
		"reference": "e2e-1",
		"postings": [
			{"account":"equity:opening-balances","amount":"100.00","currency":"BRL"},
			{"account":"liabilities:users:1","amount":"-100.00","currency":"BRL"}
		]}`, nil, &res)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	if res.TransactionID == "" {
		t.Fatal("no transaction id in the response")
	}

	var balance struct {
		Balance  string `json:"balance"`
		Signed   string `json:"signed"`
		Currency string `json:"currency"`
	}
	if code := a.do("GET", "/v1/accounts/liabilities:users:1/balance", "", nil, &balance); code != http.StatusOK {
		t.Fatalf("balance status = %d", code)
	}
	// The holder sees what they hold; the signed value is what sums to zero
	// across the book.
	if balance.Balance != "100.00" || balance.Signed != "-100.00" || balance.Currency != "BRL" {
		t.Errorf("balance = %+v", balance)
	}

	var tx struct {
		ID       string `json:"id"`
		Postings []struct {
			Account   string `json:"account"`
			Amount    string `json:"amount"`
			Direction string `json:"direction"`
		} `json:"postings"`
	}
	if code := a.do("GET", "/v1/transactions/"+res.TransactionID, "", nil, &tx); code != http.StatusOK {
		t.Fatalf("transaction status = %d", code)
	}
	if len(tx.Postings) != 2 || tx.Postings[0].Direction != "debit" || tx.Postings[1].Direction != "credit" {
		t.Errorf("postings = %+v", tx.Postings)
	}
}

func TestIdempotencyOverHTTP(t *testing.T) {
	a := newAPI(t)
	a.openAccount("equity:opening-balances", "credit", true)
	a.openAccount("liabilities:users:1", "credit", false)

	body := `{"postings":[
		{"account":"equity:opening-balances","amount":"25.00","currency":"BRL"},
		{"account":"liabilities:users:1","amount":"-25.00","currency":"BRL"}
	]}`
	headers := map[string]string{httpapi.IdempotencyHeader: "pay-1"}

	var first, second struct {
		Seq           int64  `json:"seq"`
		TransactionID string `json:"transaction_id"`
		Replayed      bool   `json:"replayed"`
	}
	if code := a.do("POST", "/v1/transactions", body, headers, &first); code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", code)
	}
	// A retry created nothing, so it is 200, not 201.
	if code := a.do("POST", "/v1/transactions", body, headers, &second); code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", code)
	}
	if !second.Replayed || second.TransactionID != first.TransactionID {
		t.Errorf("retry = %+v, want a replay of %+v", second, first)
	}

	// The same key with a different body is a conflict, not a retry.
	conflicting := strings.Replace(body, "25.00", "30.00", 2)
	var problem httpapi.Problem
	if code := a.do("POST", "/v1/transactions", conflicting, headers, &problem); code != http.StatusConflict {
		t.Fatalf("conflicting status = %d, want 409", code)
	}
	if !strings.HasSuffix(problem.Type, "idempotency-conflict") {
		t.Errorf("problem = %+v", problem)
	}
}

// TestBitemporalOverHTTP exercises the query the whole design exists for.
func TestBitemporalOverHTTP(t *testing.T) {
	a := newAPI(t)
	a.openAccount("equity:opening-balances", "credit", true)
	a.openAccount("liabilities:users:1", "credit", false)

	day1 := base
	post := func(effectiveAt time.Time, amount string) {
		t.Helper()
		body := fmt.Sprintf(`{
			"effective_at": %q,
			"postings": [
				{"account":"equity:opening-balances","amount":"%s","currency":"BRL"},
				{"account":"liabilities:users:1","amount":"-%s","currency":"BRL"}
			]}`, effectiveAt.Format(time.RFC3339), amount, amount)
		if code := a.do("POST", "/v1/transactions", body, nil, nil); code != http.StatusCreated {
			t.Fatalf("commit returned %d", code)
		}
	}

	post(day1, "100.00")
	var afterFirst struct {
		Seq int64 `json:"seq"`
	}
	a.do("GET", "/healthz", "", nil, &afterFirst)

	// Three days later, a late settlement describing day 2 arrives.
	*a.now = base.Add(72 * time.Hour)
	post(day1.Add(24*time.Hour), "25.00")

	balanceAt := func(query string) string {
		t.Helper()
		var out struct {
			Balance string `json:"balance"`
		}
		if code := a.do("GET", "/v1/accounts/liabilities:users:1/balance"+query, "", nil, &out); code != http.StatusOK {
			t.Fatalf("balance%s returned %d", query, code)
		}
		return out.Balance
	}

	if got := balanceAt(""); got != "125.00" {
		t.Errorf("current balance = %s, want 125.00", got)
	}
	if got := balanceAt("?as_of_effective=" + day1.Add(12*time.Hour).Format(time.RFC3339)); got != "100.00" {
		t.Errorf("balance effective mid day 1 = %s, want 100.00", got)
	}
	if got := balanceAt(fmt.Sprintf("?as_of_seq=%d", afterFirst.Seq)); got != "100.00" {
		t.Errorf("balance as recorded on day 1 = %s, want 100.00", got)
	}
}

func TestRevertOverHTTP(t *testing.T) {
	a := newAPI(t)
	a.openAccount("equity:opening-balances", "credit", true)
	a.openAccount("liabilities:users:1", "credit", false)

	var committed struct {
		TransactionID string `json:"transaction_id"`
	}
	a.do("POST", "/v1/transactions", `{"postings":[
		{"account":"equity:opening-balances","amount":"40.00","currency":"BRL"},
		{"account":"liabilities:users:1","amount":"-40.00","currency":"BRL"}
	]}`, nil, &committed)

	path := "/v1/transactions/" + committed.TransactionID + "/revert"
	if code := a.do("POST", path, `{"reason":"chargeback"}`, nil, nil); code != http.StatusCreated {
		t.Fatalf("revert status = %d, want 201", code)
	}
	// An empty body is allowed: every field of a reversal has a default.
	var problem httpapi.Problem
	if code := a.do("POST", path, "", nil, &problem); code != http.StatusConflict {
		t.Fatalf("second revert status = %d, want 409", code)
	}
	if !strings.HasSuffix(problem.Type, "already-reverted") {
		t.Errorf("problem = %+v", problem)
	}

	var balance struct {
		Balance string `json:"balance"`
	}
	a.do("GET", "/v1/accounts/liabilities:users:1/balance", "", nil, &balance)
	if balance.Balance != "0.00" {
		t.Errorf("balance after reversal = %s, want 0.00", balance.Balance)
	}
}

func TestEntriesPagination(t *testing.T) {
	a := newAPI(t)
	a.openAccount("equity:opening-balances", "credit", true)
	a.openAccount("liabilities:users:1", "credit", false)
	for i := 0; i < 10; i++ {
		a.do("POST", "/v1/transactions", `{"postings":[
			{"account":"equity:opening-balances","amount":"1.00","currency":"BRL"},
			{"account":"liabilities:users:1","amount":"-1.00","currency":"BRL"}
		]}`, nil, nil)
	}

	type page struct {
		Entries []struct {
			Seq   int64 `json:"seq"`
			Index int   `json:"index"`
		} `json:"entries"`
		Next *struct {
			AfterSeq   int64 `json:"after_seq"`
			AfterIndex int   `json:"after_index"`
		} `json:"next"`
	}

	var (
		total int
		query = "?limit=6"
	)
	for i := 0; i < 20; i++ {
		var p page
		if code := a.do("GET", "/v1/entries"+query, "", nil, &p); code != http.StatusOK {
			t.Fatalf("entries status = %d", code)
		}
		total += len(p.Entries)
		if p.Next == nil {
			break
		}
		query = fmt.Sprintf("?limit=6&after_seq=%d&after_index=%d", p.Next.AfterSeq, p.Next.AfterIndex)
	}
	if total != 20 {
		t.Errorf("paged through %d entries, want 20", total)
	}

	var scoped page
	a.do("GET", "/v1/accounts/liabilities:users:1/entries?limit=100", "", nil, &scoped)
	if len(scoped.Entries) != 10 {
		t.Errorf("account entries = %d, want 10", len(scoped.Entries))
	}
}

func TestEventsAndVerify(t *testing.T) {
	a := newAPI(t)
	a.openAccount("equity:opening-balances", "credit", true)
	a.openAccount("liabilities:users:1", "credit", false)
	a.do("POST", "/v1/transactions", `{"postings":[
		{"account":"equity:opening-balances","amount":"5.00","currency":"BRL"},
		{"account":"liabilities:users:1","amount":"-5.00","currency":"BRL"}
	]}`, nil, nil)

	var events struct {
		Events []struct {
			Seq      int64           `json:"seq"`
			Type     string          `json:"type"`
			Payload  json.RawMessage `json:"payload"`
			PrevHash string          `json:"prev_hash"`
			Hash     string          `json:"hash"`
		} `json:"events"`
	}
	if code := a.do("GET", "/v1/events", "", nil, &events); code != http.StatusOK {
		t.Fatalf("events status = %d", code)
	}
	if len(events.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(events.Events))
	}
	// The chain is visible to the caller: each event names the one before it.
	for i := 1; i < len(events.Events); i++ {
		if events.Events[i].PrevHash != events.Events[i-1].Hash {
			t.Errorf("event %d does not link to event %d", i+1, i)
		}
	}
	if !strings.Contains(string(events.Events[2].Payload), `"reference"`) &&
		!strings.Contains(string(events.Events[2].Payload), `"postings"`) {
		t.Errorf("payload was not passed through: %s", events.Events[2].Payload)
	}

	var verify struct {
		Verified bool  `json:"verified"`
		Seq      int64 `json:"seq"`
	}
	if code := a.do("GET", "/v1/verify", "", nil, &verify); code != http.StatusOK {
		t.Fatalf("verify status = %d", code)
	}
	if !verify.Verified || verify.Seq != 3 {
		t.Errorf("verify = %+v", verify)
	}
}

// TestErrorMapping pins the contract a client codes against: which failures
// are the caller's fault, which are retryable, and which are permanent.
func TestErrorMapping(t *testing.T) {
	a := newAPI(t)
	a.openAccount("equity:opening-balances", "credit", true)
	a.openAccount("liabilities:users:1", "credit", false)

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantType   string
	}{
		{
			name: "unknown account", method: "GET",
			path: "/v1/accounts/nowhere:at:all", wantStatus: 404,
			wantType: "account-not-found",
		},
		{
			name: "unknown transaction", method: "GET",
			path:       "/v1/transactions/01920000-0000-7000-8000-000000000001",
			wantStatus: 404, wantType: "transaction-not-found",
		},
		{
			name: "malformed transaction id", method: "GET",
			path: "/v1/transactions/not-a-uuid", wantStatus: 400, wantType: "invalid-id",
		},
		{
			name: "account already exists", method: "POST", path: "/v1/accounts",
			body:       `{"name":"liabilities:users:1","currency":"BRL","normal":"credit"}`,
			wantStatus: 409, wantType: "account-exists",
		},
		{
			name: "unbalanced transaction", method: "POST", path: "/v1/transactions",
			body: `{"postings":[
				{"account":"equity:opening-balances","amount":"10.00","currency":"BRL"},
				{"account":"liabilities:users:1","amount":"-9.00","currency":"BRL"}
			]}`,
			wantStatus: 400, wantType: "invalid-transaction",
		},
		{
			name: "insufficient funds", method: "POST", path: "/v1/transactions",
			body: `{"postings":[
				{"account":"liabilities:users:1","amount":"10.00","currency":"BRL"},
				{"account":"equity:opening-balances","amount":"-10.00","currency":"BRL"}
			]}`,
			wantStatus: 422, wantType: "insufficient-funds",
		},
		{
			name: "posting to an unknown account", method: "POST", path: "/v1/transactions",
			body: `{"postings":[
				{"account":"equity:opening-balances","amount":"10.00","currency":"BRL"},
				{"account":"nowhere:at:all","amount":"-10.00","currency":"BRL"}
			]}`,
			wantStatus: 404, wantType: "account-not-found",
		},
		{
			name: "amount with too many decimals", method: "POST", path: "/v1/transactions",
			body: `{"postings":[
				{"account":"equity:opening-balances","amount":"10.005","currency":"BRL"},
				{"account":"liabilities:users:1","amount":"-10.005","currency":"BRL"}
			]}`,
			wantStatus: 400, wantType: "invalid-amount",
		},
		{
			name: "effective time far in the future", method: "POST", path: "/v1/transactions",
			body: `{"effective_at":"2030-01-01T00:00:00Z","postings":[
				{"account":"equity:opening-balances","amount":"10.00","currency":"BRL"},
				{"account":"liabilities:users:1","amount":"-10.00","currency":"BRL"}
			]}`,
			wantStatus: 422, wantType: "effective-out-of-range",
		},
		{
			name: "unknown field", method: "POST", path: "/v1/accounts",
			body:       `{"name":"assets:x","currency":"BRL","normal":"debit","surprise":true}`,
			wantStatus: 400, wantType: "bad-request",
		},
		{
			name: "malformed json", method: "POST", path: "/v1/accounts",
			body: `{`, wantStatus: 400, wantType: "bad-request",
		},
		{
			name: "bad timestamp in query", method: "GET",
			path:       "/v1/accounts/liabilities:users:1/balance?as_of_effective=yesterday",
			wantStatus: 400, wantType: "bad-request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var problem httpapi.Problem
			code := a.do(tt.method, tt.path, tt.body, nil, &problem)
			if code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (problem %+v)", code, tt.wantStatus, problem)
			}
			if !strings.HasSuffix(problem.Type, tt.wantType) {
				t.Errorf("type = %q, want a suffix of %q", problem.Type, tt.wantType)
			}
			if problem.Status != tt.wantStatus {
				t.Errorf("problem.Status = %d, want %d", problem.Status, tt.wantStatus)
			}
			if problem.Title == "" {
				t.Error("problem has no title")
			}
		})
	}
}

func TestBodySizeLimit(t *testing.T) {
	a := newAPI(t)
	huge := `{"name":"assets:x","currency":"BRL","normal":"debit","metadata":{"k":"` +
		strings.Repeat("v", httpapi.MaxBodyBytes+1) + `"}}`
	if code := a.do("POST", "/v1/accounts", huge, nil, nil); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

// --- the driving port ---

// stubLedger implements [httpapi.Ledger] with nothing behind it. Every method
// is a field, so a test can make one of them behave badly and leave the rest
// alone.
//
// Its existence is the point of the port: the transport is exercisable with no
// store, no database, and no events -- including on failures a real ledger will
// not produce on demand.
type stubLedger struct {
	id      string
	commit  func(context.Context, app.CommitCommand) (app.Result, error)
	balance func(context.Context, domain.BalanceQuery) (domain.Amount, error)
	account func(context.Context, domain.AccountName) (domain.Account, error)
	head    func(context.Context) (domain.Head, error)
}

func (s stubLedger) ID() string { return s.id }

func (s stubLedger) OpenAccount(context.Context, app.OpenAccountCommand) (domain.Account, app.Result, error) {
	return domain.Account{}, app.Result{}, nil
}

func (s stubLedger) Commit(ctx context.Context, cmd app.CommitCommand) (app.Result, error) {
	if s.commit != nil {
		return s.commit(ctx, cmd)
	}
	return app.Result{}, nil
}

func (s stubLedger) Revert(context.Context, app.RevertCommand) (app.Result, error) {
	return app.Result{}, nil
}

func (s stubLedger) Balance(ctx context.Context, q domain.BalanceQuery) (domain.Amount, error) {
	if s.balance != nil {
		return s.balance(ctx, q)
	}
	return domain.Zero(domain.MustCurrency("BRL")), nil
}

func (s stubLedger) Entries(context.Context, domain.EntryQuery) ([]domain.Entry, error) {
	return nil, nil
}

func (s stubLedger) Account(ctx context.Context, name domain.AccountName) (domain.Account, error) {
	if s.account != nil {
		return s.account(ctx, name)
	}
	return domain.Account{Name: name, Currency: domain.MustCurrency("BRL"), Normal: domain.Credit}, nil
}

func (s stubLedger) Accounts(context.Context, domain.AccountName) ([]domain.Account, error) {
	return nil, nil
}

func (s stubLedger) Transaction(context.Context, domain.ID) (domain.RecordedTransaction, error) {
	return domain.RecordedTransaction{}, nil
}

func (s stubLedger) Head(ctx context.Context) (domain.Head, error) {
	if s.head != nil {
		return s.head(ctx)
	}
	return domain.Head{}, nil
}

func (s stubLedger) Events(context.Context, int64, int) ([]domain.Event, error) {
	return nil, nil
}

func (s stubLedger) Verify(context.Context) (domain.Head, error) {
	return domain.Head{}, nil
}

var _ httpapi.Ledger = stubLedger{}

// TestTransportRunsWithoutTheCore is what the driving port buys. Every case
// here reaches a response with no store, no database and no event -- and two
// of them exercise failures a real ledger will not produce to order.
func TestTransportRunsWithoutTheCore(t *testing.T) {
	tests := []struct {
		name       string
		stub       stubLedger
		method     string
		path       string
		body       string
		wantStatus int
		wantType   string
	}{
		{
			name: "a lost write race is reported as retryable",
			// ErrConflict means another writer advanced the stream first. It is
			// the one error whose HTTP mapping cannot be checked against a real
			// ledger, because the single-writer lock exists to prevent it.
			stub: stubLedger{commit: func(context.Context, app.CommitCommand) (app.Result, error) {
				return app.Result{}, domain.ErrConflict
			}},
			method: "POST", path: "/v1/transactions",
			body: `{"postings":[
				{"account":"a:b","amount":"1.00","currency":"BRL"},
				{"account":"c:d","amount":"-1.00","currency":"BRL"}]}`,
			wantStatus: http.StatusServiceUnavailable, wantType: "write-conflict",
		},
		{
			name: "a broken chain is a server fault, and leaks nothing",
			stub: stubLedger{head: func(context.Context) (domain.Head, error) {
				return domain.Head{}, fmt.Errorf("%w: event 7 hashes wrong", domain.ErrChainBroken)
			}},
			method: "GET", path: "/healthz",
			wantStatus: http.StatusInternalServerError, wantType: "chain-broken",
		},
		{
			name:   "the happy path needs no ledger at all",
			stub:   stubLedger{id: "stub"},
			method: "GET", path: "/healthz",
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(httpapi.New(tt.stub, nil))
			defer srv.Close()

			var body *strings.Reader
			if tt.body == "" {
				body = strings.NewReader("")
			} else {
				body = strings.NewReader(tt.body)
			}
			req, err := http.NewRequest(tt.method, srv.URL+tt.path, body)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tt.method, tt.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantType == "" {
				return
			}
			var problem httpapi.Problem
			if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
				t.Fatalf("decoding problem: %v", err)
			}
			if !strings.HasSuffix(problem.Type, tt.wantType) {
				t.Errorf("type = %q, want a suffix of %q", problem.Type, tt.wantType)
			}
			// Detail belongs on a 4xx, where it tells the caller what they
			// did wrong, and must be absent on a 5xx, where the cause is ours
			// and the wrapped message can carry internals.
			switch {
			case problem.Status >= 500 && problem.Detail != "":
				t.Errorf("a server error leaked detail to the client: %q", problem.Detail)
			case problem.Status < 500 && problem.Detail == "":
				t.Error("a client error gave no detail to act on")
			}
		})
	}
}
