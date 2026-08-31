package store

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	containerName := ""
	if databaseURL == "" {
		containerName = fmt.Sprintf("dashboard-test-postgres-%d", os.Getpid())
		cmd := exec.Command("docker", "run", "--rm", "-d", "--name", containerName,
			"-e", "POSTGRES_PASSWORD=dashboard_test_password",
			"-e", "POSTGRES_DB=dashboard_test",
			"-p", "127.0.0.1::5432", "postgres:16")
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "start postgres container: %v: %s\n", err, output)
			os.Exit(1)
		}
		portOutput, err := exec.Command("docker", "port", containerName, "5432/tcp").Output()
		if err != nil {
			fmt.Fprintln(os.Stderr, "read postgres container port:", err)
			os.Exit(1)
		}
		address := strings.TrimSpace(string(portOutput))
		_, port, found := strings.Cut(address, ":")
		if !found {
			fmt.Fprintln(os.Stderr, "unexpected postgres port:", address)
			os.Exit(1)
		}
		databaseURL = "postgres://postgres:dashboard_test_password@127.0.0.1:" + port + "/dashboard_test?sslmode=disable"
	}

	var err error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		testPool, err = pgxpool.New(ctx, databaseURL)
		if err == nil {
			err = testPool.Ping(ctx)
		}
		if err == nil {
			break
		}
		if testPool != nil {
			testPool.Close()
			testPool = nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect to test postgres:", err)
		os.Exit(1)
	}
	if err := Migrate(ctx, testPool); err != nil {
		fmt.Fprintln(os.Stderr, "migrate test postgres:", err)
		os.Exit(1)
	}

	code := m.Run()
	testPool.Close()
	if containerName != "" {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
	}
	os.Exit(code)
}

