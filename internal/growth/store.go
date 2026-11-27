package growth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	_ "modernc.org/sqlite"
)

type Evidence struct {
	CallSeq      int64  `json:"call_seq"`
	UtteranceSeq int    `json:"utterance_seq"`
	Quote        string `json:"quote"`
}

func compareEvidence(a, b Evidence) int {
	if a.CallSeq != b.CallSeq {
		if a.CallSeq < b.CallSeq {
			return -1
		}
		return 1
	}
	return a.UtteranceSeq - b.UtteranceSeq
}

type Profile struct {
	SchemaVersion  int                 `json:"schema_version"`
	Revision       int                 `json:"revision"`
	CurrentBlocker *string             `json:"current_blocker"`
	RouteInterest  *string             `json:"route_interest"`
	Evidence       map[string]Evidence `json:"evidence"`
}

type profileUpdate struct {
	Field    string   `json:"field"`
	Value    *string  `json:"value"`
	Evidence Evidence `json:"evidence"`
}

type profileJob struct {
	Updates    []profileUpdate `json:"updates"`
	Transcript []Evidence      `json:"transcript"`
}

type Store struct{ db *sql.DB }

func OpenStore(ctx context.Context, path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0755); err != nil {
		return nil, err
	}
	slashPath := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	opts := url.Values{"_pragma": {"foreign_keys(1)", "busy_timeout(5000)"}, "_txlock": {"immediate"}}
	dsn := url.URL{Scheme: "file", Path: slashPath, RawQuery: opts.Encode()}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db}
	if err = store.init(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize SQLite: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode=WAL;
		CREATE TABLE IF NOT EXISTS leads (id TEXT PRIMARY KEY, stage TEXT NOT NULL, profile TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS calls (seq INTEGER PRIMARY KEY, lead_id TEXT NOT NULL REFERENCES leads(id));
		CREATE TABLE IF NOT EXISTS profile_jobs (call_seq INTEGER PRIMARY KEY REFERENCES calls(seq), updates TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS bookings (quote_id TEXT PRIMARY KEY, lead_id TEXT NOT NULL REFERENCES leads(id),
			reference TEXT NOT NULL UNIQUE, payload TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS outreach_opt_out (lead_id TEXT PRIMARY KEY REFERENCES leads(id));`); err != nil {
		return err
	}
	for _, lead := range leads {
		profile := Profile{SchemaVersion: 1, Evidence: map[string]Evidence{}}
		if lead.ID == "meridian" {
			profile.RouteInterest = stringPtr("宁波 → 洛杉矶")
		}
		raw, err := json.Marshal(profile)
		if err != nil {
			return err
		}
		if _, err = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO leads VALUES (?, ?, ?)", lead.ID, lead.Stage, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readProfile(ctx context.Context, reader queryer, id string) (string, Profile, error) {
	var stage, raw string
	profile := Profile{SchemaVersion: 1, Evidence: map[string]Evidence{}}
	if err := reader.QueryRowContext(ctx, "SELECT stage,profile FROM leads WHERE id=?", id).Scan(&stage, &raw); err != nil {
		return "", profile, err
	}
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		return "", profile, err
	}
	if profile.SchemaVersion != 1 {
		return "", profile, fmt.Errorf("unsupported profile version %d", profile.SchemaVersion)
	}
	if profile.Evidence == nil {
		profile.Evidence = map[string]Evidence{}
	}
	return stage, profile, nil
}

func (s *Store) openCall(ctx context.Context, leadID string) (int64, error) {
	result, err := s.db.ExecContext(ctx, "INSERT INTO calls(lead_id) VALUES (?)", leadID)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) advance(ctx context.Context, leadID, before, after string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE leads SET stage=? WHERE id=? AND stage=?", after, leadID, before)
	return err
}

func (s *Store) optedOut(ctx context.Context, leadID string) (bool, error) {
	var yes bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM outreach_opt_out WHERE lead_id=?)", leadID).Scan(&yes)
	return yes, err
}

func (s *Store) optOut(ctx context.Context, leadID string) error {
	_, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO outreach_opt_out VALUES (?)", leadID)
	return err
}

func (s *Store) book(ctx context.Context, leadID string, quote Quote) (string, error) {
	raw, err := json.Marshal(quote)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO bookings VALUES (?, ?, ?, ?)", quote.ID, leadID, "BK-"+quote.ID, string(raw)); err != nil {
		return "", err
	}
	var ref, owner string
	if err = tx.QueryRowContext(ctx, "SELECT reference,lead_id FROM bookings WHERE quote_id=?", quote.ID).Scan(&ref, &owner); err != nil {
		return "", err
	}
	if owner != leadID {
		return "", errors.New("booking owner mismatch")
	}
	return ref, tx.Commit()
}

func (s *Store) enqueue(ctx context.Context, seq int64, job profileJob) (bool, error) {
	if len(job.Updates) == 0 && len(job.Transcript) == 0 {
		return false, nil
	}
	if job.Updates == nil {
		job.Updates = []profileUpdate{}
	}
	if job.Transcript == nil {
		job.Transcript = []Evidence{}
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return false, err
	}
	_, err = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO profile_jobs VALUES (?, ?)", seq, string(raw))
	return err == nil, err
}

var blockerPattern = regexp.MustCompile(`认证.{0,12}(没准备好|未准备|材料不齐|有问题)|(卡在|卡点是|难点是)[：: ]?([^。！？]{1,80})`)

func (s *Store) applyPending(ctx context.Context, leadID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT c.seq, c.lead_id, j.updates FROM profile_jobs j
		JOIN calls c ON c.seq=j.call_seq WHERE (?='' OR c.lead_id=?) ORDER BY c.seq`, leadID, leadID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type queuedJob struct {
		seq         int64
		leadID, raw string
	}
	var jobs []queuedJob
	for rows.Next() {
		var job queuedJob
		if err = rows.Scan(&job.seq, &job.leadID, &job.raw); err != nil {
			return err
		}
		jobs = append(jobs, job)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, job := range jobs {
		if err = applyProfileJob(ctx, tx, job.leadID, job.raw); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM profile_jobs WHERE call_seq=?", job.seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func applyProfileJob(ctx context.Context, tx *sql.Tx, leadID, raw string) error {
	var job profileJob
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		return err
	}
	_, profile, err := readProfile(ctx, tx, leadID)
	if err != nil {
		return err
	}
	updates := job.Updates
	for _, evidence := range job.Transcript {
		if match := blockerPattern.FindStringSubmatch(evidence.Quote); match != nil {
			blocker := match[3]
			if blocker == "" {
				blocker = "资质认证材料待准备"
			}
			updates = append(updates, profileUpdate{"current_blocker", &blocker, evidence})
		}
	}
	slices.SortStableFunc(updates, func(a, b profileUpdate) int { return compareEvidence(a.Evidence, b.Evidence) })
	changed := false
	for _, update := range updates {
		previous, exists := profile.Evidence[update.Field]
		if exists && compareEvidence(update.Evidence, previous) <= 0 {
			continue
		}
		switch update.Field {
		case "current_blocker":
			profile.CurrentBlocker = update.Value
		case "route_interest":
			profile.RouteInterest = update.Value
		default:
			return fmt.Errorf("unknown profile field %q", update.Field)
		}
		profile.Evidence[update.Field] = update.Evidence
		changed = true
	}
	if !changed {
		return nil
	}
	profile.Revision++
	payload, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE leads SET profile=? WHERE id=?", string(payload), leadID)
	return err
}
