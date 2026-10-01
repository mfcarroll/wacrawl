package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatListCountsFromCoveringIndex(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "chatlist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	// ListChats' message_count.
	rows, err := st.db.QueryContext(ctx, `explain query plan
select c.jid, (select count(*) from messages m where m.chat_jid = c.jid and m.deleted_at is null)
from chats c where c.deleted_at is null`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plan, "\n"); !strings.Contains(got, "COVERING INDEX idx_messages_chat_live") {
		t.Fatalf("chat list must count messages from the index; plan:\n%s", got)
	}
}

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
