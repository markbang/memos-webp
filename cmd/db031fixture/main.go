package main

import (
	"context"
	"fmt"
	"os"

	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db"
)

func main() {
	path := "/tmp/memos031.db"
	_ = os.Remove(path)
	p := &profile.Profile{Driver: "sqlite", DSN: path, Data: "/tmp/memos031-data", Version: "0.31.8"}
	if err := os.MkdirAll(p.Data, 0o755); err != nil {
		panic(err)
	}
	drv, err := db.NewDBDriver(p)
	if err != nil {
		panic(err)
	}
	st := store.New(drv, p)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		panic(errors.Wrap(err, "migrate"))
	}
	u, err := st.CreateUser(ctx, &store.User{Username: "bangwu", Role: store.RoleAdmin, Email: "", Nickname: "Bangwu", PasswordHash: "x"})
	if err != nil {
		panic(err)
	}
	if _, err := drv.GetDB().ExecContext(ctx, `UPDATE user SET email = NULL WHERE id = ?`, u.ID); err != nil {
		panic(err)
	}
	m1, err := st.CreateMemo(ctx, &store.Memo{UID: "memo001", CreatorID: u.ID, Content: "# public memo #tag", Visibility: store.Public, RowStatus: store.Normal})
	if err != nil {
		panic(err)
	}
	if _, err := st.CreateMemo(ctx, &store.Memo{UID: "memo002", CreatorID: u.ID, Content: "private", Visibility: store.Private, RowStatus: store.Normal}); err != nil {
		panic(err)
	}
	if _, err := st.UpsertReaction(ctx, &store.Reaction{CreatorID: u.ID, MemoID: m1.ID, ReactionType: "👍"}); err != nil {
		panic(err)
	}
	if _, err := drv.GetDB().ExecContext(ctx, `INSERT INTO user_setting (user_id, key, value) VALUES (?, 'MEMO_VIEWS', '{"memoViews":[{"name":"view1"}]}')`, u.ID); err != nil {
		panic(err)
	}
	var schema string
	_ = drv.GetDB().QueryRowContext(ctx, `SELECT value FROM system_setting WHERE name='BASIC'`).Scan(&schema)
	fmt.Println("fixture ready", path, "user", u.ID, "memo", m1.ID, "basic", schema)
}