func TestMigrationsAreIdempotentAndApplyOnlyNewFiles(t *testing.T) {
	resetDatabase(t)
	ctx := context.Background()
	first := fstest.MapFS{
		"proof/001.sql": &fstest.MapFile{Data: []byte("CREATE TABLE migration_proof (id int);")},
	}
	if err := runMigrations(ctx, testPool, first, "proof/*.sql"); err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(ctx, testPool, first, "proof/*.sql"); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second := fstest.MapFS{
		"proof/001.sql": first["proof/001.sql"],
		"proof/002.sql": &fstest.MapFile{Data: []byte("ALTER TABLE migration_proof ADD COLUMN note text;")},
	}
	if err := runMigrations(ctx, testPool, second, "proof/*.sql"); err != nil {
		t.Fatalf("add migration: %v", err)
	}

	var count int
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations WHERE name LIKE 'proof/%'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("proof migration count = %d, want 2", count)
	}
	var columnExists bool
	if err := testPool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'migration_proof' AND column_name = 'note'
		)`).Scan(&columnExists); err != nil {
		t.Fatal(err)
	}
	if !columnExists {
		t.Fatal("new migration did not add note column")
	}
	if _, err := testPool.Exec(ctx, "DROP TABLE migration_proof"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationFailureNamesFileAndRollsBack(t *testing.T) {
	bad := fstest.MapFS{
		"broken/009_bad.sql": &fstest.MapFile{Data: []byte("THIS IS NOT SQL;")},
	}
	err := runMigrations(context.Background(), testPool, bad, "broken/*.sql")
	if err == nil || !strings.Contains(err.Error(), "broken/009_bad.sql") {
		t.Fatalf("error = %v, want failing filename", err)
	}
	var recorded bool
	if err := testPool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = 'broken/009_bad.sql')",
	).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded {
		t.Fatal("failed migration was recorded")
	}
}

func TestSchemaHasExactlyTheDesignedColumns(t *testing.T) {
	expected := map[string][]string{
		"ventures": {
			"slug", "title", "field", "status", "stage", "kind", "description",
			"monetization", "abandoned_reason", "abandoned_at_stage", "url", "repo_url",
			"created_at", "first_seen_at", "last_seen_at", "missing_since",
			"snapshot_generated_at", "hidden", "design_url",
		},
		"ingest_snapshots": {
			"id", "snapshot_id", "generated_at", "received_at", "venture_count",
			"applied", "skipped_stale", "warnings", "source", "token_records",
		},
		"venture_tokens": {
			"slug", "runs", "failed_runs", "backfilled_runs", "total", "fresh",
			"cost_usd", "cost_measured_runs", "seconds",
		},
		"role_tokens": {
			"role", "runs", "failed_runs", "backfilled_runs", "total", "fresh",
			"cost_usd", "cost_measured_runs", "seconds",
		},
		"company_tokens": {
			"id", "records", "first_ts", "last_ts", "runs", "failed_runs",
			"backfilled_runs", "live_runs", "total", "fresh", "unattributed_runs",
			"unattributed_total", "unattributed_fresh", "cost_usd",
			"cost_measured_runs", "seconds", "snapshot_generated_at",
		},
	}
	for table, want := range expected {
		rows, err := testPool.Query(context.Background(), `
			SELECT column_name
			FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1
			ORDER BY ordinal_position`, table)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			got = append(got, column)
		}
		rows.Close()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s columns = %v, want %v", table, got, want)
		}
	}

	var ventureIndexes int
	if err := testPool.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'ventures'",
	).Scan(&ventureIndexes); err != nil {
		t.Fatal(err)
	}
	if ventureIndexes != 1 {
		t.Fatalf("venture index count = %d, want primary key only", ventureIndexes)
	}
}

func TestDesignURLRoundTrips(t *testing.T) {
	resetDatabase(t)
	s := New(testPool)
	stage := "DESIGN_REVIEW"
	designURL := "https://design.example.invalid/canvas/venture-1"
	venture := testVenture("design-review", "in_progress")
	venture.Stage = &stage
	venture.DesignURL = &designURL
	snapshot := Snapshot{
		SnapshotID: "design-review", GeneratedAt: time.Now().UTC(), Ventures: []Venture{venture},
	}
	if _, err := s.ApplySnapshot(context.Background(), snapshot, snapshot.Ventures, []string{venture.Slug}, nil); err != nil {
		t.Fatal(err)
	}
	ventures, err := s.ListVentures(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ventures) != 1 || ventures[0].Stage != stage || ventures[0].DesignURL != designURL {
		t.Fatalf("ventures = %#v", ventures)
	}
}

func TestReplaySameSnapshotIsNoOp(t *testing.T) {
	resetDatabase(t)
	s := New(testPool)
	generatedAt := time.Now().Add(-time.Minute).UTC()
	venture := testVenture("replay", "in_progress")
	snapshot := Snapshot{SnapshotID: "replay-id", GeneratedAt: generatedAt, Source: "test", Ventures: []Venture{venture}}

	first, err := s.ApplySnapshot(context.Background(), snapshot, []Venture{venture}, []string{venture.Slug}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ApplySnapshot(context.Background(), snapshot, []Venture{venture}, []string{venture.Slug}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Applied != 1 || second.Status != "unchanged" {
		t.Fatalf("results = %#v then %#v", first, second)
	}
	assertCount(t, "ventures", 1)
	assertCount(t, "ingest_snapshots", 1)
}

func TestRemovedVentureIsRetainedAndMarkedMissing(t *testing.T) {
	resetDatabase(t)
	s := New(testPool)
	firstTime := time.Now().Add(-2 * time.Minute).UTC()
	one := testVenture("one", "in_progress")
	two := testVenture("two", "deployed")
	first := Snapshot{SnapshotID: "all", GeneratedAt: firstTime, Ventures: []Venture{one, two}}
	if _, err := s.ApplySnapshot(context.Background(), first, first.Ventures, []string{"one", "two"}, nil); err != nil {
		t.Fatal(err)
	}

	second := Snapshot{SnapshotID: "one-only", GeneratedAt: firstTime.Add(time.Minute), Ventures: []Venture{one}}
	if _, err := s.ApplySnapshot(context.Background(), second, second.Ventures, []string{"one"}, nil); err != nil {
		t.Fatal(err)
	}

	assertCount(t, "ventures", 2)
	var missing bool
	if err := testPool.QueryRow(context.Background(), "SELECT missing_since IS NOT NULL FROM ventures WHERE slug = 'two'").Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if !missing {
		t.Fatal("removed venture missing_since is null")
	}
}

func TestStaleRetryCannotResurrectMissingVenture(t *testing.T) {
	resetDatabase(t)
	s := New(testPool)
	seenAt := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Microsecond)
	removed := testVenture("removed", "deployed")
	survivor := testVenture("survivor", "in_progress")
	first := Snapshot{
		SnapshotID: "before-removal", GeneratedAt: seenAt,
		Ventures: []Venture{removed, survivor},
	}
	if _, err := s.ApplySnapshot(context.Background(), first, first.Ventures, []string{"removed", "survivor"}, nil); err != nil {
		t.Fatal(err)
	}

	omittedAt := seenAt.Add(20 * time.Minute)
	omission := Snapshot{
		SnapshotID: "after-removal", GeneratedAt: omittedAt,
		Ventures: []Venture{survivor},
	}
	if _, err := s.ApplySnapshot(context.Background(), omission, omission.Ventures, []string{"survivor"}, nil); err != nil {
		t.Fatal(err)
	}

	staleRemoved := testVenture("removed", "in_progress")
	delayed := Snapshot{
		SnapshotID: "delayed-before-removal", GeneratedAt: seenAt.Add(10 * time.Minute),
		Ventures: []Venture{staleRemoved, survivor},
	}
	result, err := s.ApplySnapshot(context.Background(), delayed, delayed.Ventures, []string{"removed", "survivor"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != 0 || result.SkippedStale != 2 {
		t.Fatalf("result = %#v", result)
	}

	var status string
	var missing bool
	var snapshotGeneratedAt time.Time
	if err := testPool.QueryRow(context.Background(), `
		SELECT status, missing_since IS NOT NULL, snapshot_generated_at
		FROM ventures WHERE slug = 'removed'`,
	).Scan(&status, &missing, &snapshotGeneratedAt); err != nil {
		t.Fatal(err)
	}
	if status != "deployed" || !missing || !snapshotGeneratedAt.Equal(omittedAt) {
		t.Fatalf("status = %q, missing = %t, snapshot_generated_at = %s", status, missing, snapshotGeneratedAt)
	}
}

func TestStatusCanMoveFromDeployedToAbandoned(t *testing.T) {
	resetDatabase(t)
	s := New(testPool)
	generatedAt := time.Now().Add(-2 * time.Minute).UTC()
	deployed := testVenture("backwards", "deployed")
	if _, err := s.ApplySnapshot(context.Background(), Snapshot{SnapshotID: "deployed", GeneratedAt: generatedAt, Ventures: []Venture{deployed}}, []Venture{deployed}, []string{deployed.Slug}, nil); err != nil {
		t.Fatal(err)
	}
	var firstSeen, firstLastSeen time.Time
	if err := testPool.QueryRow(context.Background(),
		"SELECT first_seen_at, last_seen_at FROM ventures WHERE slug = 'backwards'",
	).Scan(&firstSeen, &firstLastSeen); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	abandoned := testVenture("backwards", "abandoned")
	reason := "The experiment disproved the premise."
	abandoned.AbandonedReason = &reason
	if _, err := s.ApplySnapshot(context.Background(), Snapshot{SnapshotID: "abandoned", GeneratedAt: generatedAt.Add(time.Minute), Ventures: []Venture{abandoned}}, []Venture{abandoned}, []string{abandoned.Slug}, nil); err != nil {
		t.Fatal(err)
	}

	var status string
	var secondFirstSeen, secondLastSeen time.Time
	if err := testPool.QueryRow(context.Background(),
		"SELECT status, first_seen_at, last_seen_at FROM ventures WHERE slug = 'backwards'",
	).Scan(&status, &secondFirstSeen, &secondLastSeen); err != nil {
		t.Fatal(err)
	}
	if status != "abandoned" {
		t.Fatalf("status = %q, want abandoned", status)
	}
	if !secondFirstSeen.Equal(firstSeen) {
		t.Fatalf("first_seen_at changed from %s to %s", firstSeen, secondFirstSeen)
	}
	if !secondLastSeen.After(firstLastSeen) {
		t.Fatalf("last_seen_at did not advance from %s to %s", firstLastSeen, secondLastSeen)
	}
}

func TestStaleGeneratedAtIsSkippedAndCounted(t *testing.T) {
	resetDatabase(t)
	s := New(testPool)
	newerTime := time.Now().Add(-time.Minute).UTC()
	newer := testVenture("stale", "deployed")
	if _, err := s.ApplySnapshot(context.Background(), Snapshot{SnapshotID: "newer", GeneratedAt: newerTime, Ventures: []Venture{newer}}, []Venture{newer}, []string{newer.Slug}, nil); err != nil {
		t.Fatal(err)
	}
	stale := testVenture("stale", "in_progress")
	result, err := s.ApplySnapshot(context.Background(), Snapshot{SnapshotID: "stale", GeneratedAt: newerTime.Add(-time.Hour), Ventures: []Venture{stale}}, []Venture{stale}, []string{stale.Slug}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied != 0 || result.SkippedStale != 1 {
		t.Fatalf("result = %#v", result)
	}
	var status string
	if err := testPool.QueryRow(context.Background(), "SELECT status FROM ventures WHERE slug = 'stale'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "deployed" {
		t.Fatalf("status = %q, want deployed", status)
	}
}

func TestTokenBlockReplacesAllAggregateTables(t *testing.T) {
	resetDatabase(t)
	s := New(testPool)
	generatedAt := time.Now().Add(-time.Minute).UTC()
	venture := testVenture("tokened", "in_progress")
	snapshot := Snapshot{
		SnapshotID: "tokens-first", GeneratedAt: generatedAt, Ventures: []Venture{venture},
		Tokens: testTokenBlock(generatedAt),
	}
	result, err := s.ApplySnapshot(context.Background(), snapshot, snapshot.Ventures, []string{venture.Slug}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.TokenRecords != 3 || len(result.Warnings) != 0 {
		t.Fatalf("result = %#v", result)
	}
	assertCount(t, "venture_tokens", 1)
	assertCount(t, "role_tokens", 2)
	assertCount(t, "company_tokens", 1)

	var records, total, fresh, unattributed int64
	if err := testPool.QueryRow(context.Background(), `
		SELECT records, total, fresh, unattributed_total FROM company_tokens`).Scan(&records, &total, &fresh, &unattributed); err != nil {
		t.Fatal(err)
	}
	if records != 3 || total != 300 || fresh != 270 || unattributed != 100 {
		t.Fatalf("company tokens = records:%d total:%d fresh:%d unattributed:%d", records, total, fresh, unattributed)
	}

	replacement := testTokenBlock(generatedAt.Add(time.Minute))
	replacement.ByVenture[0].Total = 500
	replacement.ByVenture[0].Fresh = 450
	replacement.Company.Total = 600
	replacement.Company.Fresh = 550
	replacement.Company.Runs = 4
	replacement.Company.UnattributedRuns = 2
	second := Snapshot{
		SnapshotID: "tokens-replacement", GeneratedAt: generatedAt.Add(time.Minute), Ventures: []Venture{venture}, Tokens: replacement,
	}
	if _, err := s.ApplySnapshot(context.Background(), second, second.Ventures, []string{venture.Slug}, nil); err != nil {
		t.Fatal(err)
	}
	var replacedTotal int64
	if err := testPool.QueryRow(context.Background(), "SELECT total FROM venture_tokens WHERE slug = 'tokened'").Scan(&replacedTotal); err != nil {
		t.Fatal(err)
	}
	if replacedTotal != 500 {
		t.Fatalf("venture total = %d, want replacement total", replacedTotal)
	}
	var snapshotRecords int
	if err := testPool.QueryRow(context.Background(), "SELECT token_records FROM ingest_snapshots WHERE snapshot_id = 'tokens-first'").Scan(&snapshotRecords); err != nil {
		t.Fatal(err)
	}
	if snapshotRecords != 3 {
		t.Fatalf("token_records = %d, want 3", snapshotRecords)
	}
}

func TestTokenToleranceRetainsExistingRows(t *testing.T) {
	resetDatabase(t)
	s := New(testPool)
	generatedAt := time.Now().Add(-time.Minute).UTC()
	venture := testVenture("tokened", "in_progress")
	first := Snapshot{SnapshotID: "tokens-first", GeneratedAt: generatedAt, Ventures: []Venture{venture}, Tokens: testTokenBlock(generatedAt)}
	if _, err := s.ApplySnapshot(context.Background(), first, first.Ventures, []string{venture.Slug}, nil); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		tokens  *TokenBlock
		warning string
	}{
		{name: "absent", warning: "tokens: absent from payload"},
		{name: "empty", tokens: &TokenBlock{}, warning: "tokens: empty block ignored"},
		{name: "company missing", tokens: &TokenBlock{Records: 3}, warning: "tokens: empty block ignored"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := Snapshot{
				SnapshotID: fmt.Sprintf("tokens-tolerance-%d", i), GeneratedAt: generatedAt.Add(time.Duration(i+1) * time.Minute),
				Ventures: []Venture{venture}, Tokens: tc.tokens,
			}
			result, err := s.ApplySnapshot(context.Background(), snapshot, snapshot.Ventures, []string{venture.Slug}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(result.Warnings, ";"), tc.warning) {
				t.Fatalf("warnings = %#v, want %q", result.Warnings, tc.warning)
			}
			var total int64
			if err := testPool.QueryRow(context.Background(), "SELECT total FROM company_tokens").Scan(&total); err != nil {
				t.Fatal(err)
			}
			if total != 300 {
				t.Fatalf("company total = %d, want retained 300", total)
			}
		})
	}
}

func TestStaleTokenBlockIsSkippedButIdentityMismatchOnlyWarns(t *testing.T) {
	resetDatabase(t)
	s := New(testPool)
	generatedAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	venture := testVenture("tokened", "in_progress")
	first := Snapshot{SnapshotID: "tokens-first", GeneratedAt: generatedAt, Ventures: []Venture{venture}, Tokens: testTokenBlock(generatedAt)}
	if _, err := s.ApplySnapshot(context.Background(), first, first.Ventures, []string{venture.Slug}, nil); err != nil {
		t.Fatal(err)
	}

	stale := Snapshot{SnapshotID: "tokens-stale", GeneratedAt: generatedAt.Add(-time.Minute), Ventures: []Venture{venture}, Tokens: testTokenBlock(generatedAt)}
	result, err := s.ApplySnapshot(context.Background(), stale, stale.Ventures, []string{venture.Slug}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.SkippedStale != 2 { // the venture and the token block are independently stale.
		t.Fatalf("stale result = %#v", result)
	}

	mismatched := testTokenBlock(generatedAt.Add(time.Minute))
	mismatched.Company.Total++
	mismatched.LedgerVersion = 2
	snapshot := Snapshot{SnapshotID: "tokens-mismatch", GeneratedAt: generatedAt.Add(time.Minute), Ventures: []Venture{venture}, Tokens: mismatched}
	result, err = s.ApplySnapshot(context.Background(), snapshot, snapshot.Ventures, []string{venture.Slug}, nil)
	if err != nil {
		t.Fatal(err)
	}
	joinedWarnings := strings.Join(result.Warnings, ";")
	for _, want := range []string{"tokens: company total != attributed + unattributed", "tokens: unsupported ledger version 2"} {
		if !strings.Contains(joinedWarnings, want) {
			t.Fatalf("warnings = %#v, want %q", result.Warnings, want)
		}
	}
	var total int64
	if err := testPool.QueryRow(context.Background(), "SELECT total FROM company_tokens").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 301 {
		t.Fatalf("mismatched company total = %d, want applied 301", total)
	}
}

func resetDatabase(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), "TRUNCATE ventures, ingest_snapshots, venture_tokens, role_tokens, company_tokens RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), "DELETE FROM schema_migrations WHERE name LIKE 'proof/%'"); err != nil {
		t.Fatal(err)
	}
}

func testVenture(slug, status string) Venture {
	description := "A test venture."
	return Venture{
		Slug: slug, Title: "Test venture", Field: "development", Status: status,
		Description: &description,
	}
}

func assertCount(t *testing.T, table string, want int) {
	t.Helper()
	query := ""
	switch table {
	case "ventures":
		query = "SELECT count(*) FROM ventures"
	case "ingest_snapshots":
		query = "SELECT count(*) FROM ingest_snapshots"
	case "venture_tokens":
		query = "SELECT count(*) FROM venture_tokens"
	case "role_tokens":
		query = "SELECT count(*) FROM role_tokens"
	case "company_tokens":
		query = "SELECT count(*) FROM company_tokens"
	default:
		t.Fatalf("unsupported table %q", table)
	}
	var count int
	if err := testPool.QueryRow(context.Background(), query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%s count = %d, want %d", table, count, want)
	}
}

func testTokenBlock(at time.Time) *TokenBlock {
	return &TokenBlock{
		LedgerVersion: 1, Records: 3, FirstTS: &at, LastTS: &at,
		Company: &CompanyTokens{
			TokenAggregate: TokenAggregate{Runs: 3, FailedRuns: 1, BackfilledRuns: 1, Total: 300, Fresh: 270, CostUSD: 1.25, CostMeasuredRuns: 1, Seconds: 42},
			LiveRuns:       2, UnattributedRuns: 1, UnattributedTotal: 100, UnattributedFresh: 90,
		},
		ByVenture: []VentureTokens{{
			Slug: "tokened", TokenAggregate: TokenAggregate{Runs: 2, FailedRuns: 1, BackfilledRuns: 1, Total: 200, Fresh: 180, CostUSD: 1.25, CostMeasuredRuns: 1, Seconds: 24},
		}},
		ByRole: []RoleTokens{
			{Role: "developer", TokenAggregate: TokenAggregate{Runs: 2, FailedRuns: 1, BackfilledRuns: 1, Total: 200, Fresh: 180, CostUSD: 1.25, CostMeasuredRuns: 1, Seconds: 24}},
			{Role: "ceo", TokenAggregate: TokenAggregate{Runs: 1, Total: 100, Fresh: 90, Seconds: 18}},
		},
	}
}
