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
			"snapshot_generated_at", "hidden",
		},
		"ingest_snapshots": {
			"id", "snapshot_id", "generated_at", "received_at", "venture_count",
			"applied", "skipped_stale", "warnings", "source",
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

func resetDatabase(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), "TRUNCATE ventures, ingest_snapshots RESTART IDENTITY"); err != nil {
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
