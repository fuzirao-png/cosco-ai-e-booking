package growth

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestReadLegacySQLiteAndQueuedProfile(t *testing.T) {
	ctx := context.Background()
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "previous.sqlite3"))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE leads (id TEXT PRIMARY KEY, stage TEXT NOT NULL, profile TEXT NOT NULL);
		CREATE TABLE calls (seq INTEGER PRIMARY KEY, lead_id TEXT NOT NULL REFERENCES leads(id));
		CREATE TABLE profile_jobs (call_seq INTEGER PRIMARY KEY REFERENCES calls(seq), updates TEXT NOT NULL);
		CREATE TABLE bookings (quote_id TEXT PRIMARY KEY, lead_id TEXT NOT NULL REFERENCES leads(id),
			reference TEXT NOT NULL UNIQUE, payload TEXT NOT NULL);
		CREATE TABLE outreach_opt_out (lead_id TEXT PRIMARY KEY REFERENCES leads(id));
		INSERT INTO leads VALUES ('harbor','verified','{"schema_version":1,"revision":3,"current_blocker":null,"route_interest":"上海 → 新加坡","evidence":{}}');
		INSERT INTO calls VALUES (19,'harbor');
		INSERT INTO profile_jobs VALUES (19,'{"updates":[],"transcript":[{"call_seq":19,"utterance_seq":2,"quote":"我卡在等候船期。"}]}');
		INSERT INTO bookings VALUES ('TEST','harbor','BK-TEST','{"id":"TEST"}');
		INSERT INTO outreach_opt_out VALUES ('harbor');`)
	closeErr := db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	store, err := OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.applyPending(ctx, "harbor"); err != nil {
		t.Fatal(err)
	}
	stage, profile, err := readProfile(ctx, store.db, "harbor")
	if err != nil || stage != "verified" || profile.Revision != 4 || profile.RouteInterest == nil ||
		*profile.RouteInterest != "上海 → 新加坡" || profile.CurrentBlocker == nil ||
		*profile.CurrentBlocker != "等候船期" || profile.Evidence["current_blocker"].CallSeq != 19 {
		t.Fatalf("legacy profile changed unexpectedly: %s %+v (%v)", stage, profile, err)
	}
	if optedOut, err := store.optedOut(ctx, "harbor"); err != nil || !optedOut {
		t.Fatalf("legacy opt-out = %v (%v)", optedOut, err)
	}
	var ref string
	if err = store.db.QueryRow("SELECT reference FROM bookings WHERE quote_id='TEST'").Scan(&ref); err != nil || ref != "BK-TEST" {
		t.Fatalf("legacy booking = %q (%v)", ref, err)
	}
}

func TestOlderProfileJobCannotReplaceNewEvidence(t *testing.T) {
	ctx := context.Background()
	store := makeStore(t)
	old, err := store.openCall(ctx, "sail")
	if err != nil {
		t.Fatal(err)
	}
	newer, err := store.openCall(ctx, "sail")
	if err != nil {
		t.Fatal(err)
	}
	for _, sequence := range []int64{newer, old} {
		value := "更早"
		if sequence == newer {
			value = "较新"
		}
		if _, err = store.enqueue(ctx, sequence, profileJob{
			Updates: []profileUpdate{{"route_interest", &value, Evidence{sequence, 1, value}}},
		}); err != nil {
			t.Fatal(err)
		}
		if err = store.applyPending(ctx, ""); err != nil {
			t.Fatal(err)
		}
	}
	_, profile, err := readProfile(ctx, store.db, "sail")
	if err != nil || profile.Revision != 1 || profile.RouteInterest == nil ||
		*profile.RouteInterest != "较新" || profile.Evidence["route_interest"].CallSeq != newer {
		t.Fatalf("stale update overwrote recent evidence: %+v (%v)", profile, err)
	}
}
