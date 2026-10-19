package store

import "testing"

// 将当前结构还原到 v7，供各旧版本迁移夹具共用。
func rewindTrafficBillingSchema(t *testing.T, db *DB) {
	t.Helper()
	mustExec(t, db, `DROP INDEX idx_traffic_billed_user`)
	for table, columns := range map[string][]string{
		"download_tickets": {"quota_scope", "quota_key", "billed_user_id", "billed_group_name", "share_id", "pickup_code", "resource_path"},
		"traffic_logs":     {"quota_scope", "billed_user_id", "billed_group_name"},
	} {
		for _, column := range columns {
			mustExec(t, db, "ALTER TABLE "+table+" DROP COLUMN "+column)
		}
	}
}

func TestShareTrafficMigrationKeepsLegacyTicketBilling(t *testing.T) {
	db := openTestDB(t)
	rewindShareDownloadSchema(t, db)
	rewindTrafficBillingSchema(t, db)
	rewindDirectLinkSchema(t, db)
	// Use an existing v7 record: new columns must have neutral defaults so refund
	// continues to use the original access actor's dated personal counter.
	mustExec(t, db, `INSERT INTO files(checksum,size_plain,pan_object_name,pan_size_wire,enc_algo,enc_chunk_log2,enc_nonce_prefix,enc_salt,dek_envelope,kek_key_id,status,created_at,updated_at) VALUES('legacy',1,'x',81,'AES-256-GCM',20,1,x'00',x'00','k1',1,1,1)`)
	mustExec(t, db, `INSERT INTO download_tickets(id,file_checksum,actor_type,user_id,group_name,purpose,delivery_mode,reserved_bytes,max_uses,expires_at,created_at) VALUES('legacy','legacy','user',42,'normal','download','proxy',81,10,2000000000,1)`)
	mustExec(t, db, `INSERT INTO traffic_logs(actor_type,user_id,group_name,action,bytes_wire,occurred_at) VALUES('user',42,'normal','download',81,1)`)
	mustExec(t, db, `PRAGMA user_version = 7`)
	if err := db.initSchema(); err != nil {
		t.Fatal(err)
	}
	ticket, err := GetTicket(testContext(), db.R(), "legacy")
	if err != nil || ticket.UserID != 42 || ticket.QuotaScope != "" || ticket.QuotaKey != "" {
		t.Fatalf("legacy ticket: %+v %v", ticket, err)
	}
	logs, _, err := ListTrafficLogs(testContext(), db.R(), TrafficFilter{})
	if err != nil || len(logs) != 1 || logs[0].UserID != 42 || logs[0].BytesWire != 81 {
		t.Fatalf("legacy logs: %+v %v", logs, err)
	}
	if err := db.initSchema(); err != nil {
		t.Fatal(err)
	}
}
