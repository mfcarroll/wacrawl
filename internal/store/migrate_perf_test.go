package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestReopenAtCurrentSchemaSkipsBackfills(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `insert into messages(source_pk, source_row_pk, event_id, chat_jid, msg_id, ts, from_me, raw_type, last_seen_at) values(7, 0, 'wa:7', 'chat', 'm7', 1, 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var rowPK, lastSeen int
	if err := st.db.QueryRowContext(ctx, `select source_row_pk, last_seen_at from messages where source_pk = 7`).Scan(&rowPK, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if rowPK != 0 || lastSeen != 0 {
		t.Fatalf("reopening a current archive rewrote rows: source_row_pk=%d last_seen_at=%d", rowPK, lastSeen)
	}
}
