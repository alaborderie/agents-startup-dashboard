package store

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestQuotaTokensUpsertAndCascadeWithSession(t *testing.T) {
	ctx := context.Background()
	s := New(testPool)
	id := "quota-cascade-test"
	defer s.DeleteSession(ctx, id)
	if err := s.CreateSession(ctx, id, time.Now().Add(time.Hour), "test"); err != nil {
		t.Fatal(err)
	}
	for _, blob := range [][]byte{{1, 2, 3}, {4, 5}} {
		if err := s.PutQuotaToken(ctx, id, blob); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.QuotaToken(ctx, id)
		if err != nil || !ok || !bytes.Equal(got, blob) {
			t.Fatalf("quota token = %v %v %v, want %v", got, ok, err, blob)
		}
	}
	if err := s.DeleteSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.QuotaToken(ctx, id); err != nil || ok {
		t.Fatalf("token survived logout: %v %v", ok, err)
	}
	if err := s.DeleteQuotaToken(ctx, id); err != nil {
		t.Fatal(err)
	}
}
