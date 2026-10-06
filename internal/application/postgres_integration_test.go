//go:build integration

package application_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"jungle-gaming/internal/application"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/domain"
	"jungle-gaming/internal/platform/postgres"
	"jungle-gaming/migrations"
)

type instance struct {
	service  *application.FinancialService
	store    *postgres.Store
	database *postgres.Database
}
type fixture struct {
	url string
	instance
	ctx context.Context
}

func startInstance(t *testing.T, connection string) instance {
	t.Helper()
	var node instance
	app := fx.New(fx.NopLogger, fx.Supply(config.Config{DatabaseURL: connection, DependencyTimeout: 5 * time.Second}), postgres.Module,
		fx.Provide(postgres.NewStore, application.NewFinancialService), fx.Populate(&node.service, &node.store, &node.database))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return node
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	connection := os.Getenv("TEST_DATABASE_URL")
	if connection == "" {
		t.Fatal("TEST_DATABASE_URL is required; start the real PostgreSQL container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if err := admin.Ping(ctx); err != nil {
		t.Fatalf("real PostgreSQL unavailable: %v", err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "part3_test_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(connection)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	f := fixture{url: u.String(), ctx: ctx}
	f.instance = startInstance(t, f.url)
	if err := migrations.Apply(ctx, f.database.Pool(), "up"); err != nil {
		t.Fatal(err)
	}
	return f
}

func money(t *testing.T, amount string) domain.Money {
	t.Helper()
	m, err := domain.NewMoney(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func open(t *testing.T, f fixture, player, amount string) *domain.Wallet {
	t.Helper()
	w, err := f.service.OpenWallet(f.ctx, player, money(t, amount), "open-correlation")
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func command(t *testing.T, w *domain.Wallet, kind domain.TransactionKind, amount, external string) application.ProcessCommand {
	t.Helper()
	s := w.Snapshot()
	return application.ProcessCommand{ProviderID: "provider-a", ExternalTransactionID: external, IdempotencyKey: "custom:" + external,
		WalletID: s.ID, PlayerID: s.PlayerID, RoundID: "round-1", GameID: "game-1", Kind: kind, Money: money(t, amount), CorrelationID: "correlation-1"}
}

func count(t *testing.T, f fixture, query string, args ...any) int {
	t.Helper()
	var result int
	if err := f.database.Pool().QueryRow(f.ctx, query, args...).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertBalance(t *testing.T, f fixture, walletID string, units, version int64) {
	t.Helper()
	w, err := f.store.GetWallet(f.ctx, walletID)
	if err != nil {
		t.Fatal(err)
	}
	if state := w.Snapshot(); state.Balance.MinorUnits() != units || state.Version != version {
		t.Fatalf("balance/version = %d/%d, want %d/%d", state.Balance.MinorUnits(), state.Version, units, version)
	}
	var total int64
	if err := f.database.Pool().QueryRow(f.ctx, `SELECT COALESCE(SUM(CASE direction WHEN 'CREDIT' THEN amount_minor_units::numeric ELSE -amount_minor_units::numeric END),0)::bigint FROM wallet_ledger_entries WHERE wallet_id=$1`, walletID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != units {
		t.Fatalf("ledger sum %d differs from wallet %d", total, units)
	}
}

func TestPostgresWalletOpening(t *testing.T) {
	f := newFixture(t)
	zero := open(t, f, "zero-player", "0.00")
	assertBalance(t, f, zero.Snapshot().ID, 0, 1)
	if count(t, f, `SELECT count(*) FROM wager_transactions`) != 0 || count(t, f, `SELECT count(*) FROM wallet_ledger_entries`) != 0 || count(t, f, `SELECT count(*) FROM outbox_events`) != 0 {
		t.Fatal("zero opening created financial records")
	}
	w := open(t, f, "positive-player", "100.00")
	assertBalance(t, f, w.Snapshot().ID, 10000, 1)
	if count(t, f, `SELECT count(*) FROM wager_transactions WHERE kind='OPENING' AND status='PROCESSED' AND provider_id IS NULL AND result_wallet_version=1`) != 1 {
		t.Fatal("missing internal opening")
	}
	if count(t, f, `SELECT count(*) FROM outbox_events`) != 2 {
		t.Fatal("missing opening events")
	}
	if _, err := f.service.OpenWallet(f.ctx, "positive-player", money(t, "100.00"), "correlation"); !errors.Is(err, postgres.ErrConflict) {
		t.Fatalf("duplicate opening: %v", err)
	}
}

func TestPostgresBetWinLossReplayAndConflicts(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "player", "100.00")
	bet := command(t, w, domain.Bet, "25.00", "bet-1")
	first, err := f.service.Process(f.ctx, bet)
	if err != nil {
		t.Fatal(err)
	}
	if first.IdempotentReplay || first.Transaction.Status != domain.Processed || first.Transaction.Result.Balance.MinorUnits() != 7500 {
		t.Fatalf("wrong BET result: %+v", first)
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	if count(t, f, `SELECT count(*) FROM outbox_events`) != 4 {
		t.Fatal("BET missing events")
	}
	if _, err := f.service.Process(f.ctx, command(t, w, domain.Win, "10.00", "win-1")); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, w.Snapshot().ID, 8500, 3)
	before, err := f.store.GetWallet(f.ctx, w.Snapshot().ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Process(f.ctx, command(t, w, domain.Loss, "0.00", "loss-1")); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.GetWallet(f.ctx, w.Snapshot().ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Snapshot() != after.Snapshot() {
		t.Fatal("LOSS changed wallet state")
	}
	if count(t, f, `SELECT count(*) FROM wallet_ledger_entries`) != 3 || count(t, f, `SELECT count(*) FROM outbox_events`) != 7 {
		t.Fatal("LOSS created ledger or balance event")
	}
	// A new instance has its own memory/pool; replay survives process lifetime.
	restarted := startInstance(t, f.url)
	replay, err := restarted.service.Process(f.ctx, bet)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.IdempotentReplay || replay.Transaction.Data.ID != first.Transaction.Data.ID || replay.Transaction.Result.Balance.MinorUnits() != 7500 || replay.Transaction.Result.WalletVersion != 2 {
		t.Fatal("replay did not preserve original result")
	}
	changed := bet
	changed.Money = money(t, "26.00")
	if _, err := f.service.Process(f.ctx, changed); !errors.Is(err, postgres.ErrConflict) {
		t.Fatalf("payload conflict: %v", err)
	}
	changed = bet
	changed.IdempotencyKey = "another-key"
	if _, err := f.service.Process(f.ctx, changed); !errors.Is(err, postgres.ErrConflict) {
		t.Fatalf("external identity conflict: %v", err)
	}
	if _, err := f.store.GetTransaction(f.ctx, "other-provider", first.Transaction.Data.ID); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatal("provider isolation failed")
	}
	ledger, err := f.store.GetLedger(f.ctx, w.Snapshot().ID)
	if err != nil {
		t.Fatal(err)
	}
	var debit *domain.LedgerEntryState
	for _, entry := range ledger {
		state := entry.Snapshot()
		if state.Direction == domain.DebitDirection {
			debit = &state
		}
	}
	if debit == nil || debit.Money.MinorUnits() != 2500 || debit.BalanceBefore.MinorUnits() != 10000 || debit.BalanceAfter.MinorUnits() != 7500 {
		t.Fatal("incorrect debit ledger")
	}
	assertBalance(t, f, w.Snapshot().ID, 8500, 3)
	if count(t, f, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1`, first.Transaction.Data.ID) != 1 {
		t.Fatal("replay duplicated event")
	}
}

func TestPostgresConstraintsAndAppendOnly(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "player", "100.00")
	for _, sql := range []string{
		`UPDATE wallets SET balance_minor_units=-1`,
		`UPDATE wallets SET balance_minor_units=balance_minor_units+1, version=version+1`,
		`UPDATE wallets SET version=version+1`,
		`UPDATE wallets SET version=0`,
		`UPDATE wallets SET currency='USD'`,
		`UPDATE wallet_ledger_entries SET amount_minor_units=1`,
		`DELETE FROM wallet_ledger_entries`,
		`TRUNCATE wallet_ledger_entries`,
		`UPDATE wager_transactions SET result_balance_minor_units=1`,
		`DELETE FROM wager_transactions`,
		`UPDATE outbox_events SET payload='{}'::jsonb`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := f.database.Pool().Exec(f.ctx, sql)
			var pgError *pgconn.PgError
			if !errors.As(err, &pgError) {
				t.Fatalf("expected PostgreSQL rejection, got %v", err)
			}
		})
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 1)
}

func TestPostgresRollbackAllFinancialWrites(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "player", "100.00")
	_, err := f.database.Pool().Exec(f.ctx, `CREATE FUNCTION fail_test_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected outbox failure'; END; $$;
		CREATE TRIGGER fail_test_outbox BEFORE INSERT ON outbox_events FOR EACH ROW
		WHEN (NEW.event_type='WalletBalanceChanged') EXECUTE FUNCTION fail_test_event()`)
	if err != nil {
		t.Fatal(err)
	}
	bet := command(t, w, domain.Bet, "25.00", "bet-failure")
	if _, err := f.service.Process(f.ctx, bet); err == nil {
		t.Fatal("injected failure did not abort processing")
	}
	assertBalance(t, f, w.Snapshot().ID, 10000, 1)
	if count(t, f, `SELECT count(*) FROM wager_transactions WHERE kind<>'OPENING'`) != 0 || count(t, f, `SELECT count(*) FROM wallet_ledger_entries`) != 1 || count(t, f, `SELECT count(*) FROM outbox_events`) != 2 {
		t.Fatal("partial financial commit")
	}
	if _, err := f.database.Pool().Exec(f.ctx, `DROP TRIGGER fail_test_outbox ON outbox_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Process(f.ctx, bet); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
}

func TestPostgresTwoIndependentBets(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "player", "100.00")
	nodes := []instance{startInstance(t, f.url), startInstance(t, f.url)}
	commands := []application.ProcessCommand{command(t, w, domain.Bet, "80.00", "bet-a"), command(t, w, domain.Bet, "80.00", "bet-b")}
	start := make(chan struct{})
	results := make(chan application.ProcessResult, 2)
	failures := make(chan error, 2)
	var group sync.WaitGroup
	for i := range nodes {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			result, err := nodes[i].service.Process(f.ctx, commands[i])
			if err != nil {
				failures <- err
				return
			}
			results <- result
		}(i)
	}
	close(start)
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	processed, rejected := 0, 0
	for result := range results {
		switch result.Transaction.Status {
		case domain.Processed:
			processed++
		case domain.Rejected:
			rejected++
			if result.Transaction.FailureCode != domain.FailureInsufficientBalance {
				t.Fatal("wrong rejection code")
			}
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("processed/rejected = %d/%d", processed, rejected)
	}
	assertBalance(t, f, w.Snapshot().ID, 2000, 2)
	if count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE direction='DEBIT'`) != 1 {
		t.Fatal("duplicate debit")
	}
	if count(t, f, `SELECT count(*) FROM outbox_events WHERE event_type='WagerTransactionRejected'`) != 1 {
		t.Fatal("missing rejection event")
	}
	for _, c := range commands {
		result, err := f.service.Process(f.ctx, c)
		if err != nil || !result.IdempotentReplay {
			t.Fatalf("terminal replay: %v", err)
		}
	}
	assertBalance(t, f, w.Snapshot().ID, 2000, 2)
}

func TestPostgresFiftyConcurrentReplays(t *testing.T) {
	f := newFixture(t)
	w := open(t, f, "player", "100.00")
	nodes := []instance{f.instance, startInstance(t, f.url), startInstance(t, f.url)}
	c := command(t, w, domain.Bet, "25.00", "same-bet")
	start := make(chan struct{})
	results := make(chan application.ProcessResult, 50)
	failures := make(chan error, 50)
	var group sync.WaitGroup
	for i := 0; i < 50; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			r, err := nodes[i%len(nodes)].service.Process(f.ctx, c)
			if err != nil {
				failures <- err
				return
			}
			results <- r
		}(i)
	}
	close(start)
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	originals, total := 0, 0
	id := ""
	for r := range results {
		total++
		if !r.IdempotentReplay {
			originals++
		}
		if id == "" {
			id = r.Transaction.Data.ID
		}
		if r.Transaction.Data.ID != id || r.Transaction.Result.Balance.MinorUnits() != 7500 {
			t.Fatal("inconsistent replay result")
		}
	}
	if originals != 1 || total != 50 {
		t.Fatalf("originals/total = %d/%d", originals, total)
	}
	assertBalance(t, f, w.Snapshot().ID, 7500, 2)
	if count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE direction='DEBIT'`) != 1 || count(t, f, `SELECT count(*) FROM wager_transactions WHERE kind='BET'`) != 1 || count(t, f, `SELECT count(*) FROM outbox_events`) != 4 {
		t.Fatal("duplicate persistent effects")
	}
}

func TestPostgresIndependentWalletAndCancellation(t *testing.T) {
	f := newFixture(t)
	first := open(t, f, "player-1", "100.00")
	second := open(t, f, "player-2", "100.00")
	locker, err := f.database.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	if _, err := locker.Exec(f.ctx, `SELECT id FROM wallets WHERE id=$1 FOR UPDATE`, first.Snapshot().ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	if _, err := f.service.Process(ctx, command(t, second, domain.Bet, "25.00", "independent")); err != nil {
		t.Fatalf("another wallet was blocked: %v", err)
	}
	blocked, cancelBlocked := context.WithTimeout(f.ctx, 100*time.Millisecond)
	defer cancelBlocked()
	if _, err := f.service.Process(blocked, command(t, first, domain.Bet, "25.00", "cancelled")); err == nil {
		t.Fatal("wallet lock ignored cancellation")
	}
	if err := locker.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	assertBalance(t, f, first.Snapshot().ID, 10000, 1)
	assertBalance(t, f, second.Snapshot().ID, 7500, 2)
	if count(t, f, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id='cancelled'`) != 0 {
		t.Fatal("cancelled operation was committed")
	}
}

func TestPostgresMigrationUpDownUp(t *testing.T) {
	f := newFixture(t)
	if err := migrations.Apply(f.ctx, f.database.Pool(), "up"); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(f.ctx, f.database.Pool(), "down"); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(f.ctx, f.database.Pool(), "up"); err != nil {
		t.Fatal(err)
	}
	open(t, f, "player", "0.00")
}

// This helper runs in a separate OS process and owns its own Fx graph and pool.
func TestPostgresProcessHelper(t *testing.T) {
	if os.Getenv("PART3_PROCESS_HELPER") != "1" {
		return
	}
	node := startInstance(t, os.Getenv("TEST_DATABASE_URL"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wallet, err := node.store.GetWallet(ctx, os.Getenv("PART3_PROCESS_WALLET"))
	if err != nil {
		t.Fatal(err)
	}
	c := command(t, wallet, domain.Bet, "25.00", "process-shared-bet")
	fmt.Fprintln(os.Stdout, "READY")
	var token [1]byte
	if _, err := io.ReadFull(os.Stdin, token[:]); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	failures := make(chan error, 17)
	for i := 0; i < 17; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := node.service.Process(ctx, c)
			if err != nil {
				failures <- err
				return
			}
			if result.Transaction.Status != domain.Processed || result.Transaction.Result.Balance.MinorUnits() != 7500 {
				failures <- fmt.Errorf("wrong child replay result")
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

func TestPostgresThreeIndependentProcesses(t *testing.T) {
	f := newFixture(t)
	wallet := open(t, f, "process-player", "100.00")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	type child struct {
		cmd    *exec.Cmd
		input  io.WriteCloser
		output *bufio.Reader
		stderr *bytes.Buffer
	}
	children := make([]child, 3)
	for i := range children {
		cmd := exec.CommandContext(f.ctx, executable, "-test.run=^TestPostgresProcessHelper$", "-test.timeout=40s")
		// Remove old values instead of adding duplicate environment keys.
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "TEST_DATABASE_URL=") && !strings.HasPrefix(entry, "PART3_PROCESS_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "TEST_DATABASE_URL="+f.url, "PART3_PROCESS_HELPER=1", "PART3_PROCESS_WALLET="+wallet.Snapshot().ID)
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr := &bytes.Buffer{}
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		children[i] = child{cmd, input, bufio.NewReader(output), stderr}
	}
	for _, child := range children {
		ready, err := child.output.ReadString('\n')
		if err != nil || strings.TrimSpace(ready) != "READY" {
			t.Fatalf("child failed before barrier: %s, %v, %s", ready, err, child.stderr.String())
		}
	}
	for _, child := range children {
		if _, err := child.input.Write([]byte{'S'}); err != nil {
			t.Fatal(err)
		}
		child.input.Close()
	}
	for _, child := range children {
		output, err := io.ReadAll(child.output)
		if err != nil {
			t.Fatal(err)
		}
		if err := child.cmd.Wait(); err != nil {
			t.Fatalf("independent process failed: %v\n%s\n%s", err, output, child.stderr.String())
		}
	}
	assertBalance(t, f, wallet.Snapshot().ID, 7500, 2)
	if count(t, f, `SELECT count(*) FROM wager_transactions WHERE kind='BET'`) != 1 || count(t, f, `SELECT count(*) FROM wallet_ledger_entries WHERE direction='DEBIT'`) != 1 || count(t, f, `SELECT count(*) FROM outbox_events`) != 4 {
		t.Fatal("multiple processes duplicated financial effects")
	}
}
