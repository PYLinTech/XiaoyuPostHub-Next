package store

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

func TestSchemaCheckAcceptsFreshAndMigratedDatabases(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "migrated"}[migrated], func(t *testing.T) {
			db := openTestDB(t)
			if migrated {
				rewindToBaselineSchema(t, db)
				if err := db.initSchema(); err != nil {
					t.Fatal(err)
				}
			}
			differences, err := db.schemaDifferences(testContext())
			if err != nil || len(differences) != 0 {
				t.Fatalf("normal schema must match: differences=%v err=%v", differences, err)
			}
		})
	}
}

func TestStartupLogsSchemaDriftAndContinues(t *testing.T) {
	for _, tc := range []struct{ name, sql, want string }{
		{"missing_index", `DROP INDEX idx_mailboxes_purge`, "缺少 index idx_mailboxes_purge"},
		{"changed_index", `DROP INDEX idx_users_group; CREATE INDEX idx_users_group ON users(account)`, "定义不一致 index idx_users_group"},
		{"changed_table", `ALTER TABLE upload_tasks DROP COLUMN expected_checksum`, "定义不一致 table upload_tasks"},
		{"missing_table", `DROP TABLE mail_unbind_requests`, "缺少 table mail_unbind_requests"},
		{"extra_object", `CREATE TABLE extra_probe (id INTEGER PRIMARY KEY)`, "额外对象 table extra_probe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			path := db.Path()
			mustExec(t, db, `INSERT INTO system_config (key, value, updated_at) VALUES ('schema-probe', 'keep', 1)`)
			if _, err := db.W().Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			before, err := schemaDefinitions(testContext(), db.R())
			if err != nil {
				t.Fatal(err)
			}
			version := mustUserVersion(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			reopened, err := Open(path)
			if err != nil {
				t.Fatalf("schema drift must not block startup: %v", err)
			}
			defer reopened.Close()
			if !strings.Contains(output.String(), tc.want) || !strings.Contains(output.String(), "继续启动") {
				t.Fatalf("missing diagnostic: %s", output.String())
			}
			after, err := schemaDefinitions(testContext(), reopened.R())
			if err != nil {
				t.Fatal(err)
			}
			assertSameSchema(t, before, after)
			if len(before) != len(after) {
				t.Fatal("schema check changed object count")
			}
			var value string
			mustQuery(t, reopened, `SELECT value FROM system_config WHERE key = 'schema-probe'`, &value)
			if value != "keep" || mustUserVersion(t, reopened) != version {
				t.Fatal("schema check changed data or version")
			}
		})
	}
}

func TestSchemaCheckCanonicalization(t *testing.T) {
	want, err := canonicalSchemaSQL("table", `CREATE TABLE IF NOT EXISTS sample (
name TEXT NOT NULL DEFAULT 'MiXeD, (text)', id INTEGER PRIMARY KEY, CHECK (id >= 0)) STRICT`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := canonicalSchemaSQL("table", `create table "sample" (
/* columns appended in a different order */ id integer primary key,
CHECK (id >= 0), name text not null default 'MiXeD, (text)') strict`)
	if err != nil || got != want {
		t.Fatalf("equivalent definitions differ: %v\n%s\n%s", err, want, got)
	}
	for _, ddl := range []string{
		`CREATE TABLE sample (name TEXT NOT NULL DEFAULT 'mixed, (text)', id INTEGER PRIMARY KEY, CHECK (id >= 0)) STRICT`,
		`CREATE TABLE sample (name TEXT NOT NULL DEFAULT 'MiXeD, (text)', id INTEGER PRIMARY KEY, CHECK (id > 0)) STRICT`,
		`CREATE TABLE sample (name TEXT NOT NULL DEFAULT 'MiXeD, (text)', id INTEGER PRIMARY KEY, CHECK (id >= 0))`,
		`CREATE TABLE sample (name TEXT DEFAULT 'MiXeD, (text)', id INTEGER PRIMARY KEY, CHECK (id >= 0)) STRICT`,
		`CREATE TABLE sample (name TEXT NOT NULL DEFAULT 'MiXeD, (text)', id INTEGER PRIMARY KEY REFERENCES parent(id), CHECK (id >= 0)) STRICT`,
	} {
		changed, err := canonicalSchemaSQL("table", ddl)
		if err != nil {
			t.Fatal(err)
		}
		if changed == want {
			t.Fatalf("ignored meaningful change: %s", ddl)
		}
	}
	first, err := canonicalSchemaSQL("index", `CREATE UNIQUE INDEX IF NOT EXISTS probe ON sample(id, name) WHERE name != ''`)
	if err != nil {
		t.Fatal(err)
	}
	second, err := canonicalSchemaSQL("index", `create unique index probe on sample (id,name) where name!=''`)
	if err != nil || first != second {
		t.Fatalf("index formatting mismatch: %v", err)
	}
	reversed, err := canonicalSchemaSQL("index", `CREATE UNIQUE INDEX probe ON sample(name, id) WHERE name != ''`)
	if err != nil || first == reversed {
		t.Fatalf("index column order must matter: %v", err)
	}
}

func TestSchemaCheckHonorsCancellation(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.schemaDifferences(ctx); err == nil {
		t.Fatal("cancelled comparison must return an error")
	}
}

func TestSchemaCheckFailureOnlyLogs(t *testing.T) {
	db := openTestDB(t)
	if err := db.R().Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	db.logSchemaDifferences()
	if !strings.Contains(output.String(), "结构校验未完成，继续启动") {
		t.Fatalf("missing warning: %s", output.String())
	}
}
